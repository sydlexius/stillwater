package database

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// Open opens a SQLite database at the given path with WAL mode enabled.
// It creates the parent directory if it does not exist.
func Open(dbPath string) (*sql.DB, error) {
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("creating database directory: %w", err)
	}

	// modernc.org/sqlite reads PRAGMAs from the DSN via _pragma=NAME(VALUE)
	// query parameters. The legacy _foreign_keys=ON / _journal_mode=WAL
	// shorthand used by mattn/go-sqlite3 is silently ignored here. Use the
	// modernc form so the PRAGMAs actually apply on every connection.
	//
	// Foreign key enforcement is intentionally NOT set here. Issue #1078
	// established that PRAGMA foreign_keys must be on for the ON DELETE
	// CASCADE declarations to fire, but turning it on by default broke a
	// large body of existing test fixtures that rely on insert-without-
	// parent shortcuts. EnableForeignKeys is the explicit opt-in that
	// production main calls; tests that need cascade semantics call it
	// (or a local PRAGMA) themselves.
	db, err := sql.Open(
		"sqlite",
		dbPath+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)",
	)
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}

	if err := db.PingContext(context.Background()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("pinging database: %w", err)
	}

	// Single writer connection for SQLite.
	db.SetMaxOpenConns(1)

	return db, nil
}

// EnableForeignKeys turns on PRAGMA foreign_keys for the connection pool and
// verifies that the pragma took effect. Issue #1078: modernc.org/sqlite does
// not respect the legacy _foreign_keys=ON DSN shorthand, so the only reliable
// way to get FK CASCADE enforcement is to issue PRAGMA explicitly. Production
// startup calls this immediately after Open so artist deletes actually
// cascade through artist_platform_ids, artist_images, rule_violations, and
// the other child tables. Tests that exercise cascade behavior call this
// (or run a local PRAGMA) explicitly.
func EnableForeignKeys(db *sql.DB) error {
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, "PRAGMA foreign_keys = ON"); err != nil {
		return fmt.Errorf("enabling foreign keys: %w", err)
	}
	var fkOn int
	if err := db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fkOn); err != nil {
		return fmt.Errorf("checking foreign_keys pragma: %w", err)
	}
	if fkOn != 1 {
		return fmt.Errorf("foreign_keys pragma not enabled (got %d); FK CASCADE will not fire", fkOn)
	}
	return nil
}
