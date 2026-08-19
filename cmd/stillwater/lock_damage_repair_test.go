package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/cli"
	"github.com/sydlexius/stillwater/internal/database"
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

	// The report names the candidate WITHOUT its values and WITHOUT the
	// artist's name -- the design doc classes artist names with the private
	// metadata that must not reach an outward surface.
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
	if strings.Contains(report, "Locked Artist") {
		t.Errorf("report leaks the artist name:\n%s", report)
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
// its env-var bootstrap (config load, open, version check) against a temp
// database, asserting it completes and writes nothing. The database is
// migrated by the TEST first: the entry point itself refuses to migrate (see
// the refusal test below), so unlike the pre-review version this test does
// not lean on the entry point creating its own schema. Output goes to
// os.Stdout, which the test does not capture; the report's content is pinned
// by the lockDamageDryRunDB tests above.
func TestRunLockDamageDryRun_EndToEnd(t *testing.T) {
	dir := t.TempDir()
	dbPath := dir + "/dry.db"
	t.Setenv("SW_DB_PATH", dbPath)
	t.Setenv("SW_CONFIG_PATH", dir+"/no-such-config.toml")
	t.Setenv("SW_MUSIC_PATH", dir)

	// Migrate the database up front, the way a real clone would have been
	// migrated by the server that produced it.
	mig, err := database.Open(dbPath)
	if err != nil {
		t.Fatalf("opening db for migration: %v", err)
	}
	if err := database.Migrate(mig); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	_ = mig.Close()

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

// A NAME-FIELD candidate is the sharpest leak fixture: name is a lockable,
// trackable field, so for a field=name damage row the artist's NAME is
// itself the damaged value. A report that prints the name prints the value.
// The biography-only leak test above cannot catch that, which is how the
// ArtistName column survived review once.
func TestLockDamageDryRun_NameFieldCandidateLeaksNoName(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	if _, err := db.ExecContext(ctx,
		`INSERT INTO artists (id, name, sort_name, path, locked_fields)
		 VALUES ('n1', 'PRIVATE_DAMAGED_ARTIST_NAME', 'x', '', '["name"]')`); err != nil {
		t.Fatalf("seeding artist: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO metadata_changes (id, artist_id, field, old_value, new_value, source, created_at)
		 VALUES ('nd1', 'n1', 'name', 'PRIVATE_ORIGINAL_NAME', 'PRIVATE_DAMAGED_ARTIST_NAME', 'rule:name_language_pref', '2026-05-01T10:00:01Z')`); err != nil {
		t.Fatalf("seeding damage: %v", err)
	}

	var out bytes.Buffer
	if err := lockDamageDryRunDB(ctx, db, &out); err != nil {
		t.Fatalf("lockDamageDryRunDB: %v", err)
	}
	report := out.String()
	// PRECONDITION: the row is really in the report, or the leak assertions
	// below pass against an empty section.
	if !strings.Contains(report, "would restore: 1") || !strings.Contains(report, "artist=n1") {
		t.Fatalf("fixture: the name-field candidate did not reach the report:\n%s", report)
	}
	if strings.Contains(report, "PRIVATE_DAMAGED_ARTIST_NAME") || strings.Contains(report, "PRIVATE_ORIGINAL_NAME") {
		t.Errorf("report leaks the name-field value (which is also the artist name):\n%s", report)
	}
}

// THE DRY RUN MUST NOT MIGRATE THE DATABASE IT INSPECTS. A clone of a
// released deployment is behind on migrations by construction, and the
// migrations rewrite DATA (014 rewrites lock state, 024 retracts rule
// results), so migrate-then-preview would silently alter the very state the
// preview reports on -- under a banner reading "no writes performed". The
// entry point must refuse loudly instead, and the applied-migration set must
// be byte-identical before and after the attempt.
// THE DRY RUN'S HANDLE CANNOT WRITE. "No writes performed" must be a
// property of the CONNECTION (mode=ro), not of the DryRun boolean inside the
// repair path -- a boolean a future edit can route around silently, where the
// read-only handle fails loudly. This pins the difference between "we intend
// not to write" and "we cannot write"; only the second survives a future
// edit.
func TestLockDamageDryRunHandle_RefusesWrites(t *testing.T) {
	dir := t.TempDir()
	dbPath := dir + "/ro.db"
	// Create and migrate through a normal handle first.
	mig, err := database.Open(dbPath)
	if err != nil {
		t.Fatalf("opening db for migration: %v", err)
	}
	if err := database.Migrate(mig); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	_ = mig.Close()

	// The dry run's DSN, verbatim. The file: prefix is what makes mode=ro
	// ENFORCED: without it modernc's driver silently ignores the parameter
	// and this test fails with a successful write.
	ro, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro&_pragma=busy_timeout(2000)")
	if err != nil {
		t.Fatalf("opening read-only: %v", err)
	}
	defer func() { _ = ro.Close() }()

	// Reads work; writes are rejected by SQLite itself.
	var n int
	if err := ro.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM artists`).Scan(&n); err != nil {
		t.Fatalf("read through the read-only handle failed: %v", err)
	}
	if _, err := ro.ExecContext(context.Background(),
		`INSERT INTO settings (key, value) VALUES ('ro-probe', 'x')`); err == nil {
		t.Fatal("write through the mode=ro handle succeeded; the dry run's no-writes guarantee is not enforced")
	}
}

func TestRunLockDamageDryRun_RefusesToMigrateABehindDatabase(t *testing.T) {
	dir := t.TempDir()
	dbPath := dir + "/behind.db"
	t.Setenv("SW_DB_PATH", dbPath)
	t.Setenv("SW_CONFIG_PATH", dir+"/no-such-config.toml")
	t.Setenv("SW_MUSIC_PATH", dir)

	// Build a database that is BEHIND: migrate fully, then roll the goose
	// tracker back two versions, the observable shape of a clone from an
	// older release. (Down-migrating real schema is not required: the entry
	// point's check reads the tracker, and the assertion below is that the
	// tracker is untouched.)
	mig, err := database.Open(dbPath)
	if err != nil {
		t.Fatalf("opening db for migration: %v", err)
	}
	if err := database.Migrate(mig); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	if _, err := mig.Exec(
		`DELETE FROM goose_db_version WHERE version_id > (SELECT MAX(version_id) - 2 FROM goose_db_version)`); err != nil {
		t.Fatalf("rolling back tracker: %v", err)
	}
	trackerBefore := tableSnapshot(t, context.Background(), mig, "goose_db_version")
	_ = mig.Close()

	err = runLockDamageDryRun()
	if err == nil {
		t.Fatal("runLockDamageDryRun succeeded against a behind-on-migrations database; want a refusal")
	}
	if !strings.Contains(err.Error(), "refuses to migrate") {
		t.Errorf("err = %v, want the migration refusal", err)
	}

	// THE ASSERTION THAT MATTERS: the applied-version set is unchanged. Every
	// pre-review dry-run test opened an already-migrated database, which is
	// why the migration delta was always zero and the defect invisible.
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("reopening db: %v", err)
	}
	defer func() { _ = db.Close() }()
	if got := tableSnapshot(t, context.Background(), db, "goose_db_version"); got != trackerBefore {
		t.Errorf("goose_db_version changed across the refused dry run:\nbefore:\n%s\nafter:\n%s", trackerBefore, got)
	}
}

// THE DISPATCH CONTRACT: a handled flag command must tell main NOT to fall
// through to run(). For -lock-damage-dry-run the fall-through would boot a
// live server against the operator's database copy and run the REAL write
// pass the dry run exists to preview -- the most consequential line in the
// change, and previously pinned by nothing (main() is coverage-ignored and
// untested).
//
// MUTATION PROOF: make dispatchFlagCommand return handled=false for the
// dry-run arm (the equivalent of deleting main's `return`) and the first
// subtest FAILS.
func TestDispatchFlagCommand_DryRunNeverFallsThrough(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SW_DB_PATH", dir+"/dispatch.db")
	t.Setenv("SW_CONFIG_PATH", dir+"/no-such-config.toml")
	t.Setenv("SW_MUSIC_PATH", dir)

	t.Run("dry run is handled, even when it errors", func(t *testing.T) {
		// The database does not exist and is not migrated, so the command
		// errors -- and handled must STILL be true: an errored flag command
		// exits, it does not boot the server.
		var stderr bytes.Buffer
		handled, err := dispatchFlagCommand(cli.Flags{LockDamageDryRun: true}, &stderr)
		if !handled {
			t.Fatal("handled = false for -lock-damage-dry-run; main would fall through and boot the real server")
		}
		if err == nil {
			t.Error("err = nil against a nonexistent database; the refusal should surface")
		}
	})

	t.Run("no flag set falls through to the server path", func(t *testing.T) {
		var stderr bytes.Buffer
		handled, err := dispatchFlagCommand(cli.Flags{}, &stderr)
		if handled || err != nil {
			t.Fatalf("handled=%v err=%v, want false/nil so main proceeds to run()", handled, err)
		}
	})
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

// panicValueError is a distinct error type whose MESSAGE stands in for
// library content (a field value in a panic). The privacy assertion below is
// that the type name appears in the log and the message does not.
type panicValueError struct{ msg string }

func (e *panicValueError) Error() string { return e.msg }

// panickingHistoryRepo panics from LockDamageCandidates -- the first repair
// dependency the pass touches -- with a value whose message must never reach
// a log line. Injecting the panic through a DEPENDENCY rather than a nil
// receiver keeps the test anchored to the contract: a nil-receiver panic
// stops firing the moment RepairLockDamage gains a nil guard, and the test
// would then pass while exercising nothing.
type panickingHistoryRepo struct {
	artist.HistoryRepository
}

func (panickingHistoryRepo) LockDamageCandidates(context.Context) ([]artist.LockDamageCandidate, error) {
	panic(&panicValueError{msg: "PRIVATE_FIELD_VALUE_IN_PANIC"})
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

	// Wire a real service whose history dependency panics with a
	// value-bearing message, standing in for any restore-path panic that
	// embeds library content.
	artistSvc := artist.NewService(db)
	hist := artist.NewHistoryService(db)
	artistSvc.SetHistoryService(hist)
	maint := maintenance.NewService(db, "", "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	maint.SetLockDamageDeps(panickingHistoryRepo{hist.Repo()}, artistSvc)
	app := &Application{maintenanceService: maint}
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
	if !strings.Contains(logged, "panic_type=") || !strings.Contains(logged, "panicValueError") {
		t.Errorf("log line does not carry the panic type; log:\n%s", logged)
	}
	// The panic value's MESSAGE -- the stand-in for library content -- must
	// be absent: only the type may appear.
	if strings.Contains(logged, "PRIVATE_FIELD_VALUE_IN_PANIC") {
		t.Errorf("log line carries the panic MESSAGE, not just the type; log:\n%s", logged)
	}
}

// TestStartLockDamageRepair_ClosesDoneOnPanic pins the property N2 of the
// #3088 fix round flags as untested: `defer close(done)` is registered BEFORE
// the `defer recover()` block in startLockDamageRepair's goroutine, so LIFO
// order runs recover() first and close(done) second -- meaning close(done)
// still fires even when the pass panics. That property survived the whole
// suite when close(done) was moved from a defer to the last statement of the
// goroutine body (which would make a panicking pass NEVER close done, and
// drainLockDamageRepair would then block for its full ctx deadline on every
// panicking pass -- a 30s stall on shutdown with a green suite, undetected
// until this test).
//
// Reuses panickingHistoryRepo (the same fixture
// TestStartLockDamageRepair_PanicLogsTypeOnly drives) and asserts
// app.lockDamageRepairDone is closed promptly after the panic, via drain
// rather than a raw channel read so the assertion exercises the real
// shutdown path.
func TestStartLockDamageRepair_ClosesDoneOnPanic(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	artistSvc := artist.NewService(db)
	hist := artist.NewHistoryService(db)
	artistSvc.SetHistoryService(hist)
	maint := maintenance.NewService(db, "", "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	maint.SetLockDamageDeps(panickingHistoryRepo{hist.Repo()}, artistSvc)
	app := &Application{maintenanceService: maint}
	app.startLockDamageRepair(ctx, db, slog.New(slog.NewTextHandler(io.Discard, nil)))

	drainCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := app.drainLockDamageRepair(drainCtx); err != nil {
		t.Fatalf("drainLockDamageRepair after a panicking pass: %v "+
			"(want done closed promptly -- close(done) must run even when the pass panics)", err)
	}

	select {
	case <-app.lockDamageRepairDone:
		// closed, as expected
	default:
		t.Error("lockDamageRepairDone was not closed after the panicking pass returned")
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

// blockingHistoryRepo wraps a real HistoryRepository and makes
// LockDamageCandidates block on release, signaling entered exactly once
// (sync.Once) the instant it is called. Used to prove the repair goroutine is
// GENUINELY in flight before a drain is exercised against it -- without this,
// a drain test that merely calls startLockDamageRepair then immediately
// drains cannot tell "the drain waited for a real pass" apart from "the pass
// had already finished before the drain even started" (#3088 review note:
// assert the fixture's defining property before trusting what the test
// reports).
type blockingHistoryRepo struct {
	artist.HistoryRepository
	entered chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (b *blockingHistoryRepo) LockDamageCandidates(ctx context.Context) ([]artist.LockDamageCandidate, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return b.HistoryRepository.LockDamageCandidates(ctx)
}

// TestDrainLockDamageRepair_WaitsForAnInFlightPass proves drainLockDamageRepair
// (#3088) genuinely blocks on a running repair goroutine and returns only
// after it finishes, rather than returning immediately regardless of state.
//
// Non-vacuity: <-entered blocks the test itself until the repair goroutine's
// first database read has actually started, so by the time drain is called
// the pass provably has NOT already completed. Ordering, not timing, is what
// is asserted: runtime.Gosched gives the drain goroutine a chance to run, a
// non-blocking receive proves it has NOT returned while the pass is still
// held on release (this would catch a drain that no-ops and returns
// immediately -- the actual bug #3088 fixes), then closing release lets both
// the pass and the drain complete, observed via blocking channel receives
// with a generous bound, never a fixed sleep duration.
func TestDrainLockDamageRepair_WaitsForAnInFlightPass(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	seedLockDamageFixture(t, ctx, db)

	artistSvc := artist.NewService(db)
	hist := artist.NewHistoryService(db)
	artistSvc.SetHistoryService(hist)
	maint := maintenance.NewService(db, "", "", slog.New(slog.NewTextHandler(io.Discard, nil)))

	entered := make(chan struct{})
	release := make(chan struct{})
	blocking := &blockingHistoryRepo{HistoryRepository: hist.Repo(), entered: entered, release: release}
	maint.SetLockDamageDeps(blocking, artistSvc)

	app := &Application{maintenanceService: maint}
	app.startLockDamageRepair(ctx, db, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// Block until the pass has genuinely started reading candidates.
	<-entered

	drainDone := make(chan error, 1)
	go func() { drainDone <- app.drainLockDamageRepair(context.Background()) }()

	// Give the drain goroutine a chance to run, then confirm it has NOT
	// returned while the pass is still held open. This is the assertion that
	// catches a no-op drain: without a real wait, drainDone would already
	// have a value here.
	for range 100 {
		runtime.Gosched()
	}
	select {
	case err := <-drainDone:
		t.Fatalf("drainLockDamageRepair returned (err=%v) before the in-flight pass finished", err)
	default:
	}

	// Release the pass and confirm both it and the drain complete.
	close(release)

	select {
	case err := <-drainDone:
		if err != nil {
			t.Fatalf("drainLockDamageRepair: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("drainLockDamageRepair did not return after the pass was released")
	}

	select {
	case <-app.lockDamageRepairDone:
		// closed, as expected
	default:
		t.Error("lockDamageRepairDone was not closed by the time drain returned nil")
	}
}

// TestDrainLockDamageRepair_CanceledContextReturnsPromptly proves the
// deadline path: a context that is ALREADY canceled makes drain return
// ctx.Err() without waiting for the in-flight pass, so a wedged pass cannot
// hang shutdown forever. Uses cancellation rather than a wall-clock timeout
// to stay deterministic (a real short deadline racing goroutine scheduling
// under load is exactly the flaky pattern issue #2630 warns against).
func TestDrainLockDamageRepair_CanceledContextReturnsPromptly(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	seedLockDamageFixture(t, ctx, db)

	artistSvc := artist.NewService(db)
	hist := artist.NewHistoryService(db)
	artistSvc.SetHistoryService(hist)
	maint := maintenance.NewService(db, "", "", slog.New(slog.NewTextHandler(io.Discard, nil)))

	entered := make(chan struct{})
	release := make(chan struct{})
	blocking := &blockingHistoryRepo{HistoryRepository: hist.Repo(), entered: entered, release: release}
	maint.SetLockDamageDeps(blocking, artistSvc)

	app := &Application{maintenanceService: maint}
	app.startLockDamageRepair(ctx, db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	<-entered // genuinely in flight, never released within this test

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	drainErr := app.drainLockDamageRepair(canceledCtx)
	if !errors.Is(drainErr, context.Canceled) {
		t.Fatalf("drainLockDamageRepair error = %v, want context.Canceled", drainErr)
	}

	// Clean up the still-blocked goroutine so it does not leak past the test.
	close(release)
	<-app.lockDamageRepairDone
}

// TestDrainLockDamageRepair_NilChannelReturnsImmediately covers the startup
// race #3088's comment on lockDamageRepairDone documents: a shutdown that
// lands before startLockDamageRepair ever ran (or after the completion key
// already gated it off) must not block on a channel nothing will ever close.
func TestDrainLockDamageRepair_NilChannelReturnsImmediately(t *testing.T) {
	app := &Application{}
	if err := app.drainLockDamageRepair(context.Background()); err != nil {
		t.Fatalf("drainLockDamageRepair on a never-started repair: %v", err)
	}
}
