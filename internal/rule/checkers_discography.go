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
	"github.com/sydlexius/stillwater/internal/provider"
)

// discographyFetchTimeout caps the MusicBrainz round-trip the coverage check
// makes per artist. The MusicBrainz adapter is rate-limited to roughly one
// request per second, so a slow or unreachable provider must not block a
// library-wide evaluation. On timeout the coverage comparison is skipped and
// the checker degrades to the zero-album signal only.
const discographyFetchTimeout = 15 * time.Second

// defaultDiscographyCoverageThreshold is the percentage of MusicBrainz
// release groups (after the release-type filter) that an NFO must cover
// before the discography_populated rule considers it adequately populated.
// An NFO below this fraction raises a coverage violation.
const defaultDiscographyCoverageThreshold = 50.0

// readArtistNFO opens and parses the artist.nfo file in the artist's
// directory. It returns:
//   - (parsed, nil)             when the file exists and parses cleanly
//   - (nil, os.ErrNotExist)     when no file is present (caller decides)
//   - (nil, err)                on any other read or parse failure
//
// The path is built from the trusted artist.Path, not user input.
func readArtistNFO(a *artist.Artist) (*nfo.ArtistNFO, error) {
	if a.Path == "" {
		return nil, os.ErrNotExist
	}
	nfoPath := filepath.Join(a.Path, "artist.nfo")
	f, err := os.Open(nfoPath) //nolint:gosec // G304: path from trusted artist.Path, not user input
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // Close error not actionable on a read-only handle
	parsed, err := nfo.Parse(f)
	if err != nil {
		return nil, err
	}
	return parsed, nil
}

// makeDiscographyChecker returns a Checker that flags artists whose artist.nfo
// is missing discography data.
//
// A violation is raised when either of these holds:
//
//  1. The NFO has zero <album> entries and the artist has a MusicBrainz ID.
//     This is the cheap signal: no provider round-trip is needed.
//  2. The NFO has at least one <album> entry but covers materially fewer
//     release groups than MusicBrainz lists for the artist. "Materially
//     fewer" means the coverage percentage falls below the configured
//     CoverageThreshold (default 50%). This branch requires a MusicBrainz
//     lookup and is skipped when no release-group fetcher is wired.
//
// The rule is detection-only here; the DiscographyFixer performs the merge.
// It is filesystem-dependent (registered in filesystemRules) so the engine
// already skips it for artists with no local path.
func (e *Engine) makeDiscographyChecker() Checker {
	return func(ctx context.Context, a *artist.Artist, cfg RuleConfig) *Violation {
		// An artist with no MusicBrainz ID cannot have its discography
		// fetched, so there is nothing actionable to flag. The separate
		// nfo_has_mbid rule covers the missing-MBID case.
		if a.MusicBrainzID == "" {
			return nil
		}

		parsed, err := readArtistNFO(a)
		if err != nil {
			// No NFO on disk: the nfo_exists rule owns that violation, so
			// this rule stays silent. Any other read or parse error is
			// logged and treated as "cannot evaluate" (no violation) so a
			// corrupt file does not produce a misleading discography flag.
			if !errors.Is(err, os.ErrNotExist) {
				e.logger.Warn("discography_populated: cannot read artist.nfo",
					slog.String("artist", a.Name),
					slog.String("error", err.Error()))
			}
			return nil
		}

		albumCount := len(parsed.Albums)

		// Signal 1: an entirely empty discography. No provider call needed.
		if albumCount == 0 {
			return &Violation{
				RuleID:   RuleDiscographyPopulated,
				RuleName: "Discography is populated",
				Category: string(RuleCategoryMetadata),
				Severity: effectiveSeverity(cfg),
				Message:  fmt.Sprintf("artist %s has no discography in artist.nfo", a.Name),
				Fixable:  true,
			}
		}

		// Signal 2: coverage comparison. Only attempted when a release-group
		// fetcher is wired, this rule declares the capability to reach it, AND
		// the artist is not locked (#2754 -- no outbound fetch on behalf of an
		// artist the operator has declared finished); otherwise a non-empty
		// discography is accepted, matching the mbCount <= 0 branch below.
		if e.releaseGroupFetcherFor(RuleDiscographyPopulated, a) == nil {
			return nil
		}

		mbCount := e.countMBReleaseGroups(ctx, a, cfg)
		if mbCount <= 0 {
			// MusicBrainz returned nothing usable (no release groups, an
			// error, or a timeout). Without a reliable upstream count the
			// coverage ratio is meaningless, so accept the existing NFO.
			return nil
		}

		threshold := cfg.CoverageThreshold
		if threshold <= 0 {
			threshold = defaultDiscographyCoverageThreshold
		}

		// Coverage is an approximation: albumCount counts every <album> in the
		// NFO, while mbCount is filtered to the configured release types. The
		// NFO <album> schema does not record a release type, so a discography
		// padded with off-type entries (singles, compilations) can overstate
		// coverage. The bias is toward under-flagging, the safe direction for a
		// rule that writes files, so the approximation is acceptable.
		coverage := float64(albumCount) / float64(mbCount) * 100.0
		if coverage >= threshold {
			return nil
		}

		return &Violation{
			RuleID:   RuleDiscographyPopulated,
			RuleName: "Discography is populated",
			Category: string(RuleCategoryMetadata),
			Severity: effectiveSeverity(cfg),
			Message: fmt.Sprintf(
				"artist %s discography covers %d of %d MusicBrainz release groups (%.0f%%, below the %.0f%% threshold)",
				a.Name, albumCount, mbCount, coverage, threshold),
			Fixable: true,
		}
	}
}

// countMBReleaseGroups fetches the artist's MusicBrainz release groups and
// returns the count after applying the rule's release-type filter. Returns 0
// when the fetcher is unwired, the artist is locked, the artist has no MBID, or
// the lookup fails or times out -- the caller treats 0 as "skip the coverage
// check".
func (e *Engine) countMBReleaseGroups(ctx context.Context, a *artist.Artist, cfg RuleConfig) int {
	fetcher := e.releaseGroupFetcherFor(RuleDiscographyPopulated, a)
	if fetcher == nil || a.MusicBrainzID == "" {
		return 0
	}

	// Cap the per-artist round-trip so a slow MusicBrainz response cannot
	// stall a library-wide evaluation. On timeout we degrade gracefully.
	fetchCtx, cancel := context.WithTimeout(ctx, discographyFetchTimeout)
	defer cancel()

	groups, err := e.fetchReleaseGroupsCoalesced(fetchCtx, fetcher, a.MusicBrainzID)
	if err != nil {
		e.logger.Warn("discography_populated: release-group fetch failed",
			slog.String("artist", a.Name),
			slog.String("mbid", a.MusicBrainzID),
			slog.String("error", err.Error()))
		return 0
	}

	// Count only the release groups whose primary type is in the configured
	// filter, so the coverage ratio is measured against the same set the
	// fixer would merge.
	filter := nfo.ParseReleaseTypeFilter(cfg.ReleaseTypes)
	return filter.CountReleaseGroups(groups)
}

// fetchReleaseGroupsCoalesced routes the release-group fetch through the
// per-artist EvaluationContext coalescer when one is attached to ctx (the
// canonical pipeline path), so the pre-fix and post-fix checker passes of a
// single scoped run collapse to one MusicBrainz call (#2476). When no
// EvaluationContext is present -- single-violation paths, or a pipeline wired
// without an orchestrator -- it falls back to the bare fetcher, which preserves
// the legacy behavior. fetcher is the capability-gated handle the caller
// already obtained via releaseGroupFetcherFor; it is passed in rather than read
// off the Engine so the capability gate stays the single access point.
func (e *Engine) fetchReleaseGroupsCoalesced(ctx context.Context, fetcher ReleaseGroupFetcher, mbid string) ([]provider.ReleaseGroupInfo, error) {
	if ec := EvaluationContextFromContext(ctx); ec != nil {
		return ec.GetReleaseGroups(ctx, mbid, func(fetchCtx context.Context) ([]provider.ReleaseGroupInfo, error) {
			return fetcher.GetReleaseGroups(fetchCtx, mbid)
		})
	}
	return fetcher.GetReleaseGroups(ctx, mbid)
}
