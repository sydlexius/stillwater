package maintenance

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
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
	env.setBiography("a1", "operator hotfix")

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

func (e *lockDamageEnv) setBiography(artistID, bio string) {
	e.t.Helper()
	if _, err := e.db.Exec(
		`UPDATE artists SET biography = ? WHERE id = ?`, bio, artistID); err != nil {
		e.t.Fatalf("setting biography for %s: %v", artistID, err)
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

// failingArtistRepo makes GetByID fail on demand, or -- by withholding the
// DB() accessor the guarded restore verb requires -- makes the write layer
// fail, driving the two TRANSIENT Failed branches: "could not read the
// artist" and "the restore write failed". A failed row must not abort the
// pass.
type failingArtistRepo struct {
	artist.Repository
	db      *sql.DB
	failGet bool
	// exposeDB controls whether the decorator forwards the raw handle
	// RestoreLockedFieldGuarded opens its transaction through. false makes
	// every guarded restore fail at the write layer while reads still work.
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
	if len(res.Failed) != 1 || !strings.Contains(res.Failed[0].Reason, "could not read") {
		t.Fatalf("failed = %+v, want one 'could not read the artist' entry", res.Failed)
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
	if len(res.Failed) != 1 || !strings.Contains(res.Failed[0].Reason, "write failed") {
		t.Fatalf("failed = %+v, want one 'restore write failed' entry", res.Failed)
	}
	if got := env.biography("a1"); got != "junk bio" {
		t.Errorf("biography = %q, want the damaged value still stored", got)
	}
}
