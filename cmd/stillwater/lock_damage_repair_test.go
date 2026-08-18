package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/maintenance"
)

// seedLockDamageFixture seeds one locked artist with rule-sourced biography
// damage: a restorable candidate. Returns nothing; ids are fixed literals.
func seedLockDamageFixture(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO artists (id, name, sort_name, path, biography, locked_fields)
		 VALUES ('a1', 'Locked Artist', 'Locked Artist', '', 'junk bio', '["biography"]')`); err != nil {
		t.Fatalf("seeding artist: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO metadata_changes (id, artist_id, field, old_value, new_value, source, created_at)
		 VALUES ('d1', 'a1', 'biography', 'curated bio', 'junk bio', 'rule:metadata_quality', '2026-05-01T10:00:01Z')`); err != nil {
		t.Fatalf("seeding damage: %v", err)
	}
}

// tableSnapshot returns every row of the given table as one sorted string, so
// two snapshots compare byte-for-byte. Column order comes from the schema and
// is stable within a single test process.
func tableSnapshot(t *testing.T, ctx context.Context, db *sql.DB, table string) string {
	t.Helper()
	// The table name is a test literal, never input.
	rows, err := db.QueryContext(ctx, "SELECT * FROM "+table+" ORDER BY 1")
	if err != nil {
		t.Fatalf("snapshotting %s: %v", table, err)
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("columns of %s: %v", table, err)
	}
	var b strings.Builder
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("scanning %s: %v", table, err)
		}
		for _, v := range vals {
			b.WriteString(strings.TrimSpace(strings.ReplaceAll(
				strings.ReplaceAll(fmtVal(v), "\n", " "), "\t", " ")))
			b.WriteByte('\t')
		}
		b.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating %s: %v", table, err)
	}
	return b.String()
}

func fmtVal(v any) string {
	switch x := v.(type) {
	case nil:
		return "<nil>"
	case []byte:
		return string(x)
	case string:
		return x
	default:
		return fmt.Sprint(x)
	}
}

// TestLockDamageDryRun_ReportsWithoutWriting proves the dry-run path performs
// NO write: the artists and metadata_changes tables are byte-identical before
// and after, no completion key is stamped, and the report still names the
// candidate. A dry run that writes is worse than no dry run.
func TestLockDamageDryRun_ReportsWithoutWriting(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	seedLockDamageFixture(t, ctx, db)

	// PRECONDITIONS: the fixture is a genuine candidate (locked + rule-sourced
	// damage), or "no writes" is vacuously true because nothing was selectable.
	var lf, src string
	if err := db.QueryRowContext(ctx,
		`SELECT locked_fields FROM artists WHERE id = 'a1'`).Scan(&lf); err != nil {
		t.Fatalf("fixture: reading locked_fields: %v", err)
	}
	if !strings.Contains(lf, "biography") {
		t.Fatalf("fixture: biography not locked (locked_fields = %s)", lf)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT source FROM metadata_changes WHERE id = 'd1'`).Scan(&src); err != nil {
		t.Fatalf("fixture: reading damage source: %v", err)
	}
	if src != "rule:metadata_quality" {
		t.Fatalf("fixture: damage source = %q, want rule:metadata_quality", src)
	}

	artistsBefore := tableSnapshot(t, ctx, db, "artists")
	changesBefore := tableSnapshot(t, ctx, db, "metadata_changes")

	var out bytes.Buffer
	if err := lockDamageDryRunDB(ctx, db, &out); err != nil {
		t.Fatalf("lockDamageDryRunDB: %v", err)
	}

	// The report names the candidate WITHOUT its values.
	report := out.String()
	if !strings.Contains(report, "would restore: 1") {
		t.Errorf("report does not count the candidate:\n%s", report)
	}
	if !strings.Contains(report, "artist=a1") || !strings.Contains(report, "rule=metadata_quality") {
		t.Errorf("report does not identify the candidate:\n%s", report)
	}
	if strings.Contains(report, "curated bio") || strings.Contains(report, "junk bio") {
		t.Errorf("report leaks field values:\n%s", report)
	}

	if got := tableSnapshot(t, ctx, db, "artists"); got != artistsBefore {
		t.Errorf("artists table changed during a dry run:\nbefore:\n%s\nafter:\n%s", artistsBefore, got)
	}
	if got := tableSnapshot(t, ctx, db, "metadata_changes"); got != changesBefore {
		t.Errorf("metadata_changes table changed during a dry run:\nbefore:\n%s\nafter:\n%s", changesBefore, got)
	}
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM settings WHERE key = ?`, lockDamageRepairKey).Scan(&n); err != nil {
		t.Fatalf("reading completion key: %v", err)
	}
	if n != 0 {
		t.Errorf("dry run stamped %s; it must never record completion", lockDamageRepairKey)
	}
}

// errGetByIDRepo fails every GetByID, forcing the repair loop's "could not
// read the artist" Failed branch for any candidate.
type errGetByIDRepo struct {
	artist.Repository
}

func (errGetByIDRepo) GetByID(_ context.Context, id string) (*artist.Artist, error) {
	return nil, fmt.Errorf("simulated artist read failure for %s", id)
}

// TestRunLockDamageRepairPass_CompletionGate pins the one-shot's completion
// rule: the settings key is stamped after a pass with no row-level failures
// (unrecoverable rows do NOT block it), and is NOT stamped when any row
// fails, so the next boot retries.
func TestRunLockDamageRepairPass_CompletionGate(t *testing.T) {
	ctx := context.Background()

	readKey := func(db *sql.DB) int {
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM settings WHERE key = ?`, lockDamageRepairKey).Scan(&n); err != nil {
			t.Fatalf("reading completion key: %v", err)
		}
		return n
	}

	t.Run("clean pass records completion", func(t *testing.T) {
		db := openTestDB(t)
		seedLockDamageFixture(t, ctx, db)
		// An unrecoverable row too: it must NOT block completion.
		if _, err := db.ExecContext(ctx,
			`INSERT INTO metadata_changes (id, artist_id, field, old_value, new_value, source, created_at)
			 VALUES ('d2', 'a1', 'origin', 'Seattle', 'Tacoma', 'manual', '2026-05-01T10:00:02Z')`); err != nil {
			t.Fatalf("seeding unrecoverable damage: %v", err)
		}

		artistSvc := artist.NewService(db)
		hist := artist.NewHistoryService(db)
		artistSvc.SetHistoryService(hist)
		maint := maintenance.NewService(db, "", "", slog.New(slog.NewTextHandler(io.Discard, nil)))
		maint.SetLockDamageDeps(hist.Repo(), artistSvc)

		runLockDamageRepairPass(ctx, db, slog.New(slog.NewTextHandler(io.Discard, nil)), maint)

		var bio string
		if err := db.QueryRowContext(ctx,
			`SELECT biography FROM artists WHERE id = 'a1'`).Scan(&bio); err != nil {
			t.Fatalf("reading biography: %v", err)
		}
		if bio != "curated bio" {
			t.Errorf("biography = %q, want the restored value (pass really ran)", bio)
		}
		if readKey(db) != 1 {
			t.Errorf("completion key not stamped after a clean pass with an unrecoverable row")
		}
	})

	t.Run("deterministic refusal permits completion", func(t *testing.T) {
		// A restore refused for a reason that recurs identically every boot
		// (here: the old name normalizes to no identity, so today's validator
		// refuses it) must NOT hold the one-shot open forever: it is counted
		// in FailedPermanent and the key is stamped (fix round for #3075).
		db := openTestDB(t)
		if _, err := db.ExecContext(ctx,
			`INSERT INTO artists (id, name, sort_name, path, biography, locked_fields)
			 VALUES ('a1', 'Damaged Name', 'Damaged Name', '', '', '["name"]')`); err != nil {
			t.Fatalf("seeding artist: %v", err)
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO metadata_changes (id, artist_id, field, old_value, new_value, source, created_at)
			 VALUES ('d1', 'a1', 'name', '-', 'Damaged Name', 'rule:name_language_pref', '2026-05-01T10:00:01Z')`); err != nil {
			t.Fatalf("seeding damage: %v", err)
		}

		artistSvc := artist.NewService(db)
		hist := artist.NewHistoryService(db)
		artistSvc.SetHistoryService(hist)
		maint := maintenance.NewService(db, "", "", slog.New(slog.NewTextHandler(io.Discard, nil)))
		maint.SetLockDamageDeps(hist.Repo(), artistSvc)

		runLockDamageRepairPass(ctx, db, slog.New(slog.NewTextHandler(io.Discard, nil)), maint)

		// PRECONDITION: the refusal really fired -- the name is unchanged.
		var name string
		if err := db.QueryRowContext(ctx,
			`SELECT name FROM artists WHERE id = 'a1'`).Scan(&name); err != nil {
			t.Fatalf("reading name: %v", err)
		}
		if name != "Damaged Name" {
			t.Fatalf("name = %q, want the refused restore to have written nothing", name)
		}
		if readKey(db) != 1 {
			t.Errorf("completion key not stamped; a deterministic refusal must not re-run the pass forever")
		}
	})

	t.Run("row-level failure blocks completion", func(t *testing.T) {
		db := openTestDB(t)
		seedLockDamageFixture(t, ctx, db)

		artists, providers, members, aliases, images, platformIDs, completeness :=
			artist.NewDefaultRepos(db)
		artistSvc := artist.NewServiceWithRepos(errGetByIDRepo{Repository: artists},
			providers, members, aliases, images, platformIDs, completeness)
		hist := artist.NewHistoryService(db)
		artistSvc.SetHistoryService(hist)
		maint := maintenance.NewService(db, "", "", slog.New(slog.NewTextHandler(io.Discard, nil)))
		maint.SetLockDamageDeps(hist.Repo(), artistSvc)

		runLockDamageRepairPass(ctx, db, slog.New(slog.NewTextHandler(io.Discard, nil)), maint)

		if readKey(db) != 0 {
			t.Errorf("completion key stamped despite a row-level failure; the retry is lost")
		}
	})
}

// TestStartLockDamageRepair_KeyGuard pins the startup guard itself: with the
// completion key absent the one-shot runs (and stamps the key); with the key
// already present it does not run at all. The live two-boot check verified
// this against a real binary; this test keeps it verified on every gate run.
func TestStartLockDamageRepair_KeyGuard(t *testing.T) {
	ctx := context.Background()

	newApp := func(db *sql.DB) *Application {
		artistSvc := artist.NewService(db)
		hist := artist.NewHistoryService(db)
		artistSvc.SetHistoryService(hist)
		maint := maintenance.NewService(db, "", "", slog.New(slog.NewTextHandler(io.Discard, nil)))
		maint.SetLockDamageDeps(hist.Repo(), artistSvc)
		return &Application{maintenanceService: maint}
	}

	waitForKey := func(db *sql.DB) bool {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			var n int
			if err := db.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM settings WHERE key = ?`, lockDamageRepairKey).Scan(&n); err != nil {
				t.Fatalf("reading completion key: %v", err)
			}
			if n > 0 {
				return true
			}
			time.Sleep(20 * time.Millisecond)
		}
		return false
	}

	t.Run("key absent: repair runs and stamps the key", func(t *testing.T) {
		db := openTestDB(t)
		seedLockDamageFixture(t, ctx, db)
		app := newApp(db)

		app.startLockDamageRepair(ctx, db, slog.New(slog.NewTextHandler(io.Discard, nil)))

		if !waitForKey(db) {
			t.Fatal("completion key never stamped; the one-shot did not run")
		}
		var bio string
		if err := db.QueryRowContext(ctx,
			`SELECT biography FROM artists WHERE id = 'a1'`).Scan(&bio); err != nil {
			t.Fatalf("reading biography: %v", err)
		}
		if bio != "curated bio" {
			t.Errorf("biography = %q, want the restored value", bio)
		}
	})

	t.Run("key present: repair does not run", func(t *testing.T) {
		db := openTestDB(t)
		seedLockDamageFixture(t, ctx, db)
		if _, err := db.ExecContext(ctx,
			`INSERT INTO settings (key, value) VALUES (?, 'already-done')`,
			lockDamageRepairKey); err != nil {
			t.Fatalf("seeding completion key: %v", err)
		}
		app := newApp(db)

		app.startLockDamageRepair(ctx, db, slog.New(slog.NewTextHandler(io.Discard, nil)))

		// The guard is synchronous (the settings read happens before any
		// goroutine is spawned), so an unchanged biography immediately after
		// the call, plus a short settle, proves no pass started.
		time.Sleep(150 * time.Millisecond)
		var bio string
		if err := db.QueryRowContext(ctx,
			`SELECT biography FROM artists WHERE id = 'a1'`).Scan(&bio); err != nil {
			t.Fatalf("reading biography: %v", err)
		}
		if bio != "junk bio" {
			t.Errorf("biography = %q, want the damaged value untouched (repair must not have run)", bio)
		}
	})
}

// TestLockDamageDryRun_ReportsUnrecoverableAndFailedRows covers the report's
// other sections with rows in them: an unattributable manual row on a LOCKED
// field lands under unrecoverable with its source named, a genuinely failing
// row exercises the failed print loop, and no field value appears anywhere in
// the report.
func TestLockDamageDryRun_ReportsUnrecoverableAndFailedRows(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	if _, err := db.ExecContext(ctx,
		`INSERT INTO artists (id, name, sort_name, path, biography, locked_fields)
		 VALUES ('a1', 'Locked Artist', 'Locked Artist', '', 'junk bio', '["biography"]')`); err != nil {
		t.Fatalf("seeding artist: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO metadata_changes (id, artist_id, field, old_value, new_value, source, created_at)
		 VALUES ('d1', 'a1', 'biography', 'curated bio', 'junk bio', 'manual', '2026-05-01T10:00:01Z')`); err != nil {
		t.Fatalf("seeding damage: %v", err)
	}

	t.Run("unrecoverable section, via the real entry point", func(t *testing.T) {
		var out bytes.Buffer
		if err := lockDamageDryRunDB(ctx, db, &out); err != nil {
			t.Fatalf("lockDamageDryRunDB: %v", err)
		}
		report := out.String()
		if !strings.Contains(report, "would restore: 0") {
			t.Errorf("report counts a restore for unattributable damage:\n%s", report)
		}
		if !strings.Contains(report, "unrecoverable: 1") || !strings.Contains(report, "names no rule") {
			t.Errorf("report does not carry the unrecoverable row with its reason:\n%s", report)
		}
		if strings.Contains(report, "curated bio") || strings.Contains(report, "junk bio") {
			t.Errorf("report leaks field values:\n%s", report)
		}
	})

	t.Run("failed section, via an injected read failure", func(t *testing.T) {
		// The entry point builds its own services, so a Failed row is produced
		// by running the same repair with a failing repository and printing
		// its result through the same printer the entry point uses. The
		// damage row must be RULE-sourced: an unattributable one is diverted
		// before the artist read that fails.
		if _, err := db.ExecContext(ctx,
			`UPDATE metadata_changes SET source = 'rule:metadata_quality' WHERE id = 'd1'`); err != nil {
			t.Fatalf("re-sourcing damage: %v", err)
		}
		artists, providers, members, aliases, images, platformIDs, completeness :=
			artist.NewDefaultRepos(db)
		artistSvc := artist.NewServiceWithRepos(errGetByIDRepo{Repository: artists},
			providers, members, aliases, images, platformIDs, completeness)
		hist := artist.NewHistoryService(db)
		artistSvc.SetHistoryService(hist)
		maint := maintenance.NewService(db, "", "", slog.New(slog.NewTextHandler(io.Discard, nil)))
		maint.SetLockDamageDeps(hist.Repo(), artistSvc)

		res, err := maint.RepairLockDamage(ctx, maintenance.LockDamageOpts{DryRun: true})
		if err != nil {
			t.Fatalf("RepairLockDamage: %v", err)
		}
		// PRECONDITION: the injected failure produced a Failed row, or the
		// print loop below runs over nothing.
		if len(res.Failed) != 1 {
			t.Fatalf("failed = %d, want 1 (the injected read failure)", len(res.Failed))
		}

		var out bytes.Buffer
		printLockDamageReport(&out, res)
		report := out.String()
		if !strings.Contains(report, "failed: 1") || !strings.Contains(report, "could not read") {
			t.Errorf("report does not carry the failed row with its reason:\n%s", report)
		}
		if strings.Contains(report, "curated bio") || strings.Contains(report, "junk bio") {
			t.Errorf("report leaks field values:\n%s", report)
		}
	})
}

// TestRunLockDamageDryRun_EndToEnd drives the real flag entry point through
// its env-var bootstrap (config load, migrate, open) against a temp database,
// asserting it completes and writes nothing. Output goes to os.Stdout, which
// the test does not capture; the report's content is pinned by the
// lockDamageDryRunDB tests above.
func TestRunLockDamageDryRun_EndToEnd(t *testing.T) {
	dir := t.TempDir()
	dbPath := dir + "/dry.db"
	t.Setenv("SW_DB_PATH", dbPath)
	t.Setenv("SW_CONFIG_PATH", dir+"/no-such-config.toml")
	t.Setenv("SW_MUSIC_PATH", dir)

	// First run migrates and creates the database; no candidates exist.
	if err := runLockDamageDryRun(); err != nil {
		t.Fatalf("runLockDamageDryRun: %v", err)
	}

	// Seed a candidate and prove a second run still writes nothing.
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("opening the dry-run db: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	seedLockDamageFixture(t, ctx, db)

	if err := runLockDamageDryRun(); err != nil {
		t.Fatalf("runLockDamageDryRun (seeded): %v", err)
	}
	var bio string
	if err := db.QueryRowContext(ctx,
		`SELECT biography FROM artists WHERE id = 'a1'`).Scan(&bio); err != nil {
		t.Fatalf("reading biography: %v", err)
	}
	if bio != "junk bio" {
		t.Errorf("biography = %q, want the damaged value untouched by the dry run", bio)
	}
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM settings WHERE key = ?`, lockDamageRepairKey).Scan(&n); err != nil {
		t.Fatalf("reading completion key: %v", err)
	}
	if n != 0 {
		t.Errorf("dry-run entry point stamped %s", lockDamageRepairKey)
	}
}

// A pass-level error (here: dependencies never attached) must leave the
// completion key unstamped so the next boot retries.
func TestRunLockDamageRepairPass_ErrorLeavesKeyUnstamped(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	maint := maintenance.NewService(db, "", "", slog.New(slog.NewTextHandler(io.Discard, nil)))

	runLockDamageRepairPass(ctx, db, slog.New(slog.NewTextHandler(io.Discard, nil)), maint)

	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM settings WHERE key = ?`, lockDamageRepairKey).Scan(&n); err != nil {
		t.Fatalf("reading completion key: %v", err)
	}
	if n != 0 {
		t.Errorf("completion key stamped despite the pass erroring")
	}
}

// THE PANIC HANDLER'S PRIVACY CONTRACT (acceptance criterion: field values
// never reach a log line, including the panic handler). A panicking pass is
// caught, the process survives, and the log line carries the panic TYPE only
// -- never the panic value, which on a restore-path panic can embed a field
// value.
func TestStartLockDamageRepair_PanicLogsTypeOnly(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	var logBuf syncBuffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	// A nil maintenance service makes RepairLockDamage dereference a nil
	// receiver: a real panic with a runtime error type, standing in for any
	// restore-path panic whose message could carry library content.
	app := &Application{maintenanceService: nil}
	app.startLockDamageRepair(ctx, db, logger)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(logBuf.String(), "panicked") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	logged := logBuf.String()
	if !strings.Contains(logged, "locked-field damage repair panicked") {
		t.Fatalf("panic was not caught and logged; log:\n%s", logged)
	}
	if !strings.Contains(logged, "panic_type=") {
		t.Errorf("log line does not carry the panic type; log:\n%s", logged)
	}
	// The runtime error's MESSAGE ("invalid memory address or nil pointer
	// dereference") must be absent: only its type may appear.
	if strings.Contains(logged, "nil pointer dereference") {
		t.Errorf("log line carries the panic MESSAGE, not just the type; log:\n%s", logged)
	}
}

// A dry run against a database with no schema surfaces the query error
// rather than reporting an empty (clean-looking) result.
func TestLockDamageDryRunDB_SurfacesQueryError(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", dir+"/empty.db")
	if err != nil {
		t.Fatalf("opening empty db: %v", err)
	}
	defer func() { _ = db.Close() }()

	var out bytes.Buffer
	if err := lockDamageDryRunDB(context.Background(), db, &out); err == nil {
		t.Fatal("lockDamageDryRunDB returned nil error against an unmigrated database")
	}
}
