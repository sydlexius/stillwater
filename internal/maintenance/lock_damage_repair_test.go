package maintenance

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
)

// lockDamageEnv wires the same real services the startup path uses: these
// tests assert against stored rows, so a fake repository would prove nothing
// about the SQL in internal/artist.
type lockDamageEnv struct {
	t         *testing.T
	db        *sql.DB
	svc       *Service
	artistSvc *artist.Service
}

func newLockDamageEnv(t *testing.T) *lockDamageEnv {
	t.Helper()
	db, dbPath := setupTestDBWithImages(t)
	artistSvc := artist.NewService(db)
	hist := artist.NewHistoryService(db)
	artistSvc.SetHistoryService(hist)
	svc := NewService(db, dbPath, t.TempDir(), slog.Default())
	svc.SetLockDamageDeps(hist.Repo(), artistSvc)
	return &lockDamageEnv{t: t, db: db, svc: svc, artistSvc: artistSvc}
}

// seedArtistWithLocks inserts an artist whose biography already holds the
// damaged value ("junk bio"), matching the seedDamage rows below, so the
// staleness recheck sees a live value consistent with the candidate.
func (e *lockDamageEnv) seedArtistWithLocks(id, name string, locked []string) {
	e.t.Helper()
	lf := "[]"
	if len(locked) > 0 {
		b, err := json.Marshal(locked)
		if err != nil {
			e.t.Fatalf("marshaling locked fields: %v", err)
		}
		lf = string(b)
	}
	if _, err := e.db.Exec(
		`INSERT INTO artists (id, name, sort_name, path, biography, locked_fields)
		 VALUES (?, ?, ?, '', ?, ?)`,
		id, name, name, "junk bio", lf); err != nil {
		e.t.Fatalf("seeding artist %s: %v", id, err)
	}
}

// seedBioDamage writes the canonical RULE-SOURCED biography damage row
// ("curated bio" replaced by "junk bio"): the shape the repair is designed to
// attribute. Use seedDamageWithSource for anything else. Every test in this
// file damages biography the same way; what varies is the lock, the source,
// and the naming rule.
func (e *lockDamageEnv) seedBioDamage(artistID, ruleID string) {
	e.t.Helper()
	e.insertChange(artistID+"-dmg-biography", artistID, "biography",
		"curated bio", "junk bio", "rule:"+ruleID, "2026-05-01T10:00:01Z")
}

// seedDamageWithSource seeds a damage row on a1, the locked artist every
// test here builds its fixture around, at the file's canonical damage time.
func (e *lockDamageEnv) seedDamageWithSource(field, oldV, newV, source string) {
	e.t.Helper()
	e.insertChange("a1-dmg-"+field, "a1", field, oldV, newV, source, "2026-05-01T10:00:01Z")
}

func (e *lockDamageEnv) insertChange(id, artistID, field, oldV, newV, source, at string) {
	e.t.Helper()
	if _, err := e.db.Exec(
		`INSERT INTO metadata_changes (id, artist_id, field, old_value, new_value, source, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, artistID, field, oldV, newV, source, at); err != nil {
		e.t.Fatalf("seeding change %s: %v", id, err)
	}
}

// requireLockedBio asserts the fixture's defining property: biography is
// locked on artist a1, the locked artist every test here seeds. A test whose
// lock was never seeded passes vacuously.
func (e *lockDamageEnv) requireLockedBio() {
	e.t.Helper()
	if !e.lockedFields("a1")["biography"] {
		e.t.Fatal("fixture: biography is not locked on a1")
	}
}

func (e *lockDamageEnv) requireNotLocked(artistID, field string) {
	e.t.Helper()
	if e.lockedFields(artistID)[field] {
		e.t.Fatalf("fixture: %s is unexpectedly locked on %s", field, artistID)
	}
}

func (e *lockDamageEnv) lockedFields(artistID string) map[string]bool {
	e.t.Helper()
	var raw string
	if err := e.db.QueryRow(
		`SELECT locked_fields FROM artists WHERE id = ?`, artistID).Scan(&raw); err != nil {
		e.t.Fatalf("reading locked_fields for %s: %v", artistID, err)
	}
	var fields []string
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		e.t.Fatalf("parsing locked_fields for %s: %v", artistID, err)
	}
	out := make(map[string]bool, len(fields))
	for _, f := range fields {
		out[f] = true
	}
	return out
}

func (e *lockDamageEnv) biography(artistID string) string {
	e.t.Helper()
	var bio string
	if err := e.db.QueryRow(
		`SELECT biography FROM artists WHERE id = ?`, artistID).Scan(&bio); err != nil {
		e.t.Fatalf("reading biography for %s: %v", artistID, err)
	}
	return bio
}

// THE POSITIVE CONTROL PAIR: the most important test in the file. The negative
// half is what proves the predicate is not matching everything.
//
// MUTATION PROOF (mutation A, #3075; re-anchored in the fix round): delete
// the lock check (the `if !s.artistService.IsFieldLocked` block in
// RepairLockDamage) and this test FAILS -- no longer on the restore count
// (RestoreLockedFieldGuarded re-verifies the lock inside its transaction, so
// a2 is still not written), but on a2's skip REASON: the transactional
// recheck reports "unlocked after the candidate was selected", while a
// never-locked field must be diverted by the loop's own check with "not
// currently locked". If it stays green under that mutation, the loop's lock
// condition is decorative.
func TestRepairLockDamage_RestoresLockedNotUnlocked(t *testing.T) {
	env := newLockDamageEnv(t)

	// Two artists, identical damage from the same rule. Only the lock differs.
	env.seedArtistWithLocks("a1", "Locked Artist", []string{"biography"})
	env.seedArtistWithLocks("a2", "Unlocked Artist", nil)
	for _, id := range []string{"a1", "a2"} {
		env.seedBioDamage(id, "metadata_quality")
	}

	// PRECONDITIONS. Without these the assertions can pass for the wrong
	// reason: an unseeded lock makes the negative half trivially true.
	env.requireLockedBio()
	env.requireNotLocked("a2", "biography")

	res, err := env.svc.RepairLockDamage(context.Background(), LockDamageOpts{})
	if err != nil {
		t.Fatalf("RepairLockDamage: %v", err)
	}

	if len(res.Restored) != 1 {
		t.Fatalf("restored %d pairs, want exactly 1", len(res.Restored))
	}
	if res.Restored[0].ArtistID != "a1" {
		t.Errorf("restored artist = %q, want a1", res.Restored[0].ArtistID)
	}
	if got := env.biography("a1"); got != "curated bio" {
		t.Errorf("a1 biography = %q, want the restored value", got)
	}
	if got := env.biography("a2"); got != "junk bio" {
		t.Errorf("a2 biography = %q, want it left damaged", got)
	}
	// a2 is diverted by the LOOP's lock check, not by the guarded write's
	// transactional recheck: a never-locked field is decided before any write
	// machinery runs, and the two paths report different reasons.
	if len(res.Unrecoverable) != 1 || res.Unrecoverable[0].ArtistID != "a2" ||
		!strings.Contains(res.Unrecoverable[0].Reason, "not currently locked") {
		t.Errorf("unrecoverable = %+v, want exactly a2 with the 'not currently locked' reason", res.Unrecoverable)
	}

	// THE RESTORE MUST BE RECORDED AS A REVERT. source="revert" does two jobs:
	// it makes the second pass converge, and it drops the repaired pair from
	// the blast-radius damage predicate (blastRadiusDamageWhere excludes ONLY
	// "revert"). A restore recorded under any other source reappears forever as
	// fresh damage in the blast-radius pane. Convergence alone cannot pin this:
	// a "manual"-sourced restore ALSO fails LIKE 'rule:%', so the second-pass
	// test stays green without the tag.
	//
	// MUTATION PROOF (mutation E, fix round): delete the
	// artist.ContextWithSource(ctx, "revert") call in attemptLockDamageRestore
	// and this
	// assertion FAILS: the history row is recorded with the default source
	// "manual" and the revert count reads 0.
	var revertRows int
	if err := env.db.QueryRow(
		`SELECT COUNT(*) FROM metadata_changes
		 WHERE artist_id = 'a1' AND field = 'biography' AND source = 'revert'`).Scan(&revertRows); err != nil {
		t.Fatalf("counting revert rows: %v", err)
	}
	if revertRows != 1 {
		t.Errorf("revert-sourced history rows for the restore = %d, want 1; "+
			"a restore recorded under another source re-reads as damage forever", revertRows)
	}
}

// THE ATTRIBUTION CONTROL. Damage whose source does not name a rule is
// indistinguishable from an operator edit and must never be restored. This
// pins the deliberate decision NOT to widen to unattributed damage; a future
// change that weakens the source predicate to "repair more" must fail here.
//
// MUTATION PROOF (mutation C, #3075): drop `AND r.source LIKE 'rule:%'` from
// lockDamageQuery in internal/artist/sqlite_history.go and this test FAILS:
// the manual row becomes a candidate AND still appears in the unattributed
// report, so Unrecoverable gets two entries instead of one (the capability
// check diverts the garbage rule id parsed from "manual" -- defense in depth
// working -- but the double count trips the exact-1 assertion below). Run
// together with TestLockDamageCandidates_ExcludesAnOperatorEdit in
// internal/artist, which the same mutation must also kill.
func TestRepairLockDamage_SkipsDamageWithNoAttributingRuleFix(t *testing.T) {
	env := newLockDamageEnv(t)

	env.seedArtistWithLocks("a1", "Locked Artist", []string{"biography"})
	// Damage, but a manual source: indistinguishable from an operator edit.
	env.seedDamageWithSource("biography", "curated bio", "junk bio",
		"manual")

	env.requireLockedBio()

	res, err := env.svc.RepairLockDamage(context.Background(), LockDamageOpts{})
	if err != nil {
		t.Fatalf("RepairLockDamage: %v", err)
	}

	if len(res.Restored) != 0 {
		t.Fatalf("restored %d pairs, want 0 (no attribution)", len(res.Restored))
	}
	if got := env.biography("a1"); got != "junk bio" {
		t.Errorf("biography = %q, want it untouched", got)
	}
	// The pair must be reported EXACTLY ONCE, by the unattributed report. The
	// candidate query and the unattributed query partition the damage set, so
	// a second entry means the manual row leaked into the candidate list.
	if len(res.Unrecoverable) != 1 {
		t.Fatalf("unrecoverable = %d, want exactly 1", len(res.Unrecoverable))
	}
	if !strings.Contains(res.Unrecoverable[0].Reason, "names no rule") {
		t.Errorf("unrecoverable reason = %q, want the no-attribution reason", res.Unrecoverable[0].Reason)
	}
}

// THE CAPABILITY CONTROL. A damage row can NAME a rule that never writes the
// damaged field (a forged or corrupted source); the allow-list requires the
// catalogue to corroborate the claim.
//
// MUTATION PROOF (mutation B, #3075): delete the RuleFields capability check
// (the `if !slices.Contains(...)` block in RepairLockDamage) and this test
// FAILS: biography is locked, the live value matches the candidate, and the
// write proceeds, so len(Restored) becomes 1.
func TestRepairLockDamage_SkipsFieldTheRuleCannotWrite(t *testing.T) {
	env := newLockDamageEnv(t)

	env.seedArtistWithLocks("a1", "Locked Artist", []string{"biography"})
	// origin_missing declares "origin", never "biography".
	env.seedBioDamage("a1", "origin_missing")

	env.requireLockedBio()

	res, err := env.svc.RepairLockDamage(context.Background(), LockDamageOpts{})
	if err != nil {
		t.Fatalf("RepairLockDamage: %v", err)
	}
	if len(res.Restored) != 0 {
		t.Fatalf("restored %d pairs, want 0 (rule cannot write this field)", len(res.Restored))
	}
	if got := env.biography("a1"); got != "junk bio" {
		t.Errorf("biography = %q, want it untouched", got)
	}
}

// MALFORMED AND UNKNOWN rule: SOURCES. The capability control above uses a
// REAL catalogue rule that declares a different field; these two shapes are
// the rest of the unrecognized-input space, and an allow-list whose refusal
// of unrecognized input is unproven is exactly what the design says must
// never be assumed:
//
//   - source = "rule:" exactly (the empty rule id): SUBSTR(source, 6) yields
//     "", and RuleFields("") returns nil.
//   - a rule-prefixed source naming an id absent from the catalogue entirely
//     (a deprecated or corrupted id): RuleFields returns nil for it too.
//
// For each: NOT restored, counted in the unrecoverable tally EXACTLY once,
// and -- asserted DIRECTLY by count, not inferred from the value being
// untouched -- NO source="revert" history row is written for the pair.
func TestRepairLockDamage_MalformedRuleSourceIsRefused(t *testing.T) {
	cases := []struct {
		name   string
		source string
	}{
		{name: "empty rule id (source is exactly rule:)", source: "rule:"},
		{name: "rule id absent from the catalogue", source: "rule:no_such_rule_ever"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newLockDamageEnv(t)
			env.seedArtistWithLocks("a1", "Locked Artist", []string{"biography"})
			env.seedDamageWithSource("biography", "curated bio", "junk bio", tc.source)
			env.requireLockedBio()

			res, err := env.svc.RepairLockDamage(context.Background(), LockDamageOpts{})
			if err != nil {
				t.Fatalf("RepairLockDamage: %v", err)
			}
			if len(res.Restored) != 0 {
				t.Fatalf("restored %d, want 0 -- an unrecognized source must never restore", len(res.Restored))
			}
			if got := env.biography("a1"); got != "junk bio" {
				t.Errorf("biography = %q, want it untouched", got)
			}
			// EXACT tally: the pair is counted once, not merely "non-zero".
			if len(res.Unrecoverable) != 1 {
				t.Fatalf("unrecoverable = %+v, want exactly 1 entry for the refused pair", res.Unrecoverable)
			}
			if res.Unrecoverable[0].Field != "biography" {
				t.Errorf("unrecoverable field = %q, want biography", res.Unrecoverable[0].Field)
			}
			// The refusal writes NOTHING: no revert row may exist for the
			// pair, asserted directly rather than inferred from the value.
			var reverts int
			if err := env.db.QueryRow(
				`SELECT COUNT(*) FROM metadata_changes WHERE artist_id = 'a1' AND source = 'revert'`).Scan(&reverts); err != nil {
				t.Fatalf("counting revert rows: %v", err)
			}
			if reverts != 0 {
				t.Errorf("revert rows = %d, want 0 -- a refused row must write no history", reverts)
			}
		})
	}
}

// IDEMPOTENCE. NO settings flag is consulted here: this asserts the QUERY
// converged -- the restore's source="revert" row is now newest for the pair,
// so the damage predicate stops matching. A test that relied on the flag would
// pass even if the predicate had not converged.
func TestRepairLockDamage_SecondPassRestoresNothing(t *testing.T) {
	env := newLockDamageEnv(t)
	env.seedArtistWithLocks("a1", "Locked Artist", []string{"biography"})
	env.seedBioDamage("a1", "metadata_quality")
	env.requireLockedBio()

	first, err := env.svc.RepairLockDamage(context.Background(), LockDamageOpts{})
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if len(first.Restored) != 1 {
		t.Fatalf("first pass restored %d, want 1", len(first.Restored))
	}

	second, err := env.svc.RepairLockDamage(context.Background(), LockDamageOpts{})
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if len(second.Restored) != 0 {
		t.Fatalf("second pass restored %d, want 0", len(second.Restored))
	}
	if got := env.biography("a1"); got != "curated bio" {
		t.Errorf("biography = %q after two passes, want the restored value once", got)
	}
}

// An operator edit AFTER the damage is the newest row for the pair, so the
// damage row is no longer rank 1 and must not be restored over.
func TestRepairLockDamage_OperatorEditAfterDamageBlocksRestore(t *testing.T) {
	env := newLockDamageEnv(t)
	env.seedArtistWithLocks("a1", "Locked Artist", []string{"biography"})
	env.seedBioDamage("a1", "metadata_quality")
	// The operator then wrote their own value.
	env.insertChange("a1-edit", "a1", "biography", "junk bio", "operator value",
		"manual", "2026-05-01T11:00:00Z")
	// Keep the live row consistent with the newest history row, as it would be
	// after a real operator edit.
	env.setBiography("operator value")

	env.requireLockedBio()

	res, err := env.svc.RepairLockDamage(context.Background(), LockDamageOpts{})
	if err != nil {
		t.Fatalf("RepairLockDamage: %v", err)
	}
	if len(res.Restored) != 0 {
		t.Fatalf("restored %d pairs, want 0 (operator edited after the damage)", len(res.Restored))
	}
	if got := env.biography("a1"); got != "operator value" {
		t.Errorf("biography = %q, want the operator's value untouched", got)
	}
}

// The documented unrecoverable path is PRE-#3048 damage: a manual-sourced row
// the mechanism cannot attribute. Such a row is filtered by the candidate
// QUERY, so it never reaches the repair loop -- reporting it is the job of the
// companion LockDamageUnattributed query, and this test pins that the two
// views compose into a non-silent tally.
func TestRepairLockDamage_ReportsPre3048DamageAsUnrecoverable(t *testing.T) {
	env := newLockDamageEnv(t)
	env.seedArtistWithLocks("a1", "Locked Artist", []string{"biography"})
	// Manual source: exactly what a pre-#3048 rule write looks like on disk,
	// and byte-identical to an operator edit. No attribution is possible.
	env.seedDamageWithSource("biography", "curated bio", "junk bio",
		"manual")

	env.requireLockedBio()

	res, err := env.svc.RepairLockDamage(context.Background(), LockDamageOpts{})
	if err != nil {
		t.Fatalf("RepairLockDamage: %v", err)
	}
	if len(res.Restored) != 0 {
		t.Fatalf("restored %d, want 0 -- unattributable damage must never be "+
			"restored, it is indistinguishable from an operator edit", len(res.Restored))
	}
	if len(res.Unrecoverable) != 1 {
		t.Fatalf("unrecoverable = %d, want 1 -- reporting a silent zero for "+
			"damage the mechanism cannot see is the defect this exists to stop",
			len(res.Unrecoverable))
	}
	if res.Unrecoverable[0].Field != "biography" {
		t.Errorf("unrecoverable field = %q, want biography", res.Unrecoverable[0].Field)
	}
}

// THE UNRECOVERABLE LIST IS SCOPED TO FIELDS LOCKED NOW; THE WIDER COUNT IS
// ONE NUMBER (fix round for #3075). Unfiltered, the per-row list is dominated
// by ordinary manual edits to unlocked fields (production clone: 3234 rows,
// 216 on a field locked today), burying the rows the feature can act on. Both
// numbers are reported so the filtered list can never read as the whole
// population; the blast-radius pane surfaces the full set.
//
// MUTATION PROOF (mutation G, fix round): delete the IsFieldLocked filter in
// reportUnattributedLockDamage and this test FAILS: the unlocked
// origin row joins the list and Unrecoverable becomes 2.
func TestRepairLockDamage_UnattributableListFilteredToLockedFields(t *testing.T) {
	env := newLockDamageEnv(t)
	env.seedArtistWithLocks("a1", "Locked Artist", []string{"biography"})
	// Two unattributable damage rows on the same artist: one on the locked
	// biography, one on the never-locked origin.
	env.seedDamageWithSource("biography", "curated bio", "junk bio",
		"manual")
	env.seedDamageWithSource("origin", "Seattle", "Tacoma",
		"manual")

	env.requireLockedBio()
	env.requireNotLocked("a1", "origin")

	res, err := env.svc.RepairLockDamage(context.Background(), LockDamageOpts{})
	if err != nil {
		t.Fatalf("RepairLockDamage: %v", err)
	}
	if res.UnattributableAll != 2 {
		t.Errorf("UnattributableAll = %d, want 2 (the wider count keeps every row)", res.UnattributableAll)
	}
	if len(res.Unrecoverable) != 1 {
		t.Fatalf("unrecoverable = %d, want exactly 1 (only the locked-field row is actionable)", len(res.Unrecoverable))
	}
	if res.Unrecoverable[0].Field != "biography" {
		t.Errorf("unrecoverable field = %q, want biography (the locked one)", res.Unrecoverable[0].Field)
	}
}

// A PSEUDO-SOURCE GETS AN ACCURATE REASON (fix round for #3075).
// "rule:multiple_rules" and its siblings pass LIKE 'rule:%' but name no
// catalogue rule, so the capability check's "the attributing rule does not
// write this field" would be FALSE: the row WAS rule damage, and no rule
// named multiple_rules exists to consult. The row is still never restored
// (the allow-list direction holding); only the reported why changes.
func TestRepairLockDamage_PseudoSourceGetsAccurateReason(t *testing.T) {
	env := newLockDamageEnv(t)
	env.seedArtistWithLocks("a1", "Locked Artist", []string{"biography"})
	env.seedBioDamage("a1", "multiple_rules")

	env.requireLockedBio()

	res, err := env.svc.RepairLockDamage(context.Background(), LockDamageOpts{})
	if err != nil {
		t.Fatalf("RepairLockDamage: %v", err)
	}
	if len(res.Restored) != 0 {
		t.Fatalf("restored %d, want 0 -- a pseudo-source must never be restored", len(res.Restored))
	}
	if got := env.biography("a1"); got != "junk bio" {
		t.Errorf("biography = %q, want it untouched", got)
	}
	if len(res.Unrecoverable) != 1 {
		t.Fatalf("unrecoverable = %d, want 1", len(res.Unrecoverable))
	}
	if !strings.Contains(res.Unrecoverable[0].Reason, "batch write") {
		t.Errorf("reason = %q, want the batch-write reason, not the false "+
			"'rule does not write this field' one", res.Unrecoverable[0].Reason)
	}
}

// UNATTRIBUTABLE-ALL IS THE WIDER COUNT, NOT THE FILTERED LIST'S LENGTH. The
// field exists precisely so the locked-now list can never read as the whole
// population (the 3234-vs-216 property from the production clone, in
// miniature): a regression that sets it from the filtered list, or leaves it
// zero, must fail here.
func TestRepairLockDamage_UnattributableAllCountsUnlockedRows(t *testing.T) {
	t.Run("locked field: listed and counted", func(t *testing.T) {
		env := newLockDamageEnv(t)
		env.seedArtistWithLocks("a1", "Locked Artist", []string{"biography"})
		env.seedDamageWithSource("biography", "curated bio", "junk bio",
			"manual")
		env.requireLockedBio()

		res, err := env.svc.RepairLockDamage(context.Background(), LockDamageOpts{})
		if err != nil {
			t.Fatalf("RepairLockDamage: %v", err)
		}
		if len(res.Unrecoverable) != 1 {
			t.Fatalf("unrecoverable = %d, want 1 (the locked-field row is actionable)", len(res.Unrecoverable))
		}
		if res.UnattributableAll != 1 {
			t.Errorf("UnattributableAll = %d, want 1", res.UnattributableAll)
		}
	})

	t.Run("unlocked field: filtered from the list, still counted", func(t *testing.T) {
		env := newLockDamageEnv(t)
		env.seedArtistWithLocks("a1", "Unlocked Artist", nil)
		env.seedDamageWithSource("biography", "curated bio", "junk bio",
			"manual")
		env.requireNotLocked("a1", "biography")

		res, err := env.svc.RepairLockDamage(context.Background(), LockDamageOpts{})
		if err != nil {
			t.Fatalf("RepairLockDamage: %v", err)
		}
		if len(res.Unrecoverable) != 0 {
			t.Fatalf("unrecoverable = %+v, want none (the field is not locked now)", res.Unrecoverable)
		}
		if res.UnattributableAll != 1 {
			t.Errorf("UnattributableAll = %d, want 1 -- the wider count keeps every "+
				"row; zero here means the count was taken from the filtered list", res.UnattributableAll)
		}
	})
}

// A CANCELED PASS HAS DECIDED NOTHING. If the startup context is canceled
// mid-pass, the remaining candidates must not be filed as per-row outcomes: a
// Failed entry retries forever against a cause that is not the row's, and an
// Unrecoverable entry is treated as FINAL, permanently reporting rows that
// were never examined. The pass must abort with the cancellation as its
// error, and the caller (which stamps completion only on a returned result)
// then retries the whole pass next boot.
//
// Two windows, two tests: this one cancels BEFORE the pass (the candidate
// query itself fails, which must surface as an error, never a result);
// TestRepairLockDamage_MidPassCancellationIsNotARowOutcome below wires the
// mid-pass window, where selection succeeded and cancellation lands during
// row processing.
func TestRepairLockDamage_CancellationAbortsThePass(t *testing.T) {
	env := newLockDamageEnv(t)
	env.seedArtistWithLocks("a1", "Locked Artist", []string{"biography"})
	env.seedBioDamage("a1", "metadata_quality")
	env.requireLockedBio()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := env.svc.RepairLockDamage(ctx, LockDamageOpts{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if res != nil {
		t.Fatalf("res = %+v, want nil -- a canceled pass must not return a completed-looking result", res)
	}

	// The database is untouched: no restore, no revert row.
	if got := env.biography("a1"); got != "junk bio" {
		t.Errorf("biography = %q, want the damaged value untouched", got)
	}
}

// cancelMidPassRepo delegates both list queries, then cancels the context the
// moment the loop's first per-row read arrives, wiring the exact mid-pass
// window: selection succeeded, cancellation lands during row processing.
type cancelMidPassRepo struct {
	artist.Repository
	cancel context.CancelFunc
}

func (r *cancelMidPassRepo) GetByID(ctx context.Context, id string) (*artist.Artist, error) {
	r.cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return r.Repository.GetByID(ctx, id)
}

// bgUnattributedHistoryRepo serves LockDamageUnattributed on a background
// context. WHY THE FIXTURE NEEDS THIS: without it, the canceled context also
// fails the unattributed query AFTER the loop, so the pass aborts there and
// the test cannot tell whether the LOOP filed the canceled read as a row
// outcome first -- the guard under test would be decorative and its deletion
// invisible (verified: with a plain repo, deleting the guard leaves this test
// green). Serving the tail query out of the canceled context isolates the
// loop as the only place the abort can come from.
type bgUnattributedHistoryRepo struct {
	artist.HistoryRepository
}

func (r bgUnattributedHistoryRepo) LockDamageUnattributed(context.Context) ([]artist.LockDamageUnattributedRow, error) {
	return r.HistoryRepository.LockDamageUnattributed(context.Background())
}

func TestRepairLockDamage_MidPassCancellationIsNotARowOutcome(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	artists, providers, members, aliases, images, platformIDs, completeness :=
		artist.NewDefaultRepos(db)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	raced := &cancelMidPassRepo{Repository: artists, cancel: cancel}
	artistSvc := artist.NewServiceWithRepos(raced, providers, members, aliases,
		images, platformIDs, completeness)
	hist := artist.NewHistoryService(db)
	artistSvc.SetHistoryService(hist)
	svc := NewService(db, dbPath, t.TempDir(), slog.Default())
	svc.SetLockDamageDeps(bgUnattributedHistoryRepo{hist.Repo()}, artistSvc)
	env := &lockDamageEnv{t: t, db: db, svc: svc, artistSvc: artistSvc}

	env.seedArtistWithLocks("a1", "Locked Artist", []string{"biography"})
	env.seedBioDamage("a1", "metadata_quality")
	env.requireLockedBio()

	res, err := svc.RepairLockDamage(ctx, LockDamageOpts{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled -- selection succeeded, "+
			"cancellation landed mid-pass and must abort, not be absorbed", err)
	}
	if res != nil {
		t.Fatalf("res = %+v, want nil -- the canceled candidate must not appear "+
			"as a Failed or Unrecoverable row", res)
	}
}

// cancelAfterReadRepo serves the loop's GetByID successfully and THEN cancels
// the context, so cancellation lands on the next call: the guarded write.
type cancelAfterReadRepo struct {
	artist.Repository
	cancel context.CancelFunc
}

func (r *cancelAfterReadRepo) GetByID(ctx context.Context, id string) (*artist.Artist, error) {
	a, err := r.Repository.GetByID(ctx, id)
	r.cancel()
	return a, err
}

// Guard 2 of 3: cancellation landing on the GUARDED WRITE (the artist read
// succeeded; the restore's transaction then fails on the canceled context)
// must abort the pass, never be filed as a transient Failed row that a
// completed-looking result carries. The tail query is served on a background
// context for the same reason as the mid-pass test above: without that, a
// deleted guard still aborts at the tail and this test passes vacuously.
//
// MUTATION PROOF: delete the ctx.Err() guard at the top of
// attemptLockDamageRestore's error path and this test FAILS with a Failed
// entry inside a non-nil result.
func TestRepairLockDamage_CancellationDuringWriteIsNotAFailedRow(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	artists, providers, members, aliases, images, platformIDs, completeness :=
		artist.NewDefaultRepos(db)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	decorated := &cancelAfterReadRepo{Repository: artists, cancel: cancel}
	artistSvc := artist.NewServiceWithRepos(decorated, providers, members, aliases,
		images, platformIDs, completeness)
	hist := artist.NewHistoryService(db)
	artistSvc.SetHistoryService(hist)
	svc := NewService(db, dbPath, t.TempDir(), slog.Default())
	svc.SetLockDamageDeps(bgUnattributedHistoryRepo{hist.Repo()}, artistSvc)
	env := &lockDamageEnv{t: t, db: db, svc: svc, artistSvc: artistSvc}

	env.seedArtistWithLocks("a1", "Locked Artist", []string{"biography"})
	env.seedBioDamage("a1", "metadata_quality")
	env.requireLockedBio()

	res, err := svc.RepairLockDamage(ctx, LockDamageOpts{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled -- the read succeeded, "+
			"cancellation landed on the write and must abort, not be absorbed", err)
	}
	// The ABSENCE of the row outcome is the invariant, not just the error: an
	// implementation that files the Failed entry AND returns the error would
	// pass an error-only assertion while still poisoning the result.
	if res != nil {
		t.Fatalf("res = %+v, want nil -- the canceled write must not be filed "+
			"as a transient Failed row", res)
	}
	if got := env.biography("a1"); got != "junk bio" {
		t.Errorf("biography = %q, want the damaged value untouched", got)
	}
}

// cancelAfterListHistoryRepo serves LockDamageUnattributed successfully and
// THEN cancels, so cancellation lands on the report helper's artist read.
type cancelAfterListHistoryRepo struct {
	artist.HistoryRepository
	cancel context.CancelFunc
}

func (r *cancelAfterListHistoryRepo) LockDamageUnattributed(ctx context.Context) ([]artist.LockDamageUnattributedRow, error) {
	rows, err := r.HistoryRepository.LockDamageUnattributed(ctx)
	r.cancel()
	return rows, err
}

// LockDamageChainDepths satisfies HistoryRepository. The chain depth is a
// PREVIEW-ONLY field, so a nil map (every candidate reporting depth 0) is a
// valid answer and keeps this fake focused on what it exists to exercise.
func (r *cancelAfterListHistoryRepo) LockDamageChainDepths(ctx context.Context) (map[artist.LockDamagePairKey]int, error) {
	return nil, nil
}

// Guard 3 of 3, the worst failure direction of the three: cancellation
// landing on the unattributed REPORT's artist read must abort the pass, never
// file the row as Unrecoverable -- that category is DECIDED and non-retriable,
// so a canceled read filed there permanently reports a row that was never
// examined.
//
// MUTATION PROOF: delete the ctx.Err() guard in
// reportUnattributedLockDamage's read-error path and this test FAILS with an
// Unrecoverable "lock state could not be read" entry inside a non-nil result.
func TestRepairLockDamage_CancellationDuringReportIsNotUnrecoverable(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	artistSvc := artist.NewService(db)
	hist := artist.NewHistoryService(db)
	artistSvc.SetHistoryService(hist)
	svc := NewService(db, dbPath, t.TempDir(), slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.SetLockDamageDeps(&cancelAfterListHistoryRepo{HistoryRepository: hist.Repo(), cancel: cancel}, artistSvc)
	env := &lockDamageEnv{t: t, db: db, svc: svc, artistSvc: artistSvc}

	// ONLY an unattributable manual row: the candidate loop sees nothing, so
	// the first read the cancellation can land on is the report helper's.
	env.seedArtistWithLocks("a1", "Locked Artist", []string{"biography"})
	env.seedDamageWithSource("biography", "curated bio", "junk bio", "manual")
	env.requireLockedBio()

	res, err := svc.RepairLockDamage(ctx, LockDamageOpts{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled -- the list succeeded, "+
			"cancellation landed on the report's read and must abort", err)
	}
	if res != nil {
		t.Fatalf("res = %+v, want nil -- a canceled read must never file an "+
			"Unrecoverable entry, the one category treated as FINAL", res)
	}
}

// STALENESS. The candidate list is read at the top of the pass and the repair
// runs in a goroutine at startup while the server is serving, so the live
// value can diverge from the candidate with NO new history row (a direct
// write, or simply time passing between the read and the write). The guarded
// conditional write (artist.Service.RestoreLockedFieldGuarded) compares the
// stored value to the candidate's NewValue inside its transaction and diverts
// on mismatch, so the restore can never overwrite data newer than the damage
// it attributed. This test seeds the divergence BEFORE the pass; the
// mid-window variant (a concurrent edit injected between the loop's artist
// read and the write, through a repository decorator) lands with the
// follow-up test unit, and the verb's own transactional guarantee is pinned
// directly in internal/artist/lock_restore_test.go. Mutation F (making the
// conditional write unconditional) kills this test either way.
func TestRepairLockDamage_LiveValueDivergedBlocksRestore(t *testing.T) {
	env := newLockDamageEnv(t)
	env.seedArtistWithLocks("a1", "Locked Artist", []string{"biography"})
	env.seedBioDamage("a1", "metadata_quality")
	// The live value changed WITHOUT a history row, so the damage row is still
	// rank 1 and the pair is still a candidate -- only the recheck can catch it.
	env.setBiography("operator hotfix")

	env.requireLockedBio()
	if got := env.biography("a1"); got != "operator hotfix" {
		t.Fatalf("fixture: biography = %q, want the diverged value", got)
	}

	res, err := env.svc.RepairLockDamage(context.Background(), LockDamageOpts{})
	if err != nil {
		t.Fatalf("RepairLockDamage: %v", err)
	}
	if len(res.Restored) != 0 {
		t.Fatalf("restored %d, want 0 -- the live value diverged after selection", len(res.Restored))
	}
	if got := env.biography("a1"); got != "operator hotfix" {
		t.Errorf("biography = %q, want the diverged value untouched", got)
	}
	if len(res.Unrecoverable) != 1 {
		t.Fatalf("unrecoverable = %d, want 1 (the diverged pair, counted)", len(res.Unrecoverable))
	}
}

// raceGetByIDRepo wires the actual race the guarded conditional write closes:
// a concurrent operator edit landing between the repair loop's artist read
// (its lock check) and the write. After serving the loop's GetByID it edits
// the field directly, so by the time RestoreLockedFieldGuarded runs, the
// stored value no longer equals the damaged value the candidate was selected
// for. DB() is forwarded so the guarded verb can open its transaction through
// the decorator.
type raceGetByIDRepo struct {
	artist.Repository
	db    *sql.DB
	calls int
	tb    testing.TB
	// mutate runs after the first GetByID returns, standing in for the
	// concurrent operator action (an edit, or an unlock).
	mutate func(ctx context.Context)
}

func (r *raceGetByIDRepo) DB() *sql.DB { return r.db }

func (r *raceGetByIDRepo) GetByID(ctx context.Context, id string) (*artist.Artist, error) {
	a, err := r.Repository.GetByID(ctx, id)
	r.calls++
	if r.calls == 1 && r.mutate != nil {
		r.mutate(ctx)
	}
	return a, err
}

func newRacedEnv(t *testing.T, mutate func(ctx context.Context, db *sql.DB)) (*lockDamageEnv, *raceGetByIDRepo) {
	t.Helper()
	db, dbPath := setupTestDBWithImages(t)
	artists, providers, members, aliases, images, platformIDs, completeness :=
		artist.NewDefaultRepos(db)
	raced := &raceGetByIDRepo{Repository: artists, db: db, tb: t}
	raced.mutate = func(ctx context.Context) { mutate(ctx, db) }
	artistSvc := artist.NewServiceWithRepos(raced, providers, members, aliases,
		images, platformIDs, completeness)
	hist := artist.NewHistoryService(db)
	artistSvc.SetHistoryService(hist)
	svc := NewService(db, dbPath, t.TempDir(), slog.Default())
	svc.SetLockDamageDeps(hist.Repo(), artistSvc)
	return &lockDamageEnv{t: t, db: db, svc: svc, artistSvc: artistSvc}, raced
}

// THE RACE WINDOW IS CLOSED, NOT NARROWED (fix round for #3075). An operator
// edit landing between the repair's lock-check read and its write must not be
// overwritten: the guarded verb decides value-still-damaged atomically in the
// repository layer, so the restore does not land, the operator's edit
// survives, and the pair is counted as a decided (unrecoverable) outcome that
// neither blocks completion nor retries.
//
// MUTATION PROOF (mutation F, fix round): make the conditional write
// unconditional (delete the stored-vs-damaged compare AND the repeated
// `AND <col> = ?` condition in RestoreLockedFieldGuarded's UPDATE) and this
// test FAILS: the operator's mid-pass edit is overwritten and Restored
// becomes 1.
func TestRepairLockDamage_ConcurrentEditBetweenReadAndWriteIsNotOverwritten(t *testing.T) {
	env, raced := newRacedEnv(t, func(ctx context.Context, db *sql.DB) {
		if _, err := db.ExecContext(ctx,
			`UPDATE artists SET biography = 'operator mid-pass edit' WHERE id = 'a1'`); err != nil {
			t.Fatalf("simulating the concurrent edit: %v", err)
		}
	})

	env.seedArtistWithLocks("a1", "Locked Artist", []string{"biography"})
	env.seedBioDamage("a1", "metadata_quality")

	env.requireLockedBio()
	if got := env.biography("a1"); got != "junk bio" {
		t.Fatalf("fixture: biography = %q, want the damaged value", got)
	}

	res, err := env.svc.RepairLockDamage(context.Background(), LockDamageOpts{})
	if err != nil {
		t.Fatalf("RepairLockDamage: %v", err)
	}
	// PRECONDITION on the race itself: the decorator must have injected the
	// edit, or the guarded compare was never contested and the assertions
	// below pass vacuously.
	if raced.calls < 1 {
		t.Fatalf("fixture: GetByID never called; the race was never injected")
	}
	if got := env.biography("a1"); got != "operator mid-pass edit" {
		t.Fatalf("biography = %q, want the operator's concurrent edit preserved", got)
	}
	if len(res.Restored) != 0 {
		t.Fatalf("restored %d, want 0 -- the restore must not land over a concurrent edit", len(res.Restored))
	}
	if len(res.Failed) != 0 {
		t.Fatalf("failed = %d, want 0 -- a diverged value is a decided outcome, not a retry", len(res.Failed))
	}
	if len(res.Unrecoverable) != 1 || !strings.Contains(res.Unrecoverable[0].Reason, "changed after the candidate") {
		t.Fatalf("unrecoverable = %+v, want exactly the diverged pair with its reason", res.Unrecoverable)
	}
}

// AN UNLOCK IN THE SAME WINDOW ALSO DIVERTS THE RESTORE. The loop's lock
// check reads the artist once; an operator unlocking the field after that
// read has withdrawn the intent the repair serves, and the guarded verb
// re-verifies the lock inside its transaction.
//
// MUTATION PROOF (fix round, companion to mutation F): delete the
// locked-fields recheck inside RestoreLockedFieldGuarded and this test FAILS:
// the restore lands into the now-unlocked field and Restored becomes 1.
func TestRepairLockDamage_UnlockBetweenReadAndWriteBlocksRestore(t *testing.T) {
	env, raced := newRacedEnv(t, func(ctx context.Context, db *sql.DB) {
		if _, err := db.ExecContext(ctx,
			`UPDATE artists SET locked_fields = '[]' WHERE id = 'a1'`); err != nil {
			t.Fatalf("simulating the concurrent unlock: %v", err)
		}
	})

	env.seedArtistWithLocks("a1", "Locked Artist", []string{"biography"})
	env.seedBioDamage("a1", "metadata_quality")

	env.requireLockedBio()

	res, err := env.svc.RepairLockDamage(context.Background(), LockDamageOpts{})
	if err != nil {
		t.Fatalf("RepairLockDamage: %v", err)
	}
	if raced.calls < 1 {
		t.Fatalf("fixture: GetByID never called; the unlock was never injected")
	}
	if got := env.biography("a1"); got != "junk bio" {
		t.Fatalf("biography = %q, want the damaged value untouched (field was unlocked)", got)
	}
	if len(res.Restored) != 0 {
		t.Fatalf("restored %d, want 0 -- the field was unlocked mid-window", len(res.Restored))
	}
	if len(res.Unrecoverable) != 1 || !strings.Contains(res.Unrecoverable[0].Reason, "unlocked after the candidate") {
		t.Fatalf("unrecoverable = %+v, want exactly the unlocked pair with its reason", res.Unrecoverable)
	}
}

// setBiography moves a1's live biography without writing a history row: the
// shape of a value that diverged from the damage a candidate was selected
// for. Fixed to a1 -- every fixture in these files builds its locked artist
// there, and a parameter no caller varies is a false suggestion that it does.
func (e *lockDamageEnv) setBiography(bio string) {
	e.t.Helper()
	if _, err := e.db.Exec(
		`UPDATE artists SET biography = ? WHERE id = 'a1'`, bio); err != nil {
		e.t.Fatalf("setting biography for a1: %v", err)
	}
}

// DRY RUN at the service level: candidates are reported as would-restore, but
// no write happens and no revert row is recorded, so a later REAL pass still
// sees the candidate.
func TestRepairLockDamage_DryRunWritesNothing(t *testing.T) {
	env := newLockDamageEnv(t)
	env.seedArtistWithLocks("a1", "Locked Artist", []string{"biography"})
	env.seedBioDamage("a1", "metadata_quality")
	env.requireLockedBio()

	res, err := env.svc.RepairLockDamage(context.Background(), LockDamageOpts{DryRun: true})
	if err != nil {
		t.Fatalf("RepairLockDamage(DryRun): %v", err)
	}
	if len(res.Restored) != 1 {
		t.Fatalf("dry run reported %d restorable, want 1", len(res.Restored))
	}
	if got := env.biography("a1"); got != "junk bio" {
		t.Errorf("biography = %q, want the damaged value untouched", got)
	}
	var reverts int
	if err := env.db.QueryRow(
		`SELECT COUNT(*) FROM metadata_changes WHERE artist_id = 'a1' AND source = 'revert'`).Scan(&reverts); err != nil {
		t.Fatalf("counting revert rows: %v", err)
	}
	if reverts != 0 {
		t.Errorf("dry run recorded %d revert rows, want 0", reverts)
	}
}

// RepairLockDamage must refuse to run before SetLockDamageDeps: a nil-dep
// pass would panic in a startup goroutine, and the panic handler there
// deliberately reports only the type.
func TestRepairLockDamage_ErrorsWithoutDeps(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	svc := NewService(db, dbPath, t.TempDir(), slog.Default())

	// The SPECIFIC sentinel, not any error: a bare err != nil stays green if
	// the pass later fails for an unrelated reason, which is a vacuous
	// assertion on the condition this test exists to pin.
	if _, err := svc.RepairLockDamage(context.Background(), LockDamageOpts{}); !errors.Is(err, ErrLockDamageDepsNotSet) {
		t.Fatalf("err = %v, want ErrLockDamageDepsNotSet", err)
	}
}

// DETERMINISTIC REFUSALS DO NOT BLOCK COMPLETION FOREVER (fix round for
// #3075). A restore the write layer refuses for a reason that recurs
// identically every boot -- the old value failing today's validation, or a
// name restore that would recreate a collision a later rename removed --
// lands in FailedPermanent, not Failed, so the pass can complete instead of
// re-running the full repair on every start with no way to retire it. Typed
// error checks (errors.As/Is on the real types), never string matching.
func TestRepairLockDamage_DeterministicRefusalIsPermanentNotRetried(t *testing.T) {
	t.Run("validation refusal: the old name normalizes to no identity", func(t *testing.T) {
		env := newLockDamageEnv(t)
		// The stored name is the damaged value; the operator's original was
		// "-", which today's validator refuses (it normalizes to an empty
		// identity key). name_language_pref declares "name".
		env.seedArtistWithLocks("a1", "Damaged Name", []string{"name"})
		env.seedDamageWithSource("name", "-", "Damaged Name",
			"rule:name_language_pref")

		if !env.lockedFields("a1")["name"] {
			t.Fatal("fixture: name is not locked on a1")
		}

		res, err := env.svc.RepairLockDamage(context.Background(), LockDamageOpts{})
		if err != nil {
			t.Fatalf("RepairLockDamage: %v", err)
		}
		if len(res.Restored) != 0 {
			t.Fatalf("restored %d, want 0", len(res.Restored))
		}
		if len(res.Failed) != 0 {
			t.Fatalf("failed = %+v, want 0 transient failures -- this refusal recurs identically", res.Failed)
		}
		if len(res.FailedPermanent) != 1 {
			t.Fatalf("failed permanent = %d, want 1", len(res.FailedPermanent))
		}
	})

	t.Run("name collision: the old name now belongs to another artist", func(t *testing.T) {
		env := newLockDamageEnv(t)
		env.seedArtistWithLocks("a1", "Damaged Name", []string{"name"})
		// After the damage, the operator gave ANOTHER artist the original
		// name; restoring it would recreate the duplicate the guard exists to
		// prevent.
		env.seedArtistWithLocks("a2", "Original Name", nil)
		env.seedDamageWithSource("name", "Original Name", "Damaged Name",
			"rule:name_language_pref")

		if !env.lockedFields("a1")["name"] {
			t.Fatal("fixture: name is not locked on a1")
		}

		res, err := env.svc.RepairLockDamage(context.Background(), LockDamageOpts{})
		if err != nil {
			t.Fatalf("RepairLockDamage: %v", err)
		}
		if len(res.Restored) != 0 {
			t.Fatalf("restored %d, want 0", len(res.Restored))
		}
		if len(res.Failed) != 0 {
			t.Fatalf("failed = %+v, want 0 transient failures", res.Failed)
		}
		if len(res.FailedPermanent) != 1 {
			t.Fatalf("failed permanent = %d, want 1 (the collision refusal)", len(res.FailedPermanent))
		}
		// The refused restore wrote nothing.
		var name string
		if err := env.db.QueryRow(`SELECT name FROM artists WHERE id = 'a1'`).Scan(&name); err != nil {
			t.Fatalf("reading name: %v", err)
		}
		if name != "Damaged Name" {
			t.Errorf("a1 name = %q, want the stored value untouched by the refused restore", name)
		}
	})
}

// TRANSIENT ROW FAILURES DO NOT ABORT THE PASS (recovered for #3088).
// LockDamageResult.Failed is the TRANSIENT bucket -- a row the pass could not
// read or could not restore, counted so the pass continues and the unstamped
// completion key retries it next boot. Every other test in THIS file asserts
// it EMPTY.
//
// WHERE THE OTHER COVERAGE IS, so nobody deletes it believing this file is the
// only place: the READ-failure branch is also covered end-to-end at the entry
// point, by the errGetByIDRepo fixture in
// cmd/stillwater/lock_damage_repair_test.go, which exercises the completion
// gate and the dry-run report printer. The WRITE-failure branch had no test
// anywhere before these. These two add maintenance-level assertions the entry
// point does not make on a NON-dry-run pass: that the pass returns a nil
// error, and that the stored value is left untouched.
//
// failingArtistRepo drives both branches. failGet makes GetByID fail outright.
// Withholding the DB() accessor instead makes the guarded restore fail before
// it can open its transaction (Service.artistDB type-asserts for it), which is
// the same non-typed-error path a genuine write failure takes -- so the write
// test pins the pass's handling of a failed restore, NOT a failing UPDATE.
// No SQL write is attempted; that branch remains untested.
//
// Note the withheld accessor is not a single-variable change: Service's
// library hydration type-asserts the same interface and silently no-ops
// without it. Harmless here (neither test touches LibraryID), but a fault
// injected this way has that blast radius.
type failingArtistRepo struct {
	artist.Repository
	db      *sql.DB
	failGet bool
	// exposeDB controls whether the decorator forwards the raw handle
	// RestoreLockedFieldGuarded opens its transaction through. false makes
	// every guarded restore fail before it opens that transaction, while
	// reads still work.
	exposeDB bool
}

var errForcedRepoFailure = errors.New("forced repository failure")

func (f *failingArtistRepo) GetByID(ctx context.Context, id string) (*artist.Artist, error) {
	if f.failGet {
		return nil, errForcedRepoFailure
	}
	return f.Repository.GetByID(ctx, id)
}

// failingArtistRepoWithDB adds the DB accessor. A separate wrapper type
// rather than a conditional method: Go interface satisfaction is static, so
// "has DB()" must be a property of the type.
type failingArtistRepoWithDB struct{ *failingArtistRepo }

func (f failingArtistRepoWithDB) DB() *sql.DB { return f.db }

func newFailingEnv(t *testing.T, failGet, failWrite bool) *lockDamageEnv {
	t.Helper()
	db, dbPath := setupTestDBWithImages(t)
	artists, providers, members, aliases, images, platformIDs, completeness :=
		artist.NewDefaultRepos(db)
	failing := &failingArtistRepo{Repository: artists, db: db, failGet: failGet, exposeDB: !failWrite}
	var repo artist.Repository = failing
	if failing.exposeDB {
		repo = failingArtistRepoWithDB{failing}
	}
	artistSvc := artist.NewServiceWithRepos(repo, providers, members, aliases,
		images, platformIDs, completeness)
	hist := artist.NewHistoryService(db)
	artistSvc.SetHistoryService(hist)
	svc := NewService(db, dbPath, t.TempDir(), slog.Default())
	svc.SetLockDamageDeps(hist.Repo(), artistSvc)
	return &lockDamageEnv{t: t, db: db, svc: svc, artistSvc: artistSvc}
}

func TestRepairLockDamage_ArtistReadFailureIsCountedNotFatal(t *testing.T) {
	env := newFailingEnv(t, true, false)
	env.seedArtistWithLocks("a1", "Locked Artist", []string{"biography"})
	env.seedBioDamage("a1", "metadata_quality")
	env.requireLockedBio()

	res, err := env.svc.RepairLockDamage(context.Background(), LockDamageOpts{})
	if err != nil {
		t.Fatalf("RepairLockDamage returned an error; a row-level failure must not abort the pass: %v", err)
	}
	if len(res.Restored) != 0 {
		t.Fatalf("restored %d, want 0", len(res.Restored))
	}
	if len(res.Failed) != 1 || res.Failed[0].Reason != "could not read the artist" {
		t.Fatalf("failed = %+v, want one entry with the exact reason %q", res.Failed, "could not read the artist")
	}
	// Pin WHICH row failed, not just that one did. A skip built from the wrong
	// loop variable names another artist as needing a retry while the damaged
	// row reads clean in the operator's pane.
	if got := res.Failed[0]; got.ArtistID != "a1" || got.Field != "biography" || got.RuleID != "metadata_quality" {
		t.Errorf("failed[0] identity = %+v, want a1/biography/metadata_quality", got)
	}
	// A transient failure is retried next boot; it must not ALSO be filed in a
	// FINAL bucket. Filing both would report the row as permanently decided in
	// the operator's pane while the pass keeps retrying it forever.
	if len(res.Unrecoverable) != 0 || len(res.FailedPermanent) != 0 {
		t.Errorf("unrecoverable = %+v, failedPermanent = %+v, want both empty -- a transient read failure is retried, never also decided",
			res.Unrecoverable, res.FailedPermanent)
	}
	if got := env.biography("a1"); got != "junk bio" {
		t.Errorf("biography = %q, want it untouched", got)
	}
}

func TestRepairLockDamage_WriteFailureIsCountedNotFatal(t *testing.T) {
	env := newFailingEnv(t, false, true)
	env.seedArtistWithLocks("a1", "Locked Artist", []string{"biography"})
	env.seedBioDamage("a1", "metadata_quality")
	env.requireLockedBio()

	res, err := env.svc.RepairLockDamage(context.Background(), LockDamageOpts{})
	if err != nil {
		t.Fatalf("RepairLockDamage returned an error; a row-level failure must not abort the pass: %v", err)
	}
	if len(res.Restored) != 0 {
		t.Fatalf("restored %d, want 0", len(res.Restored))
	}
	if len(res.Failed) != 1 || res.Failed[0].Reason != "the restore write failed" {
		t.Fatalf("failed = %+v, want one entry with the exact reason %q", res.Failed, "the restore write failed")
	}
	if got := res.Failed[0]; got.ArtistID != "a1" || got.Field != "biography" || got.RuleID != "metadata_quality" {
		t.Errorf("failed[0] identity = %+v, want a1/biography/metadata_quality", got)
	}
	// FailedPermanent is the bucket that PERMITS stamping the completion key.
	// Cross-filing a transient failure into it retires a row the pass never
	// actually repaired, so the retry it is owed never happens.
	if len(res.FailedPermanent) != 0 || len(res.Unrecoverable) != 0 {
		t.Errorf("failedPermanent = %+v, unrecoverable = %+v, want both empty -- a transient write failure is retried, never also decided",
			res.FailedPermanent, res.Unrecoverable)
	}
	if got := env.biography("a1"); got != "junk bio" {
		t.Errorf("biography = %q, want the damaged value still stored", got)
	}
}

// TestRepairLockDamage_TransactionalWriteFailureIsCountedNotFatal covers a
// GENUINE write failure inside RestoreLockedFieldGuarded's transaction --
// the gap TestRepairLockDamage_WriteFailureIsCountedNotFatal leaves open (see
// the comment above newFailingEnv): that test withholds the DB() accessor,
// so Service.artistDB fails its type assertion and RestoreLockedFieldGuarded
// never reaches db.BeginTx -- no transaction opens and no SQL runs.
//
// This test uses the REAL artist.Service (the same wiring newLockDamageEnv
// uses, with a genuine DB() accessor), and forces the failure with a SQL
// trigger that fires only on `UPDATE OF biography` on the artists table.
// RestoreLockedFieldGuarded's SELECT (which re-verifies the lock and the
// stored value inside the same transaction) is untouched by the trigger, so
// by the time the trigger can fire, BeginTx has succeeded and that SELECT
// has already read back a still-locked, still-matching row -- the write
// itself is what fails, not a step before it (#3089 CR finding B).
func TestRepairLockDamage_TransactionalWriteFailureIsCountedNotFatal(t *testing.T) {
	env := newLockDamageEnv(t)
	env.seedArtistWithLocks("a1", "Locked Artist", []string{"biography"})
	env.seedBioDamage("a1", "metadata_quality")
	env.requireLockedBio()

	if _, err := env.db.Exec(
		`CREATE TRIGGER force_biography_write_failure
		 BEFORE UPDATE OF biography ON artists
		 FOR EACH ROW BEGIN SELECT RAISE(ABORT, 'forced write failure for test'); END`); err != nil {
		t.Fatalf("installing the write-failure trigger: %v", err)
	}

	// Prove the fixture's defining property BEFORE trusting what the repair
	// pass reports: the trigger genuinely blocks an UPDATE of biography (a
	// non-vacuity check -- a trigger that never fires would leave this test
	// passing for the wrong reason), and the probe UPDATE it blocks must not
	// have landed.
	if _, err := env.db.Exec(`UPDATE artists SET biography = ? WHERE id = ?`, "probe value", "a1"); err == nil {
		t.Fatal("fixture: a direct UPDATE of biography succeeded; the trigger is not wired")
	} else if !strings.Contains(err.Error(), "forced write failure") {
		t.Fatalf("fixture: UPDATE failed for the wrong reason: %v", err)
	}
	if got := env.biography("a1"); got != "junk bio" {
		t.Fatalf("fixture: the blocked probe UPDATE mutated biography to %q anyway", got)
	}

	res, err := env.svc.RepairLockDamage(context.Background(), LockDamageOpts{})
	if err != nil {
		t.Fatalf("RepairLockDamage returned an error; a row-level failure must not abort the pass: %v", err)
	}
	if len(res.Restored) != 0 {
		t.Fatalf("restored %d, want 0", len(res.Restored))
	}
	if len(res.Failed) != 1 || res.Failed[0].Reason != "the restore write failed" {
		t.Fatalf("failed = %+v, want one entry with the exact reason %q", res.Failed, "the restore write failed")
	}
	if got := res.Failed[0]; got.ArtistID != "a1" || got.Field != "biography" || got.RuleID != "metadata_quality" {
		t.Errorf("failed[0] identity = %+v, want a1/biography/metadata_quality", got)
	}
	// FailedPermanent is the bucket that PERMITS stamping the completion key.
	// Cross-filing a transient failure into it retires a row the pass never
	// actually repaired, so the retry it is owed never happens.
	if len(res.FailedPermanent) != 0 || len(res.Unrecoverable) != 0 {
		t.Errorf("failedPermanent = %+v, unrecoverable = %+v, want both empty -- a transient write failure is retried, never also decided",
			res.FailedPermanent, res.Unrecoverable)
	}
	if got := env.biography("a1"); got != "junk bio" {
		t.Errorf("biography = %q, want the damaged value still stored", got)
	}
}

// lockField adds one field to an artist's locked_fields, as the operator's UI
// toggle does. It exists for the digest-gate test, where a lock added BETWEEN
// the preview and the write is the exact drift the gate must refuse -- and
// where doing it any other way (re-seeding the artist) would also reset the
// damage the test depends on.
// ADDITIVE, matching what the UI toggle does and what this helper's name
// says (CodeRabbit, PR #3136). It previously REPLACED the whole column with
// `["<field>"]`, silently unlocking every other field. Harmless with the one
// caller that existed, but lock state is the exact precondition these tests
// assert, so a future two-lock caller would have lost a lock and the test
// would still have looked right.
//
// Sorted before marshaling so the stored JSON is deterministic: a fixture
// whose column contents depend on insertion order makes any later assertion
// on that column flaky for a reason unrelated to what it tests.
func (e *lockDamageEnv) lockField(artistID, field string) {
	e.t.Helper()

	var raw string
	if err := e.db.QueryRow(
		`SELECT COALESCE(locked_fields, '') FROM artists WHERE id = ?`,
		artistID).Scan(&raw); err != nil {
		e.t.Fatalf("reading locked_fields for %s: %v", artistID, err)
	}
	locked := artist.UnmarshalStringSlice(raw)
	if slices.Contains(locked, field) {
		return
	}
	locked = append(locked, field)
	slices.Sort(locked)

	if _, err := e.db.Exec(
		`UPDATE artists SET locked_fields = ? WHERE id = ?`,
		artist.MarshalStringSlice(locked), artistID); err != nil {
		e.t.Fatalf("locking %s on %s: %v", field, artistID, err)
	}
	// EVERY previously-locked field is still locked, not merely the new one.
	// Asserting only the new field would pass against the replacing version
	// this fix replaced.
	after := e.lockedFields(artistID)
	for _, f := range locked {
		if !after[f] {
			e.t.Fatalf("fixture: %s is not locked on %s after locking %s; the helper "+
				"dropped an existing lock", f, artistID, field)
		}
	}
}
