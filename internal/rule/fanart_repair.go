// Package rule -- fanart_repair.go
// Library-wide fanart-duplicate remediation (#2540 PR-2). A thin batch runner
// over the existing ImageDuplicateFixer: the rule engine's per-artist collapse
// already exists, but nothing invokes it across the whole library because the
// image_duplicate_exact checker keys on stored (stale/empty) hashes and so never
// raises violations for platform-sprayed duplicates. These methods re-hash each
// artist's fanart from disk and act on the fresh result.
package rule

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/sydlexius/stillwater/internal/artist"
)

// ArtistFanartDup is one artist's within-artist fanart redundancy.
type ArtistFanartDup struct {
	ArtistID   string
	Name       string
	ExactDrops int // byte-identical redundant slots (safe to auto-collapse)
}

// FanartDupReport is the library-wide blast radius.
type FanartDupReport struct {
	ArtistsAffected     int
	ExactRedundantSlots int
	PerArtist           []ArtistFanartDup
	ScanErrors          int // artists whose scan failed and were SKIPPED; surfaced so a partial scan is never mistaken for a clean one (no silent truncation)
}

// scanFanartPageSize bounds each artist-list page during a scan. Must be
// within artist.ListParams.Validate's [10, 500] range or it gets silently
// clamped to 50.
const scanFanartPageSize = 200

// ScanFanartDuplicates walks every artist with a path and reports within-artist
// fanart duplication. It re-hashes each artist's fanart files FROM DISK
// (findImageDuplicates fresh=true), so it is correct even when artist_images
// hashes are stale or empty. The fresh pass re-persists corrected slot hashes as
// a side effect -- an idempotent hash repair, not a content mutation. Read-only
// with respect to image files.
func (p *Pipeline) ScanFanartDuplicates(ctx context.Context) (FanartDupReport, error) {
	if p.engine == nil || p.engine.db == nil || p.artistService == nil {
		return FanartDupReport{}, fmt.Errorf("scan fanart duplicates: pipeline not fully wired")
	}
	primaryName := resolveFanartPrimaryName(ctx, p.engine.platformService)
	if primaryName == "" {
		return FanartDupReport{}, fmt.Errorf("scan fanart duplicates: no fanart naming convention")
	}

	var report FanartDupReport
	page := 1
	for {
		artists, _, err := p.artistService.List(ctx, artist.ListParams{Page: page, PageSize: scanFanartPageSize})
		if err != nil {
			return FanartDupReport{}, fmt.Errorf("listing artists at page %d: %w", page, err)
		}
		if len(artists) == 0 {
			break
		}
		for i := range artists {
			a := &artists[i]
			if a.Path == "" {
				continue
			}
			res, err := findImageDuplicates(ctx, p.engine.db, a, primaryName, defaultImageDupTolerance, p.engine.imageHashRecorder, true, p.logger)
			if err != nil {
				p.logger.Warn("skipping artist in fanart duplicate scan", slog.String("artist_id", a.ID), slog.String("error", err.Error()))
				report.ScanErrors++
				continue
			}
			exact := len(res.exactFanartToDelete)
			if exact == 0 {
				continue
			}
			report.ArtistsAffected++
			report.ExactRedundantSlots += exact
			report.PerArtist = append(report.PerArtist, ArtistFanartDup{
				ArtistID: a.ID, Name: a.Name, ExactDrops: exact,
			})
		}
		if len(artists) < scanFanartPageSize {
			break
		}
		page++
	}
	return report, nil
}

// FanartRepairFailure records one artist whose collapse failed; the batch
// continues past it.
type FanartRepairFailure struct {
	ArtistID string
	Err      string
}

// FanartRepairResult summarizes a remediation run.
type FanartRepairResult struct {
	ArtistsProcessed int
	SlotsRemoved     int
	Failures         []FanartRepairFailure
}

// imageDupFixer returns the live ImageDuplicateFixer from the pipeline's
// fixer set, or nil if none is wired.
func (p *Pipeline) imageDupFixer() *ImageDuplicateFixer {
	for _, fx := range p.fixers {
		if df, ok := fx.(*ImageDuplicateFixer); ok {
			return df
		}
	}
	return nil
}

// RemediateFanartDuplicates collapses EXACT (byte-identical) within-artist
// fanart duplicates library-wide by driving the existing ImageDuplicateFixer
// with a synthetic image_duplicate_exact violation. Byte-identical means the
// dropped file equals the survivor, so nothing is lost. #2533-protected
// slots are filtered by Fix itself. A per-artist failure is collected into
// Failures rather than aborting the batch; only the artist-list paging error
// and the nil-wiring guards are hard returns.
func (p *Pipeline) RemediateFanartDuplicates(ctx context.Context) (FanartRepairResult, error) {
	if p.engine == nil || p.engine.db == nil || p.artistService == nil {
		return FanartRepairResult{}, fmt.Errorf("remediate fanart duplicates: pipeline not fully wired")
	}
	fixer := p.imageDupFixer()
	if fixer == nil {
		return FanartRepairResult{}, fmt.Errorf("remediate fanart duplicates: no image-duplicate fixer wired")
	}

	var result FanartRepairResult
	page := 1
	for {
		artists, _, err := p.artistService.List(ctx, artist.ListParams{Page: page, PageSize: scanFanartPageSize})
		if err != nil {
			return result, fmt.Errorf("listing artists at page %d: %w", page, err)
		}
		if len(artists) == 0 {
			break
		}
		for i := range artists {
			p.remediateOneArtistFanart(ctx, &artists[i], fixer, &result)
		}
		if len(artists) < scanFanartPageSize {
			break
		}
		page++
	}
	return result, nil
}

// remediateOneArtistFanart collapses exact fanart duplicates for a single
// artist, updating result in place. Any failure for this artist is collected
// into result.Failures rather than propagated, so the caller's batch loop
// never needs its own error handling for a single bad artist.
func (p *Pipeline) remediateOneArtistFanart(ctx context.Context, a *artist.Artist, fixer *ImageDuplicateFixer, result *FanartRepairResult) {
	if a.Path == "" {
		return
	}
	// Fix re-detects fresh from disk (fresh=true) itself, so no separate
	// pre-scan is needed: driving it with the exact-duplicate rule collapses
	// only byte-identical redundant slots, filters #2533-protected slots, and
	// reports the ACTUAL number of files removed via FixResult.SlotsRemoved --
	// which is what we tally, so a partial-lock case (some slots of a group
	// locked) is counted honestly rather than as the requested count.
	fr, fixErr := fixer.Fix(ctx, a, &Violation{RuleID: RuleImageDuplicateExact})
	if fixErr != nil {
		result.Failures = append(result.Failures, FanartRepairFailure{ArtistID: a.ID, Err: fixErr.Error()})
		return
	}
	if fr == nil || !fr.Fixed {
		return
	}
	// Persist the collapsed fanart rows, mirroring Pipeline.FixViolation.
	if err := p.artistService.Update(ctx, a); err != nil {
		result.Failures = append(result.Failures, FanartRepairFailure{ArtistID: a.ID, Err: fmt.Sprintf("update after collapse: %v", err)})
		return
	}
	result.ArtistsProcessed++
	result.SlotsRemoved += fr.SlotsRemoved
	p.logger.Info("fanart duplicates collapsed",
		slog.String("artist_id", a.ID), slog.String("artist", a.Name),
		slog.Int("slots_removed", fr.SlotsRemoved), slog.String("detail", fr.Message))
}
