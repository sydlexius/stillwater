package rule

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
)

// The mbid_resolves rule (#2810) is informational and event-driven: the
// re-validation sweep raises it, the engine never evaluates it, and it has no
// automated fix. These tests pin all three against the real query and
// evaluation paths on a real SQLite database, because each one, if broken,
// fails in the direction of silently discarding an operator's finding.

// seedMBIDResolvesViolation creates an artist and raises one mbid_resolves
// entry for it, returning the artist and the persisted violation id.
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

// TestMBIDResolvesHonorsTheConfiguredSeverity asserts the one knob the rules
// catalogue advertises for this rule actually works.
//
// The catalogue renders "Configurable: Severity only." for every rule exposing
// no other parameter, and this rule is raised OUTSIDE an evaluation pass -- so
// it never reaches the engine's backfill of an unset violation severity from
// r.Config.Severity. A hard-coded severity here would leave the operator with a
// documented setting they can change, save, and watch do nothing: they would
// discover it by finding "warning" rows after asking for "error" ones.
func TestMBIDResolvesHonorsTheConfiguredSeverity(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	artistSvc := artist.NewService(db)
	ruleSvc := NewService(db)
	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}

	// PRECONDITION: the seeded severity is NOT the one this test asks for, so a
	// hard-coded implementation cannot pass by coincidence.
	seeded, err := ruleSvc.GetByID(ctx, RuleMBIDResolves)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if seeded.Config.Severity != "warning" {
		t.Fatalf("precondition: seeded severity = %q, want %q", seeded.Config.Severity, "warning")
	}

	seeded.Config.Severity = "error"
	if err := ruleSvc.Update(ctx, seeded); err != nil {
		t.Fatalf("Update: %v", err)
	}
	// PRECONDITION: the operator's change really is stored. Without this the
	// assertion below could fail for a reason that has nothing to do with the
	// raise path.
	reread, err := ruleSvc.GetByID(ctx, RuleMBIDResolves)
	if err != nil {
		t.Fatalf("re-reading the rule: %v", err)
	}
	if reread.Config.Severity != "error" {
		t.Fatalf("precondition: the configured severity did not persist, got %q", reread.Config.Severity)
	}

	a := &artist.Artist{Name: "Severity Subject", SortName: "Severity Subject", Path: t.TempDir()}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	if err := ruleSvc.RaiseMBIDValidationFailure(ctx, a.ID, a.Name, "a failed re-validation"); err != nil {
		t.Fatalf("RaiseMBIDValidationFailure: %v", err)
	}

	got, _, err := ruleSvc.ListViolationsFilteredPaged(ctx, ViolationListParams{Status: "active"})
	if err != nil {
		t.Fatalf("listing violations: %v", err)
	}
	var found *RuleViolation
	for i := range got {
		if got[i].RuleID == RuleMBIDResolves {
			found = &got[i]
		}
	}
	if found == nil {
		t.Fatal("no mbid_resolves violation was raised")
	}
	if found.Severity != "error" {
		t.Errorf("Severity = %q, want %q: the catalogue documents severity as configurable for this rule", found.Severity, "error")
	}
}

// TestMBIDResolvesFallsBackToTheDefaultSeverity is the inverse, and without it
// the test above could be satisfied by an implementation that read severity
// from somewhere unreliable and left it empty when it could not. An empty
// severity would reach the Action Queue as an unstyled, unsortable row.
func TestMBIDResolvesFallsBackToTheDefaultSeverity(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	artistSvc := artist.NewService(db)
	ruleSvc := NewService(db)
	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}

	seeded, err := ruleSvc.GetByID(ctx, RuleMBIDResolves)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	seeded.Config.Severity = ""
	if err := ruleSvc.Update(ctx, seeded); err != nil {
		t.Fatalf("Update: %v", err)
	}
	// PRECONDITION: the stored config really carries no severity, so the
	// fallback branch is the one under test.
	reread, err := ruleSvc.GetByID(ctx, RuleMBIDResolves)
	if err != nil {
		t.Fatalf("re-reading the rule: %v", err)
	}
	if reread.Config.Severity != "" {
		t.Fatalf("precondition: stored severity = %q, want it empty", reread.Config.Severity)
	}

	a := &artist.Artist{Name: "Fallback Subject", SortName: "Fallback Subject", Path: t.TempDir()}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	if err := ruleSvc.RaiseMBIDValidationFailure(ctx, a.ID, a.Name, "a failed re-validation"); err != nil {
		t.Fatalf("RaiseMBIDValidationFailure: %v", err)
	}

	got, _, err := ruleSvc.ListViolationsFilteredPaged(ctx, ViolationListParams{Status: "active"})
	if err != nil {
		t.Fatalf("listing violations: %v", err)
	}
	for _, v := range got {
		if v.RuleID != RuleMBIDResolves {
			continue
		}
		if v.Severity != "warning" {
			t.Errorf("Severity = %q, want the seeded default %q when the rule carries none", v.Severity, "warning")
		}
		return
	}
	t.Fatal("no mbid_resolves violation was raised")
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

// TestMBIDResolvesChipsTheMusicBrainzIDField asserts the finding reaches the
// field it is about.
//
// The artist-detail screen reads RuleFields to render an inline chip on each
// field a live violation touches. This rule inspects the stored MusicBrainz ID
// and nothing else, so an entry with no Fields would leave an operator
// reviewing the artist with no marker on the one row they have to look at to
// judge the finding -- the finding would still be listed, just detached from
// its evidence.
func TestMBIDResolvesChipsTheMusicBrainzIDField(t *testing.T) {
	// PRECONDITION: the field key this rule must declare is the same one the
	// screen already renders for another rule on that row. Comparing against
	// nfo_has_mbid rather than a literal means a future rename of the field key
	// cannot leave this test passing against a key the UI no longer uses.
	want := RuleFields(RuleNFOHasMBID)
	if len(want) != 1 || want[0] != "musicbrainz_id" {
		t.Fatalf("precondition: RuleFields(nfo_has_mbid) = %v, want exactly [musicbrainz_id]", want)
	}

	got := RuleFields(RuleMBIDResolves)
	if len(got) != 1 || got[0] != want[0] {
		t.Errorf("RuleFields(mbid_resolves) = %v, want %v: the finding must chip the field it is about", got, want)
	}
}

// severityLogRecorder captures records so a test can assert on the LEVEL a
// condition was reported at, which is the whole subject of the cancellation
// test below. A JSON buffer would work too, but the level is what matters here
// and reading it off the record is exact.
type severityLogRecorder struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *severityLogRecorder) Enabled(context.Context, slog.Level) bool { return true }

func (h *severityLogRecorder) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *severityLogRecorder) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *severityLogRecorder) WithGroup(string) slog.Handler      { return h }

// messagesAtLevel returns the messages logged at exactly the given level.
func (h *severityLogRecorder) messagesAtLevel(level slog.Level) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, r := range h.records {
		if r.Level == level {
			out = append(out, r.Message)
		}
	}
	return out
}

// TestConfiguredSeverityDoesNotWarnOnCancellation is the rule-side half of the
// invariant the sweep already holds: a condition on OUR side is never reported
// as though the artist were at fault.
//
// RaiseMBIDValidationFailure reads the configured severity once per failed
// artist, on the sweep's own context. A shutdown part-way through a pass fails
// that read for every remaining artist at once, so a WARN there produces one
// line per artist -- each naming an artist that is fine -- which is exactly the
// burst the sweep's own cancellation branches were added to prevent, recreated
// one layer down.
func TestConfiguredSeverityDoesNotWarnOnCancellation(t *testing.T) {
	db := setupTestDB(t)
	rec := &severityLogRecorder{}
	ruleSvc := NewService(db).WithLogger(slog.New(rec))
	if err := ruleSvc.SeedDefaults(context.Background()); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// PRECONDITION: the lookup really does fail, and it fails with a WRAPPED
	// cancellation rather than the bare sentinel. Both halves are load-bearing:
	// without a failure the branch under test is never entered, and without the
	// wrapping an == comparison would satisfy this test while silently ceasing
	// to match in production, where the repository layer wraps with %w.
	_, err := ruleSvc.GetByID(ctx, RuleMBIDResolves)
	if err == nil {
		t.Fatal("precondition: GetByID succeeded on a canceled context; the branch under test is unreachable")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("precondition: GetByID error %v does not wrap context.Canceled", err)
	}
	if errors.Unwrap(err) == nil {
		t.Fatalf("precondition: GetByID error %v is the bare sentinel, so an == comparison could pass this test", err)
	}

	got := ruleSvc.configuredSeverity(ctx, RuleMBIDResolves, "warning")
	if got != "warning" {
		t.Errorf("configuredSeverity = %q, want the seeded default %q: a cancellation must still fall back", got, "warning")
	}
	if msgs := rec.messagesAtLevel(slog.LevelWarn); len(msgs) != 0 {
		t.Errorf("a canceled severity lookup logged at WARN: %v -- a shutdown would print one line per artist", msgs)
	}
	if msgs := rec.messagesAtLevel(slog.LevelError); len(msgs) != 0 {
		t.Errorf("a canceled severity lookup logged at ERROR: %v", msgs)
	}
}

// TestConfiguredSeverityStillWarnsOnARealFailure is the other half, and without
// it the fix above could be a blanket "never warn" that swallows the genuine
// failures the WARN exists for -- a missing rule row, a broken database -- and
// nobody would learn that the severity an operator configured is not being
// read.
func TestConfiguredSeverityStillWarnsOnARealFailure(t *testing.T) {
	db := setupTestDB(t)
	rec := &severityLogRecorder{}
	ruleSvc := NewService(db).WithLogger(slog.New(rec))

	ctx := context.Background()
	// PRECONDITION: the rule is deliberately NOT seeded, so the lookup fails
	// for a real reason, and that reason is NOT a cancellation -- otherwise this
	// test would be asserting the same branch as the one above.
	_, err := ruleSvc.GetByID(ctx, RuleMBIDResolves)
	if err == nil {
		t.Fatal("precondition: the rule must not be seeded, so the lookup genuinely fails")
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("precondition: the failure must NOT be a cancellation, got %v", err)
	}

	got := ruleSvc.configuredSeverity(ctx, RuleMBIDResolves, "warning")
	if got != "warning" {
		t.Errorf("configuredSeverity = %q, want the fallback %q", got, "warning")
	}
	if msgs := rec.messagesAtLevel(slog.LevelWarn); len(msgs) != 1 {
		t.Errorf("WARN messages = %v, want exactly 1: a real lookup failure must still be reported", msgs)
	}
}
