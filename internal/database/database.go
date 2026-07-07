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
// active on the connection it runs on. This is the TEST-ENABLE path: test
// fixtures call it on an Open (FK-off) handle to obtain cascade semantics for
// the handle's lifetime (Open handles are single-writer, so the mutated
// connection is the only one). It is NOT the runtime self-check -- because it
// MUTATES FK on before reading it back, it cannot detect a DSN/driver
// regression that fails to enforce FK on fresh connections (it would turn FK on
// itself and then observe it on). Use VerifyForeignKeys for the runtime
// self-check.
//
// Since #2272 the AUTHORITATIVE mechanism for FK enforcement on the runtime
// pool is the foreign_keys(1) DSN pragma set by OpenRuntime, which applies to
// every (including recycled) connection. EnableForeignKeys is NOT a substitute
// for the DSN pragma -- the PRAGMA it issues only affects the connection(s)
// currently checked out, whereas the DSN pragma covers every connection the
// pool later opens.
func EnableForeignKeys(db *sql.DB) error {
	ctx := context.Background()
	// Pin a SINGLE connection for both PRAGMA statements. PRAGMA foreign_keys is
	// per-connection, so enabling it on one pooled connection and reading it
	// back on another would be unreliable. The pool is single-writer via
	// SetMaxOpenConns(1), but pinning the connection makes this
	// correct-by-construction rather than reliant on that invariant.
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquiring connection to enable foreign keys: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys = ON"); err != nil {
		return fmt.Errorf("enabling foreign keys: %w", err)
	}
	var fkOn int
	if err := conn.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fkOn); err != nil {
		return fmt.Errorf("checking foreign_keys pragma: %w", err)
	}
	if fkOn != 1 {
		return fmt.Errorf("foreign_keys pragma not enabled (got %d); FK CASCADE will not fire", fkOn)
	}
	return nil
}

// VerifyForeignKeys is the runtime FK self-check. Unlike EnableForeignKeys it
// does NOT mutate: it acquires a connection from the pool (via db.Conn, which
// may be a newly opened or a reused idle connection) and reads PRAGMA
// foreign_keys on it WITHOUT first executing PRAGMA foreign_keys = ON. Because
// the OpenRuntime DSN sets foreign_keys(1), EVERY connection the pool opens is
// FK-on; a DSN/driver regression would instead leave them FK-off. Reading the
// state (rather than setting it first) is what lets this CATCH such a regression
// instead of masking it -- a mutating check would turn FK on itself and then
// always observe it on.
//
// It returns a clear error if FK enforcement is not active (value != 1),
// meaning the OpenRuntime DSN pragma is not enforcing foreign keys and
// ON DELETE CASCADE would not fire. Production calls this once on the runtime
// pool at startup so a regression fails loudly rather than silently orphaning
// child rows (see #2272).
func VerifyForeignKeys(db *sql.DB) error {
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquiring connection for foreign_keys self-check: %w", err)
	}
	defer func() { _ = conn.Close() }()

	var fkOn int
	if err := conn.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fkOn); err != nil {
		return fmt.Errorf("checking foreign_keys pragma: %w", err)
	}
	if fkOn != 1 {
		return fmt.Errorf("foreign_keys pragma not enforced on a pool connection (got %d); "+
			"the OpenRuntime DSN pragma is not enforcing foreign keys, so ON DELETE CASCADE will not fire", fkOn)
	}
	return nil
}
