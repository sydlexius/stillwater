package rule

import (
	"context"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
)

// The mbid_resolves rule (#2810) is informational and event-driven: the
// re-validation sweep raises it, the engine never evaluates it, and it has no
// automated fix. These tests pin all three against the real query and
// evaluation paths on a real SQLite database, because each one, if broken,
// fails in the direction of silently discarding an operator's finding.

// seedMBIDResolvesViolation creates an artist and raises one mbid_resolves
// entry for it, returning the artist, the services, and the persisted id.
func seedMBIDResolvesViolation(t *testing.T, ruleSvc *Service, artistSvc *artist.Service) (*artist.Artist, string) {
	t.Helper()
	ctx := context.Background()

	// PRECONDITION: the rule is seeded, and seeded DISABLED. It being seeded at
	// all is what makes it a valid FK target for its violations; it being
	// disabled is the case these tests cover. Without this check they would
	// pass just as happily against an enabled rule and prove nothing.
	r, err := ruleSvc.GetByID(ctx, RuleMBIDResolves)
	if err != nil {
		t.Fatalf("rule %q is not seeded (it is the FK target for its violations): %v", RuleMBIDResolves, err)
	}
	if r.Enabled {
		t.Fatalf("precondition failed: rule %q is seeded ENABLED; it must be seeded disabled", RuleMBIDResolves)
	}

	a := &artist.Artist{Name: "Revalidation Subject", SortName: "Revalidation Subject", Path: t.TempDir()}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	const msg = "The stored MusicBrainz ID resolves to \"Someone Else\", which lists no releases at all, while 3 album(s) are on disk."
	if err := ruleSvc.RaiseMBIDValidationFailure(ctx, a.ID, a.Name, msg); err != nil {
		t.Fatalf("RaiseMBIDValidationFailure: %v", err)
	}

	got, _, err := ruleSvc.ListViolationsFilteredPaged(ctx, ViolationListParams{Status: "active"})
	if err != nil {
		t.Fatalf("listing violations: %v", err)
	}
	for _, v := range got {
		if v.RuleID == RuleMBIDResolves {
			return a, v.ID
		}
	}
	t.Fatalf("the raised mbid_resolves violation is not on the Action Queue: %+v", got)
	return nil, ""
}

// TestMBIDResolvesViolation_IsRaisedAndNotFixable asserts the entry reaches the
// operator's queue AND that it is marked not-fixable.
//
// The Fixable flag is the load-bearing half. #2810's acceptance criteria say no
// identity is changed automatically as a result of the pass, and a true here
// would put a Fix button on the finding -- offering exactly the automatic
// identity revert the issue forbids, and one no Fixer.CanFix would answer.
func TestMBIDResolvesViolation_IsRaisedAndNotFixable(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	artistSvc := artist.NewService(db)
	ruleSvc := NewService(db)
	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}

	a, vid := seedMBIDResolvesViolation(t, ruleSvc, artistSvc)

	got, _, err := ruleSvc.ListViolationsFilteredPaged(ctx, ViolationListParams{Status: "active"})
	if err != nil {
		t.Fatalf("listing violations: %v", err)
	}
	var found *RuleViolation
	for i := range got {
		if got[i].ID == vid {
			found = &got[i]
		}
	}
	if found == nil {
		t.Fatalf("violation %s is not on the Action Queue", vid)
	}
	if found.ArtistID != a.ID {
		t.Errorf("ArtistID = %q, want %q", found.ArtistID, a.ID)
	}
	if found.Fixable {
		t.Error("mbid_resolves must be marked NOT fixable: #2810 forbids an automatic identity change")
	}
	if found.Status != ViolationStatusOpen {
		t.Errorf("Status = %q, want %q", found.Status, ViolationStatusOpen)
	}
}

// TestMBIDResolvesViolation_UpsertsRatherThanAccumulates asserts a re-check
// updates the single open entry. Without this the sweep would add one row per
// pass, so an artist re-checked daily would grow a queue of identical findings.
func TestMBIDResolvesViolation_UpsertsRatherThanAccumulates(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	artistSvc := artist.NewService(db)
	ruleSvc := NewService(db)
	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}

	a, vid := seedMBIDResolvesViolation(t, ruleSvc, artistSvc)

	if err := ruleSvc.RaiseMBIDValidationFailure(ctx, a.ID, a.Name, "a second pass reached the same conclusion"); err != nil {
		t.Fatalf("second RaiseMBIDValidationFailure: %v", err)
	}

	got, _, err := ruleSvc.ListViolationsFilteredPaged(ctx, ViolationListParams{Status: "active"})
	if err != nil {
		t.Fatalf("listing violations: %v", err)
	}
	var count int
	for _, v := range got {
		if v.RuleID == RuleMBIDResolves {
			count++
			if v.ID != vid {
				t.Errorf("violation id changed on re-raise: %q, want %q", v.ID, vid)
			}
			if v.Message != "a second pass reached the same conclusion" {
				t.Errorf("message was not refreshed: %q", v.Message)
			}
		}
	}
	if count != 1 {
		t.Errorf("found %d mbid_resolves entries after two raises, want exactly 1", count)
	}
}

// TestMBIDResolvesIsEventDriven asserts the structural exclusion from engine
// evaluation.
//
// The finding took two MusicBrainz requests to reach and no local checker can
// re-derive it, so being CONSIDERED is what would destroy it: a considered rule
// reporting no violation is recorded as a PASS, and a pass resolves the open
// entry for that (rule, artist) pair in the same transaction. The exclusion has
// to be structural rather than keyed off the Enabled toggle, or silent data
// loss would sit one UI click away.
func TestMBIDResolvesIsEventDriven(t *testing.T) {
	if !IsEventDriven(RuleMBIDResolves) {
		t.Error("mbid_resolves must be event-driven; engine evaluation would resolve its violations")
	}
	// The parity invariant the engine asserts: an event-driven rule still needs
	// a registered checker and a seeded row.
	var seeded bool
	for _, r := range defaultRules {
		if r.ID == RuleMBIDResolves {
			seeded = true
			if r.Enabled {
				t.Error("mbid_resolves must be seeded disabled")
			}
		}
	}
	if !seeded {
		t.Fatal("mbid_resolves is not in defaultRules; its violations would have no FK target")
	}
}

// TestMBIDResolvesCatalogueEntryOffersNoFix asserts the documentation surface
// agrees with the code: no fix behavior, no fix example. A catalogue entry
// describing a fix for a rule that has none would document a capability the
// product deliberately refuses to have.
func TestMBIDResolvesCatalogueEntryOffersNoFix(t *testing.T) {
	if !CatalogueEntryPresent(RuleMBIDResolves) {
		t.Fatal("mbid_resolves has no catalogue entry")
	}
	e := CatalogueEntry(RuleMBIDResolves)
	if e.FixBehavior != "" {
		t.Errorf("FixBehavior = %q, want empty: this rule has no automated fix", e.FixBehavior)
	}
	if e.FixExample != "" {
		t.Errorf("FixExample = %q, want empty", e.FixExample)
	}
	if e.Guards == "" {
		t.Error("Guards must explain what the rule detects")
	}
	if len(e.Caveats) == 0 {
		t.Error("Caveats must record that nothing is corrected automatically")
	}
}
