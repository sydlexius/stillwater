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
