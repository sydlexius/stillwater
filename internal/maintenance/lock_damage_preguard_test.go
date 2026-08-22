package maintenance

import (
	"context"
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
// moved on from the damage the candidate was selected for, the guarded write
// declines, and the pair must be reported as decided -- never counted in
// Restored, which is the number the operator reads as "work done".
func TestRepairLockDamagePreGuard_DivergedValueIsNotCountedAsRepaired(t *testing.T) {
	env := newLockDamageEnv(t)
	env.seedArtistWithLocks("a1", "Locked Artist", []string{"biography"})
	env.seedPreGuardBioDamage("a1", beforeCutoff())
	// The live value moved on after the damage row was written, without a
	// history row: the guarded write's compare-and-set must decline.
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
	if len(res.Unrecoverable) != 1 ||
		!strings.Contains(res.Unrecoverable[0].Reason, "changed after the candidate was selected") {
		t.Fatalf("unrecoverable = %+v, want the diverged pair with its reason", res.Unrecoverable)
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
