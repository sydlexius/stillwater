package rule

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/nfo"
)

// DiscographyFixer resolves discography_populated violations by fetching an
// artist's release groups from MusicBrainz and merging them into the on-disk
// artist.nfo.
//
// The fixer reuses nfo.MergeDiscographyFromMBReleaseGroups (issue #1065): the
// merge never overwrites user-added albums and is the single canonical merge
// path shared with the manual "Fetch discography" button. The fixer does NOT
// implement CandidateDiscoverer: it writes a file to disk, so in manual mode
// the pipeline records the violation and waits for the user to trigger the fix
// individually rather than running it speculatively during evaluation.
type DiscographyFixer struct {
	fetcher         ReleaseGroupFetcher
	fsCheck         *SharedFSCheck
	snapshotService *nfo.SnapshotService
	logger          *slog.Logger
}

// NewDiscographyFixer creates a DiscographyFixer.
//
//   - fetcher resolves MusicBrainz release groups; when nil the fixer reports
//     a non-fatal "not available" result rather than failing the run.
//   - fsCheck guards against writing into shared-filesystem libraries where a
//     media server may overwrite the file; when nil the guard is skipped.
//   - snapshotService takes a pre-write copy of the NFO so the user has a
//     recovery path; when nil no snapshot is taken (the merge itself is still
//     non-destructive for user-added albums).
func NewDiscographyFixer(fetcher ReleaseGroupFetcher, fsCheck *SharedFSCheck, snapshotService *nfo.SnapshotService, logger *slog.Logger) *DiscographyFixer {
	if logger == nil {
		logger = slog.Default()
	}
	return &DiscographyFixer{
		fetcher:         fetcher,
		fsCheck:         fsCheck,
		snapshotService: snapshotService,
		logger:          logger,
	}
}

// CanFix returns true for the discography_populated rule.
func (f *DiscographyFixer) CanFix(v *Violation) bool {
	return v.RuleID == RuleDiscographyPopulated
}

// Fix fetches release groups from MusicBrainz and merges them into the
// artist's on-disk NFO. The merge preserves user-added albums and existing
// MBID-keyed entries; only genuinely new release groups are appended. The
// file is rewritten atomically (tmp/bak/rename via filesystem.WriteFileAtomic)
// and only when the merge actually added at least one entry.
func (f *DiscographyFixer) Fix(ctx context.Context, a *artist.Artist, v *Violation) (*FixResult, error) {
	// Refuse to write into a library a media server also manages: the
	// platform's own NFO saver could overwrite the merged file and the two
	// would round-trip indefinitely.
	if f.fsCheck != nil && f.fsCheck.IsShared(ctx, a) {
		return &FixResult{
			RuleID:  RuleDiscographyPopulated,
			Fixed:   false,
			Message: "skipped: NFO write disabled for shared-filesystem library; platform may overwrite",
		}, nil
	}

	if a.MusicBrainzID == "" {
		return &FixResult{
			RuleID:  RuleDiscographyPopulated,
			Fixed:   false,
			Message: fmt.Sprintf("cannot fetch discography for %s: no MusicBrainz ID", a.Name),
		}, nil
	}
	if a.Path == "" {
		return &FixResult{
			RuleID:  RuleDiscographyPopulated,
			Fixed:   false,
			Message: fmt.Sprintf("cannot write discography for %s: artist has no filesystem path", a.Name),
		}, nil
	}
	if f.fetcher == nil {
		return &FixResult{
			RuleID:  RuleDiscographyPopulated,
			Fixed:   false,
			Message: "MusicBrainz release-group provider is not available",
		}, nil
	}

	// Parse the existing on-disk NFO so user-added <album> entries survive
	// the merge. A missing file is seeded from the DB artist; a corrupt file
	// is refused outright -- overwriting it would silently destroy the user's
	// hand-edited content.
	nfoPath := filepath.Join(a.Path, "artist.nfo")
	var existingNFO *nfo.ArtistNFO
	parsed, parseErr := readArtistNFO(a)
	switch {
	case parseErr == nil:
		existingNFO = parsed
	case errors.Is(parseErr, os.ErrNotExist):
		existingNFO = nfo.FromArtist(a)
	default:
		return &FixResult{
			RuleID:  RuleDiscographyPopulated,
			Fixed:   false,
			Message: fmt.Sprintf("existing artist.nfo for %s could not be parsed; fix or remove it before fixing discography", a.Name),
		}, nil
	}

	// Fetch release groups from MusicBrainz. The adapter honors the shared
	// rate limiter; cap the round-trip so a slow provider cannot stall the run.
	fetchCtx, cancel := context.WithTimeout(ctx, discographyFetchTimeout)
	defer cancel()
	groups, err := f.fetcher.GetReleaseGroups(fetchCtx, a.MusicBrainzID)
	if err != nil {
		return nil, fmt.Errorf("fetching release groups from MusicBrainz: %w", err)
	}

	// Merge using the canonical shared helper (issue #1065). filter controls
	// which release types are merged; the same filter is applied by the
	// checker's coverage count so detection and fix stay consistent.
	filter := nfo.ParseReleaseTypeFilter(v.Config.ReleaseTypes)
	mergedAlbums, mergeResult := nfo.MergeDiscographyFromMBReleaseGroups(
		existingNFO.Albums,
		groups,
		filter,
	)

	// Nothing new to merge: the violation may have been raised by the
	// coverage signal against a release type the user excluded, or MB simply
	// has no additional entries. Report a non-fatal no-op; the pipeline keeps
	// the violation open so the user can adjust the release-type filter.
	if mergeResult.Added == 0 {
		return &FixResult{
			RuleID:  RuleDiscographyPopulated,
			Fixed:   false,
			Message: fmt.Sprintf("no new release groups to add for %s (kept %d, skipped %d of %d)", a.Name, mergeResult.Kept, mergeResult.Skipped, mergeResult.Total),
		}, nil
	}

	existingNFO.Albums = mergedAlbums

	// Stamp provenance so a later external overwrite can be detected on read.
	existingNFO.Stillwater = &nfo.StillwaterMeta{
		Version: nfo.StillwaterVersion,
		Written: time.Now().UTC().Format(time.RFC3339),
	}

	// Take a recovery snapshot of the pre-merge file (best effort). A snapshot
	// failure is logged but never blocks the write -- the merge itself is
	// non-destructive for user content, so the snapshot is a safety net only.
	if f.snapshotService != nil {
		if existing, readErr := os.ReadFile(filepath.Clean(nfoPath)); readErr == nil && len(existing) > 0 {
			if _, snapErr := f.snapshotService.Save(ctx, a.ID, string(existing)); snapErr != nil {
				f.logger.Warn("NFO snapshot save failed before discography fix",
					slog.String("artist_id", a.ID),
					slog.String("path", nfoPath),
					slog.String("error", snapErr.Error()))
			}
		}
	}

	if err := nfo.WriteNFOAtomic(nfoPath, existingNFO); err != nil {
		return nil, fmt.Errorf("writing NFO after discography merge: %w", err)
	}

	f.logger.Info("discography populated by rule fixer",
		slog.String("artist_id", a.ID),
		slog.String("mbid", a.MusicBrainzID),
		slog.Int("added", mergeResult.Added),
		slog.Int("kept", mergeResult.Kept),
		slog.Int("skipped", mergeResult.Skipped),
		slog.Int("total", mergeResult.Total))

	return &FixResult{
		RuleID: RuleDiscographyPopulated,
		Fixed:  true,
		Message: fmt.Sprintf("populated discography for %s: added %d, kept %d, skipped %d of %d release groups",
			a.Name, mergeResult.Added, mergeResult.Kept, mergeResult.Skipped, mergeResult.Total),
	}, nil
}
