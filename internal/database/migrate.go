package database

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"log/slog"

	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrations embed.FS

// gooseLogger adapts slog to the goose.Logger interface.
type gooseLogger struct {
	logger *slog.Logger
}

func (g *gooseLogger) Fatalf(format string, v ...interface{}) {
	g.logger.Error(fmt.Sprintf(format, v...))
}
func (g *gooseLogger) Printf(format string, v ...interface{}) {
	g.logger.Info(fmt.Sprintf(format, v...))
}

// Migrate runs all pending database migrations.
func Migrate(db *sql.DB) error {
	goose.SetBaseFS(migrations)
	goose.SetLogger(&gooseLogger{logger: slog.Default().With("component", "database")})

	if err := goose.SetDialect("sqlite3"); err != nil {
		return fmt.Errorf("setting goose dialect: %w", err)
	}

	if err := goose.Up(db, "migrations"); err != nil {
		return fmt.Errorf("running migrations: %w", err)
	}

	// Policy: all schema lives in 001_initial_schema.sql (never adds new
	// migration files). Goose only runs 001 once per DB, so columns added
	// to that file do not appear on DBs that already recorded 001 as done.
	// The helpers below bridge that gap by applying idempotent ALTER TABLE
	// statements at runtime for each post-freeze column addition.
	if err := ensureConnectionColumns(db); err != nil {
		return fmt.Errorf("ensuring connection columns: %w", err)
	}

	// Issue #699: seed rule_results rows for every open / pending_choice
	// violation already in the database. Without this, pre-existing
	// violations would have a missing first_failed_at until the next
	// pipeline pass re-evaluated the artist, losing the "how long has
	// this been broken" signal. INSERT OR IGNORE makes this idempotent
	// on every startup: once a (artist_id, rule_id) row exists, repeat
	// runs are cheap no-ops.
	if err := backfillRuleResultsFromViolations(db); err != nil {
		return fmt.Errorf("backfilling rule_results from violations: %w", err)
	}

	return nil
}

// backfillRuleResultsFromViolations seeds rule_results rows for every
// currently-active violation (open or pending_choice) that does not already
// have a row. Historical dismissed / resolved violations are skipped: they
// are no longer authoritative outcomes, and the next Run Rules pass will
// fill in fresh pass/fail rows via the pipeline's UpsertRuleResultPass and
// the transactional UpsertViolation writes.
//
// The INSERT carries rule_violations.created_at into first_failed_at so the
// "how long has this been broken" signal survives the backfill, rather than
// resetting to "now" and losing history.
func backfillRuleResultsFromViolations(db *sql.DB) error {
	ctx := context.Background()
	// Use INSERT OR IGNORE so re-running Migrate is a no-op once the rows
	// exist. The PRIMARY KEY (artist_id, rule_id) enforces idempotency.
	// The JOINs filter out orphaned rule_violations rows whose artist or
	// rule no longer exists. Without them a single stale violation would
	// abort the whole INSERT on a FK violation (rule_results has FKs to
	// artists and rules) and leave the backfill half-applied, aborting
	// migration and blocking startup.
	_, err := db.ExecContext(ctx, `
		INSERT OR IGNORE INTO rule_results (
			artist_id, rule_id, passed, violation_id, evaluated_at,
			violation_message, first_failed_at
		)
		SELECT
			rv.artist_id, rv.rule_id, 0, rv.id, rv.updated_at, rv.message, rv.created_at
		FROM rule_violations rv
		JOIN artists a ON a.id = rv.artist_id
		JOIN rules r ON r.id = rv.rule_id
		WHERE rv.status IN ('open', 'pending_choice')
	`)
	if err != nil {
		return fmt.Errorf("inserting rule_results backfill rows: %w", err)
	}
	return nil
}

// ensureConnectionColumns idempotently adds columns that were appended to
// the connections table after the 001 migration was first applied on a
// deployed instance. SQLite's ADD COLUMN is an O(1) metadata change, so
// running these on every startup is cheap. Each column is checked with
// PRAGMA table_info to avoid the "duplicate column" error SQLite raises
// when a column already exists.
func ensureConnectionColumns(db *sql.DB) error {
	return ensureColumn(db, "connections", "platform_server_id", "TEXT NOT NULL DEFAULT ''")
}

// ensureColumn adds a column to a table if it does not already exist.
// The definition string is concatenated directly into the ALTER TABLE
// statement, so callers must only pass internal literals.
func ensureColumn(db *sql.DB, table, column, definition string) error {
	ctx := context.Background()
	rows, err := db.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s)", table)) //nolint:gosec // G201: table is a hard-coded literal, not user input
	if err != nil {
		return fmt.Errorf("reading %s schema: %w", table, err)
	}
	defer rows.Close() //nolint:errcheck

	for rows.Next() {
		var (
			cid       int
			name      string
			ctype     string
			notnull   int
			dfltValue sql.NullString
			pk        int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dfltValue, &pk); err != nil {
			return fmt.Errorf("scanning %s schema row: %w", table, err)
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating %s schema: %w", table, err)
	}

	stmt := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, definition) //nolint:gosec // G201: table, column, and definition are hard-coded literals
	if _, err := db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("adding %s.%s: %w", table, column, err)
	}
	return nil
}
