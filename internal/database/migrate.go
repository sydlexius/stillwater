package database

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"strings"

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

	// legacy code paths (or sessions without
	// PRAGMA foreign_keys=ON) left orphan rows in artist_platform_ids whose
	// artist_id or connection_id no longer exist. Sweep them at startup so the
	// invariant the FK declarations imply actually holds. Idempotent.
	if err := cleanupOrphanArtistPlatformIDs(db); err != nil {
		return fmt.Errorf("cleaning orphan artist_platform_ids: %w", err)
	}

	// dedupe duplicate artist rows mapping to the same
	// (connection_id, platform_artist_id) pair before enforcing the unique
	// index. Goose only ever runs 001 once per DB, so the index added in
	// 001_initial_schema.sql does not appear on databases that already
	// recorded 001 as applied; create it idempotently here too.
	if err := ensureArtistPlatformIDsUnique(db); err != nil {
		return fmt.Errorf("ensuring artist_platform_ids unique constraint: %w", err)
	}

	// rule_results.violation_id FK is ON DELETE SET
	// NULL, so every rule_violations DELETE scans rule_results without an
	// index. Now that foreign key enforcement is actually on, that scan
	// shows up in the query-plan regression test. Create the index
	// idempotently for databases that already recorded 001 as applied.
	if _, err := db.ExecContext(context.Background(),
		`CREATE INDEX IF NOT EXISTS idx_rule_results_violation_id ON rule_results(violation_id)`); err != nil {
		return fmt.Errorf("ensuring rule_results.violation_id index: %w", err)
	}

	// seed rule_results rows for every open / pending_choice
	// violation already in the database. Without this, pre-existing
	// violations would have a missing first_failed_at until the next
	// pipeline pass re-evaluated the artist, losing the "how long has
	// this been broken" signal. INSERT OR IGNORE makes this idempotent
	// on every startup: once a (artist_id, rule_id) row exists, repeat
	// runs are cheap no-ops.
	if err := backfillRuleResultsFromViolations(db); err != nil {
		return fmt.Errorf("backfilling rule_results from violations: %w", err)
	}

	// artists become M:N with libraries via artist_libraries.
	// On fresh installs this only ensures the table exists (001 already has
	// it). On pre-1004 DBs this also backfills memberships from the orphan
	// artists.library_id column and collapses legacy duplicate artist rows
	// (one canonical row per real-world artist, with the loser library_ids
	// folded into artist_libraries memberships). Idempotent: on second
	// startup the helper finds no work to do.
	if err := ensureArtistLibrariesMembership(db); err != nil {
		return fmt.Errorf("ensuring artist_libraries membership: %w", err)
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
// Earlier audits caught real orphans in the wild -- presumably from a delete path that
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

// ensureArtistPlatformIDsUnique collapses duplicate platform mapping rows
// that share the same (connection_id, platform_artist_id) pair, then creates
// the UNIQUE index that prevents future duplicates. documented
// the scenario: scanner and import paths could each create a mapping row for
// the same Emby/Jellyfin item, race to claim the platform id, and produce
// inconsistent server-side lock state.
//
// Dedup strategy: group by (connection_id, platform_artist_id), pick the
// keeper artist by most recent updated_at, and delete only the losing
// artist_platform_ids rows. The losing artist rows themselves are left
// intact: legacy duplicates are not always true duplicate artists (a
// connection-library artist and a filesystem/manual artist could
// temporarily share a mapping), so cascade-deleting an artist row would
// silently remove its images, rule state, and library association.
// Whoever needs to reconcile the surviving artist rows can do so with
// real visibility, after this helper has resolved the unique-key conflict.
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

		// Delete only the conflicting mapping rows. The losing artist rows
		// remain in place; collapsing the mapping is enough to satisfy the
		// new unique index, and we avoid the silent data loss that whole-
		// artist deletion would cause when legacy duplicates turn out to
		// be distinct artists that briefly shared a platform id.
		res, err := db.ExecContext(ctx, `
			DELETE FROM artist_platform_ids
			WHERE connection_id = ? AND platform_artist_id = ? AND artist_id != ?
		`, d.connID, d.platformID, keeperID)
		if err != nil {
			return fmt.Errorf("deleting duplicate platform mappings for (%s, %s): %w", d.connID, d.platformID, err)
		}
		n, _ := res.RowsAffected()
		logger.Info(
			"collapsed duplicate platform mapping rows",
			"connection_id", d.connID,
			"platform_artist_id", d.platformID,
			"keeper_artist_id", keeperID,
			"deleted_mappings", n,
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

// ensureArtistLibrariesMembership ensures the artist_libraries table exists,
// backfills memberships from the legacy artists.library_id column on pre-1004
// DBs, and collapses any duplicate artist rows produced by the old
// per-library-scoped scanner. Idempotent: on a fresh install or already-
// migrated DB it is a fast no-op.
//
// The collapse step picks a canonical artist per duplicate group as follows:
// 1. Group by MusicBrainz ID when present (strongest identity).
// 2. Group by LOWER(name) for rows without an MBID, excluding any artist
// already claimed by an MBID group.
// 3. Within each group, prefer the row whose library is filesystem-sourced
// (or whose library_id is null and so represents the canonical
// filesystem-only path); tie-break by oldest created_at, then artist id.
//
// Re-pointing FK rows to the canonical artist uses INSERT OR IGNORE for
// composite-PK / unique-indexed tables (the canonical row's existing entry
// wins) and UPDATE OR IGNORE for single-PK tables with secondary uniques.
// Loser artist rows are then deleted; ON DELETE CASCADE cleans up any FK
// rows that did not get re-pointed (i.e. duplicates that conflicted with the
// canonical's existing data).
func ensureArtistLibrariesMembership(db *sql.DB) error {
	ctx := context.Background()
	logger := slog.Default().With("component", "database")

	// Step 1: idempotent table + index. 001 has these for fresh installs;
	// pre-1004 DBs need them at startup. The CHECK constraint includes
	// 'lidarr' (later addition); rebuildArtistLibrariesIfStaleCheck
	// detects an old shape (no lidarr) and rewrites the table in place.
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS artist_libraries (
			artist_id TEXT NOT NULL REFERENCES artists(id) ON DELETE CASCADE,
			library_id TEXT NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
			source TEXT NOT NULL CHECK (source IN ('filesystem','emby','jellyfin','lidarr','manual')),
			added_at TEXT NOT NULL DEFAULT (datetime('now')),
			PRIMARY KEY (artist_id, library_id)
		)
	`); err != nil {
		return fmt.Errorf("ensuring artist_libraries table: %w", err)
	}
	if err := rebuildArtistLibrariesIfStaleCheck(ctx, db); err != nil {
		return fmt.Errorf("rebuilding artist_libraries with current CHECK: %w", err)
	}
	if _, err := db.ExecContext(ctx, `
		CREATE INDEX IF NOT EXISTS idx_artist_libraries_library
		ON artist_libraries(library_id)
	`); err != nil {
		return fmt.Errorf("ensuring artist_libraries index: %w", err)
	}

	// Step 2: backfill memberships from the orphan artists.library_id column.
	// Fresh installs skip silently because the column does not exist.
	hasOrphan, err := columnExists(db, "artists", "library_id")
	if err != nil {
		return fmt.Errorf("checking artists.library_id presence: %w", err)
	}
	if !hasOrphan {
		return nil
	}

	res, err := db.ExecContext(ctx, `
		INSERT OR IGNORE INTO artist_libraries (artist_id, library_id, source, added_at)
		SELECT
			a.id,
			a.library_id,
			CASE
				WHEN c.type = 'emby' THEN 'emby'
				WHEN c.type = 'jellyfin' THEN 'jellyfin'
				WHEN c.type = 'lidarr' THEN 'lidarr'
				ELSE 'filesystem'
			END,
			a.created_at
		FROM artists a
		JOIN libraries l ON l.id = a.library_id
		LEFT JOIN connections c ON c.id = l.connection_id
		WHERE a.library_id IS NOT NULL AND a.library_id != ''
	`)
	if err != nil {
		return fmt.Errorf("backfilling artist_libraries from orphan library_id: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		logger.Info("backfilled artist_libraries memberships from legacy library_id",
			"rows", n)
	}

	// Step 3: collapse legacy duplicate artists.
	return collapseDuplicateArtists(ctx, db, logger)
}

// collapseGroup describes a duplicate artist group: one canonical row that
// keeps its identity, and one or more loser rows whose FK children get
// re-pointed at the canonical before the losers are deleted.
type collapseGroup struct {
	canonicalID string
	loserIDs    []string
}

// collapseDuplicateArtists finds duplicate artist groups by MBID then by
// case-insensitive name and collapses them per the rules in
// ensureArtistLibrariesMembership. The whole collapse runs in a single
// transaction so a partial failure rolls back to a consistent state.
func collapseDuplicateArtists(ctx context.Context, db *sql.DB, logger *slog.Logger) error {
	mbidGroups, err := findDuplicateGroupsByMBID(ctx, db)
	if err != nil {
		return fmt.Errorf("finding mbid duplicates: %w", err)
	}

	claimed := make(map[string]bool, len(mbidGroups)*2)
	for _, g := range mbidGroups {
		claimed[g.canonicalID] = true
		for _, l := range g.loserIDs {
			claimed[l] = true
		}
	}

	nameGroups, err := findDuplicateGroupsByName(ctx, db, claimed)
	if err != nil {
		return fmt.Errorf("finding name duplicates: %w", err)
	}

	allGroups := append(mbidGroups, nameGroups...)
	if len(allGroups) == 0 {
		return nil
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin collapse tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	var totalRepointed, totalRemoved int64
	for _, g := range allGroups {
		repointed, err := repointArtistFKs(ctx, tx, g.canonicalID, g.loserIDs)
		if err != nil {
			return fmt.Errorf("re-pointing canonical=%s: %w", g.canonicalID, err)
		}
		totalRepointed += repointed

		// Carry the losers' library memberships onto the canonical artist.
		// This catches the case where a loser had a library that the
		// canonical did not yet know about (the typical Emby+Jellyfin
		// duplicate scenario).
		//
		// Two sources are unioned: (1) any rows already materialized in
		// artist_libraries for the losers (the M:N source of truth), and
		// (2) the legacy artists.library_id column for losers that have
		// not been backfilled into artist_libraries yet. Without (1) a
		// loser with multiple memberships would only carry one of them.
		placeholders, inArgs := buildInList(g.loserIDs)
		args := []any{g.canonicalID}
		args = append(args, inArgs...)
		args = append(args, g.canonicalID)
		args = append(args, inArgs...)
		//nolint:gosec // G201: placeholders is a "?,?,..." literal built by buildInList from a known-length loop
		insertSQL := fmt.Sprintf(`
			INSERT OR IGNORE INTO artist_libraries (artist_id, library_id, source, added_at)
			SELECT
				?,
				al.library_id,
				al.source,
				al.added_at
			FROM artist_libraries al
			WHERE al.artist_id IN (%s)
			UNION ALL
			SELECT
				?,
				a.library_id,
				CASE
					WHEN c.type = 'emby' THEN 'emby'
					WHEN c.type = 'jellyfin' THEN 'jellyfin'
					WHEN c.type = 'lidarr' THEN 'lidarr'
					ELSE 'filesystem'
				END,
				a.created_at
			FROM artists a
			JOIN libraries l ON l.id = a.library_id
			LEFT JOIN connections c ON c.id = l.connection_id
			WHERE a.id IN (%s)
			 AND a.library_id IS NOT NULL AND a.library_id != ''
		`, placeholders, placeholders)
		if _, err := tx.ExecContext(ctx, insertSQL, args...); err != nil {
			return fmt.Errorf("inserting loser memberships under canonical=%s: %w",
				g.canonicalID, err)
		}

		// Explicitly clean any FK children still owned by losers before the
		// parent delete. The OR-IGNORE re-point steps above intentionally
		// leave loser-side rows behind for slots the canonical already
		// claimed; those rows would normally be reaped by ON DELETE
		// CASCADE when the loser is deleted, but database.Migrate may run
		// before EnableForeignKeys (e.g. some test setups), in which case
		// the cascade does not fire and the rows survive as orphans. The
		// explicit DELETE is correct under both pragma states.
		delPh, delArgs := buildInList(g.loserIDs)
		childTables := []string{
			"artist_libraries",
			"artist_provider_ids",
			"artist_platform_ids",
			"artist_images",
			"mb_snapshots",
			"rule_results",
			"rule_violations",
			"artist_aliases",
			"band_members",
			"nfo_snapshots",
			"metadata_changes",
		}
		for _, t := range childTables {
			childSQL := fmt.Sprintf(`DELETE FROM %s WHERE artist_id IN (%s)`, t, delPh) //nolint:gosec // G201: t is a hard-coded literal, delPh is a "?,?,..." literal
			if _, err := tx.ExecContext(ctx, childSQL, delArgs...); err != nil {
				return fmt.Errorf("cleaning %s for losers under canonical=%s: %w",
					t, g.canonicalID, err)
			}
		}

		// Delete losers. With FKs ON, ON DELETE CASCADE would also reap
		// the children above (idempotent); with FKs OFF, the explicit
		// cleanup above is what kept the schema consistent.
		deleteSQL := fmt.Sprintf(`DELETE FROM artists WHERE id IN (%s)`, delPh) //nolint:gosec // G201: delPh is a "?,?,..." literal
		delRes, err := tx.ExecContext(ctx, deleteSQL, delArgs...)
		if err != nil {
			return fmt.Errorf("deleting losers under canonical=%s: %w",
				g.canonicalID, err)
		}
		n, _ := delRes.RowsAffected()
		totalRemoved += n
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit collapse tx: %w", err)
	}

	logger.Info("collapsed duplicate artist rows",
		"groups", len(allGroups),
		"repointed_rows", totalRepointed,
		"removed_artists", totalRemoved,
	)
	return nil
}

// pickCanonicalCTE produces the CASE expression that ranks rows within a
// duplicate group. Lower rank wins. Filesystem-source rows rank highest:
// either the artist has no library (legacy data) or its library is not
// attached to a connection (manual filesystem library). Connection-backed
// rows (Emby, Jellyfin, ...) fall back. The MusicBrainz ID lives on
// artist_provider_ids, so we LEFT JOIN it here for the MBID grouping query.
// The CTE is reused by both group finders so the canonical-pick rule is
// identical.
//
// created_at is normalized via datetime() because the artists table has
// a mix of formats in the wild (SQLite "YYYY-MM-DD HH:MM:SS" written by
// older code, RFC3339 "T"-separated strings written by newer Go callers).
// Lexical TEXT ordering would otherwise pick the wrong winner whenever
// the formats mix in the same duplicate group.
const pickCanonicalCTE = `
WITH ranked AS (
	SELECT
		a.id,
		COALESCE(ap.provider_id, '') AS mbid,
		a.name,
		datetime(a.created_at) AS created_at_norm,
		CASE
			WHEN a.library_id IS NULL OR a.library_id = '' THEN 0
			WHEN c.type IS NULL THEN 1
			ELSE 2
		END AS source_rank
	FROM artists a
	LEFT JOIN artist_provider_ids ap
		ON ap.artist_id = a.id AND ap.provider = 'musicbrainz'
	LEFT JOIN libraries l ON l.id = a.library_id
	LEFT JOIN connections c ON c.id = l.connection_id
)
`

// findDuplicateGroupsByMBID returns one collapseGroup per MBID that has more
// than one artist row. Within each group the canonical is the lowest
// (source_rank, created_at, id) row.
func findDuplicateGroupsByMBID(ctx context.Context, db *sql.DB) ([]collapseGroup, error) {
	rows, err := db.QueryContext(ctx, pickCanonicalCTE+`
		SELECT id, mbid, source_rank, created_at_norm
		FROM ranked
		WHERE mbid != ''
		 AND mbid IN (
			SELECT mbid FROM ranked
			WHERE mbid != ''
			GROUP BY mbid HAVING COUNT(*) > 1
		)
		ORDER BY mbid, source_rank, created_at_norm, id
	`)
	if err != nil {
		return nil, fmt.Errorf("querying mbid duplicate groups: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	groups := []collapseGroup{}
	var current *collapseGroup
	var currentKey string
	for rows.Next() {
		var id, mbid, createdAt string
		var rank int
		if err := rows.Scan(&id, &mbid, &rank, &createdAt); err != nil {
			return nil, fmt.Errorf("scanning mbid duplicate row: %w", err)
		}
		if mbid != currentKey {
			currentKey = mbid
			groups = append(groups, collapseGroup{canonicalID: id})
			current = &groups[len(groups)-1]
			continue
		}
		current.loserIDs = append(current.loserIDs, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating mbid duplicate rows: %w", err)
	}
	return groups, nil
}

// findDuplicateGroupsByName returns collapseGroups for case-insensitive name
// duplicates, excluding any artist already claimed by an MBID group. This
// catches duplicates that exist because one or both rows lack an MBID (typical
// for filesystem-only artists that were also imported from Emby/Jellyfin).
func findDuplicateGroupsByName(ctx context.Context, db *sql.DB, claimed map[string]bool) ([]collapseGroup, error) {
	rows, err := db.QueryContext(ctx, pickCanonicalCTE+`
		SELECT id, name, source_rank, created_at_norm
		FROM ranked
		WHERE LOWER(name) IN (
			SELECT LOWER(name) FROM ranked
			GROUP BY LOWER(name) HAVING COUNT(*) > 1
		)
		ORDER BY LOWER(name), source_rank, created_at_norm, id
	`)
	if err != nil {
		return nil, fmt.Errorf("querying name duplicate groups: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	type rawRow struct {
		id, name string
	}
	byName := map[string][]rawRow{}
	for rows.Next() {
		var id, name, createdAt string
		var rank int
		if err := rows.Scan(&id, &name, &rank, &createdAt); err != nil {
			return nil, fmt.Errorf("scanning name duplicate row: %w", err)
		}
		key := name
		// LOWER for grouping; preserve original case in the row for logging.
		// (SQLite's GROUP BY in the subquery uses LOWER, so we re-key here.)
		// We could push LOWER(name) into the SELECT, but storing the
		// original-case name keeps the slog line readable.
		_ = key
		k := lowercaseASCII(name)
		byName[k] = append(byName[k], rawRow{id: id, name: name})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating name duplicate rows: %w", err)
	}

	groups := []collapseGroup{}
	for _, rs := range byName {
		// Drop rows already in an MBID group; if fewer than two remain there
		// is no duplicate left to collapse for this name.
		filtered := rs[:0]
		for _, r := range rs {
			if !claimed[r.id] {
				filtered = append(filtered, r)
			}
		}
		if len(filtered) < 2 {
			continue
		}
		// First filtered row is canonical (rows already arrive sorted by
		// source_rank, created_at, id from the ORDER BY above).
		g := collapseGroup{canonicalID: filtered[0].id}
		for _, r := range filtered[1:] {
			g.loserIDs = append(g.loserIDs, r.id)
		}
		groups = append(groups, g)
	}
	return groups, nil
}

// repointArtistFKs moves every child row owned by losers to canonical, with
// per-table conflict resolution. Returns the total number of rows actually
// re-pointed (informational; rows that conflicted with the canonical's
// existing data are dropped on cascade when losers are deleted).
//
// Tables with composite PK (artist_id, X) get INSERT OR IGNORE: the
// canonical's existing row wins for any (canonical, X) collision; loser rows
// for keys the canonical does not have are absorbed.
//
// Tables with single-id PK and a secondary UNIQUE on (artist_id, ...) get
// UPDATE OR IGNORE: rows that would create a unique-violation when re-keyed
// to canonical are silently dropped (canonical already has data for that
// slot and we keep canonical's).
//
// Tables with single-id PK and no other uniques get a plain UPDATE.
func repointArtistFKs(ctx context.Context, tx *sql.Tx, canonical string, losers []string) (int64, error) {
	loserPh, loserArgs := buildInList(losers)
	var total int64

	// Composite-PK tables without secondary uniques: INSERT OR IGNORE the
	// loser rows under canonical. (artist_id, X) PK is the only conflict
	// surface, so canonical's existing rows win and loser's surplus rows
	// are absorbed. We list columns explicitly because each schema differs.
	//
	// artist_platform_ids is intentionally NOT in this group: it carries an
	// extra UNIQUE (connection_id, platform_artist_id) that would cause
	// INSERT OR IGNORE to drop every loser row whose platform mapping is
	// already claimed by the loser itself, then the cascade-delete of the
	// loser would lose the mapping entirely. It uses UPDATE OR IGNORE
	// below, which moves the loser's row onto canonical (preserving the
	// platform mapping) and only drops the row if canonical already has
	// its own mapping for the same connection.
	insertOrIgnore := []struct {
		table   string
		columns string
		selectX string // "X" portion of the SELECT after the canonical id
	}{
		{
			"artist_provider_ids",
			"(artist_id, provider, provider_id, fetched_at)",
			"provider, provider_id, fetched_at",
		},
		{
			"rule_results",
			"(artist_id, rule_id, passed, violation_id, evaluated_at, violation_message, first_failed_at)",
			"rule_id, passed, violation_id, evaluated_at, violation_message, first_failed_at",
		},
	}
	for _, t := range insertOrIgnore {
		args := append([]any{canonical}, loserArgs...)
		stmt := fmt.Sprintf( //nolint:gosec // G201: table/columns/loserPh are hard-coded literals or static placeholder strings
			`INSERT OR IGNORE INTO %s %s SELECT ?, %s FROM %s WHERE artist_id IN (%s)`,
			t.table, t.columns, t.selectX, t.table, loserPh,
		)
		res, err := tx.ExecContext(ctx, stmt, args...)
		if err != nil {
			return total, fmt.Errorf("re-pointing %s: %w", t.table, err)
		}
		n, _ := res.RowsAffected()
		total += n
	}

	// Tables with at least one UNIQUE constraint that includes artist_id:
	// UPDATE OR IGNORE moves the loser row's artist_id to canonical, and
	// silently drops the row when the move would conflict with a row
	// canonical already owns for the same slot. Whatever loser-side rows
	// remain after this UPDATE are removed by the cascade when the loser
	// artist is deleted.
	//
	// artist_platform_ids is here (not in insertOrIgnore) because of the
	// UNIQUE (connection_id, platform_artist_id) added in a prior change.
	// Moving the loser's row onto canonical preserves the platform mapping
	// when canonical does not yet have one for the same connection.
	updateOrIgnore := []string{
		"artist_images",       // UNIQUE (artist_id, image_type, slot_index)
		"mb_snapshots",        // UNIQUE (artist_id, field)
		"rule_violations",     // UNIQUE (rule_id, artist_id)
		"artist_platform_ids", // PK (artist_id, connection_id) + UNIQUE (connection_id, platform_artist_id)
	}
	for _, t := range updateOrIgnore {
		args := append([]any{canonical}, loserArgs...)
		stmt := fmt.Sprintf( //nolint:gosec // G201: table is a hard-coded literal
			`UPDATE OR IGNORE %s SET artist_id = ? WHERE artist_id IN (%s)`,
			t, loserPh,
		)
		res, err := tx.ExecContext(ctx, stmt, args...)
		if err != nil {
			return total, fmt.Errorf("re-pointing %s: %w", t, err)
		}
		n, _ := res.RowsAffected()
		total += n
	}

	// Plain UPDATE: no secondary uniques, all loser rows survive.
	plainUpdate := []string{
		"artist_aliases",
		"band_members",
		"nfo_snapshots",
		"metadata_changes",
	}
	for _, t := range plainUpdate {
		args := append([]any{canonical}, loserArgs...)
		stmt := fmt.Sprintf( //nolint:gosec // G201: table is a hard-coded literal
			`UPDATE %s SET artist_id = ? WHERE artist_id IN (%s)`,
			t, loserPh,
		)
		res, err := tx.ExecContext(ctx, stmt, args...)
		if err != nil {
			return total, fmt.Errorf("re-pointing %s: %w", t, err)
		}
		n, _ := res.RowsAffected()
		total += n
	}

	return total, nil
}

// rebuildArtistLibrariesIfStaleCheck detects pre-existing artist_libraries
// tables whose CHECK constraint predates the addition of 'lidarr' as a
// permitted source value and rewrites them in place.
// SQLite does not support ALTER ... DROP CHECK, so we do the standard
// rebuild dance: create a temp table with the current shape, copy data
// across, drop the old, rename. Idempotent: when the existing CHECK
// already matches, this is a fast no-op.
func rebuildArtistLibrariesIfStaleCheck(ctx context.Context, db *sql.DB) error {
	var sqlText sql.NullString
	if err := db.QueryRowContext(ctx,
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='artist_libraries'`).Scan(&sqlText); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("reading artist_libraries CREATE: %w", err)
	}
	if !sqlText.Valid || strings.Contains(sqlText.String, "'lidarr'") {
		return nil
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin rebuild tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE artist_libraries_new (
			artist_id TEXT NOT NULL REFERENCES artists(id) ON DELETE CASCADE,
			library_id TEXT NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
			source TEXT NOT NULL CHECK (source IN ('filesystem','emby','jellyfin','lidarr','manual')),
			added_at TEXT NOT NULL DEFAULT (datetime('now')),
			PRIMARY KEY (artist_id, library_id)
		)
	`); err != nil {
		return fmt.Errorf("creating artist_libraries_new: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO artist_libraries_new (artist_id, library_id, source, added_at)
		SELECT artist_id, library_id, source, added_at FROM artist_libraries
	`); err != nil {
		return fmt.Errorf("copying artist_libraries rows: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE artist_libraries`); err != nil {
		return fmt.Errorf("dropping old artist_libraries: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE artist_libraries_new RENAME TO artist_libraries`); err != nil {
		return fmt.Errorf("renaming artist_libraries_new: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit rebuild: %w", err)
	}
	slog.Default().With("component", "database").Info(
		"rebuilt artist_libraries table to refresh CHECK constraint")
	return nil
}

// columnExists returns true if the named column is present on the table.
// Used by ensureArtistLibrariesMembership to decide whether to backfill from
// the orphan artists.library_id column (present on pre-1004 DBs only).
func columnExists(db *sql.DB, table, column string) (bool, error) {
	rows, err := db.QueryContext(context.Background(),
		fmt.Sprintf("PRAGMA table_info(%s)", table)) //nolint:gosec // G201: table is a hard-coded literal
	if err != nil {
		return false, fmt.Errorf("reading %s schema: %w", table, err)
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
			return false, fmt.Errorf("scanning %s schema row: %w", table, err)
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

// buildInList returns ("?,?,?", []any{ids...}) for use in WHERE x IN (...).
// Returns an empty-string SQL literal when the input is empty so the IN
// clause stays valid (matches nothing instead of producing a syntax error).
func buildInList(ids []string) (string, []any) {
	if len(ids) == 0 {
		return "''", nil
	}
	placeholders := make([]byte, 0, len(ids)*2)
	args := make([]any, 0, len(ids))
	for i, id := range ids {
		if i > 0 {
			placeholders = append(placeholders, ',')
		}
		placeholders = append(placeholders, '?')
		args = append(args, id)
	}
	return string(placeholders), args
}

// lowercaseASCII lowercases a string using ASCII semantics. Matches SQLite's
// default LOWER() behavior for the ASCII range, which is what the duplicate
// detection grouping uses. Non-ASCII bytes pass through unchanged in both
// SQLite and here, so the grouping stays consistent.
func lowercaseASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
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
