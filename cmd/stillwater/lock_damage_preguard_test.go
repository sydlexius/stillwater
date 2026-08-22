package main

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"log/slog"
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
	if !strings.Contains(report, "excluded, newer than the cutoff") ||
		!strings.Contains(report, ": 1") {
		t.Errorf("report does not state what the time bound withheld:\n%s", report)
	}
	if !strings.Contains(report, maintenance.PreGuardCutoff().Format(time.RFC3339)) {
		t.Errorf("report does not name the cutoff it applied:\n%s", report)
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
	runLockDamageRepairPass(ctx, db, slog.New(slog.NewTextHandler(io.Discard, nil)), maint,
		maintenance.LockDamageOpts{PreGuard: true}, lockDamagePreGuardKey)

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
	if err := runLockDamagePreGuardRepair(logger); err != nil {
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
	if err := runLockDamagePreGuardRepair(logger); err != nil {
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

	err := guardPreGuardPanic(logger, func() {
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
	if err := guardPreGuardPanic(logger, func() { ran = true }); err != nil {
		t.Errorf("a non-panicking pass returned %v, want nil", err)
	}
	if !ran {
		t.Error("guardPreGuardPanic did not run the pass")
	}
}
