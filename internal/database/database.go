package database

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// Open opens a SQLite database at the given path with WAL mode enabled and
// foreign key enforcement intentionally OFF. It creates the parent directory
// if it does not exist.
//
// This is the MIGRATION / test-fixture open path. Goose migrations run against
// a handle from Open so they can rebuild tables and rewrite child rows without
// tripping FK constraints (migration 015 rebuilds the artists table; migration
// 009/019 rewrite child rows). Test fixtures that rely on insert-without-parent
// shortcuts also use Open. The long-lived RUNTIME pool must instead come from
// OpenRuntime so ON DELETE CASCADE fires on every connection (see #2272).
func Open(dbPath string) (*sql.DB, error) {
	return open(dbPath, "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
}

// OpenRuntime opens the long-lived SQLite pool the running server serves from.
// Unlike Open, its DSN sets foreign_keys(1) so PRAGMA foreign_keys is ON for
// EVERY connection the pool hands out, including connections that are recycled
// or freshly reopened by database/sql.
//
// Issue #2272: PRAGMA foreign_keys is a PER-CONNECTION setting that defaults
// OFF. The previous design turned it on once via EnableForeignKeys after Open,
// but that only affected the connection(s) then in the pool; when a pooled
// connection was discarded and reopened, FK enforcement reverted OFF and artist
// delete/merge cascades silently no-opped, orphaning child rows across the
// child tables that declare REFERENCES artists(id) ON DELETE CASCADE. Setting
// foreign_keys(1) in the DSN makes modernc.org/sqlite apply the pragma on each
// new connection, so the cascade fires reliably. Migrations still use Open
// (FK-off); only the serving pool uses OpenRuntime.
func OpenRuntime(dbPath string) (*sql.DB, error) {
	return open(dbPath, "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
}

// open is the shared implementation for Open and OpenRuntime. dsnParams is the
// DSN query string (leading '?') carrying the modernc PRAGMA parameters.
//
// modernc.org/sqlite reads PRAGMAs from the DSN via _pragma=NAME(VALUE) query
// parameters. The legacy _foreign_keys=ON / _journal_mode=WAL shorthand used by
// mattn/go-sqlite3 is silently ignored here, so the modernc form is required
// for the PRAGMAs to actually apply on every connection.
func open(dbPath, dsnParams string) (*sql.DB, error) {
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("creating database directory: %w", err)
	}

	db, err := sql.Open("sqlite", dbPath+dsnParams)
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

// EnableForeignKeys issues PRAGMA foreign_keys = ON and verifies the pragma is
// active on the pool.
//
// Since #2272 the AUTHORITATIVE mechanism for FK enforcement on the runtime
// pool is the foreign_keys(1) DSN pragma set by OpenRuntime, which applies to
// every (including recycled) connection. EnableForeignKeys is retained as a
// startup SELF-CHECK: production calls it once on the runtime handle to confirm
// FK enforcement is genuinely active and fail loudly if not (a defense against
// a driver/DSN regression). It is no longer the sole mechanism, and it is NOT a
// substitute for the DSN pragma -- the PRAGMA it issues only affects the
// connection(s) currently checked out, whereas the DSN pragma covers every
// connection the pool later opens. Tests that want cascade semantics on an
// Open (FK-off) handle may still call it, but such handles are single-writer so
// the effect holds for their lifetime.
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
