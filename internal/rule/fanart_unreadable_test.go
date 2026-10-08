package rule

import (
	"context"
	"database/sql"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
)

// The fanart_unreadable rule (#3200) is informational, event-driven and has no
// fixer: the publish path raises it when a backdrop file cannot be read, and it
// must (a) stay visible to the operator even though the rule is seeded
// disabled, (b) survive every engine pass, and (c) never be able to touch a
// file. These tests drive the real read, evaluation and fix paths on SQLite.

// seedFanartUnreadable seeds the rules, creates an artist and raises one
// finding. It asserts the rule is seeded disabled first, so the visibility
// tests cannot pass against an enabled rule and prove nothing.
func seedFanartUnreadable(t *testing.T, db *sql.DB, slots []int) (*artist.Artist, *Service, *artist.Service) {
	t.Helper()
	ctx := context.Background()
	artistSvc := artist.NewService(db)
	ruleSvc := NewService(db)
	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}
	r, err := ruleSvc.GetByID(ctx, RuleFanartUnreadable)
	if err != nil {
		t.Fatalf("rule %q is not seeded (FK target for its violations): %v", RuleFanartUnreadable, err)
	}
	if r.Enabled {
		t.Fatalf("precondition: %q is seeded ENABLED; the visibility tests cover the disabled seed", RuleFanartUnreadable)
	}
	a := &artist.Artist{Name: "Unreadable Subject", SortName: "Unreadable Subject", Path: t.TempDir()}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	if err := ruleSvc.RaiseFanartUnreadable(ctx, a.ID, slots, ""); err != nil {
		t.Fatalf("RaiseFanartUnreadable: %v", err)
	}
	return a, ruleSvc, artistSvc
}

func fanartUnreadableRows(t *testing.T, db *sql.DB, artistID string) (count int, status, message string) {
	t.Helper()
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM rule_violations WHERE rule_id = ? AND artist_id = ?`,
		RuleFanartUnreadable, artistID).Scan(&count); err != nil {
		t.Fatalf("counting rows: %v", err)
	}
	if count == 0 {
		return 0, "", ""
	}
	if err := db.QueryRow(
		`SELECT status, message FROM rule_violations WHERE rule_id = ? AND artist_id = ?`,
		RuleFanartUnreadable, artistID).Scan(&status, &message); err != nil {
		t.Fatalf("reading row: %v", err)
	}
	return count, status, message
}

func TestFanartUnreadable_RaiseIsIdempotentAndUpdatesSlots(t *testing.T) {
	db := setupTestDB(t)
	a, svc, _ := seedFanartUnreadable(t, db, []int{1})

	_, _, first := fanartUnreadableRows(t, db, a.ID)
	if !strings.Contains(first, " 2 ") {
		t.Fatalf("first message should name position 2 (slot index 1): %q", first)
	}
	var firstID string
	if err := db.QueryRow(`SELECT id FROM rule_violations WHERE rule_id = ?`, RuleFanartUnreadable).Scan(&firstID); err != nil {
		t.Fatal(err)
	}

	if err := svc.RaiseFanartUnreadable(t.Context(), a.ID, []int{3, 1, 3}, "The file is not readable."); err != nil {
		t.Fatalf("second raise: %v", err)
	}
	count, status, msg := fanartUnreadableRows(t, db, a.ID)
	if count != 1 {
		t.Fatalf("%d rows after two raises, want exactly 1", count)
	}
	if status != ViolationStatusOpen {
		t.Errorf("status = %q, want open", status)
	}
	if !strings.Contains(msg, "Backdrop file(s) 2, 4 could not") || !strings.Contains(msg, "not readable") {
		t.Errorf("second raise did not replace the slot list (want sorted, de-duplicated 2, 4): %q", msg)
	}
	var secondID string
	if err := db.QueryRow(`SELECT id FROM rule_violations WHERE rule_id = ?`, RuleFanartUnreadable).Scan(&secondID); err != nil {
		t.Fatal(err)
	}
	if secondID != firstID {
		t.Errorf("row id changed on re-raise: %q -> %q", firstID, secondID)
	}
	for _, bad := range [][]int{nil, {}, {-1}, {1, -2}, {math.MaxInt}} {
		if err := svc.RaiseFanartUnreadable(t.Context(), a.ID, bad, ""); err == nil {
			t.Errorf("a raise with slots %v must be refused, not stored", bad)
		}
	}
	if _, _, after := fanartUnreadableRows(t, db, a.ID); after != msg {
		t.Errorf("a refused raise changed the stored message: %q", after)
	}
}

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

// A clean snapshot older than the stored finding must not resolve it. The
// rule_results ordering guard rejects the older pass row, and the violation
// UPDATE has to be undone with it or the two tables disagree.
func TestFanartUnreadable_StaleResolveLeavesANewerFindingOpen(t *testing.T) {
	db := setupTestDB(t)
	a, _, _ := seedFanartUnreadable(t, db, []int{1})
	// Both fake times sit after the seed's real-clock raise, so the only thing
	// under test is newer-raise versus older-resolve.
	t0 := time.Now().UTC().Add(time.Hour)
	newer := NewService(db).WithClock(fixedClock{t0.Add(10 * time.Second)})
	older := NewService(db).WithClock(fixedClock{t0})
	if err := newer.RaiseFanartUnreadable(t.Context(), a.ID, []int{1}, ""); err != nil {
		t.Fatal(err)
	}
	if err := older.ResolveFanartUnreadable(t.Context(), a.ID); err != nil {
		t.Fatalf("stale resolve: %v", err)
	}
	if _, status, _ := fanartUnreadableRows(t, db, a.ID); status != ViolationStatusOpen {
		t.Errorf("status = %q: an older clean snapshot resolved a newer finding", status)
	}
	var passed int
	if err := db.QueryRow(`SELECT passed FROM rule_results WHERE artist_id = ? AND rule_id = ?`,
		a.ID, RuleFanartUnreadable).Scan(&passed); err != nil || passed != 0 {
		t.Errorf("rule_results passed = %d (err %v), want 0 (still failing)", passed, err)
	}
}

// Resolving one artist must not touch another artist's open finding.
func TestFanartUnreadable_ResolveIsScopedToOneArtist(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	a, svc, artistSvc := seedFanartUnreadable(t, db, []int{1})
	b := &artist.Artist{Name: "Second Subject", SortName: "Second Subject", Path: t.TempDir()}
	if err := artistSvc.Create(ctx, b); err != nil {
		t.Fatal(err)
	}
	if err := svc.RaiseFanartUnreadable(ctx, b.ID, []int{2}, ""); err != nil {
		t.Fatal(err)
	}
	if err := svc.ResolveFanartUnreadable(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, status, _ := fanartUnreadableRows(t, db, a.ID); status != ViolationStatusResolved {
		t.Errorf("resolved artist status = %q, want resolved", status)
	}
	if _, status, _ := fanartUnreadableRows(t, db, b.ID); status != ViolationStatusOpen {
		t.Errorf("other artist status = %q: resolving one artist resolved another", status)
	}
	var passed int
	if err := db.QueryRow(`SELECT passed FROM rule_results WHERE artist_id = ? AND rule_id = ?`,
		b.ID, RuleFanartUnreadable).Scan(&passed); err != nil || passed != 0 {
		t.Errorf("other artist rule_results passed = %d (err %v), want 0", passed, err)
	}
}

func TestFanartUnreadable_ResolveClearsAndIsANoOpWhenNothingIsOpen(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	a, svc, _ := seedFanartUnreadable(t, db, []int{1})

	if err := svc.ResolveFanartUnreadable(ctx, a.ID); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, status, _ := fanartUnreadableRows(t, db, a.ID); status != ViolationStatusResolved {
		t.Fatalf("status = %q after resolve, want resolved", status)
	}
	active, err := svc.ListViolationsFiltered(ctx, ViolationListParams{Status: "active", ArtistID: a.ID})
	if err != nil {
		t.Fatal(err)
	}
	for i := range active {
		if active[i].RuleID == RuleFanartUnreadable {
			t.Error("resolved finding is still in the active list")
		}
	}
	// The pass row keeps rule_results consistent with the resolved violation.
	var passed int
	if err := db.QueryRow(`SELECT passed FROM rule_results WHERE artist_id = ? AND rule_id = ?`,
		a.ID, RuleFanartUnreadable).Scan(&passed); err != nil || passed != 1 {
		t.Errorf("rule_results passed = %d (err %v), want 1", passed, err)
	}

	// Nothing open: no error, no new row, no change.
	if err := svc.ResolveFanartUnreadable(ctx, a.ID); err != nil {
		t.Errorf("resolve with nothing open: %v", err)
	}
	if err := svc.ResolveFanartUnreadable(ctx, "no-such-artist"); err != nil {
		t.Errorf("resolve for an artist with no entry: %v", err)
	}
	// A dismissed entry stays dismissed; a later raise does not resurrect it.
	if err := svc.RaiseFanartUnreadable(ctx, a.ID, []int{1}, ""); err != nil {
		t.Fatal(err)
	}
	var id string
	if err := db.QueryRow(`SELECT id FROM rule_violations WHERE rule_id = ?`, RuleFanartUnreadable).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := svc.DismissViolation(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := svc.ResolveFanartUnreadable(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, status, _ := fanartUnreadableRows(t, db, a.ID); status != ViolationStatusDismissed {
		t.Errorf("dismissed entry became %q after resolve", status)
	}
}

// TestFanartUnreadable_VisibleThroughTheReadPathsWhileDisabled is the design
// check for the seed default. The rule is seeded disabled; the finding must
// still reach the operator through each read path the UI uses:
//   - ListViolationsFilteredPaged (Action Queue / dashboard, handlers_dashboard.go)
//   - ListViolationsFiltered with ArtistID (the artist "Other findings" card,
//     handlers_notifications.go handleArtistViolationsTab; also
//     handlers_artist_detail.go buildFieldFindings and the notifications list)
//   - GetViolationsForArtists (Reports, handlers_report.go)
//   - CountActiveViolationsByRule (the rule facet counts on the Action Queue)
//
// All four read rule_violations and join rules without an enabled filter. If
// one of them gained `AND r.enabled = 1`, the finding would silently vanish.
func TestFanartUnreadable_VisibleThroughTheReadPathsWhileDisabled(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	a, svc, _ := seedFanartUnreadable(t, db, []int{1})

	paged, _, err := svc.ListViolationsFilteredPaged(ctx, ViolationListParams{Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	if !hasFanartUnreadable(paged) {
		t.Error("Action Queue read path (ListViolationsFilteredPaged) dropped the finding of a disabled rule")
	}
	// The category-filtered query is the one that INNER JOINs rules.
	cat, _, err := svc.ListViolationsFilteredPaged(ctx, ViolationListParams{Status: "active", Category: TriFilter{Include: []string{string(RuleCategoryImage)}}})
	if err != nil {
		t.Fatal(err)
	}
	if !hasFanartUnreadable(cat) {
		t.Error("category-filtered Action Queue path dropped the finding of a disabled rule")
	}

	// The exact query handleArtistViolationsTab (handlers_notifications.go)
	// issues for the artist's "Other findings" card. That card keeps only rules
	// with no Fields tag (nonFieldViolations), so the rule must also be untagged
	// or the finding would vanish from it.
	forArtist, err := svc.ListViolationsFiltered(ctx, ViolationListParams{
		Status: "active", ArtistID: a.ID, Sort: "severity", Order: "desc",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !hasFanartUnreadable(forArtist) {
		t.Error("artist findings read path (ListViolationsFiltered by artist) dropped the finding of a disabled rule")
	}
	if len(RuleFields(RuleFanartUnreadable)) != 0 {
		t.Error("fanart_unreadable carries a Fields tag, so the artist 'Other findings' card would filter it out")
	}

	reports, err := svc.GetViolationsForArtists(ctx, []string{a.ID})
	if err != nil {
		t.Fatal(err)
	}
	var inReports bool
	for _, v := range reports[a.ID] {
		if v.RuleID == RuleFanartUnreadable {
			inReports = true
			if v.Fixable {
				t.Error("the Reports projection shows the finding as fixable")
			}
		}
	}
	if !inReports {
		t.Error("Reports read path (GetViolationsForArtists) dropped the finding of a disabled rule")
	}

	counts, err := svc.CountActiveViolationsByRule(ctx, ViolationListParams{})
	if err != nil {
		t.Fatal(err)
	}
	var counted bool
	for _, c := range counts {
		counted = counted || c.RuleID == RuleFanartUnreadable
	}
	if !counted {
		t.Error("rule facet counts dropped the finding of a disabled rule")
	}
}

func hasFanartUnreadable(vs []RuleViolation) bool {
	for i := range vs {
		if vs[i].RuleID == RuleFanartUnreadable {
			return true
		}
	}
	return false
}

// TestFanartUnreadable_SurvivesEngineRunEvenWhenEnabled runs the real pipeline
// with the rule flipped ON (the configuration an operator can reach). The
// engine must not consider it: a considered rule with a nil checker is
// recorded as a pass, which resolves the open entry.
func TestFanartUnreadable_SurvivesEngineRunEvenWhenEnabled(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	a, svc, artistSvc := seedFanartUnreadable(t, db, []int{1})

	r, err := svc.GetByID(ctx, RuleFanartUnreadable)
	if err != nil {
		t.Fatal(err)
	}
	r.Enabled = true
	if err := svc.Update(ctx, r); err != nil {
		t.Fatal(err)
	}
	if check, err := svc.GetByID(ctx, RuleFanartUnreadable); err != nil || !check.Enabled {
		t.Fatalf("precondition: rule did not persist as enabled (err=%v)", err)
	}

	pipeline := NewPipeline(NewEngine(svc, db, nil, nil, testLogger()), artistSvc, svc, nil, nil, testLogger())
	res, err := pipeline.RunAllScoped(ctx, RunScopeAll)
	if err != nil {
		t.Fatal(err)
	}
	if res.ArtistsProcessed < 1 {
		t.Fatal("the sweep processed no artists, so this proves nothing")
	}
	var otherPasses int
	if err := db.QueryRow(`SELECT COUNT(*) FROM rule_results WHERE artist_id = ? AND rule_id != ? AND passed = 1`,
		a.ID, RuleFanartUnreadable).Scan(&otherPasses); err != nil || otherPasses == 0 {
		t.Fatalf("engine wrote no pass rows for other rules (err %v): the sweep considered nothing", err)
	}

	count, status, _ := fanartUnreadableRows(t, db, a.ID)
	if count != 1 || status != ViolationStatusOpen {
		t.Errorf("after an engine run: %d rows, status %q; want 1 open", count, status)
	}
	var passed int
	if err := db.QueryRow(`SELECT passed FROM rule_results WHERE artist_id = ? AND rule_id = ?`,
		a.ID, RuleFanartUnreadable).Scan(&passed); err != nil {
		t.Fatal(err)
	}
	if passed != 0 {
		t.Errorf("rule_results passed = %d: the engine considered a rule it must skip", passed)
	}
}

// TestFanartUnreadable_HasNoFixer asserts no registered fixer type claims the
// rule, the finding is stored not-fixable (the flag fix-all filters on), and
// FixViolation refuses it. The constructors only store their dependencies and
// CanFix reads the rule id alone, so nil dependencies are fine here.
func TestFanartUnreadable_HasNoFixer(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	a, svc, artistSvc := seedFanartUnreadable(t, db, []int{1})

	log := testLogger()
	fixers := []Fixer{
		NewNFOFixer(nil, nil, nil, nil, nil, nil),
		NewMetadataFixer(nil, log),
		NewNameLanguageFixer(nil, log),
		NewImageFixer(nil, nil, nil, log),
		NewExtraneousImagesFixer(nil, nil, log),
		NewLogoPaddingFixer(nil, nil, log),
		NewDirectoryRenameFixer(nil, nil, log),
		NewBackdropSequencingFixer(nil, nil, nil, log),
		NewImageDuplicateFixer(db, nil, nil, nil, log),
		NewDiscographyFixer(nil, nil, nil, log),
		NewProviderIDBackfillFixer(nil, nil, log),
		NewCrossArtistBackdropCollisionFixer(log),
	}
	for _, f := range fixers {
		if f.CanFix(&Violation{RuleID: RuleFanartUnreadable, Fixable: true}) {
			t.Errorf("%T claims fanart_unreadable; this rule must never be fixable", f)
		}
	}

	var id string
	var fixable bool
	if err := db.QueryRow(`SELECT id, fixable FROM rule_violations WHERE rule_id = ? AND artist_id = ?`,
		RuleFanartUnreadable, a.ID).Scan(&id, &fixable); err != nil {
		t.Fatal(err)
	}
	if fixable {
		t.Fatal("stored Fixable = true; fix-all would pick this finding up")
	}
	pipeline := NewPipeline(NewEngine(svc, db, nil, nil, testLogger()), artistSvc, svc, fixers, nil, testLogger())
	fr, err := pipeline.FixViolation(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if fr.Fixed || fr.Message != "violation is not fixable" {
		t.Errorf("FixViolation = {Fixed:%v Message:%q}, want a not-fixable refusal", fr.Fixed, fr.Message)
	}
	if _, status, _ := fanartUnreadableRows(t, db, a.ID); status != ViolationStatusOpen {
		t.Errorf("status = %q after a fix attempt, want open", status)
	}
}

// The catalogue entry must stay detection-only and documented. Nothing else
// asserts it, and a fix description here would promise a fix that cannot exist.
func TestFanartUnreadable_CatalogueEntryIsDetectionOnly(t *testing.T) {
	e := CatalogueEntry(RuleFanartUnreadable)
	if e.FixBehavior != "" || e.FixExample != "" {
		t.Errorf("fanart_unreadable must have no fix text: %q / %q", e.FixBehavior, e.FixExample)
	}
	if e.Guards == "" || len(e.Caveats) == 0 {
		t.Error("catalogue entry needs Guards and Caveats")
	}
}
