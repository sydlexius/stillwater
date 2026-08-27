package maintenance

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// lock_damage_preguard_test.go -- the PRE-GUARD population (#3079).
//
// These share newLockDamageEnv and its seed helpers with
// lock_damage_repair_test.go deliberately: the two modes select from ONE
// damage set, so a fixture that means "damage" in one file must mean the same
// thing in the other. What differs is the source (never a rule) and the
// timestamp (the upper bound is the whole safety property). The dry run's
// no-write property is pinned at the cmd layer, by row-level diff over three
// tables (TestLockDamagePreGuardDryRun_ReportsWithoutWriting).

// beforeCutoff and afterCutoff bracket preGuardCutoff, derived FROM it rather
// than written as literals. Hardcoding "2026-05-01" would make these fixtures
// silently stop straddling the bound if the constant ever moved, and the
// straddle is the only thing they assert.
func beforeCutoff() string {
	return preGuardCutoff.Add(-24 * time.Hour).UTC().Format(time.RFC3339)
}

func afterCutoff() string {
	return preGuardCutoff.Add(24 * time.Hour).UTC().Format(time.RFC3339)
}

// seedPreGuardBioDamage writes the canonical pre-guard biography damage:
// manual-sourced (the shape of every row written before #3048, and
// byte-identical to an operator edit) at the given timestamp.
func (e *lockDamageEnv) seedPreGuardBioDamage(artistID, at string) {
	e.t.Helper()
	e.insertChange(artistID+"-dmg-biography", artistID, "biography",
		"curated bio", "junk bio", "manual", at)
}

// requirePreGuardRow asserts, FROM THE DATABASE, that a fixture row really is
// damage-shaped, really is unattributed, and really sits on the side of the
// bound the test believes it does. Every assertion below rests on these: a
// "not selected" test whose row was silently stored on the wrong side, or was
// never damage-shaped at all, passes against a predicate matching nothing.
func (e *lockDamageEnv) requirePreGuardRow(changeID string, wantBefore bool) {
	e.t.Helper()
	var oldV, newV, src, raw string
	if err := e.db.QueryRow(
		`SELECT old_value, new_value, source, created_at FROM metadata_changes WHERE id = ?`,
		changeID).Scan(&oldV, &newV, &src, &raw); err != nil {
		e.t.Fatalf("fixture: reading change %s: %v", changeID, err)
	}
	if oldV == "" || oldV == newV {
		e.t.Fatalf("fixture: change %s is not damage-shaped", changeID)
	}
	if strings.HasPrefix(src, "rule:") {
		e.t.Fatalf("fixture: change %s has source %q; this population names no rule", changeID, src)
	}
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		e.t.Fatalf("fixture: unparsable created_at %q on %s: %v", raw, changeID, err)
	}
	if got := at.Before(preGuardCutoff); got != wantBefore {
		e.t.Fatalf("fixture: change %s before the cutoff = %v, want %v (created_at=%s, cutoff=%s)",
			changeID, got, wantBefore, raw, preGuardCutoff.Format(time.RFC3339))
	}
}

// THE POSITIVE CONTROL PAIR FOR THE UPPER TIME BOUND, AND THE TEST THE WHOLE
// FEATURE RESTS ON.
//
// Two artists, identical unattributable damage on an identically locked
// field. The ONLY difference is which side of preGuardCutoff the damage sits
// on. The old one is restored; the new one is not, and is COUNTED as excluded
// rather than silently dropped. The negative half is what proves the bound
// exists at all: without it the positive half stays green against a predicate
// carrying no time condition.
//
// MUTATION PROOF (the acceptance criterion's named mutation): delete the
//
//	if !u.DamagedAt.Before(preGuardCutoff) { res.PreGuardTooNew++; continue }
//
// block in selectPreGuardCandidates and this test FAILS: a2 becomes a
// candidate, is restored, and PreGuardTooNew reads 0. Verified by running it
// both ways; see the report in the PR for the verbatim output.
func TestRepairLockDamagePreGuard_TimeBoundIsEnforced(t *testing.T) {
	env := newLockDamageEnv(t)

	env.seedArtistWithLocks("a1", "Old Damage", []string{"biography"})
	env.seedArtistWithLocks("a2", "New Damage", []string{"biography"})
	env.seedPreGuardBioDamage("a1", beforeCutoff())
	env.seedPreGuardBioDamage("a2", afterCutoff())

	// PRECONDITIONS. a2 must be eligible on EVERY axis except the time bound,
	// or "a2 was not selected" is true for the wrong reason.
	env.requireLockedBio()
	if !env.lockedFields("a2")["biography"] {
		t.Fatal("fixture: biography is not locked on a2")
	}
	env.requirePreGuardRow("a1-dmg-biography", true)
	env.requirePreGuardRow("a2-dmg-biography", false)

	res, err := env.svc.RepairLockDamage(context.Background(), LockDamageOpts{PreGuard: true})
	if err != nil {
		t.Fatalf("RepairLockDamage(PreGuard): %v", err)
	}

	if len(res.Restored) != 1 {
		t.Fatalf("restored %d pairs, want exactly 1 (only the pre-cutoff damage)", len(res.Restored))
	}
	if res.Restored[0].ArtistID != "a1" {
		t.Errorf("restored artist = %q, want a1", res.Restored[0].ArtistID)
	}
	if got := env.biography("a1"); got != "curated bio" {
		t.Errorf("a1 biography = %q, want the restored value", got)
	}
	if got := env.biography("a2"); got != "junk bio" {
		t.Errorf("a2 biography = %q, want it left alone: its damage is newer than the cutoff", got)
	}
	// The bound's effect is REPORTED, never silent.
	if res.PreGuardTooNew != 1 {
		t.Errorf("PreGuardTooNew = %d, want 1; a row the bound excluded must be counted, not dropped",
			res.PreGuardTooNew)
	}
	// And no revert row exists for a2: nothing was written on its behalf.
	var a2Reverts int
	if err := env.db.QueryRow(
		`SELECT COUNT(*) FROM metadata_changes WHERE artist_id = 'a2' AND source = 'revert'`).Scan(&a2Reverts); err != nil {
		t.Fatalf("counting a2 revert rows: %v", err)
	}
	if a2Reverts != 0 {
		t.Errorf("a2 has %d revert rows, want 0", a2Reverts)
	}
}

// The lock is still condition 1 in this mode: an unlocked field is ordinary
// metadata churn, not damage the operator declared must not change. It is
// COUNTED rather than listed -- on a real library these outnumber the
// eligible rows by an order of magnitude and would bury them.
func TestRepairLockDamagePreGuard_UnlockedFieldIsNotRestored(t *testing.T) {
	env := newLockDamageEnv(t)

	env.seedArtistWithLocks("a1", "Locked Artist", []string{"biography"})
	env.seedArtistWithLocks("a2", "Unlocked Artist", nil)
	env.seedPreGuardBioDamage("a1", beforeCutoff())
	env.seedPreGuardBioDamage("a2", beforeCutoff())

	env.requireLockedBio()
	env.requireNotLocked("a2", "biography")
	env.requirePreGuardRow("a2-dmg-biography", true)

	res, err := env.svc.RepairLockDamage(context.Background(), LockDamageOpts{PreGuard: true})
	if err != nil {
		t.Fatalf("RepairLockDamage(PreGuard): %v", err)
	}
	if len(res.Restored) != 1 || res.Restored[0].ArtistID != "a1" {
		t.Fatalf("restored = %+v, want exactly a1", res.Restored)
	}
	if got := env.biography("a2"); got != "junk bio" {
		t.Errorf("a2 biography = %q, want it untouched: the field is not locked", got)
	}
	if res.PreGuardUnlocked != 1 {
		t.Errorf("PreGuardUnlocked = %d, want 1", res.PreGuardUnlocked)
	}
}

// RESTORES ARE REVERSIBLE IN FACT, NOT MERELY IN PRINCIPLE. #3074's objection
// treated a false positive as permanent loss. It is not: the restore's own
// history row carries the value it overwrote. This test PERFORMS the
// recovery rather than asserting a row exists and calling it reversible.
func TestRepairLockDamagePreGuard_RestoreIsReversibleInFact(t *testing.T) {
	ctx := context.Background()
	env := newLockDamageEnv(t)
	env.seedArtistWithLocks("a1", "Locked Artist", []string{"biography"})
	env.seedPreGuardBioDamage("a1", beforeCutoff())
	env.requireLockedBio()
	env.requirePreGuardRow("a1-dmg-biography", true)

	res, err := env.svc.RepairLockDamage(ctx, LockDamageOpts{PreGuard: true})
	if err != nil {
		t.Fatalf("RepairLockDamage(PreGuard): %v", err)
	}
	if len(res.Restored) != 1 {
		t.Fatalf("restored %d, want 1", len(res.Restored))
	}

	// The overwritten value survives ON the restore's own row, as old_value.
	var oldV, newV string
	if err := env.db.QueryRow(
		`SELECT old_value, new_value FROM metadata_changes
		 WHERE artist_id = 'a1' AND field = 'biography' AND source = 'revert'`,
	).Scan(&oldV, &newV); err != nil {
		t.Fatalf("reading the restore's history row: %v", err)
	}
	if oldV != "junk bio" || newV != "curated bio" {
		t.Fatalf("restore row = (old=%q, new=%q), want the overwritten value preserved as old_value",
			oldV, newV)
	}

	// RE-REVERTING RECOVERS IT. This is the operator's undo path (the same
	// verb the history Revert button uses), driven here to prove the state is
	// genuinely recoverable rather than merely recorded.
	changed, err := env.artistSvc.UpdateField(ctx, "a1", "biography", oldV)
	if err != nil {
		t.Fatalf("re-reverting: %v", err)
	}
	if !changed {
		t.Fatal("re-revert reported no change; the restore is then not undoable by this path")
	}
	if got := env.biography("a1"); got != "junk bio" {
		t.Errorf("biography after re-revert = %q, want the pre-restore value back", got)
	}
}

// A SECOND PASS RESTORES NOTHING, PROVEN BY THE QUERY AND NOT BY A BOOLEAN.
// No settings key is read anywhere in this package (the run-once guard lives
// in cmd/stillwater), so this converges on its own merits: the restore writes
// a source="revert" row, which becomes the newest row for the pair and drops
// it from the damage predicate both modes share.
func TestRepairLockDamagePreGuard_SecondPassRestoresNothing(t *testing.T) {
	env := newLockDamageEnv(t)
	env.seedArtistWithLocks("a1", "Locked Artist", []string{"biography"})
	env.seedPreGuardBioDamage("a1", beforeCutoff())
	env.requireLockedBio()

	first, err := env.svc.RepairLockDamage(context.Background(), LockDamageOpts{PreGuard: true})
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if len(first.Restored) != 1 {
		t.Fatalf("first pass restored %d, want 1", len(first.Restored))
	}

	second, err := env.svc.RepairLockDamage(context.Background(), LockDamageOpts{PreGuard: true})
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if len(second.Restored) != 0 {
		t.Fatalf("second pass restored %d, want 0", len(second.Restored))
	}
	// And the pair left the population entirely rather than merely failing a
	// write: the query itself no longer sees it.
	if second.UnattributableAll != 0 {
		t.Errorf("UnattributableAll = %d on the second pass, want 0; the revert row must retire the pair",
			second.UnattributableAll)
	}
	if got := env.biography("a1"); got != "curated bio" {
		t.Errorf("biography = %q after two passes, want the restored value once", got)
	}
}

// THE TWO MODES PARTITION THE DAMAGE SET. A rule-sourced row belongs to the
// attributed pass and must never be selected here. Without this, a widening
// of either predicate could make both act on the same pair -- restoring it,
// then "restoring" the restore.
func TestRepairLockDamagePreGuard_DoesNotTakeRuleSourcedRows(t *testing.T) {
	env := newLockDamageEnv(t)
	env.seedArtistWithLocks("a1", "Rule Damage", []string{"biography"})
	// Rule-sourced, and old enough to pass the time bound: only the SOURCE
	// keeps it out of this mode.
	env.insertChange("a1-dmg-biography", "a1", "biography",
		"curated bio", "junk bio", "rule:metadata_quality", beforeCutoff())

	env.requireLockedBio()
	// Deliberately NOT requirePreGuardRow: that helper refuses a rule: source,
	// and a rule: source is exactly this fixture's point. Assert the two
	// properties that matter here instead -- the row is old enough to clear
	// the bound, so only the SOURCE can be keeping it out.
	var src, at string
	if err := env.db.QueryRow(
		`SELECT source, created_at FROM metadata_changes WHERE id = 'a1-dmg-biography'`).Scan(&src, &at); err != nil {
		t.Fatalf("fixture: reading the damage row: %v", err)
	}
	if !strings.HasPrefix(src, "rule:") {
		t.Fatalf("fixture: source = %q, want a rule: source", src)
	}
	if ts, err := time.Parse(time.RFC3339, at); err != nil || !ts.Before(preGuardCutoff) {
		t.Fatalf("fixture: created_at %q is not before the cutoff (err=%v)", at, err)
	}

	res, err := env.svc.RepairLockDamage(context.Background(), LockDamageOpts{PreGuard: true})
	if err != nil {
		t.Fatalf("RepairLockDamage(PreGuard): %v", err)
	}
	if len(res.Restored) != 0 {
		t.Fatalf("pre-guard mode restored %d rule-sourced pairs, want 0: that population belongs to the attributed pass",
			len(res.Restored))
	}
	if res.UnattributableAll != 0 {
		t.Errorf("UnattributableAll = %d, want 0: a rule-sourced row is not unattributable", res.UnattributableAll)
	}
}

// A RESTORE THAT WRITES NOTHING IS NOT A REPAIR. When the stored value has
// moved on from the damage the candidate was selected for, nothing is
// written, and the pair must never be counted in Restored -- the number the
// operator reads as "work done".
//
// WHERE THE DIVERGENCE IS CAUGHT MOVED IN THE #3079 REVIEW FIX (HIGH-2), AND
// THIS TEST MOVED WITH IT. It used to reach the guarded write and be filed
// into Unrecoverable; now selectPreGuardCandidates asks the same question
// first, so the pair never becomes a candidate and is counted in
// PreGuardDiverged. That is the POINT of the change: the dry run short-
// circuits before the guarded write, so a divergence only the write could see
// was previewed as "would restore" and then silently declined -- measured on
// a clone as preview 215, repair 214.
//
// THE GUARDED WRITE'S CHECK IS NOT REDUNDANT AND WAS NOT REMOVED. This one
// runs during selection and closes the PREVIEW's honesty. That one runs
// inside the write transaction and closes the RACE between this read and the
// write, which is the window an operator editing during the pass opens. Their
// coverage overlaps here only because this fixture diverges BEFORE the pass
// starts; TestRepairLockDamage_ConcurrentEditBetweenReadAndWriteIsNotOverwritten
// drives the window only the transactional check can see.
func TestRepairLockDamagePreGuard_DivergedValueIsNotCountedAsRepaired(t *testing.T) {
	env := newLockDamageEnv(t)
	env.seedArtistWithLocks("a1", "Locked Artist", []string{"biography"})
	env.seedPreGuardBioDamage("a1", beforeCutoff())
	// The live value moved on after the damage row was written, without a
	// history row, so the damage row is still rank 1 and the SQL still selects
	// it: only a value comparison can catch this.
	env.setBiography("something else entirely")

	env.requireLockedBio()
	env.requirePreGuardRow("a1-dmg-biography", true)

	res, err := env.svc.RepairLockDamage(context.Background(), LockDamageOpts{PreGuard: true})
	if err != nil {
		t.Fatalf("RepairLockDamage(PreGuard): %v", err)
	}
	if len(res.Restored) != 0 {
		t.Fatalf("restored %d, want 0: nothing was written", len(res.Restored))
	}
	if res.PreGuardDiverged != 1 {
		t.Fatalf("PreGuardDiverged = %d, want 1: the pair must be COUNTED as excluded, "+
			"not silently dropped", res.PreGuardDiverged)
	}
	if got := env.biography("a1"); got != "something else entirely" {
		t.Errorf("biography = %q, want the newer value untouched", got)
	}
}

// THE CUTOFF IS DERIVED AND AUDITABLE, not a number somebody picked. Pinning
// it to v1.6.2's commit timestamp -- the first RELEASE carrying the persist
// chokepoint -- makes any change to the constant a deliberate edit here too,
// with a new derivation to write down.
func TestPreGuardCutoff_IsTheV162ReleaseInstant(t *testing.T) {
	// git log -1 --format=%cI v1.6.2  ->  2026-08-18T18:56:23-07:00
	want := time.Date(2026, time.August, 19, 1, 56, 23, 0, time.UTC)
	if !preGuardCutoff.Equal(want) {
		t.Errorf("preGuardCutoff = %s, want %s (v1.6.2, commit ec8c8100)",
			preGuardCutoff.Format(time.RFC3339), want.Format(time.RFC3339))
	}
	if PreGuardCutoff() != preGuardCutoff {
		t.Error("PreGuardCutoff() does not report the constant the predicate uses")
	}
	// The bound must be in the PAST, which is what makes the population
	// closed. A cutoff drifting into the future would silently reopen it.
	if !preGuardCutoff.Before(time.Now()) {
		t.Error("preGuardCutoff is not in the past; the population is then not closed")
	}
}

// damageDirection is the ONLY thing about a field's content this package
// emits, so its vocabulary is pinned. Rune-based, so an accented character
// does not read as growth.
func TestDamageDirection(t *testing.T) {
	cases := []struct {
		name, oldV, newV, want string
	}{
		{"emptied", "abc", "", "emptied"},
		{"shorter", "abcdef", "abc", "shorter"},
		{"longer", "abc", "abcdef", "longer"},
		{"same length, different content", "abc", "xyz", "same-length"},
		{"multibyte is measured in runes, not bytes", "aaaa", "éé", "shorter"},
	}
	for _, tc := range cases {
		if got := damageDirection(tc.oldV, tc.newV); got != tc.want {
			t.Errorf("%s: damageDirection = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// NO FIELD VALUE ESCAPES THE PASS. Every string on the reported struct is
// checked against the fixture's values: the reported set is what a preview
// prints and what a report quotes, and this is private library content.
func TestRepairLockDamagePreGuard_ReportsCarryNoFieldValues(t *testing.T) {
	const secretOld = "SECRET_CURATED_BIOGRAPHY"
	const secretNew = "SECRET_JUNK_BIOGRAPHY"
	env := newLockDamageEnv(t)
	env.seedArtistWithLocks("a1", "SECRET_ARTIST_NAME", []string{"biography"})
	env.insertChange("a1-dmg-biography", "a1", "biography",
		secretOld, secretNew, "manual", beforeCutoff())
	env.setBiography(secretNew)
	env.requireLockedBio()

	res, err := env.svc.RepairLockDamage(context.Background(),
		LockDamageOpts{DryRun: true, PreGuard: true})
	if err != nil {
		t.Fatalf("RepairLockDamage(DryRun, PreGuard): %v", err)
	}
	if len(res.Restored) != 1 {
		t.Fatalf("reported %d restorable, want 1", len(res.Restored))
	}
	r := res.Restored[0]
	for _, field := range []string{r.ArtistID, r.ArtistName, r.Field, r.RuleID, r.Direction} {
		for _, secret := range []string{secretOld, secretNew, "SECRET_ARTIST_NAME"} {
			if strings.Contains(field, secret) {
				t.Errorf("a reported field carries private content (%q)", secret)
			}
		}
	}
	// The direction descriptor is present and drawn from the fixed
	// vocabulary: the preview cannot group the cut without it.
	switch r.Direction {
	case "emptied", "shorter", "longer", "same-length":
	default:
		t.Errorf("Direction = %q, want one of the four descriptors", r.Direction)
	}
}

// TestRepairLockDamagePreGuard_BoundaryInstantIsExcluded pins the STRICTNESS
// of the upper time bound, which the existing straddle test does not.
//
// # WHY THIS EXISTS (#3079 review, LOW-1)
//
// The header of selectPreGuardCandidates claims "the comparison is STRICT --
// a row exactly at the boundary is excluded, the allow-list direction holding
// on an ambiguous instant." That claim had no test with teeth. Every fixture
// straddled the bound by +/-24h, so mutating
//
//	!u.DamagedAt.Before(preGuardCutoff)   // exclusive, correct
//
// to
//
//	u.DamagedAt.After(preGuardCutoff)     // INCLUSIVE, admits the boundary
//
// left the ENTIRE internal/maintenance suite green: at +/-24h the two
// predicates agree, and nothing sat on the one instant where they differ.
//
// MUTATION PROOF: apply that exact mutation and this test FAILS -- a1 is
// restored (1 candidate, PreGuardTooNew 0) where it must be excluded (0
// candidates, PreGuardTooNew 1). Verified both ways.
//
// WHY EXCLUSIVE IS THE CORRECT DIRECTION. The bound means "damage written
// before the release that ended this damage". A row stamped at the release
// instant itself cannot be shown to predate the fix, and the allow-list rule
// this feature is built on resolves an ambiguous row to NOT RESTORED. The
// cost of excluding it is that one genuinely-damaged row stays damaged and
// visible in the blast-radius pane; the cost of admitting it is a write over
// a value that may postdate the guard. Those are not symmetric.
func TestRepairLockDamagePreGuard_BoundaryInstantIsExcluded(t *testing.T) {
	env := newLockDamageEnv(t)

	// EXACTLY the cutoff instant, to the second, formatted the way the
	// repository stores timestamps. Derived from the constant, never a
	// literal: a moved cutoff must move this fixture with it, or the test
	// silently stops sitting on the boundary and goes vacuous.
	atBoundary := preGuardCutoff.UTC().Format(time.RFC3339)

	env.seedArtistWithLocks("a1", "Boundary Damage", []string{"biography"})
	env.seedPreGuardBioDamage("a1", atBoundary)

	// PRECONDITIONS. The row must be eligible on every other axis, and it must
	// really sit ON the instant -- not one second either side, which is what
	// makes this different from the straddle test.
	env.requireLockedBio()
	env.requirePreGuardRow("a1-dmg-biography", false)
	var raw string
	if err := env.db.QueryRow(
		`SELECT created_at FROM metadata_changes WHERE id = ?`,
		"a1-dmg-biography").Scan(&raw); err != nil {
		t.Fatalf("fixture: reading the boundary row: %v", err)
	}
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatalf("fixture: unparsable created_at %q: %v", raw, err)
	}
	if !at.Equal(preGuardCutoff) {
		t.Fatalf("fixture: created_at %s is not EXACTLY the cutoff %s; "+
			"this test is vacuous unless the row sits on the boundary instant",
			at.Format(time.RFC3339), preGuardCutoff.Format(time.RFC3339))
	}

	res, err := env.svc.RepairLockDamage(context.Background(), LockDamageOpts{PreGuard: true})
	if err != nil {
		t.Fatalf("RepairLockDamage(PreGuard): %v", err)
	}

	if len(res.Restored) != 0 {
		t.Errorf("restored %d pairs, want 0; a row AT the cutoff instant must be excluded "+
			"(the comparison is strict, so the bound admits only rows strictly before it)",
			len(res.Restored))
	}
	if res.PreGuardTooNew != 1 {
		t.Errorf("PreGuardTooNew = %d, want 1; the boundary row must be COUNTED as excluded, "+
			"not silently dropped", res.PreGuardTooNew)
	}
	// The field must be untouched: an excluded row is not a written row.
	if got := env.biography("a1"); got != "junk bio" {
		t.Errorf("a1 biography = %q, want the damaged value left in place", got)
	}
}

// TestRepairLockDamagePreGuard_DryRunExcludesDivergedRows closes the
// preview's OVERSTATEMENT (#3079 review, HIGH-2).
//
// The dry run used to short-circuit at `if opts.DryRun { append }` BEFORE
// RestoreLockedFieldGuarded ran, so it never consulted the guarded
// compare-and-set. A pair whose stored value had already moved on was
// previewed as "would restore" and then silently declined at write time:
// measured on a clone as dry-run 215 / repair 214, with nothing comparing the
// two.
//
// The preview must answer the SAME question the write answers. It does now,
// through the same exported predicate (artist.FieldValueStillDamaged), and
// the excluded row is COUNTED rather than dropped.
//
// MUTATION PROOF: delete the PreGuardDiverged check in
// selectPreGuardCandidates and this fails -- the diverged pair is previewed
// as restorable (1, not 0) and PreGuardDiverged reads 0.
func TestRepairLockDamagePreGuard_DryRunExcludesDivergedRows(t *testing.T) {
	env := newLockDamageEnv(t)
	env.seedArtistWithLocks("a1", "Diverged", []string{"biography"})
	env.seedPreGuardBioDamage("a1", beforeCutoff())
	// The live value moved on WITHOUT a history row, so the damage row is
	// still rank 1 and the pair is still selected by the SQL -- only the
	// divergence check can catch it.
	env.setBiography("operator hotfix")

	env.requireLockedBio()
	env.requirePreGuardRow("a1-dmg-biography", true)
	if got := env.biography("a1"); got != "operator hotfix" {
		t.Fatalf("fixture: biography = %q, want the diverged value", got)
	}

	res, err := env.svc.RepairLockDamage(context.Background(),
		LockDamageOpts{PreGuard: true, DryRun: true})
	if err != nil {
		t.Fatalf("RepairLockDamage(PreGuard,DryRun): %v", err)
	}

	if len(res.Restored) != 0 {
		t.Errorf("the preview offered %d row(s), want 0: the stored value diverged, "+
			"so the write would decline it and the preview must say so", len(res.Restored))
	}
	if res.PreGuardDiverged != 1 {
		t.Errorf("PreGuardDiverged = %d, want 1; a row excluded for divergence must be "+
			"counted, not silently dropped", res.PreGuardDiverged)
	}
}

// TestRepairLockDamagePreGuard_DigestGate covers the token that makes the
// preview BINDING on the write (#3079 review, HIGH-1).
//
// Three cases, and the SECOND is the one that matters: the reviewer's
// reproduction was a lock toggled between preview and repair, which pulls a
// previously-invisible row into the population. Before the gate the write
// simply took it.
func TestRepairLockDamagePreGuard_DigestGate(t *testing.T) {
	t.Run("the approved digest lets the write proceed", func(t *testing.T) {
		env := newLockDamageEnv(t)
		env.seedArtistWithLocks("a1", "Approved", []string{"biography"})
		env.seedPreGuardBioDamage("a1", beforeCutoff())
		env.requireLockedBio()

		preview, err := env.svc.RepairLockDamage(context.Background(),
			LockDamageOpts{PreGuard: true, DryRun: true})
		if err != nil {
			t.Fatalf("preview: %v", err)
		}
		digest := LockDamageDigest(preview.Restored)
		if digest == "" {
			t.Fatal("the preview produced an empty digest")
		}

		res, err := env.svc.RepairLockDamage(context.Background(),
			LockDamageOpts{PreGuard: true, ApprovedDigest: digest})
		if err != nil {
			t.Fatalf("the write refused a digest taken from its own preview: %v", err)
		}
		if len(res.Restored) != 1 {
			t.Fatalf("restored %d, want 1", len(res.Restored))
		}
		if got := env.biography("a1"); got != "curated bio" {
			t.Errorf("biography = %q, want the restored value", got)
		}
	})

	t.Run("a lock added after the preview REFUSES the write", func(t *testing.T) {
		env := newLockDamageEnv(t)
		// a1 is previewed. a2 carries identical damage on a field that is NOT
		// locked yet, so it is invisible to the preview.
		env.seedArtistWithLocks("a1", "Previewed", []string{"biography"})
		env.seedArtistWithLocks("a2", "Locked Later", nil)
		env.seedPreGuardBioDamage("a1", beforeCutoff())
		env.seedPreGuardBioDamage("a2", beforeCutoff())

		preview, err := env.svc.RepairLockDamage(context.Background(),
			LockDamageOpts{PreGuard: true, DryRun: true})
		if err != nil {
			t.Fatalf("preview: %v", err)
		}
		if len(preview.Restored) != 1 {
			t.Fatalf("preview offered %d, want 1 (a2's field is not locked yet)", len(preview.Restored))
		}
		digest := LockDamageDigest(preview.Restored)

		// THE TOGGLE. An ordinary operator action between the two commands.
		env.lockField("a2", "biography")

		_, err = env.svc.RepairLockDamage(context.Background(),
			LockDamageOpts{PreGuard: true, ApprovedDigest: digest})
		var drift *LockDamageDriftError
		if !errors.As(err, &drift) {
			t.Fatalf("err = %v, want a *LockDamageDriftError: the set grew from 1 to 2 "+
				"after approval, and the write must refuse it", err)
		}
		if drift.ActualCount != 2 {
			t.Errorf("drift.ActualCount = %d, want 2", drift.ActualCount)
		}
		// NOTHING WRITTEN. The refusal must be total, not partial: a gate that
		// restores the approved row and then refuses the rest has already done
		// half the damage it exists to prevent.
		if got := env.biography("a1"); got != "junk bio" {
			t.Errorf("a1 biography = %q, want it UNTOUCHED; a refused pass writes nothing at all", got)
		}
		if got := env.biography("a2"); got != "junk bio" {
			t.Errorf("a2 biography = %q, want it untouched", got)
		}
	})

	t.Run("a digest is not required for a dry run", func(t *testing.T) {
		env := newLockDamageEnv(t)
		env.seedArtistWithLocks("a1", "Preview Only", []string{"biography"})
		env.seedPreGuardBioDamage("a1", beforeCutoff())

		res, err := env.svc.RepairLockDamage(context.Background(),
			LockDamageOpts{PreGuard: true, DryRun: true})
		if err != nil {
			t.Fatalf("a dry run must not require a digest: %v", err)
		}
		if len(res.Restored) != 1 {
			t.Fatalf("restored %d, want 1", len(res.Restored))
		}
	})
}

// TestLockDamageDigest_DetectsASwap pins the property a COUNT cannot carry.
// One row leaving the set and another joining it keeps the count identical
// while changing what gets written, which is exactly the case an operator
// produces by locking one field and unlocking another in the same sitting.
func TestLockDamageDigest_DetectsASwap(t *testing.T) {
	approved := []LockDamageRestore{{ChangeID: "c1"}, {ChangeID: "c2"}}
	swapped := []LockDamageRestore{{ChangeID: "c1"}, {ChangeID: "c3"}}

	if len(approved) != len(swapped) {
		t.Fatal("fixture: the two sets must be the same SIZE, or this proves nothing about counts")
	}
	if LockDamageDigest(approved) == LockDamageDigest(swapped) {
		t.Error("a swap of equal size produced the same digest; the gate would wave it through")
	}

	// ORDER-INDEPENDENT: the digest describes membership, not the query's
	// ORDER BY or the preview's display order.
	reordered := []LockDamageRestore{{ChangeID: "c2"}, {ChangeID: "c1"}}
	if LockDamageDigest(approved) != LockDamageDigest(reordered) {
		t.Error("the digest changed when only the ORDER changed; two runs selecting the " +
			"same rows must agree")
	}
	// A DROP and an ADD both move it.
	if LockDamageDigest(approved) == LockDamageDigest(approved[:1]) {
		t.Error("dropping a row did not move the digest")
	}
	if LockDamageDigest(approved) == LockDamageDigest(append(slices.Clone(approved),
		LockDamageRestore{ChangeID: "c9"})) {
		t.Error("adding a row did not move the digest")
	}
}
