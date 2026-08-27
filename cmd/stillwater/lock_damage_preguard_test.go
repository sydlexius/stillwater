package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/cli"
	"github.com/sydlexius/stillwater/internal/database"
	"github.com/sydlexius/stillwater/internal/maintenance"
)

// lock_damage_preguard_test.go -- the startup/CLI surface of the pre-guard
// repair (#3079): its own completion key, its own flags, and the dry run's
// report of what the time bound withheld. The completion gate itself
// (stamp only on a pass with no row-level failures) is pinned by
// TestRunLockDamageRepairPass_CompletionGate: both modes share that function.

// preGuardBefore and preGuardAfter bracket the cutoff, derived from the
// exported bound rather than written as literals so a moved constant cannot
// silently leave these fixtures on the wrong side of it.
func preGuardBefore() string {
	return maintenance.PreGuardCutoff().Add(-48 * time.Hour).UTC().Format(time.RFC3339)
}

func preGuardAfter() string {
	return maintenance.PreGuardCutoff().Add(48 * time.Hour).UTC().Format(time.RFC3339)
}

// seedPreGuardFixture seeds one locked artist with UNATTRIBUTABLE (manual)
// biography damage older than the cutoff -- a pre-guard candidate -- and a
// second locked artist whose identical damage is NEWER than the cutoff, which
// the bound must withhold.
func seedPreGuardFixture(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	for _, a := range []struct{ id, name string }{
		{"a1", "Old Damage"}, {"a2", "New Damage"},
	} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO artists (id, name, sort_name, path, biography, locked_fields)
			 VALUES (?, ?, ?, '', 'junk bio', '["biography"]')`,
			a.id, a.name, a.name); err != nil {
			t.Fatalf("seeding artist %s: %v", a.id, err)
		}
	}
	for _, c := range []struct{ id, artist, at string }{
		{"d1", "a1", preGuardBefore()},
		{"d2", "a2", preGuardAfter()},
	} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO metadata_changes (id, artist_id, field, old_value, new_value, source, created_at)
			 VALUES (?, ?, 'biography', 'curated bio', 'junk bio', 'manual', ?)`,
			c.id, c.artist, c.at); err != nil {
			t.Fatalf("seeding damage %s: %v", c.id, err)
		}
	}
}

// requirePreGuardFixture asserts the fixture's defining properties before any
// assertion trusts what the run reports: both fields locked, both rows
// manual-sourced, and the two rows on OPPOSITE sides of the bound.
func requirePreGuardFixture(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	for _, id := range []string{"a1", "a2"} {
		var lf string
		if err := db.QueryRowContext(ctx,
			`SELECT locked_fields FROM artists WHERE id = ?`, id).Scan(&lf); err != nil {
			t.Fatalf("fixture: reading locked_fields for %s: %v", id, err)
		}
		if !strings.Contains(lf, "biography") {
			t.Fatalf("fixture: biography not locked on %s (locked_fields = %s)", id, lf)
		}
	}
	cutoff := maintenance.PreGuardCutoff()
	for _, c := range []struct {
		id         string
		wantBefore bool
	}{{"d1", true}, {"d2", false}} {
		var src, at string
		if err := db.QueryRowContext(ctx,
			`SELECT source, created_at FROM metadata_changes WHERE id = ?`, c.id).Scan(&src, &at); err != nil {
			t.Fatalf("fixture: reading change %s: %v", c.id, err)
		}
		if strings.HasPrefix(src, "rule:") {
			t.Fatalf("fixture: change %s has source %q; the pre-guard population names no rule", c.id, src)
		}
		ts, err := time.Parse(time.RFC3339, at)
		if err != nil {
			t.Fatalf("fixture: unparsable created_at %q on %s: %v", at, c.id, err)
		}
		if got := ts.Before(cutoff); got != c.wantBefore {
			t.Fatalf("fixture: change %s before the cutoff = %v, want %v", c.id, got, c.wantBefore)
		}
	}
}

// THE PRE-GUARD DRY RUN WRITES NOTHING AND SAYS WHAT THE BOUND WITHHELD.
//
// Asserted by ROW-LEVEL DIFF over artists, metadata_changes and settings --
// never by a file hash. A WAL database's main file is unchanged by a write,
// and opening the DB can run a pending migration that moves the hash on its
// own, so a hash comparison is green for two unrelated reasons.
func TestLockDamagePreGuardDryRun_ReportsWithoutWriting(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	seedPreGuardFixture(t, ctx, db)
	requirePreGuardFixture(t, ctx, db)

	artistsBefore := tableSnapshot(t, ctx, db, "artists")
	changesBefore := tableSnapshot(t, ctx, db, "metadata_changes")
	settingsBefore := tableSnapshot(t, ctx, db, "settings")

	var out bytes.Buffer
	if err := lockDamageDryRunDB(ctx, db, &out,
		maintenance.LockDamageOpts{DryRun: true, PreGuard: true}); err != nil {
		t.Fatalf("lockDamageDryRunDB(PreGuard): %v", err)
	}

	report := out.String()
	if !strings.Contains(report, "would restore: 1") {
		t.Errorf("report does not count the single eligible candidate:\n%s", report)
	}
	if !strings.Contains(report, "artist=a1") {
		t.Errorf("report does not identify the candidate:\n%s", report)
	}
	if strings.Contains(report, "artist=a2") {
		t.Errorf("report names a candidate the time bound excluded:\n%s", report)
	}
	// The bound's effect is PRINTED. A preview that silently withheld a row
	// reads exactly like a library with nothing to withhold.
	//
	// THE WHOLE LINE, NOT TWO INDEPENDENT SUBSTRINGS (CodeRabbit, PR #3136).
	// This used to test `Contains("excluded, newer than the cutoff")` and
	// `Contains(": 1")` separately, and BOTH matched for the wrong reason: the
	// label prints unconditionally whatever the count is, and `": 1"` matched
	// the `would restore: 1` line elsewhere in the report. So the assertion
	// held even when the count was 0 -- it enforced nothing about the property
	// it names, in the test guarding the one bound the issue's AC singles out.
	//
	// Built from the SAME format string the printer uses, so the count is
	// anchored to its own line and the cutoff it names is checked in the same
	// comparison (which is why the separate timestamp assertion that used to
	// follow is gone -- this subsumes it).
	//
	// MUTATION PROOF: drop the `res.PreGuardTooNew++` in
	// selectPreGuardCandidates, leaving the row excluded but the count silently
	// 0. The old assertion PASSED; this one fails with `: 0`. Verified both
	// ways.
	wantExcluded := fmt.Sprintf("excluded, newer than the cutoff (%s): 1",
		maintenance.PreGuardCutoff().Format(time.RFC3339))
	if !strings.Contains(report, wantExcluded) {
		t.Errorf("report does not state what the time bound withheld; want %q in:\n%s",
			wantExcluded, report)
	}
	// direction lets the maintainer group the cut without being shown values.
	if !strings.Contains(report, "direction=") {
		t.Errorf("report omits the direction descriptor:\n%s", report)
	}
	for _, secret := range []string{"curated bio", "junk bio", "Old Damage", "New Damage"} {
		if strings.Contains(report, secret) {
			t.Errorf("report leaks private content (%q):\n%s", secret, report)
		}
	}

	if got := tableSnapshot(t, ctx, db, "artists"); got != artistsBefore {
		t.Errorf("artists table changed during a dry run:\nbefore:\n%s\nafter:\n%s", artistsBefore, got)
	}
	if got := tableSnapshot(t, ctx, db, "metadata_changes"); got != changesBefore {
		t.Errorf("metadata_changes changed during a dry run:\nbefore:\n%s\nafter:\n%s", changesBefore, got)
	}
	if got := tableSnapshot(t, ctx, db, "settings"); got != settingsBefore {
		t.Errorf("settings changed during a dry run:\nbefore:\n%s\nafter:\n%s", settingsBefore, got)
	}
}

// THE TWO ONE-SHOTS HAVE SEPARATE KEYS, AND THAT IS LOAD-BEARING. Every
// deployment of v1.6.2 or later has already completed the ATTRIBUTED pass, so
// a shared key would retire the pre-guard pass on exactly the databases it
// exists to repair.
func TestPreGuardCompletionKey_IsDistinctFromTheAttributedOne(t *testing.T) {
	if lockDamagePreGuardKey == lockDamageRepairKey {
		t.Fatal("the pre-guard pass shares the attributed pass's completion key; " +
			"every already-repaired database would skip it")
	}

	ctx := context.Background()
	db := openTestDB(t)
	seedPreGuardFixture(t, ctx, db)
	requirePreGuardFixture(t, ctx, db)
	// The attributed pass has already completed here, as it has on every real
	// deployment carrying #3075.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO settings (key, value) VALUES (?, 'already-done')`,
		lockDamageRepairKey); err != nil {
		t.Fatalf("seeding the attributed completion key: %v", err)
	}

	maint := newPreGuardMaint(t, db)
	// The write pass carries the digest from its own preview, because a
	// digestless pre-guard write is refused inside internal/maintenance now.
	// Taken from a real preview rather than hardcoded: a literal would keep
	// passing even if the two sides computed the digest differently.
	preview, err := maint.RepairLockDamage(ctx,
		maintenance.LockDamageOpts{PreGuard: true, DryRun: true})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if len(preview.Restored) != 1 {
		t.Fatalf("fixture: the preview offers %d row(s), want 1", len(preview.Restored))
	}
	if err := runLockDamageRepairPass(ctx, db,
		slog.New(slog.NewTextHandler(io.Discard, nil)), maint,
		maintenance.LockDamageOpts{PreGuard: true,
			ApprovedDigest: maintenance.LockDamageDigest(preview.Restored)},
		lockDamagePreGuardKey); err != nil {
		t.Fatalf("runLockDamageRepairPass: %v", err)
	}

	var bio string
	if err := db.QueryRowContext(ctx,
		`SELECT biography FROM artists WHERE id = 'a1'`).Scan(&bio); err != nil {
		t.Fatalf("reading biography: %v", err)
	}
	if bio != "curated bio" {
		t.Errorf("biography = %q, want the restored value: the attributed key must not gate this pass", bio)
	}
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM settings WHERE key = ?`, lockDamagePreGuardKey).Scan(&n); err != nil {
		t.Fatalf("reading the pre-guard completion key: %v", err)
	}
	if n != 1 {
		t.Errorf("pre-guard completion key rows = %d, want 1", n)
	}
}

// newPreGuardMaint builds the same real service the entry point builds. A
// fake would prove nothing about the SQL the pass runs.
func newPreGuardMaint(t *testing.T, db *sql.DB) *maintenance.Service {
	t.Helper()
	artistSvc := artist.NewService(db)
	hist := artist.NewHistoryService(db)
	artistSvc.SetHistoryService(hist)
	maint := maintenance.NewService(db, "", "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	maint.SetLockDamageDeps(hist.Repo(), artistSvc)
	return maint
}

// BOTH NEW FLAGS ARE HANDLED AND NEITHER FALLS THROUGH. Falling through would
// boot a live server against the operator's database copy -- and for the
// repair flag, run a real pass against whatever the server path opens, the
// one thing the preview-then-approve shape exists to prevent.
func TestDispatchFlagCommand_PreGuardFlagsNeverFallThrough(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SW_DB_PATH", dir+"/dispatch.db")
	t.Setenv("SW_CONFIG_PATH", dir+"/no-such-config.toml")
	t.Setenv("SW_MUSIC_PATH", dir)

	cases := []struct {
		name  string
		flags cli.Flags
	}{
		{"pre-guard dry run", cli.Flags{LockDamagePreGuardDryRun: true}},
		{"pre-guard repair", cli.Flags{LockDamagePreGuardRepair: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			handled, err := dispatchFlagCommand(tc.flags, &stderr)
			if !handled {
				t.Fatalf("handled = false for %s; main would fall through and boot the real server", tc.name)
			}
			// The dry run refuses an unmigrated database; the repair path
			// migrates its own. Either way an error must not be swallowed
			// into a false success -- assert only that handled held.
			_ = err
		})
	}
}

// THE WRITE ENTRY POINT END TO END: restores the eligible pair, leaves the
// too-new one alone, stamps its own key, no-ops on a second invocation.
func TestRunLockDamagePreGuardRepair_EndToEnd(t *testing.T) {
	dir := t.TempDir()
	dbPath := dir + "/preguard.db"
	t.Setenv("SW_DB_PATH", dbPath)
	t.Setenv("SW_CONFIG_PATH", dir+"/no-such-config.toml")
	t.Setenv("SW_MUSIC_PATH", dir)

	ctx := context.Background()
	mig, err := database.Open(dbPath)
	if err != nil {
		t.Fatalf("opening db for migration: %v", err)
	}
	if err := database.Migrate(mig); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	seedPreGuardFixture(t, ctx, mig)
	requirePreGuardFixture(t, ctx, mig)
	_ = mig.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// THE OPERATOR'S REAL WORKFLOW: preview, read the digest off the report,
	// pass it to the repair. Taking the digest from the preview rather than
	// hardcoding one is the point -- a hardcoded token would pass even if the
	// two sides computed it differently.
	digest := previewDigest(t, dbPath)
	if err := runLockDamagePreGuardRepair(logger, digest); err != nil {
		t.Fatalf("runLockDamagePreGuardRepair: %v", err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}
	defer func() { _ = db.Close() }()

	var bio1, bio2 string
	if err := db.QueryRowContext(ctx, `SELECT biography FROM artists WHERE id = 'a1'`).Scan(&bio1); err != nil {
		t.Fatalf("reading a1: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT biography FROM artists WHERE id = 'a2'`).Scan(&bio2); err != nil {
		t.Fatalf("reading a2: %v", err)
	}
	if bio1 != "curated bio" {
		t.Errorf("a1 biography = %q, want the restored value", bio1)
	}
	if bio2 != "junk bio" {
		t.Errorf("a2 biography = %q, want it untouched: its damage is newer than the cutoff", bio2)
	}
	var stamped int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM settings WHERE key = ?`, lockDamagePreGuardKey).Scan(&stamped); err != nil {
		t.Fatalf("reading completion key: %v", err)
	}
	if stamped != 1 {
		t.Fatalf("completion key rows = %d, want 1", stamped)
	}

	// A SECOND INVOCATION IS A NO-OP. Damage the field again, older than the
	// cutoff, so the QUERY would select it -- only the key stops the pass.
	if _, err := db.ExecContext(ctx,
		`UPDATE artists SET biography = 'junk bio' WHERE id = 'a1'`); err != nil {
		t.Fatalf("re-damaging a1: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO metadata_changes (id, artist_id, field, old_value, new_value, source, created_at)
		 VALUES ('d3', 'a1', 'biography', 'curated bio', 'junk bio', 'manual', ?)`,
		preGuardBefore()); err != nil {
		t.Fatalf("seeding second damage: %v", err)
	}
	if err := runLockDamagePreGuardRepair(logger, digest); err != nil {
		t.Fatalf("second runLockDamagePreGuardRepair: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT biography FROM artists WHERE id = 'a1'`).Scan(&bio1); err != nil {
		t.Fatalf("re-reading a1: %v", err)
	}
	if bio1 != "junk bio" {
		t.Errorf("a1 biography = %q after a second invocation; the one-shot must be retired by its key", bio1)
	}
}

// THE PANIC HANDLER LEAKS NOTHING. A panic from the restore path can carry a
// field value in its message; only the TYPE may be logged, and the returned
// error must not carry the message either. Driven with panicValueError, whose
// Error() is a stand-in for library content.
func TestGuardPreGuardPanic_LogsTypeOnly(t *testing.T) {
	var logged bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logged, nil))

	err := guardPreGuardPanic(logger, func() error {
		panic(&panicValueError{msg: "PRIVATE_FIELD_VALUE_IN_PANIC"})
	})
	if err == nil {
		t.Fatal("a panicking pass returned nil; the caller would report success")
	}
	if strings.Contains(err.Error(), "PRIVATE_FIELD_VALUE_IN_PANIC") {
		t.Errorf("the returned error carries the panic value: %v", err)
	}
	out := logged.String()
	if strings.Contains(out, "PRIVATE_FIELD_VALUE_IN_PANIC") {
		t.Errorf("the log carries the panic value:\n%s", out)
	}
	if !strings.Contains(out, "panic_type") {
		t.Errorf("the log does not record the panic type:\n%s", out)
	}

	// NON-VACUITY: the guard must be transparent when nothing panics, or the
	// assertions above could hold for a function that never runs its pass.
	ran := false
	if err := guardPreGuardPanic(logger, func() error { ran = true; return nil }); err != nil {
		t.Errorf("a non-panicking pass returned %v, want nil", err)
	}
	if !ran {
		t.Error("guardPreGuardPanic did not run the pass")
	}

	// THE PASS'S OWN ERROR IS RETURNED, NOT SWALLOWED (#3079 review,
	// MEDIUM-1). Without this the guard would convert every non-panicking
	// failure into a nil, which is exactly the exit-0-on-failure defect.
	sentinel := errors.New("the pass failed")
	if err := guardPreGuardPanic(logger, func() error { return sentinel }); !errors.Is(err, sentinel) {
		t.Errorf("guardPreGuardPanic returned %v, want the pass's own error", err)
	}
}

// TestPrintLockDamageReport_UnambiguousLossesFirst pins the preview's
// ORDERING, which is the half of #3079's safety argument the predicate does
// not carry: the pass restores an unattributed population, so what makes it
// safe is a human ruling on the cut, and a preview is only an approval
// mechanism if the rows needing no thought are separable from the rows
// needing the most. On this deployment the list runs to 215 rows; ordered by
// timestamp, the 4 emptied fields sit scattered among 48 that grew.
//
// MUTATION PROOF. Deleting the orderedForPreview call in
// printLockDamageReport (printing res.Restored directly, the pre-fix
// behavior) fails this test: the input below is deliberately seeded in the
// WORST order, exactly inverted, so a report that preserves input order
// prints "longer" first and the emptied row last.
//
// It asserts the ORDER of the direction groups, never that a row is absent:
// every candidate must still be printed. The count assertion below is what
// stops a future "sort" that also filters -- hard-filtering on direction is
// precisely what the predicate refuses to do (a provider can return a longer
// WRONG value), and the preview must not reintroduce it in the renderer.
func TestPrintLockDamageReport_UnambiguousLossesFirst(t *testing.T) {
	res := &maintenance.LockDamageResult{
		Restored: []maintenance.LockDamageRestore{
			{ArtistID: "a-longer", Field: "biography", Direction: "longer"},
			{ArtistID: "a-same", Field: "biography", Direction: "same-length"},
			{ArtistID: "a-shorter", Field: "biography", Direction: "shorter"},
			{ArtistID: "a-emptied", Field: "biography", Direction: "emptied"},
		},
	}

	var out bytes.Buffer
	printLockDamageReport(&out, res, true)
	report := out.String()

	// Every row still printed: ordering must not become filtering.
	for _, id := range []string{"a-longer", "a-same", "a-shorter", "a-emptied"} {
		if !strings.Contains(report, "artist="+id+" ") {
			t.Fatalf("candidate %s is missing from the report; ordering must not drop rows:\n%s", id, report)
		}
	}

	// The direction groups appear least-ambiguous first.
	want := []string{"a-emptied", "a-shorter", "a-same", "a-longer"}
	at := make([]int, len(want))
	for i, id := range want {
		at[i] = strings.Index(report, "artist="+id+" ")
	}
	for i := 1; i < len(at); i++ {
		if at[i-1] >= at[i] {
			t.Fatalf("%s printed at %d, %s at %d; want the unambiguous losses first:\n%s",
				want[i-1], at[i-1], want[i], at[i], report)
		}
	}
}

// TestOrderedForPreview_DoesNotMutateItsInput guards the copy. The result the
// preview renders is the SAME slice the write pass consumes, so an in-place
// sort here would silently reorder the restore loop -- a reporting concern
// reaching into the write path.
func TestOrderedForPreview_DoesNotMutateItsInput(t *testing.T) {
	in := []maintenance.LockDamageRestore{
		{ArtistID: "a1", Direction: "longer"},
		{ArtistID: "a2", Direction: "emptied"},
	}
	_ = orderedForPreview(in)
	if in[0].ArtistID != "a1" || in[1].ArtistID != "a2" {
		t.Errorf("orderedForPreview reordered its input: %v", in)
	}
}

// previewDigest runs the pre-guard PREVIEW against dbPath and returns the
// approval digest it computed, the way an operator reads it off the report.
//
// It goes through the real dry-run path (read-only handle, real selection) so
// the token it returns is the one the printed report carries -- a test that
// hardcoded a digest would pass even if the two sides disagreed about how to
// compute it, which is the one thing the gate must get right.
func previewDigest(t *testing.T, dbPath string) string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		t.Fatalf("opening the preview handle: %v", err)
	}
	defer func() { _ = db.Close() }()

	var out bytes.Buffer
	if err := lockDamageDryRunDB(context.Background(), db, &out,
		maintenance.LockDamageOpts{DryRun: true, PreGuard: true}); err != nil {
		t.Fatalf("preview: %v", err)
	}
	// Parse it out of the REPORT, not from a second call to the digest
	// function: this asserts the operator can actually obtain the token from
	// what they are shown.
	const marker = "approval digest: "
	i := strings.Index(out.String(), marker)
	if i < 0 {
		t.Fatalf("the preview report carries no %q line:\n%s", marker, out.String())
	}
	rest := out.String()[i+len(marker):]
	digest, _, _ := strings.Cut(rest, "\n")
	digest = strings.TrimSpace(digest)
	if digest == "" {
		t.Fatalf("the preview printed an empty digest:\n%s", out.String())
	}
	return digest
}

// TestRunLockDamagePreGuardRepair_RequiresTheApprovalDigest pins that the
// gate CANNOT BE SKIPPED BY OMITTING THE ARGUMENT (#3079 review, HIGH-1).
//
// An empty digest must be refused outright rather than treated as "restore
// whatever the predicate finds". The refusal happens before the database is
// even opened, so a missing token cannot migrate a database as a side effect
// of a command that was going to be rejected anyway.
func TestRunLockDamagePreGuardRepair_RequiresTheApprovalDigest(t *testing.T) {
	dir := t.TempDir()
	dbPath := dir + "/preguard-nodigest.db"
	t.Setenv("SW_DB_PATH", dbPath)
	t.Setenv("SW_CONFIG_PATH", dir+"/no-such-config.toml")
	t.Setenv("SW_MUSIC_PATH", dir)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, empty := range []string{"", "   ", "\t\n"} {
		err := runLockDamagePreGuardRepair(logger, empty)
		if err == nil {
			t.Fatalf("runLockDamagePreGuardRepair(%q) returned nil; a write pass with no "+
				"approval digest must be refused", empty)
		}
		if !strings.Contains(err.Error(), "lock-damage-pre-guard-approve") {
			t.Errorf("the refusal does not name the flag the operator must pass: %v", err)
		}
	}

	// NOTHING WAS OPENED OR CREATED. The refusal precedes openMigratedRuntimeDB,
	// so a rejected command leaves no migrated database behind.
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Errorf("the refused command created %s (stat err = %v); it must refuse before "+
			"opening or migrating anything", dbPath, err)
	}
}

// TestRunLockDamagePreGuardRepair_StaleDigestRefusesAndWritesNothing is the
// end-to-end form of the drift refusal: a lock toggled after the preview
// enlarges the candidate set, and the repair must decline the whole pass.
//
// It asserts all four properties of a correct refusal: a non-nil error, the
// damaged values UNTOUCHED (not partially restored), no history rows written,
// and the completion key NOT stamped -- a stamped key would retire the
// one-shot on a pass that did nothing.
func TestRunLockDamagePreGuardRepair_StaleDigestRefusesAndWritesNothing(t *testing.T) {
	dir := t.TempDir()
	dbPath := dir + "/preguard-stale.db"
	t.Setenv("SW_DB_PATH", dbPath)
	t.Setenv("SW_CONFIG_PATH", dir+"/no-such-config.toml")
	t.Setenv("SW_MUSIC_PATH", dir)

	ctx := context.Background()
	mig, err := database.Open(dbPath)
	if err != nil {
		t.Fatalf("opening db for migration: %v", err)
	}
	if err := database.Migrate(mig); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	seedPreGuardFixture(t, ctx, mig)
	requirePreGuardFixture(t, ctx, mig)
	// a2's damage is newer than the cutoff, so it can never join the set. Add
	// a THIRD artist whose damage is eligible but whose field is not locked
	// yet: that is the row the toggle will pull in.
	if _, err := mig.ExecContext(ctx,
		`INSERT INTO artists (id, name, sort_name, path, biography, locked_fields, created_at, updated_at)
		 VALUES ('a3', 'Locked Later', 'Locked Later', '/a3', 'junk bio', '[]', ?, ?)`,
		preGuardBefore(), preGuardBefore()); err != nil {
		t.Fatalf("seeding a3: %v", err)
	}
	if _, err := mig.ExecContext(ctx,
		`INSERT INTO metadata_changes (id, artist_id, field, old_value, new_value, source, created_at)
		 VALUES ('d3', 'a3', 'biography', 'curated bio', 'junk bio', 'manual', ?)`,
		preGuardBefore()); err != nil {
		t.Fatalf("seeding a3 damage: %v", err)
	}
	_ = mig.Close()

	// PREVIEW, then the operator's lock toggle, then the repair with the now
	// STALE digest.
	digest := previewDigest(t, dbPath)

	toggle, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("opening for the toggle: %v", err)
	}
	if _, err := toggle.ExecContext(ctx,
		`UPDATE artists SET locked_fields = '["biography"]' WHERE id = 'a3'`); err != nil {
		t.Fatalf("toggling the lock: %v", err)
	}
	_ = toggle.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	err = runLockDamagePreGuardRepair(logger, digest)
	if err == nil {
		t.Fatal("the repair accepted a digest that no longer describes the candidate set; " +
			"a lock added after the preview must refuse the pass")
	}
	var drift *maintenance.LockDamageDriftError
	if !errors.As(err, &drift) {
		t.Fatalf("err = %v, want a *maintenance.LockDamageDriftError", err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}
	defer func() { _ = db.Close() }()

	for _, id := range []string{"a1", "a3"} {
		var bio string
		if err := db.QueryRowContext(ctx,
			`SELECT biography FROM artists WHERE id = ?`, id).Scan(&bio); err != nil {
			t.Fatalf("reading %s: %v", id, err)
		}
		if bio != "junk bio" {
			t.Errorf("%s biography = %q, want it UNTOUCHED; a refused pass writes nothing, "+
				"not even the rows that WERE approved", id, bio)
		}
	}
	var reverts int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM metadata_changes WHERE source = 'revert'`).Scan(&reverts); err != nil {
		t.Fatalf("counting revert rows: %v", err)
	}
	if reverts != 0 {
		t.Errorf("revert rows = %d, want 0", reverts)
	}
	var stamped int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM settings WHERE key = ?`, lockDamagePreGuardKey).Scan(&stamped); err != nil {
		t.Fatalf("reading the completion key: %v", err)
	}
	if stamped != 0 {
		t.Errorf("completion key rows = %d, want 0; a refused pass must not retire the one-shot", stamped)
	}
}

// TestRunLockDamageRepairPass_FailurePropagatesAnError pins MEDIUM-1: a pass
// that fails outright must surface an error the caller can turn into a
// non-zero exit code.
//
// Before this the error died in a log line and the entry point returned nil,
// so an operator scripting `stillwater -lock-damage-pre-guard-repair && echo
// done` was told the repair succeeded when it had done nothing. The
// migration-failure path already exited 1, so the old behavior was
// inconsistent as well as wrong.
//
// The failure is induced the way the reviewer induced it: rename the table
// the pass reads, so selection fails for a reason no retry can change within
// the run.
func TestRunLockDamageRepairPass_FailurePropagatesAnError(t *testing.T) {
	dir := t.TempDir()
	dbPath := dir + "/preguard-fail.db"

	ctx := context.Background()
	db, err := database.Open(dbPath)
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := database.Migrate(db); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`ALTER TABLE metadata_changes RENAME TO metadata_changes_gone`); err != nil {
		t.Fatalf("renaming the table away: %v", err)
	}

	var logged bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logged, nil))
	artistSvc := artist.NewService(db)
	hist := artist.NewHistoryService(db)
	artistSvc.SetHistoryService(hist)
	maint := maintenance.NewService(db, dbPath, "", logger)
	maint.SetLockDamageDeps(hist.Repo(), artistSvc)

	err = runLockDamageRepairPass(ctx, db, logger, maint,
		maintenance.LockDamageOpts{PreGuard: true, ApprovedDigest: "irrelevant"},
		lockDamagePreGuardKey)
	if err == nil {
		t.Fatal("runLockDamageRepairPass returned nil for a pass that could not read its " +
			"own input; the CLI would exit 0 and a script would read that as success")
	}
	// STILL LOGGED. The error is returned IN ADDITION to the log line, not
	// instead of it: the startup path discards the error and relies on the log.
	if !strings.Contains(logged.String(), "locked-field damage repair failed") {
		t.Errorf("the failure was returned but not logged:\n%s", logged.String())
	}
	// NOT STAMPED. A failed pass must remain retriable.
	var stamped int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM settings WHERE key = ?`, lockDamagePreGuardKey).Scan(&stamped); err != nil {
		t.Fatalf("reading the completion key: %v", err)
	}
	if stamped != 0 {
		t.Errorf("completion key rows = %d, want 0", stamped)
	}
}

// TestPrintLockDamageReport_AttributedModeOmitsPreGuardExclusions pins the
// asymmetry CodeRabbit flagged on PR #3136.
//
// The three exclusion counts describe filters only the PRE-GUARD pass
// applies. Printed unconditionally, the attributed report stated
// `excluded, newer than the cutoff (<instant>): 0` -- naming a bound that
// pass never applies. A zero there is a positive claim that a filter ran, on
// the report an operator reads before authorizing a data restore, and it is
// false.
//
// The test asserts BOTH directions from ONE result, so it cannot pass by the
// lines being absent everywhere (which would break the pre-guard preview's
// own "the bound's effect is PRINTED" property) or present everywhere.
func TestPrintLockDamageReport_AttributedModeOmitsPreGuardExclusions(t *testing.T) {
	// THREE DISTINCT NON-ZERO COUNTS, and the distinctness is load-bearing:
	// equal values would make a counter SWAP (reading PreGuardUnlocked into
	// the cutoff line, say) invisible to any assertion below. In attributed
	// mode all three are zero by construction, so setting them here proves the
	// gate is on the MODE, not on the values.
	res := &maintenance.LockDamageResult{
		PreGuardTooNew:   3,
		PreGuardUnlocked: 4,
		PreGuardDiverged: 5,
	}

	// LABELS ONLY, and ONLY for the ABSENCE half. Absence is exactly what a
	// label expresses: asserting that a fully-rendered line is missing would
	// pass if the label were present carrying a different number, which is
	// WEAKER than what this half needs. The presence half below uses rendered
	// lines instead, for the mirror-image reason.
	preGuardOnlyLabels := []string{
		"excluded, newer than the cutoff",
		"excluded, field not locked now",
		"excluded, the field changed since the damage",
	}

	var attributed bytes.Buffer
	printLockDamageReport(&attributed, res, false)
	for _, label := range preGuardOnlyLabels {
		if strings.Contains(attributed.String(), label) {
			t.Errorf("the ATTRIBUTED report carries %q; that filter does not run on this "+
				"path, so the line asserts a bound the pass never applied:\n%s",
				label, attributed.String())
		}
	}
	// The attributed report must still carry its own sections, or "omits the
	// pre-guard lines" would also be true of a printer that emitted nothing.
	if !strings.Contains(attributed.String(), "unrecoverable:") {
		t.Errorf("the attributed report lost its own sections:\n%s", attributed.String())
	}

	// FULLY-RENDERED LINES FOR THE PRESENCE HALF (CodeRabbit, PR #3136 round
	// 2). This half used to reuse the label slice above, so it checked that
	// the three labels appeared and never checked the NUMBERS -- a printer
	// emitting 0 for all three, or reading the wrong counter into the wrong
	// line, passed it. That is the SAME VACUITY CLASS as the ": 1" assertion
	// fixed one commit earlier, reintroduced in the test written to fix a
	// different finding.
	//
	// The point of this half is that the bound's EFFECT is printed, not that a
	// label exists, and an effect is a number. Each expected line pins one
	// count to its own label, so a swap between any two of them fails here.
	// The cutoff line is built from the printer's own format string, so the
	// timestamp cannot drift out of sync with the constant.
	//
	// MUTATION PROOF: swap PreGuardTooNew and PreGuardUnlocked in the printer.
	// The old label-only assertion PASSED; this one fails on both lines.
	// Verified both ways.
	wantPreGuardLines := []string{
		fmt.Sprintf("excluded, newer than the cutoff (%s): 3",
			maintenance.PreGuardCutoff().Format(time.RFC3339)),
		"excluded, field not locked now: 4",
		"excluded, the field changed since the damage: 5",
	}

	var preGuard bytes.Buffer
	printLockDamageReport(&preGuard, res, true)
	for _, line := range wantPreGuardLines {
		if !strings.Contains(preGuard.String(), line) {
			t.Errorf("the PRE-GUARD report does not carry %q; the bound's effect must be "+
				"PRINTED -- a label with the wrong number states that a filter ran and "+
				"found something it did not:\n%s", line, preGuard.String())
		}
	}
}
