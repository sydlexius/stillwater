package rule

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/event"
	img "github.com/sydlexius/stillwater/internal/image"
	"github.com/sydlexius/stillwater/internal/nfo"
	"github.com/sydlexius/stillwater/internal/platform"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/internal/watcher"
)

// BulkExecutor runs bulk jobs asynchronously. Only one job runs at a time.
type BulkExecutor struct {
	bulkService     *BulkService
	artistService   *artist.Service
	orchestrator    *provider.Orchestrator
	pipeline        PipelineRunner
	snapshotService *nfo.SnapshotService
	platformService *platform.Service
	expectedWrites  *watcher.ExpectedWrites
	logger          *slog.Logger
	eventBus        *event.Bus

	mu        sync.Mutex
	cancelFn  context.CancelFunc
	currentID string
}

// SetEventBus sets the event bus for publishing bulk job events.
func (e *BulkExecutor) SetEventBus(bus *event.Bus) {
	e.eventBus = bus
}

// NewBulkExecutor creates a BulkExecutor.
func NewBulkExecutor(bulkService *BulkService, artistService *artist.Service, orchestrator *provider.Orchestrator, pipeline PipelineRunner, snapshotService *nfo.SnapshotService, platformService *platform.Service, expectedWrites *watcher.ExpectedWrites, logger *slog.Logger) *BulkExecutor {
	return &BulkExecutor{
		bulkService:     bulkService,
		artistService:   artistService,
		orchestrator:    orchestrator,
		pipeline:        pipeline,
		snapshotService: snapshotService,
		platformService: platformService,
		expectedWrites:  expectedWrites,
		logger:          logger.With(slog.String("component", "bulk-executor")),
	}
}

// Start begins executing a bulk job in a background goroutine.
func (e *BulkExecutor) Start(ctx context.Context, job *BulkJob) error {
	e.mu.Lock()
	if e.currentID != "" {
		e.mu.Unlock()
		return fmt.Errorf("a bulk job is already running: %s", e.currentID)
	}
	jobCtx, cancel := context.WithCancel(ctx)
	e.cancelFn = cancel
	e.currentID = job.ID
	e.mu.Unlock()

	go e.run(jobCtx, job)
	return nil
}

// Cancel stops the currently running bulk job.
func (e *BulkExecutor) Cancel() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.cancelFn == nil {
		return fmt.Errorf("no bulk job is running")
	}
	e.cancelFn()
	return nil
}

func (e *BulkExecutor) run(ctx context.Context, job *BulkJob) {
	defer func() {
		e.mu.Lock()
		e.cancelFn = nil
		e.currentID = ""
		e.mu.Unlock()
	}()

	now := time.Now().UTC()
	job.Status = BulkStatusRunning
	job.StartedAt = &now
	if err := e.bulkService.UpdateJob(ctx, job); err != nil {
		e.logger.Error("updating job start", "job_id", job.ID, "error", err)
		return
	}

	// Collect target artists: specific IDs if provided, otherwise all non-excluded
	var artists []artist.Artist
	if len(job.ArtistIDs) > 0 {
		for _, id := range job.ArtistIDs {
			if ctx.Err() != nil {
				e.finishJob(ctx, job, BulkStatusCanceled, "")
				return
			}
			a, err := e.artistService.GetByID(ctx, id)
			if err != nil {
				e.logger.Warn("skipping unknown artist in bulk job", "id", id, "error", err)
				continue
			}
			if !a.IsExcluded {
				artists = append(artists, *a)
			}
		}
	} else {
		const pageSize = 200
		params := artist.ListParams{Page: 1, PageSize: pageSize, Sort: "name"}

		for {
			if err := ctx.Err(); err != nil {
				e.finishJob(ctx, job, BulkStatusCanceled, fmt.Sprintf("bulk job canceled: %v", err))
				return
			}
			page, _, err := e.artistService.List(ctx, params)
			if err != nil {
				e.finishJob(ctx, job, BulkStatusFailed, fmt.Sprintf("listing artists: %v", err))
				return
			}
			if len(page) == 0 {
				break
			}
			for _, a := range page {
				if !a.IsExcluded {
					artists = append(artists, a)
				}
			}
			if len(page) < pageSize {
				break
			}
			params.Page++
		}
	}

	job.TotalItems = len(artists)
	_ = e.bulkService.UpdateJob(ctx, job)

	for i := range artists {
		if ctx.Err() != nil {
			e.finishJob(ctx, job, BulkStatusCanceled, "")
			return
		}

		a := &artists[i]
		item := &BulkJobItem{
			JobID:      job.ID,
			ArtistID:   a.ID,
			ArtistName: a.Name,
			Status:     BulkItemPending,
		}

		status, message := e.processArtist(ctx, a, job)
		item.Status = status
		item.Message = message

		if err := e.bulkService.CreateItem(ctx, item); err != nil {
			e.logger.Warn("recording job item", "artist", a.Name, "error", err)
		}

		job.ProcessedItems++
		switch status {
		case BulkItemFixed:
			job.FixedItems++
		case BulkItemSkipped:
			job.SkippedItems++
		case BulkItemFailed:
			job.FailedItems++
		}

		// Periodic progress update (every 10 items)
		if job.ProcessedItems%10 == 0 {
			_ = e.bulkService.UpdateJob(ctx, job)
		}
	}

	e.finishJob(ctx, job, BulkStatusCompleted, "")
}

func (e *BulkExecutor) processArtist(ctx context.Context, a *artist.Artist, job *BulkJob) (string, string) {
	switch job.Type {
	case BulkTypeFetchMetadata:
		return e.fetchMetadata(ctx, a, job.Mode)
	case BulkTypeFetchImages:
		return e.fetchImages(ctx, a, job.Mode)
	default:
		return BulkItemFailed, fmt.Sprintf("unknown job type: %s", job.Type)
	}
}

func (e *BulkExecutor) fetchMetadata(ctx context.Context, a *artist.Artist, mode string) (string, string) {
	if a.MusicBrainzID != "" && a.Biography != "" {
		return BulkItemSkipped, "already has MBID and biography"
	}

	result, err := e.orchestrator.FetchMetadata(ctx, a.MusicBrainzID, a.Name, a.ProviderIDMap())
	if err != nil {
		return BulkItemFailed, fmt.Sprintf("fetch failed: %v", err)
	}

	if result.Metadata == nil {
		return BulkItemSkipped, "no metadata returned"
	}

	changed := false

	if a.MusicBrainzID == "" && result.Metadata.MusicBrainzID != "" {
		if mode == BulkModeManual {
			return BulkItemSkipped, "manual mode: skipped MBID assignment"
		}
		a.MusicBrainzID = result.Metadata.MusicBrainzID
		changed = true
	}

	if a.Biography == "" && result.Metadata.Biography != "" {
		a.Biography = result.Metadata.Biography
		changed = true
	}

	if a.AudioDBID == "" && result.Metadata.AudioDBID != "" {
		a.AudioDBID = result.Metadata.AudioDBID
		changed = true
	}
	if a.DiscogsID == "" && result.Metadata.DiscogsID != "" {
		a.DiscogsID = result.Metadata.DiscogsID
		changed = true
	}
	if a.WikidataID == "" && result.Metadata.WikidataID != "" {
		a.WikidataID = result.Metadata.WikidataID
		changed = true
	}
	if a.DeezerID == "" && result.Metadata.DeezerID != "" {
		a.DeezerID = result.Metadata.DeezerID
		changed = true
	}
	if len(a.Genres) == 0 && len(result.Metadata.Genres) > 0 {
		a.Genres = result.Metadata.Genres
		changed = true
	}

	if !changed {
		return BulkItemSkipped, "no new metadata to apply"
	}

	if err := e.artistService.Update(ctx, a); err != nil {
		return BulkItemFailed, fmt.Sprintf("update failed: %v", err)
	}

	UpdateProviderFetchTimestamps(ctx, e.artistService, a.ID, result.AttemptedProviders, e.logger)

	if a.NFOExists {
		writeArtistNFO(ctx, a, e.snapshotService, e.expectedWrites, e.logger)
	}

	return BulkItemFixed, "metadata updated"
}

func (e *BulkExecutor) fetchImages(ctx context.Context, a *artist.Artist, mode string) (string, string) {
	if a.MusicBrainzID == "" {
		if mode == BulkModeManual || mode == BulkModeDisambiguate {
			return BulkItemSkipped, "no MBID"
		}
		results, err := e.orchestrator.Search(ctx, a.Name)
		if err != nil || len(results) == 0 {
			return BulkItemSkipped, "no MBID and provider search found nothing"
		}
		for _, r := range results {
			if r.MusicBrainzID != "" {
				a.MusicBrainzID = r.MusicBrainzID
				_ = e.artistService.Update(ctx, a)
				break
			}
		}
		if a.MusicBrainzID == "" {
			return BulkItemSkipped, "no MBID found from providers"
		}
	}

	needed := make(map[string]bool)
	if !a.ThumbExists {
		needed["thumb"] = true
	}
	if !a.FanartExists {
		needed["fanart"] = true
	}
	if !a.LogoExists {
		needed["logo"] = true
	}

	if len(needed) == 0 {
		return BulkItemSkipped, "all images present"
	}

	imgResult, err := e.orchestrator.FetchImages(ctx, a.MusicBrainzID, a.ProviderIDMap())
	if err != nil {
		return BulkItemFailed, fmt.Sprintf("image fetch failed: %v", err)
	}

	fixed := 0
	for imageType := range needed {
		if e.saveBestImage(ctx, a, imageType, imgResult) {
			fixed++
		}
	}

	if fixed == 0 {
		return BulkItemSkipped, "no suitable images found"
	}

	if err := e.artistService.Update(ctx, a); err != nil {
		return BulkItemFailed, fmt.Sprintf("update failed: %v", err)
	}

	return BulkItemFixed, fmt.Sprintf("saved %d image(s)", fixed)
}

func (e *BulkExecutor) saveBestImage(ctx context.Context, a *artist.Artist, imageType string, result *provider.FetchResult) bool {
	var candidates []provider.ImageResult
	for _, im := range result.Images {
		if string(im.Type) == imageType {
			candidates = append(candidates, im)
		}
	}
	if len(candidates) == 0 {
		return false
	}

	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Likes != candidates[j].Likes {
			return candidates[i].Likes > candidates[j].Likes
		}
		return (candidates[i].Width * candidates[i].Height) > (candidates[j].Width * candidates[j].Height)
	})

	// Pre-resolve naming and symlink config once (does not depend on candidate URL).
	useSymlinks := activeUseSymlinks(ctx, e.platformService)
	naming := existingImageFileNames(ctx, a.Path, imageType, e.platformService)
	for _, c := range candidates {
		meta := &img.ExifMeta{
			Source:  c.Source,
			Fetched: time.Now().UTC(),
			URL:     c.URL,
			Rule:    "",
			Mode:    "auto",
		}
		saved, err := SaveImageFromURL(ctx, a, imageType, c.URL, naming, useSymlinks, meta, e.platformService, e.logger)
		if err != nil {
			e.logger.Debug("image candidate failed", "url", c.URL, "error", err)
			continue
		}
		// Record provenance from the saved file so artist_images.source is populated.
		// Guard: artistService is nil in some test configurations.
		if e.artistService != nil && len(saved) > 0 && a.Path != "" {
			recordSavedImageProvenance(ctx, e.artistService, a.ID, imageType, filepath.Join(a.Path, saved[0]), e.logger)
		}
		return true
	}

	return false
}

func (e *BulkExecutor) finishJob(ctx context.Context, job *BulkJob, status, errMsg string) {
	now := time.Now().UTC()
	job.Status = status
	job.CompletedAt = &now
	job.Error = errMsg
	if err := e.bulkService.UpdateJob(ctx, job); err != nil {
		e.logger.Error("finishing bulk job", "job_id", job.ID, "error", err)
	}

	if e.eventBus != nil {
		e.eventBus.Publish(event.Event{
			Type: event.BulkCompleted,
			Data: map[string]any{
				"job_id":          job.ID,
				"type":            job.Type,
				"status":          status,
				"total_items":     job.TotalItems,
				"processed_items": job.ProcessedItems,
				"failed_items":    job.FailedItems,
			},
		})
	}
}
