package rule

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/filesystem"
	img "github.com/sydlexius/stillwater/internal/image"
	"github.com/sydlexius/stillwater/internal/nfo"
	"github.com/sydlexius/stillwater/internal/provider"
)

const (
	fetchTimeout  = 30 * time.Second
	maxImageBytes = 25 << 20 // 25 MB
)

// NFOFixer creates missing artist.nfo files from the artist's current metadata.
type NFOFixer struct {
	SnapshotService *nfo.SnapshotService
}

// CanFix returns true for the nfo_exists rule.
func (f *NFOFixer) CanFix(v *Violation) bool {
	return v.RuleID == RuleNFOExists
}

// Fix creates an artist.nfo file in the artist's directory.
// If the file already exists and was modified externally, returns without overwriting.
func (f *NFOFixer) Fix(_ context.Context, a *artist.Artist, _ *Violation) (*FixResult, error) {
	target := filepath.Join(a.Path, "artist.nfo")

	// Check for external modifications before writing
	conflict := nfo.CheckFileConflict(target, a.UpdatedAt)
	if conflict.HasConflict {
		return &FixResult{
			RuleID:  RuleNFOExists,
			Fixed:   false,
			Message: fmt.Sprintf("NFO conflict for %s: %s", a.Name, conflict.Reason),
		}, nil
	}

	nfoData := nfo.FromArtist(a)
	var buf bytes.Buffer
	if err := nfo.Write(&buf, nfoData); err != nil {
		return nil, fmt.Errorf("generating nfo: %w", err)
	}

	if err := filesystem.WriteFileAtomic(target, buf.Bytes(), 0o644); err != nil {
		return nil, fmt.Errorf("writing nfo: %w", err)
	}

	a.NFOExists = true

	return &FixResult{
		RuleID:  RuleNFOExists,
		Fixed:   true,
		Message: fmt.Sprintf("created artist.nfo for %s", a.Name),
	}, nil
}

// MetadataFixer populates missing metadata (MBID, biography) from providers.
type MetadataFixer struct {
	orchestrator    *provider.Orchestrator
	snapshotService *nfo.SnapshotService
}

// NewMetadataFixer creates a MetadataFixer.
func NewMetadataFixer(orchestrator *provider.Orchestrator, snapshotService *nfo.SnapshotService) *MetadataFixer {
	return &MetadataFixer{orchestrator: orchestrator, snapshotService: snapshotService}
}

// CanFix returns true for nfo_has_mbid and bio_exists rules.
func (f *MetadataFixer) CanFix(v *Violation) bool {
	return v.RuleID == RuleNFOHasMBID || v.RuleID == RuleBioExists
}

// Fix searches providers and populates the missing metadata.
func (f *MetadataFixer) Fix(ctx context.Context, a *artist.Artist, v *Violation) (*FixResult, error) {
	switch v.RuleID {
	case RuleNFOHasMBID:
		return f.fixMBID(ctx, a)
	case RuleBioExists:
		return f.fixBio(ctx, a)
	default:
		return nil, fmt.Errorf("unsupported rule: %s", v.RuleID)
	}
}

func (f *MetadataFixer) fixMBID(ctx context.Context, a *artist.Artist) (*FixResult, error) {
	results, err := f.orchestrator.Search(ctx, a.Name)
	if err != nil {
		return nil, fmt.Errorf("searching providers: %w", err)
	}

	if len(results) == 0 {
		return &FixResult{
			RuleID:  RuleNFOHasMBID,
			Fixed:   false,
			Message: fmt.Sprintf("no provider results for %s", a.Name),
		}, nil
	}

	// Pick the best match with an MBID
	var best *provider.ArtistSearchResult
	for i := range results {
		if results[i].MusicBrainzID == "" {
			continue
		}
		if best == nil || results[i].Score > best.Score {
			best = &results[i]
		}
	}

	if best == nil {
		return &FixResult{
			RuleID:  RuleNFOHasMBID,
			Fixed:   false,
			Message: "no results with MusicBrainz ID found",
		}, nil
	}

	a.MusicBrainzID = best.MusicBrainzID

	if a.NFOExists {
		writeArtistNFO(a, f.snapshotService)
	}

	return &FixResult{
		RuleID:  RuleNFOHasMBID,
		Fixed:   true,
		Message: fmt.Sprintf("set MBID to %s for %s", best.MusicBrainzID, a.Name),
	}, nil
}

func (f *MetadataFixer) fixBio(ctx context.Context, a *artist.Artist) (*FixResult, error) {
	result, err := f.orchestrator.FetchMetadata(ctx, a.MusicBrainzID, a.Name)
	if err != nil {
		return nil, fmt.Errorf("fetching metadata: %w", err)
	}

	if result.Metadata == nil || result.Metadata.Biography == "" {
		return &FixResult{
			RuleID:  RuleBioExists,
			Fixed:   false,
			Message: fmt.Sprintf("no biography found for %s", a.Name),
		}, nil
	}

	a.Biography = result.Metadata.Biography

	if a.NFOExists {
		writeArtistNFO(a, f.snapshotService)
	}

	return &FixResult{
		RuleID:  RuleBioExists,
		Fixed:   true,
		Message: fmt.Sprintf("populated biography for %s", a.Name),
	}, nil
}

// ImageFixer fetches missing or low-quality images from providers.
type ImageFixer struct {
	orchestrator *provider.Orchestrator
	logger       *slog.Logger
}

// NewImageFixer creates an ImageFixer.
func NewImageFixer(orchestrator *provider.Orchestrator, logger *slog.Logger) *ImageFixer {
	return &ImageFixer{
		orchestrator: orchestrator,
		logger:       logger,
	}
}

// CanFix returns true for image-related rules.
func (f *ImageFixer) CanFix(v *Violation) bool {
	switch v.RuleID {
	case RuleThumbExists, RuleFanartExists, RuleLogoExists, RuleThumbSquare, RuleThumbMinRes:
		return true
	default:
		return false
	}
}

// Fix fetches the best available image from providers and saves it.
func (f *ImageFixer) Fix(ctx context.Context, a *artist.Artist, v *Violation) (*FixResult, error) {
	if a.MusicBrainzID == "" {
		return &FixResult{
			RuleID:  v.RuleID,
			Fixed:   false,
			Message: "no MBID, cannot search image providers",
		}, nil
	}

	imageType := ruleToImageType(v.RuleID)
	if imageType == "" {
		return nil, fmt.Errorf("no image type for rule %s", v.RuleID)
	}

	result, err := f.orchestrator.FetchImages(ctx, a.MusicBrainzID)
	if err != nil {
		return nil, fmt.Errorf("fetching images: %w", err)
	}

	// Filter by image type and sort by quality
	var candidates []provider.ImageResult
	for _, im := range result.Images {
		if string(im.Type) == imageType {
			candidates = append(candidates, im)
		}
	}

	if len(candidates) == 0 {
		return &FixResult{
			RuleID:  v.RuleID,
			Fixed:   false,
			Message: fmt.Sprintf("no %s images found from providers", imageType),
		}, nil
	}

	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Likes != candidates[j].Likes {
			return candidates[i].Likes > candidates[j].Likes
		}
		return (candidates[i].Width * candidates[i].Height) > (candidates[j].Width * candidates[j].Height)
	})

	// Try downloading candidates until one succeeds
	for _, c := range candidates {
		data, err := fetchImageURL(c.URL)
		if err != nil {
			f.logger.Debug("image download failed", "url", c.URL, "error", err)
			continue
		}

		resized, _, err := img.Resize(bytes.NewReader(data), 3000, 3000)
		if err != nil {
			f.logger.Debug("image resize failed", "url", c.URL, "error", err)
			continue
		}

		naming := img.FileNamesForType(img.DefaultFileNames, imageType)
		saved, err := img.Save(a.Path, imageType, resized, naming, f.logger)
		if err != nil {
			f.logger.Debug("image save failed", "url", c.URL, "error", err)
			continue
		}

		setImageFlag(a, imageType)

		return &FixResult{
			RuleID:  v.RuleID,
			Fixed:   true,
			Message: fmt.Sprintf("saved %s from %s (%v)", imageType, c.Source, saved),
		}, nil
	}

	return &FixResult{
		RuleID:  v.RuleID,
		Fixed:   false,
		Message: fmt.Sprintf("all %d image downloads failed", len(candidates)),
	}, nil
}

// ruleToImageType maps a rule ID to a provider image type string.
func ruleToImageType(ruleID string) string {
	switch ruleID {
	case RuleThumbExists, RuleThumbSquare, RuleThumbMinRes:
		return "thumb"
	case RuleFanartExists:
		return "fanart"
	case RuleLogoExists:
		return "logo"
	default:
		return ""
	}
}

// setImageFlag updates the appropriate image flag on the artist.
func setImageFlag(a *artist.Artist, imageType string) {
	switch imageType {
	case "thumb":
		a.ThumbExists = true
	case "fanart":
		a.FanartExists = true
	case "logo":
		a.LogoExists = true
	}
}

// writeArtistNFO writes the artist's current metadata to an artist.nfo file (best effort).
// If a SnapshotService is provided, saves a snapshot of the existing NFO before overwriting.
func writeArtistNFO(a *artist.Artist, ss *nfo.SnapshotService) {
	target := filepath.Join(a.Path, "artist.nfo")

	// Save a snapshot of the existing NFO before overwriting
	if ss != nil {
		if existing, err := os.ReadFile(target); err == nil && len(existing) > 0 { //nolint:gosec // G304: path from trusted artist.Path
			_, _ = ss.Save(context.Background(), a.ID, string(existing))
		}
	}

	nfoData := nfo.FromArtist(a)
	var buf bytes.Buffer
	if err := nfo.Write(&buf, nfoData); err != nil {
		return
	}
	_ = filesystem.WriteFileAtomic(target, buf.Bytes(), 0o644)
}

// fetchImageURL downloads image data from a URL with timeout and size limits.
func fetchImageURL(rawURL string) ([]byte, error) {
	client := &http.Client{Timeout: fetchTimeout}

	resp, err := client.Get(rawURL) //nolint:gosec,noctx // URL from trusted provider results
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxImageBytes {
		return nil, fmt.Errorf("image exceeds 25MB limit")
	}

	return data, nil
}
