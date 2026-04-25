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

	// Issue #1078: legacy code paths (or sessions without
	// PRAGMA foreign_keys=ON) left orphan rows in artist_platform_ids whose
	// artist_id or connection_id no longer exist. Sweep them at startup so the
	// invariant the FK declarations imply actually holds. Idempotent.
	if err := cleanupOrphanArtistPlatformIDs(db); err != nil {
		return fmt.Errorf("cleaning orphan artist_platform_ids: %w", err)
	}

	// Issue #1076: dedupe duplicate artist rows mapping to the same
	// (connection_id, platform_artist_id) pair before enforcing the unique
	// index. Goose only ever runs 001 once per DB, so the index added in
	// 001_initial_schema.sql does not appear on databases that already
	// recorded 001 as applied; create it idempotently here too.
	if err := ensureArtistPlatformIDsUnique(db); err != nil {
		return fmt.Errorf("ensuring artist_platform_ids unique constraint: %w", err)
	}

	// Issue #1078 follow-on: rule_results.violation_id FK is ON DELETE SET
	// NULL, so every rule_violations DELETE scans rule_results without an
	// index. Now that foreign key enforcement is actually on, that scan
	// shows up in the query-plan regression test. Create the index
	// idempotently for databases that already recorded 001 as applied.
	if _, err := db.ExecContext(context.Background(),
		`CREATE INDEX IF NOT EXISTS idx_rule_results_violation_id ON rule_results(violation_id)`); err != nil {
		return fmt.Errorf("ensuring rule_results.violation_id index: %w", err)
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
	if err := ensureColumn(db, "connections", "platform_server_id", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := ensureColumn(db, "connections", "feature_manage_server_files", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	return ensureColumn(db, "connections", "pre_stillwater_config_json", "TEXT NOT NULL DEFAULT ''")
}

// cleanupOrphanArtistPlatformIDs removes rows from artist_platform_ids whose
// artist_id or connection_id no longer reference an existing row. Such rows
// should be impossible given the ON DELETE CASCADE foreign keys, but issue
// #1078 caught real orphans in the wild -- presumably from a delete path that
// ran without PRAGMA foreign_keys=ON, or from raw SQL bypassing the
// repository. Running this on every startup is idempotent and cheap.
func cleanupOrphanArtistPlatformIDs(db *sql.DB) error {
	ctx := context.Background()
	res, err := db.ExecContext(ctx, `
		DELETE FROM artist_platform_ids
		WHERE artist_id NOT IN (SELECT id FROM artists)
		   OR connection_id NOT IN (SELECT id FROM connections)
	`)
	if err != nil {
		return fmt.Errorf("deleting orphan artist_platform_ids: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		slog.Default().With("component", "database").Info(
			"removed orphan artist_platform_ids rows",
			"count", n,
		)
	}
	return nil
}

// ensureArtistPlatformIDsUnique collapses duplicate artist rows that map to
// the same (connection_id, platform_artist_id) pair, then creates the
// UNIQUE index that prevents future duplicates. Issue #1076 documented this
// scenario: scanner and import paths could each create an artist row for the
// same on-disk directory after renames or re-scans, race to claim the same
// Emby/Jellyfin item, and produce inconsistent server-side lock state.
//
// Dedup strategy: group by (connection_id, platform_artist_id), keep the
// artist row with the most recent updated_at, and reassign the surviving
// platform-id row to that artist. The losing artist rows are deleted, which
// cascades to their platform_ids, images, violations, and rule_results.
//
// The index is created LAST so a partial dedup never hits a unique-constraint
// violation that aborts the whole startup. Re-running this on a clean DB is
// a no-op: the GROUP BY HAVING COUNT > 1 returns no rows, and the index
// already exists from 001_initial_schema.sql.
// dupPlatformKey is one (connection_id, platform_artist_id) pair that has
// more than one artist row mapped to it. Used by the dedup helper.
type dupPlatformKey struct {
	connID, platformID string
}

// collectDuplicatePlatformKeys returns the set of (connection_id,
// platform_artist_id) pairs that currently have more than one artist row
// mapped to them. Split out so the rows.Close() can use defer (sqlclosecheck).
func collectDuplicatePlatformKeys(ctx context.Context, db *sql.DB) ([]dupPlatformKey, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT connection_id, platform_artist_id
		FROM artist_platform_ids
		GROUP BY connection_id, platform_artist_id
		HAVING COUNT(*) > 1
	`)
	if err != nil {
		return nil, fmt.Errorf("scanning duplicate artist_platform_ids: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var dupes []dupPlatformKey
	for rows.Next() {
		var d dupPlatformKey
		if err := rows.Scan(&d.connID, &d.platformID); err != nil {
			return nil, fmt.Errorf("scanning duplicate row: %w", err)
		}
		dupes = append(dupes, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating duplicate rows: %w", err)
	}
	return dupes, nil
}

func ensureArtistPlatformIDsUnique(db *sql.DB) error {
	ctx := context.Background()
	logger := slog.Default().With("component", "database")

	// Find all (connection_id, platform_artist_id) tuples with more than one
	// artist row. For each, pick the keeper artist (most recent updated_at)
	// and delete the rest. The CASCADE FKs clean up children automatically.
	dupes, err := collectDuplicatePlatformKeys(ctx, db)
	if err != nil {
		return err
	}

	for _, d := range dupes {
		// Keep the artist with the latest updated_at; tie-break on artist id
		// so the choice is deterministic across runs.
		var keeperID string
		err := db.QueryRowContext(ctx, `
			SELECT a.id
			FROM artists a
			JOIN artist_platform_ids ap ON ap.artist_id = a.id
			WHERE ap.connection_id = ? AND ap.platform_artist_id = ?
			ORDER BY a.updated_at DESC, a.id ASC
			LIMIT 1
		`, d.connID, d.platformID).Scan(&keeperID)
		if err != nil {
			return fmt.Errorf("picking keeper for (%s, %s): %w", d.connID, d.platformID, err)
		}

		// Delete losing artist rows. CASCADE removes their child rows
		// (artist_platform_ids, artist_images, rule_violations, etc.).
		res, err := db.ExecContext(ctx, `
			DELETE FROM artists
			WHERE id IN (
				SELECT a.id
				FROM artists a
				JOIN artist_platform_ids ap ON ap.artist_id = a.id
				WHERE ap.connection_id = ? AND ap.platform_artist_id = ? AND a.id != ?
			)
		`, d.connID, d.platformID, keeperID)
		if err != nil {
			return fmt.Errorf("deleting duplicate artists for (%s, %s): %w", d.connID, d.platformID, err)
		}
		n, _ := res.RowsAffected()
		logger.Info(
			"collapsed duplicate artist rows mapping to the same platform id",
			"connection_id", d.connID,
			"platform_artist_id", d.platformID,
			"keeper_artist_id", keeperID,
			"deleted_count", n,
		)
	}

	// Create the unique index. CREATE UNIQUE INDEX IF NOT EXISTS is a no-op
	// if the index already exists (clean DBs running 001 today).
	if _, err := db.ExecContext(ctx, `
		CREATE UNIQUE INDEX IF NOT EXISTS idx_artist_platform_ids_unique
		ON artist_platform_ids(connection_id, platform_artist_id)
	`); err != nil {
		return fmt.Errorf("creating unique index on artist_platform_ids: %w", err)
	}
	return nil
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
