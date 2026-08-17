package rule

import (
	"context"
	"database/sql"
	"slices"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
)

// #3037: a fixer must not claim credit for a write the lock guard reverted.
//
// READ THIS BEFORE EDITING. Every "the row stayed open" assertion is PAIRED
// with a positive control on an otherwise identical UNLOCKED artist that must
// be RESOLVED. That branch is reachable only when a fixer genuinely ran and
// genuinely wrote a guarded field, so an unpaired assertion would pass the
// moment the harness stopped reaching the fixer at all.

// lockRevertFixture seeds one artist with `bio` and an OPEN, fixable
// bio_exists violation, pinning `lockFields` through the lock mutator -- not by
// setting LockedFields on the struct, which the chokepoint pins away.
func lockRevertFixture(t *testing.T, bio string, lockFields ...string) (*sql.DB, *artist.Service, *artist.Artist, *Service, *RuleViolation, context.Context) {
	t.Helper()
	db := setupTestDB(t)
	ctx := context.Background()

	artistSvc := artist.NewService(db)
	ruleSvc := NewService(db)
	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}

	a := &artist.Artist{Name: "Lock Revert Subject", SortName: "Lock Revert Subject", Path: t.TempDir(), Biography: bio}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	if len(lockFields) > 0 {
		if err := artistSvc.SetLockedFields(ctx, a.ID, lockFields); err != nil {
			t.Fatalf("locking %v: %v", lockFields, err)
		}
	}
	stored, err := artistSvc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("reloading artist: %v", err)
	}
	// PRECONDITIONS: without both, the guard never fires and every later
	// assertion holds vacuously.
	if stored.Biography != bio {
		t.Fatalf("precondition: biography = %q, want %q", stored.Biography, bio)
	}
	if len(stored.LockedFields) != len(lockFields) {
		t.Fatalf("precondition: locked_fields = %v, want %v; the lock did not persist", stored.LockedFields, lockFields)
	}

	rv := &RuleViolation{
		RuleID:     RuleBioExists,
		ArtistID:   a.ID,
		ArtistName: a.Name,
		Severity:   "error",
		Message:    "biography needs work",
		Fixable:    true,
		Status:     ViolationStatusOpen,
	}
	if err := ruleSvc.UpsertViolation(ctx, rv); err != nil {
		t.Fatalf("upserting violation: %v", err)
	}
	return db, artistSvc, stored, ruleSvc, rv, ctx
}

// TestFixViolation_LockRevertedFixLeavesTheRowOpen is the headline regression.
// A fixer whose ONLY change was reverted must not report Fixed and must not
// resolve the row.
//
// OPEN, not dismissed, and that is argued: a lock is operator-REVOCABLE, so the
// outcome is not terminal. A dismissed row has no un-dismiss route
// (UpsertViolation pins 'dismissed' per #1107; ReopenViolation only accepts
// 'resolved'), which would mean that after the operator unlocks, the finding
// never returns and the field is never repaired.
func TestFixViolation_LockRevertedFixLeavesTheRowOpen(t *testing.T) {
	const pinned = "the operator wrote this by hand"

	// POSITIVE CONTROL: the identical fixer on an UNLOCKED artist must land,
	// report Fixed, and RESOLVE. If this fails, the harness never reaches the
	// write and the locked case below would stay open for the wrong reason.
	db, artistSvc, a, ruleSvc, rv, ctx := lockRevertFixture(t, pinned)
	fixer := &bioOverwritingFixer{ruleID: RuleBioExists, newBio: "a rule wrote this"}
	pipeline := NewPipeline(NewEngine(ruleSvc, nil, nil, nil, testLogger()), artistSvc, ruleSvc, []Fixer{fixer}, nil, testLogger())

	fr, err := pipeline.FixViolation(ctx, rv.ID)
	if err != nil {
		t.Fatalf("control FixViolation: %v", err)
	}
	if !fr.Fixed {
		t.Fatalf("positive control FAILED: an UNLOCKED fix reported Fixed=false (%s); the write "+
			"under test is unreachable here", fr.Message)
	}
	stored, err := artistSvc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("control reload: %v", err)
	}
	if stored.Biography != "a rule wrote this" {
		t.Fatalf("positive control FAILED: the UNLOCKED biography is %q, so the write never "+
			"landed and the locked case proves nothing", stored.Biography)
	}
	if got := violationStatus(t, db, a.ID, RuleBioExists); got != ViolationStatusResolved {
		t.Fatalf("positive control FAILED: an UNLOCKED fix left the violation %q, want resolved", got)
	}

	// THE REGRESSION: the same fixer, same rule, on an artist with biography
	// PINNED. The chokepoint restores the value, so the fix did not happen.
	db, artistSvc, a, ruleSvc, rv, ctx = lockRevertFixture(t, pinned, "biography")
	fixer = &bioOverwritingFixer{ruleID: RuleBioExists, newBio: "a rule wrote this"}
	pipeline = NewPipeline(NewEngine(ruleSvc, nil, nil, nil, testLogger()), artistSvc, ruleSvc, []Fixer{fixer}, nil, testLogger())

	fr, err = pipeline.FixViolation(ctx, rv.ID)
	if err != nil {
		t.Fatalf("FixViolation: %v", err)
	}
	// PRECONDITION: the fixer must have RUN, or the open row means nothing.
	if fixer.fixCalls == 0 {
		t.Fatal("precondition: the fixer was never invoked, so nothing exercised the reverted write")
	}
	stored, err = artistSvc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("reloading: %v", err)
	}
	// PRECONDITION: the guard must actually have reverted it.
	if stored.Biography != pinned {
		t.Fatalf("precondition: the pinned biography is %q, want %q -- the chokepoint did not "+
			"revert the write", stored.Biography, pinned)
	}

	if fr.Fixed {
		t.Errorf("FixResult.Fixed = true for a change the lock guard reverted; the operator is "+
			"shown a repair that did not happen (#3037). Message: %s", fr.Message)
	}
	if fr.Dismissed {
		t.Errorf("FixResult.Dismissed = true; a lock is operator-REVOCABLE, so this is not a " +
			"terminal outcome, and a dismissed row can never be un-dismissed")
	}
	if got := violationStatus(t, db, a.ID, RuleBioExists); got != ViolationStatusOpen {
		t.Errorf("violation status = %q, want open; a resolved row closes a finding that "+
			"was never repaired, and a dismissed one closes it permanently", got)
	}
}

// TestFixViolation_PartiallyRevertedFixStillResolves is the OVER-gating guard:
// ALL, not ANY. A rule whose fixer writes two fields with one locked still did
// useful work on the other, and treating that as a failed fix would turn a
// per-FIELD lock into a per-RULE one.
func TestFixViolation_PartiallyRevertedFixStillResolves(t *testing.T) {
	db, artistSvc, a, ruleSvc, rv, ctx := lockRevertFixture(t, "the operator wrote this", "biography")
	fixer := &bioAndOriginFixer{ruleID: RuleBioExists, newBio: "a rule wrote this", newOrigin: "Somewhere, XX"}
	pipeline := NewPipeline(NewEngine(ruleSvc, nil, nil, nil, testLogger()), artistSvc, ruleSvc, []Fixer{fixer}, nil, testLogger())

	fr, err := pipeline.FixViolation(ctx, rv.ID)
	if err != nil {
		t.Fatalf("FixViolation: %v", err)
	}
	stored, err := artistSvc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("reloading: %v", err)
	}
	// PRECONDITIONS: exactly one of the two writes was reverted.
	if stored.Biography != "the operator wrote this" {
		t.Fatalf("precondition: the LOCKED biography is %q; it should have been reverted", stored.Biography)
	}
	if stored.Origin != "Somewhere, XX" {
		t.Fatalf("precondition: the UNLOCKED origin is %q; it should have landed", stored.Origin)
	}

	if !fr.Fixed {
		t.Errorf("Fixed = false although origin really changed; a lock on ONE of a rule's "+
			"fields must not disable the rule. Message: %s", fr.Message)
	}
	if fr.Dismissed {
		t.Error("Dismissed = true for a PARTIALLY reverted fix that really did change a field")
	}
	if got := violationStatus(t, db, a.ID, RuleBioExists); got != ViolationStatusResolved {
		t.Errorf("violation status = %q, want resolved", got)
	}
}

// TestRunForArtist_LockRevertedAutoFixStaysOpenWithItsResultRow covers the
// UNATTENDED path. That is the one that matters at scale: FixViolation is one
// artist at a time, while the scheduled pass sweeps the whole library, so a
// correction that holds only on the click path protects nothing overnight.
//
// It also pins the health trap. Had this path written 'dismissed',
// UpsertViolation would emit NO paired rule_results FAIL row (#1107 writes it
// only for an open/pending row) and every later pass would preserve the
// dismiss, so the row would never appear and offlineHealthScore would refuse to
// score the artist -- freezing its health permanently. Writing the row OPEN
// sidesteps the trap rather than managing it, and this test asserts the
// rule_results row exists to prove that.
func TestRunForArtist_LockRevertedAutoFixStaysOpenWithItsResultRow(t *testing.T) {
	// SHORT, so the bio_exists CHECKER genuinely flags this artist and the auto
	// path dispatches a fix. A long one passes the rule and reaches nothing.
	const pinned = "short"
	db, artistSvc, a, ruleSvc, _, ctx := lockRevertFixture(t, pinned, "biography")
	enableRuleAuto(t, ctx, ruleSvc, RuleBioExists)

	// FIRST-PASS CONDITIONS. The fixture's seeded violation also writes the
	// paired rule_results FAIL row, which would MASK the trap below. Clearing
	// both makes the code under test the only writer of either row.
	if _, err := db.ExecContext(ctx, `DELETE FROM rule_violations WHERE artist_id = ?`, a.ID); err != nil {
		t.Fatalf("clearing seeded violations: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM rule_results WHERE artist_id = ?`, a.ID); err != nil {
		t.Fatalf("clearing seeded rule results: %v", err)
	}
	if _, exists := ruleResultRow(t, db, a.ID, RuleBioExists); exists {
		t.Fatal("precondition: a rule_results row survived the clear, so the trap below is masked")
	}
	// ALSO too short, which is the faithful shape: the guard reverts the write,
	// so the rule still fails after the pass. A passing replacement would make
	// the post-fix evaluation (run against the in-memory struct, BEFORE the
	// persist reverts it) write a passed=1 row that masks the trap below.
	fixer := &bioOverwritingFixer{ruleID: RuleBioExists, newBio: "brief bio"}
	pipeline := NewPipeline(NewEngine(ruleSvc, nil, nil, nil, testLogger()), artistSvc, ruleSvc, []Fixer{fixer}, nil, testLogger())

	if _, err := pipeline.RunForArtist(ctx, a); err != nil {
		t.Fatalf("RunForArtist: %v", err)
	}
	// PRECONDITION: the auto path must have dispatched a fix. A pass where the
	// checker never flagged the artist reaches none of the code under test.
	if fixer.fixCalls == 0 {
		t.Fatal("precondition: no fix was attempted, so the auto-fix revert path was never reached")
	}

	if got := violationStatus(t, db, a.ID, RuleBioExists); got != ViolationStatusOpen {
		t.Fatalf("the auto path left the violation %q for a fix the lock guard fully reverted, "+
			"want open; a resolved row hides a finding that was never repaired, and a "+
			"dismissed one hides it permanently even after the operator unlocks", got)
	}

	// THE TRAP: the paired rule_results row must exist. Without the baseline
	// open upsert it never would, and offlineHealthScore refuses to score an
	// artist with no complete evaluation -- its health freezes permanently.
	if _, exists := ruleResultRow(t, db, a.ID, RuleBioExists); !exists {
		t.Error("no rule_results row exists for this rule. UpsertViolation writes the paired FAIL " +
			"row only for an open/pending row (#1107), so a status that is not open leaves the " +
			"artist with no evaluation baseline and offlineHealthScore freezes its health " +
			"permanently -- #3060's trap.")
	}
}

// ruleFixHistoryCount returns how many "rule_fix" history rows the artist has.
// That row and the activity.recent rail event share one emitter
// (recordRuleFixHistory), so counting rows is how a test observes whether the
// un-recallable rail event was pushed.
func ruleFixHistoryCount(t *testing.T, ctx context.Context, h *artist.HistoryService, artistID string) int {
	t.Helper()
	changes, _, err := h.List(ctx, artistID, 100, 0)
	if err != nil {
		t.Fatalf("listing history: %v", err)
	}
	n := 0
	for _, c := range changes {
		if c.Field == "rule_fix" {
			n++
		}
	}
	return n
}

// bioFixEntry returns the single bio_exists entry in a run's results, failing on
// any other count. Exactly one is the point: two would mean the pass dispatched
// the rule twice and the assertion could be reading either one.
func bioFixEntry(t *testing.T, results []FixResult) FixResult {
	t.Helper()
	var found []FixResult
	for _, r := range results {
		if r.RuleID == RuleBioExists {
			found = append(found, r)
		}
	}
	if len(found) != 1 {
		t.Fatalf("results holds %d bio_exists entries, want exactly 1: %+v", len(found), results)
	}
	return found[0]
}

// runPathCreditCase drives one whole-library run path against a locked artist
// and its unlocked positive control. The paths differ only in which orchestrator
// they call and where they report the tally, which is exactly what a mutation
// severing one path's report would hide.
type runPathCreditCase struct {
	name string
	// run executes the path and returns (fixesAttempted, fixesSucceeded, results).
	run func(t *testing.T, ctx context.Context, p *Pipeline, a *artist.Artist) (int, int, []FixResult)
}

// TestRunPaths_LockRevertedAutoFixEarnsNoCredit is finding 1's regression AND
// finding 2's missing RunAll coverage. Correcting only the violation row left
// every OTHER surface claiming the repair: the run-complete toast reads
// RunResult.FixesSucceeded, and recordRuleFixHistory writes a metadata_changes
// row plus pushes an activity.recent rail event that cannot be recalled.
//
// Each case is PAIRED with an unlocked positive control that must earn all
// three, so a harness that stopped reaching the fixer would fail rather than
// report a satisfying zero.
func TestRunPaths_LockRevertedAutoFixEarnsNoCredit(t *testing.T) {
	cases := []runPathCreditCase{{
		name: "RunForArtist",
		run: func(t *testing.T, ctx context.Context, p *Pipeline, a *artist.Artist) (int, int, []FixResult) {
			t.Helper()
			res, err := p.RunForArtist(ctx, a)
			if err != nil {
				t.Fatalf("RunForArtist: %v", err)
			}
			return res.FixesAttempted, res.FixesSucceeded, res.Results
		},
	}, {
		// THE SCHEDULED WHOLE-LIBRARY SWEEP, and the reason M12 mattered:
		// updateHealthScore's restored return is consumed only here, so a
		// mutation dropping it left every RunForArtist test green.
		name: "processArtistForRunAll",
		run: func(t *testing.T, ctx context.Context, p *Pipeline, a *artist.Artist) (int, int, []FixResult) {
			t.Helper()
			contrib, _ := p.processArtistForRunAll(ctx, a)
			return contrib.fixesAttempted, contrib.fixesSucceeded, contrib.results
		},
	}, {
		// The single-rule sweep, which reaches persistHealthAfterRun rather
		// than updateHealthScore.
		name: "processArtistForRunRule",
		run: func(t *testing.T, ctx context.Context, p *Pipeline, a *artist.Artist) (int, int, []FixResult) {
			t.Helper()
			r, err := p.ruleService.GetByID(ctx, RuleBioExists)
			if err != nil {
				t.Fatalf("loading rule: %v", err)
			}
			contrib, _ := p.processArtistForRunRule(ctx, a, RuleBioExists, r)
			return contrib.fixesAttempted, contrib.fixesSucceeded, contrib.results
		},
	}}

	// build wires a pipeline with history recording ON -- without it
	// recordRuleFixHistory's row write is skipped and the audit assertion below
	// passes vacuously.
	build := func(t *testing.T, lock bool) (*Pipeline, *artist.Artist, *artist.HistoryService, *bioOverwritingFixer, context.Context) {
		t.Helper()
		locks := []string{}
		if lock {
			locks = []string{"biography"}
		}
		db, artistSvc, a, ruleSvc, _, ctx := lockRevertFixture(t, "short", locks...)
		historySvc := artist.NewHistoryService(db)
		artistSvc.SetHistoryService(historySvc)
		enableRuleAuto(t, ctx, ruleSvc, RuleBioExists)
		// Still short after the fix, the faithful shape: the guard reverts the
		// write, so the rule keeps failing.
		fixer := &bioOverwritingFixer{ruleID: RuleBioExists, newBio: "brief bio"}
		p := NewPipeline(NewEngine(ruleSvc, nil, nil, nil, testLogger()), artistSvc, ruleSvc, []Fixer{fixer}, nil, testLogger())
		p.SetHistoryService(historySvc)
		return p, a, historySvc, fixer, ctx
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// POSITIVE CONTROL first: unlocked, so all three credits are earned.
			p, a, hist, fixer, ctx := build(t, false)
			_, succeeded, _ := tc.run(t, ctx, p, a)
			if fixer.fixCalls == 0 {
				t.Fatal("positive control FAILED: the fixer never ran, so this path exercises nothing")
			}
			if succeeded != 1 {
				t.Fatalf("positive control FAILED: FixesSucceeded = %d for an UNLOCKED fix, want 1; "+
					"the credit path is broken and the locked case below proves nothing", succeeded)
			}
			if n := ruleFixHistoryCount(t, ctx, hist, a.ID); n != 1 {
				t.Fatalf("positive control FAILED: %d rule_fix history rows for an UNLOCKED fix, want 1", n)
			}

			// THE REGRESSION: biography pinned, so the guard reverts the only
			// change the fixer made.
			p, a, hist, fixer, ctx = build(t, true)
			attempted, succeeded, results := tc.run(t, ctx, p, a)
			if fixer.fixCalls == 0 {
				t.Fatal("precondition: no fix was attempted, so the revert path was never reached")
			}
			if attempted < 1 {
				t.Errorf("FixesAttempted = %d, want at least 1: a reverted fix is a REFUSED "+
					"WRITE, not an error, and the run really did attempt it", attempted)
			}
			if succeeded != 0 {
				t.Errorf("FixesSucceeded = %d for a fix the lock guard reverted, want 0; the "+
					"run-complete toast tells the operator %d were fixed while the violation "+
					"correctly stays open (#3037)", succeeded, succeeded)
			}
			if n := ruleFixHistoryCount(t, ctx, hist, a.ID); n != 0 {
				t.Errorf("%d rule_fix history rows for a reverted fix, want 0. That row is written "+
					"beside an activity.recent SSE event that CANNOT be recalled, so it must "+
					"never be emitted rather than emitted and reversed", n)
			}
			// The whole-artist paths evaluate every enabled rule, so index by
			// rule id rather than by position -- a positional assertion would
			// break the moment the seeded rule set changes.
			entry := bioFixEntry(t, results)
			if entry.Fixed {
				t.Errorf("the fix's entry in results still reports Fixed=true: %q", entry.Message)
			}
		})
	}
}

// TestPersistHealthAfterRun_NonAuthoritativeBranchReportsRestorations pins the
// branch the code comment defends -- "the restorations there are just as real"
// -- which was the untested one (M11b). It is reached when the post-fix
// evaluation FAILED but a fixer mutation must still be flushed, so the run is
// not authoritative and yet a real write, with real lock restorations, happens.
func TestPersistHealthAfterRun_NonAuthoritativeBranchReportsRestorations(t *testing.T) {
	_, artistSvc, a, ruleSvc, _, ctx := lockRevertFixture(t, "the operator wrote this", "biography")
	p := NewPipeline(NewEngine(ruleSvc, nil, nil, nil, testLogger()), artistSvc, ruleSvc, nil, nil, testLogger())

	a.Biography = "a rule wrote this"
	// postEval nil (evaluation failed) + mustPersist true is the branch.
	authoritative, restored := p.persistHealthAfterRun(ctx, a, nil, true, false, "")
	if authoritative {
		t.Fatal("precondition: a nil postEval must never be authoritative")
	}
	stored, err := artistSvc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("reloading: %v", err)
	}
	if stored.Biography != "the operator wrote this" {
		t.Fatalf("precondition: the pinned biography is %q, so the guard never reverted anything "+
			"and the report below would be empty for the wrong reason", stored.Biography)
	}
	if !slices.Contains(restored, "biography") {
		t.Errorf("restored = %v, want it to contain \"biography\". A non-authoritative run still "+
			"WROTE, so its restorations are just as real -- dropping them here lets the caller "+
			"grant credit for a fix the guard threw away", restored)
	}
}

// TestLockRevertedFixResult_DiskEffectsDefeatTheRevert is finding 3: the
// SavedPath / RemovedFiles / SlotsRemoved arm. No shipped fixer both writes a
// guarded field and touches disk, so this is a forward-guard with no live case
// -- and an untested forward-guard is one the next reader deletes as dead code.
func TestLockRevertedFixResult_DiskEffectsDefeatTheRevert(t *testing.T) {
	base := func(mut func(*FixResult)) *FixResult {
		fr := &FixResult{RuleID: RuleBioExists, Fixed: true, Message: "wrote it"}
		mut(fr)
		return fr
	}
	cases := []struct {
		name       string
		fr         *FixResult
		wantRevert bool
	}{
		{"no disk effect reverts", base(func(*FixResult) {}), true},
		{"SavedPath survives", base(func(f *FixResult) { f.SavedPath = "/lib/a/fanart.jpg" }), false},
		{"RemovedFiles survives", base(func(f *FixResult) { f.RemovedFiles = true }), false},
		{"SlotsRemoved survives", base(func(f *FixResult) { f.SlotsRemoved = 2 }), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := lockRevertedFixResult(tc.fr, []string{"biography"}, []string{"biography"})
			if tc.wantRevert != (got != nil) {
				t.Fatalf("lockRevertedFixResult non-nil = %v, want %v. A fix that left something "+
					"on DISK is real whatever the artists row ended up holding, so reporting it "+
					"reverted would hide a file the operator now has", got != nil, tc.wantRevert)
			}
		})
	}
}

// TestFixResultForRule_MatchesOnRuleID pins the lookup key. Without the rule-id
// match the helper returns the FIRST pending credit whatever rule made it, so a
// multi-rule pass would judge one rule's row against another rule's disk
// effects -- silently, and in the direction that GRANTS the resolve.
func TestFixResultForRule_MatchesOnRuleID(t *testing.T) {
	other := &FixResult{RuleID: "other_rule", Fixed: true, SavedPath: "/lib/a/fanart.jpg"}
	mine := &FixResult{RuleID: RuleBioExists, Fixed: true}
	acc := &runForArtistAccum{pendingCredits: []pendingFixCredit{
		{ruleID: "other_rule", fr: other},
		{ruleID: RuleBioExists, fr: mine},
	}}
	if got := acc.fixResultForRule(RuleBioExists); got != mine {
		t.Errorf("fixResultForRule(%q) = %+v, want this rule's own result; a match on position "+
			"rather than rule id reads another rule's disk effects", RuleBioExists, got)
	}
	// No credit for the rule: nil, never a synthesized Fixed stand-in, which
	// would assert the very property that could not be read.
	if got := acc.fixResultForRule("absent_rule"); got != nil {
		t.Errorf("fixResultForRule(absent) = %+v, want nil", got)
	}
}

// bioWithDiskEffectFixer writes a guarded field AND reports a disk effect. No
// SHIPPED fixer does both, which is exactly why the guard needs a synthetic one:
// it is the only shape that can make the row surface and the credit surface
// disagree.
type bioWithDiskEffectFixer struct {
	ruleID   string
	newBio   string
	fixCalls int
}

func (f *bioWithDiskEffectFixer) CanFix(v *Violation) bool { return v.RuleID == f.ruleID }

func (f *bioWithDiskEffectFixer) Fix(_ context.Context, a *artist.Artist, v *Violation) (*FixResult, error) {
	f.fixCalls++
	a.Biography = f.newBio
	return &FixResult{
		RuleID:    v.RuleID,
		Fixed:     true,
		Message:   "overwrote biography and saved a file",
		SavedPath: "/lib/a/fanart.jpg",
	}, nil
}

// TestRunForArtist_DiskEffectKeepsTheRowAndTheCreditInAgreement is the
// two-surface consistency regression.
//
// splitLockRevertedRows used to build a SYNTHETIC FixResult{Fixed: true} rather
// than reading the fixer's real one. That stand-in has no SavedPath, no
// RemovedFiles and no SlotsRemoved, so lockRevertedFixResult's disk-effects arm
// could never fire on the row path while grantFixCredits -- which passes the
// real result -- saw the disk effect and granted the credit. The run then
// reported one fix succeeded while the violation it repaired stayed OPEN: the
// same split-surface failure this whole change exists to close, reintroduced
// inside the fix for it.
//
// The assertion is AGREEMENT, not a particular verdict. Both surfaces read the
// same input now, so both must reach "the fix stands".
func TestRunForArtist_DiskEffectKeepsTheRowAndTheCreditInAgreement(t *testing.T) {
	db, artistSvc, a, ruleSvc, _, ctx := lockRevertFixture(t, "short", "biography")
	enableRuleAuto(t, ctx, ruleSvc, RuleBioExists)

	// Still short after the fix: the guard reverts the write, so the rule keeps
	// failing and the reverted-fix path is genuinely reached.
	fixer := &bioWithDiskEffectFixer{ruleID: RuleBioExists, newBio: "brief bio"}
	p := NewPipeline(NewEngine(ruleSvc, nil, nil, nil, testLogger()), artistSvc, ruleSvc, []Fixer{fixer}, nil, testLogger())

	res, err := p.RunForArtist(ctx, a)
	if err != nil {
		t.Fatalf("RunForArtist: %v", err)
	}
	// PRECONDITIONS. Without both, the two surfaces agree for the wrong reason:
	// no fix ran, or the guard never reverted anything.
	if fixer.fixCalls == 0 {
		t.Fatal("precondition: the fixer never ran, so neither surface was exercised")
	}
	stored, err := artistSvc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("reloading: %v", err)
	}
	if stored.Biography != "short" {
		t.Fatalf("precondition: the pinned biography is %q, want \"short\" -- the chokepoint did "+
			"not revert the write, so nothing here tests the reverted path", stored.Biography)
	}

	// The disk effect defeats the revert, so the credit is granted.
	if res.FixesSucceeded != 1 {
		t.Errorf("FixesSucceeded = %d, want 1: the fix left a file on disk, which is real "+
			"whatever the artists row ended up holding", res.FixesSucceeded)
	}
	// ...and the ROW must reach the SAME verdict. A resolved count beside an
	// open row tells the operator one fix succeeded while its finding is still
	// outstanding, with no way to tell which claim is true.
	if got := violationStatus(t, db, a.ID, RuleBioExists); got != ViolationStatusResolved {
		t.Errorf("violation status = %q, want resolved. The credit surface granted this fix and "+
			"the row surface re-opened it: splitLockRevertedRows must read the fixer's REAL "+
			"FixResult, not a synthetic one that can never carry a disk effect", got)
	}
}

// bioAndOriginFixer changes TWO guarded fields, so a test can pin one and leave
// the other free -- with a single-field fixer every revert is total and the
// partial branch is unreachable.
type bioAndOriginFixer struct {
	ruleID    string
	newBio    string
	newOrigin string
	fixCalls  int
}

func (f *bioAndOriginFixer) CanFix(v *Violation) bool { return v.RuleID == f.ruleID }

func (f *bioAndOriginFixer) Fix(_ context.Context, a *artist.Artist, v *Violation) (*FixResult, error) {
	f.fixCalls++
	a.Biography = f.newBio
	a.Origin = f.newOrigin
	return &FixResult{RuleID: v.RuleID, Fixed: true, Message: "overwrote biography and origin"}, nil
}
