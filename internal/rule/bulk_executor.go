package rule

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/event"
	"github.com/sydlexius/stillwater/internal/httpsafe"
	img "github.com/sydlexius/stillwater/internal/image"
	"github.com/sydlexius/stillwater/internal/nfo"
	"github.com/sydlexius/stillwater/internal/platform"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/internal/publish"
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
	publisher       *publish.Publisher
	logger          *slog.Logger
	eventBus        *event.Bus
	// httpClient is the HTTP client used by saveBestImage to download image
	// bytes from provider-supplied URLs. It defaults to an SSRF-safe client
	// backed by httpsafe.SafeTransport so the bulk-fix pipeline cannot be
	// coerced into fetching internal-network resources via a malicious or
	// compromised provider response. Tests override this field with a plain
	// *http.Client when exercising the loopback-bound httptest path.
	httpClient *http.Client
	// collision is the #2540 cross-artist backdrop check for saveBestImage.
	// Late-wired via SetCollisionGuard (mirroring SetEventBus) rather than added
	// to the already-nine-parameter constructor. Nil is a supported no-op state.
	collision *collisionGuard

	mu        sync.Mutex
	cancelFn  context.CancelFunc
	currentID string
}

// SetEventBus sets the event bus for publishing bulk job events.
func (e *BulkExecutor) SetEventBus(bus *event.Bus) {
	e.eventBus = bus
}

// SetCollisionGuard late-wires the #2540 cross-artist backdrop-collision seam
// for the bulk auto-fix path. Passing a nil notifier or indexer disables it.
func (e *BulkExecutor) SetCollisionGuard(notifier backdropCollisionNotifier, indexer fanartIdentityIndexer) {
	e.collision = newCollisionGuard(notifier, indexer, e.logger)
}

// NewBulkExecutor creates a BulkExecutor.
func NewBulkExecutor(bulkService *BulkService, artistService *artist.Service, orchestrator *provider.Orchestrator, pipeline PipelineRunner, snapshotService *nfo.SnapshotService, platformService *platform.Service, expectedWrites *watcher.ExpectedWrites, publisher *publish.Publisher, logger *slog.Logger) *BulkExecutor {
	return &BulkExecutor{
		bulkService:     bulkService,
		artistService:   artistService,
		orchestrator:    orchestrator,
		pipeline:        pipeline,
		snapshotService: snapshotService,
		platformService: platformService,
		expectedWrites:  expectedWrites,
		publisher:       publisher,
		logger:          logger.With(slog.String("component", "bulk-executor")),
		httpClient:      httpsafe.SafeClient(fetchTimeout),
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

//nolint:gocognit // Bulk worker drives per-artist progress through evaluate -> fix -> persist while watching context cancellation and accumulating counts; the loop body's status transitions and cancellation checkpoints share state (job pointer, counters, mu) that cannot be split without leaking the mutex into helpers.
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

	// Collect target artists: specific IDs if provided, otherwise all non-excluded.
	// When ArtistIDs is set, cap at that length; per-row Get/exclusion may skip
	// entries but never adds beyond it.
	var artists []artist.Artist
	if len(job.ArtistIDs) > 0 {
		artists = make([]artist.Artist, 0, len(job.ArtistIDs))
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
			if !a.IsExcluded && !a.Locked {
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
			for i := range page {
				a := &page[i]
				if !a.IsExcluded && !a.Locked {
					artists = append(artists, *a)
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

	// #2540 (#2565) BULK SCOPE DECISION: build the cross-artist fanart identity
	// registry ONCE PER JOB, here, and thread it down to saveBestImage.
	//
	// The other #2540 chokepoints are single-artist, so "once per scope" and
	// "once per artist" coincide. A bulk job is different: it walks the WHOLE
	// library. BuildFanartIdentityIndex is a full artist_images fanart read, so
	// rebuilding it per artist would be N whole-library scans across N artists --
	// quadratic, on the unattended auto-fix path this guard exists to protect.
	// That cost is not affordable, so the index is built once and reused.
	//
	// A once-per-job index is stale the moment the job writes its first fanart, so
	// it is also GROWN IN PLACE: saveBestImage appends each CONFIRMED fanart write
	// (fanartIndex.add), O(1) per write with no extra database read.
	//
	// That append covers the primary bulk threat. fetchImages holds the
	// misresolution source itself: for an artist with no MBID in auto mode it
	// name-searches and takes the first result carrying one, so N artists can
	// resolve to the same wrong MBID in one run and all receive the same backdrop.
	// If the TRUE OWNER is not in the library (or is itself missing that fanart)
	// the pre-run index contains that hash ZERO times, so without the append NONE
	// of the N is flagged and the guard stays silent through the exact event it
	// exists to catch. With it, artists 2..N are flagged against artist 1.
	//
	// WHAT IS STILL NOT COVERED: the FIRST write of a given backdrop in a job whose
	// pre-run library does not already contain it. Nothing exists to compare it
	// against at that moment, so it is written silently and only becomes a
	// reference for the writes after it. The #2564 detector sweep is the backstop
	// for that residue; this guard is not.
	//
	// Nil when the guard is unwired, and empty on a build failure (fail-open).
	var identityIdx *fanartIndex
	if e.collision.active("fanart") {
		identityIdx = &fanartIndex{entries: e.collision.buildIndex(ctx)}
	}

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

		status, message := e.processArtist(ctx, a, job, identityIdx)
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

func (e *BulkExecutor) processArtist(ctx context.Context, a *artist.Artist, job *BulkJob, identityIdx *fanartIndex) (string, string) {
	switch job.Type {
	case BulkTypeFetchMetadata:
		return e.fetchMetadata(ctx, a, job.Mode)
	case BulkTypeFetchImages:
		return e.fetchImages(ctx, a, job.Mode, identityIdx)
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

	u := artist.FetchResultToUpdate(result)
	if u == nil {
		return BulkItemSkipped, "no metadata returned"
	}

	// In manual mode, do not assign MBID through the merge helper.
	if mode == BulkModeManual && a.MusicBrainzID == "" && u.MusicBrainzID != "" {
		return BulkItemSkipped, "manual mode: skipped MBID assignment"
	}

	changed := artist.ApplyMetadata(a, u, artist.FillEmpty, artist.MergeOptions{})
	if !changed {
		return BulkItemSkipped, "no new metadata to apply"
	}

	if err := e.artistService.Update(ctx, a); err != nil {
		return BulkItemFailed, fmt.Sprintf("update failed: %v", err)
	}

	UpdateProviderFetchTimestamps(ctx, e.artistService, a.ID, result.AttemptedProviders, e.logger)

	e.publisher.PublishMetadata(ctx, a)

	return BulkItemFixed, "metadata updated"
}

func (e *BulkExecutor) fetchImages(ctx context.Context, a *artist.Artist, mode string, identityIdx *fanartIndex) (string, string) {
	if a.MusicBrainzID == "" {
		if mode == BulkModeManual || mode == BulkModeDisambiguate {
			return BulkItemSkipped, "no MBID"
		}
		results, err := e.orchestrator.Search(ctx, a.Name)
		if err != nil || len(results) == 0 {
			return BulkItemSkipped, "no MBID and provider search found nothing"
		}
		for i := range results {
			if results[i].MusicBrainzID != "" {
				a.MusicBrainzID = results[i].MusicBrainzID
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

	// Track saved file paths so provenance can be recorded after Update()
	// creates the artist_images rows.
	type savedImage struct {
		imageType string
		filePath  string
	}
	var savedImages []savedImage

	fixed := 0
	for imageType := range needed {
		if path := e.saveBestImage(ctx, a, imageType, imgResult, identityIdx); path != "" {
			fixed++
			savedImages = append(savedImages, savedImage{imageType, path})
		}
	}

	if fixed == 0 {
		return BulkItemSkipped, "no suitable images found"
	}

	if err := e.artistService.Update(ctx, a); err != nil {
		return BulkItemFailed, fmt.Sprintf("update failed: %v", err)
	}

	// Record provenance after Update() so the artist_images rows exist,
	// then sync each saved image to connected platforms.
	for _, si := range savedImages {
		if e.artistService != nil {
			recordSavedImageProvenance(ctx, e.artistService, a.ID, si.imageType, si.filePath, e.logger)
		}
		e.publisher.SyncImageToPlatforms(ctx, a, si.imageType)
	}

	return BulkItemFixed, fmt.Sprintf("saved %d image(s)", fixed)
}

// saveBestImage saves the best candidate image for the given type. Returns the
// full file path of the saved image, or empty string if no candidate succeeded.
func (e *BulkExecutor) saveBestImage(ctx context.Context, a *artist.Artist, imageType string, result *provider.FetchResult, identityIdx *fanartIndex) string {
	var candidates []provider.ImageResult
	for _, im := range result.Images {
		if string(im.Type) == imageType {
			candidates = append(candidates, im)
		}
	}
	if len(candidates) == 0 {
		return ""
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
		// #2540 NOTIFY-ONLY. SaveImageFromURL is a thin fetch-then-delegate wrapper
		// around the already-exported SaveImageFromData, whose doc comment exists
		// precisely to serve callers that need to inspect the downloaded bytes. So
		// this site splits the wrapper open rather than changing any signature:
		// fetch, convert (what actually lands on disk -- ConvertFormat re-encodes
		// WebP to PNG), decide the verdict, then save the same bytes.
		//
		// The verdict is computed here but HELD until the save is confirmed below.
		// Its durable half is a fixable Action Queue entry whose auto-fix backs
		// artwork OUT of the artist; raising it for a save that then failed would
		// aim a destructive remediation at a file that never existed. This path is
		// bulk Mode "auto" -- unattended and library-wide -- so getting that
		// ordering right matters more here than anywhere else.
		//
		// The gate is collision.active(imageType), NOT a bare index check. The index
		// is job-scoped but FANART-ONLY (BuildFanartIdentityIndex deliberately loads
		// only fanart rows), while this function runs once per NEEDED type -- thumb,
		// fanart, logo. Gating on the index alone hashes a thumb or logo candidate
		// against the fanart registry: a meaningless comparison that can raise a
		// BACKDROP collision, and its artwork-back-out auto-fix, for a write that
		// was never a backdrop. This mirrors the type gate in fixers.go.
		//
		// Fail-open: with no guard wired or a non-fanart type this takes the
		// untouched SaveImageFromURL branch. An empty index still enters here -- it
		// yields no verdict, but it does seed the in-run index for later writes.
		var collisionResult *img.IdentityResult
		var phash uint64
		var saved []string
		var err error
		if e.collision.active(imageType) {
			var data []byte
			if data, err = fetchImageURL(ctx, e.httpClient, c.URL); err == nil {
				var converted []byte
				if converted, _, err = img.ConvertFormat(bytes.NewReader(data)); err == nil {
					collisionResult, phash = e.collision.verdictAndHash(a.ID, converted, identityIdx.list())
					// Pass the CONVERTED bytes: SaveImageFromData converts again, and
					// ConvertFormat is idempotent (its output is always JPEG or PNG,
					// both of which it passes through untouched), so the file on disk
					// is byte-for-byte identical either way -- but a WebP source is
					// decoded and re-encoded once instead of twice.
					saved, err = SaveImageFromData(ctx, a, imageType, converted, naming, useSymlinks, meta, e.platformService, e.logger)
				}
			}
		} else {
			saved, err = SaveImageFromURL(ctx, e.httpClient, a, imageType, c.URL, naming, useSymlinks, meta, e.platformService, e.logger)
		}
		if err != nil {
			// Source identifies the provider without leaking the signed URL.
			e.logger.Debug("image candidate failed", "source", c.Source, "error", err)
			continue
		}
		// The save returned no error, so the image the verdict was computed on is
		// genuinely on disk. Only now is it correct to notify -- and only now is it
		// correct to add this fanart to the in-run index, for the same reason: an
		// entry for a failed write would poison every later comparison in the job.
		// phash is non-zero only on the fanart branch above, so this is inert for
		// thumb and logo.
		e.collision.notify(ctx, a.ID, a.Name, collisionResult)
		identityIdx.add(a.ID, phash)
		if len(saved) > 0 && a.Path != "" {
			return filepath.Join(a.Path, saved[0])
		}
		return ""
	}

	return ""
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
