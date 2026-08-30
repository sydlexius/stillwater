package rule

import (
	"context"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
)

// #3066: a lock-blocked fix must close its row the SAME WAY regardless of
// which rule wrote it. provider_id_missing's own fixer-level branch is
// covered in fixers_provider_id_test.go; this file wires the misbehavior
// through the REAL dispatch chain (processArtistForRunAll -- the unattended
// pass, and Pipeline.FixViolation -- the operator's own click) so the fix is
// proven at the seam between the fixer's verdict and the pipeline's
// persistence, not just at the fixer's return value.
//
// Every "stays open" assertion below is paired with a control that proves
// the harness actually reached the fixer: TestProcessArtistForRunAll_
// ProviderIDBackfill_PersistsResultRow (fixer_runall_test.go) already covers
// the unlocked, successful case for the auto path, and
// TestProviderIDBackfill_LockRefusalIsATerminalSkip/_LockedPlusNoRelationStaysOpen
// (fixers_provider_id_test.go) cover the fixer unit directly. This file adds
// the one shape neither of those exercises: every derivable ID pinned, driven
// through the pipeline, asserting the PERSISTED row.

// providerIDLockConvergenceArtist creates an artist with an MBID and every
// in-scope provider ID empty and LOCKED, wires a fixer that can derive all
// three from MusicBrainz relations, and enables provider_id_missing in auto
// mode -- so a same-pass auto-fix genuinely attempts the backfill and every
// attempt is genuinely refused.
func providerIDLockConvergenceArtist(t *testing.T) (*artist.Service, *Service, *Pipeline, *artist.Artist) {
	t.Helper()
	db := setupTestDB(t)
	ctx := context.Background()
	artistSvc := artist.NewService(db)
	ruleSvc := NewService(db)
	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding default rules: %v", err)
	}

	r, err := ruleSvc.GetByID(ctx, RuleProviderIDMissing)
	if err != nil {
		t.Fatalf("loading provider_id_missing rule: %v", err)
	}
	r.Enabled = true
	r.AutomationMode = AutomationModeAuto
	if err := ruleSvc.Update(ctx, r); err != nil {
		t.Fatalf("enabling provider_id_missing: %v", err)
	}

	engine := NewEngine(ruleSvc, db, nil, nil, testLogger())
	engine.SetProviderAvailability(&stubProviderAvailability{available: allThreeAvailable()})

	fetcher := &stubMetadataProvider{metadata: mbURLMetadata()}
	fixer := NewProviderIDBackfillFixer(fetcher, artistSvc, testLogger())
	p := NewPipeline(engine, artistSvc, ruleSvc, []Fixer{fixer}, nil, testLogger())

	a := &artist.Artist{Name: "Locked Provider IDs", SortName: "Locked Provider IDs", MusicBrainzID: "mbid-abc", Path: t.TempDir()}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	if err := artistSvc.SetLockedFields(ctx, a.ID, []string{"discogs_id", "deezer_id", "spotify_id"}); err != nil {
		t.Fatalf("locking provider IDs: %v", err)
	}
	stored, err := artistSvc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("reloading artist: %v", err)
	}
	// PRECONDITION: the lock actually persisted and every in-scope ID is
	// genuinely empty, or the fixer has nothing to refuse and every
	// assertion below would hold for the wrong reason.
	if len(stored.LockedFields) != 3 {
		t.Fatalf("precondition: locked_fields = %v, want 3 entries", stored.LockedFields)
	}
	if stored.DiscogsID != "" || stored.DeezerID != "" || stored.SpotifyID != "" {
		t.Fatalf("precondition: provider IDs must start empty, got discogs=%q deezer=%q spotify=%q",
			stored.DiscogsID, stored.DeezerID, stored.SpotifyID)
	}
	return artistSvc, ruleSvc, p, stored
}

// TestProcessArtistForRunAll_ProviderIDBackfill_LockedStaysOpen is the
// unattended-pass shape of #3066: every derivable provider ID pinned, run
// through processArtistForRunAll exactly like a scheduled Run Rules pass.
// Before the fix this closed the violation as ViolationStatusDismissed;
// #3066 requires it to persist OPEN, matching lock_reverted_fix.go's outcome
// for every other locked-field fixer.
func TestProcessArtistForRunAll_ProviderIDBackfill_LockedStaysOpen(t *testing.T) {
	_, ruleSvc, p, a := providerIDLockConvergenceArtist(t)
	ctx := context.Background()

	contrib, ok := p.processArtistForRunAll(ctx, a)
	if !ok {
		t.Fatal("processArtistForRunAll reported a persist failure")
	}

	// The fixer's own verdict, carried in this pass's FixResult -- this is
	// where the lock detail lives; the persisted violation row keeps the
	// checker's generic "missing provider IDs" message regardless of outcome.
	var fixerResult *FixResult
	for i := range contrib.results {
		if contrib.results[i].RuleID == RuleProviderIDMissing {
			fixerResult = &contrib.results[i]
			break
		}
	}
	if fixerResult == nil {
		t.Fatal("no FixResult for provider_id_missing in this pass; the fixer was never dispatched")
	}
	if fixerResult.Fixed {
		t.Errorf("Fixed=true with every provider ID locked; nothing was actually written")
	}
	if fixerResult.Dismissed {
		t.Errorf("Dismissed=true; #3066 requires this to stay open, matching every other locked-field fix")
	}
	if !strings.Contains(fixerResult.Message, "locked by the operator") {
		t.Errorf("fixer message %q does not tell the operator the field is locked", fixerResult.Message)
	}

	violations, err := ruleSvc.ListViolations(ctx, ViolationStatusOpen)
	if err != nil {
		t.Fatalf("listing open violations: %v", err)
	}
	var found *RuleViolation
	for i := range violations {
		if violations[i].ArtistID == a.ID && violations[i].RuleID == RuleProviderIDMissing {
			found = &violations[i]
			break
		}
	}
	if found == nil {
		t.Fatal("provider_id_missing violation is not open; a lock-refused fix must not close it (#3066)")
	}

	dismissed, err := ruleSvc.ListViolations(ctx, ViolationStatusDismissed)
	if err != nil {
		t.Fatalf("listing dismissed violations: %v", err)
	}
	for _, v := range dismissed {
		if v.ArtistID == a.ID && v.RuleID == RuleProviderIDMissing {
			t.Fatal("provider_id_missing violation was dismissed; a lock is operator-revocable and must not close the row permanently (#3066)")
		}
	}

	// The paired rule_results FAIL row must exist -- UpsertViolation writes
	// one only for an open/pending row (#1107) -- so offlineHealthScore is not
	// left unable to score this artist for this rule.
	rows, err := ruleSvc.GetRuleResultsForArtist(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetRuleResultsForArtist: %v", err)
	}
	var passRow *RuleResult
	for i := range rows {
		if rows[i].RuleID == RuleProviderIDMissing {
			passRow = &rows[i]
			break
		}
	}
	if passRow == nil {
		t.Fatalf("no rule_results row for %s; offlineHealthScore would freeze this artist's score", RuleProviderIDMissing)
	}
	if passRow.Passed {
		t.Errorf("rule_results row for %s reports passed=true while every ID is still missing and locked", RuleProviderIDMissing)
	}
}

// TestFixViolation_ProviderIDBackfill_LockedStaysOpen is the operator's-own-
// click shape of the same convergence: FixViolation, not the unattended
// pass. Before #3066 this dismissed the row via DismissViolation; it must now
// leave it open, and it must remain fixable (unlike DismissViolation, which
// zeroes fixability on the click path via the operator-facing UI's
// already-dismissed short-circuit).
func TestFixViolation_ProviderIDBackfill_LockedStaysOpen(t *testing.T) {
	artistSvc, ruleSvc, p, a := providerIDLockConvergenceArtist(t)
	ctx := context.Background()

	rv := &RuleViolation{
		RuleID:     RuleProviderIDMissing,
		ArtistID:   a.ID,
		ArtistName: a.Name,
		Severity:   "warning",
		Message:    "provider IDs missing",
		Fixable:    true,
		Status:     ViolationStatusOpen,
	}
	if err := ruleSvc.UpsertViolation(ctx, rv); err != nil {
		t.Fatalf("seeding violation: %v", err)
	}

	fr, err := p.FixViolation(ctx, rv.ID)
	if err != nil {
		t.Fatalf("FixViolation: %v", err)
	}
	if fr.Fixed {
		t.Errorf("Fixed=true with every provider ID locked; nothing was actually written")
	}
	if fr.Dismissed {
		t.Errorf("Dismissed=true; #3066 requires this to stay open, matching every other locked-field fix")
	}

	reloaded, err := ruleSvc.GetViolationByID(ctx, rv.ID)
	if err != nil {
		t.Fatalf("reloading violation: %v", err)
	}
	if reloaded.Status != ViolationStatusOpen {
		t.Fatalf("violation status = %q, want %q", reloaded.Status, ViolationStatusOpen)
	}

	// Second click: the row must still be reachable and still refuse the
	// same way -- proving this is a live, revocable state rather than a
	// one-shot open that a second pass would silently dismiss.
	fr2, err := p.FixViolation(ctx, rv.ID)
	if err != nil {
		t.Fatalf("second FixViolation: %v", err)
	}
	if fr2.Dismissed {
		t.Errorf("second click reported Dismissed=true")
	}

	// Positive control: unlocking the fields lets the SAME fixer succeed on
	// the very next click, which is the whole reason #3066 forbids dismissing
	// this row -- a dismissed row has no route back from here.
	if err := artistSvc.SetLockedFields(ctx, a.ID, nil); err != nil {
		t.Fatalf("unlocking provider IDs: %v", err)
	}
	fr3, err := p.FixViolation(ctx, rv.ID)
	if err != nil {
		t.Fatalf("post-unlock FixViolation: %v", err)
	}
	if !fr3.Fixed {
		t.Fatalf("positive control FAILED: unlocking did not let the fix land, got %+v", fr3)
	}
	reloaded, err = ruleSvc.GetViolationByID(ctx, rv.ID)
	if err != nil {
		t.Fatalf("reloading violation after unlock: %v", err)
	}
	if reloaded.Status != ViolationStatusResolved {
		t.Fatalf("positive control FAILED: violation status = %q after unlocking and fixing, want %q", reloaded.Status, ViolationStatusResolved)
	}
}
