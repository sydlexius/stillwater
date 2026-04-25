// Command uat-1004 seeds a Stillwater SQLite database with a synthetic
// duplicate-artist topology for hand UAT of issue #1004 (M:N artist-libraries
// rework). Usage:
//
//	go run ./scripts/uat-1004 /tmp/stillwater-uat-1004.db
//
// What the seed builds:
//   - One filesystem library, one Emby connection-library, one Jellyfin
//     connection-library, all pointing at /tmp paths (no real media required).
//   - A duplicate-by-MBID artist "12 Stones" with three rows: filesystem,
//     Emby, and Jellyfin. After the next Stillwater startup the migration
//     should collapse them into one row whose canonical is the filesystem
//     row, with all three library memberships and both platform mappings.
//   - A duplicate-by-name artist "Veridia" / "VERIDIA" without MBIDs across
//     filesystem and Jellyfin. Should collapse to one row.
//   - A clean control artist "Skillet" present only in the Emby library.
//     Should remain as a single row with one library membership.
//
// After running the seed, start Stillwater pointing at the same DB:
//
//	STILLWATER_DB=/tmp/stillwater-uat-1004.db ./stillwater
//
// On startup the migration runs and emits a slog line like:
//
//	collapsed duplicate artist rows (issue #1004) groups=2 ...
//
// Then open http://localhost:1973/artists and verify one row per artist.
//
// Re-running the seed against the same path overwrites the file.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"

	"github.com/sydlexius/stillwater/internal/database"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: uat-1004 <db-path>")
		os.Exit(2)
	}
	path := os.Args[1]
	if err := run(path); err != nil {
		slog.Error("seed failed", "error", err)
		os.Exit(1)
	}
}

func run(path string) error {
	// Start clean so reruns are deterministic. path is provided by the
	// developer as a command-line argument; this is a UAT helper, not a
	// production code path.
	_ = os.Remove(path) //nolint:gosec // G304: path is a developer-supplied UAT-only argument

	db, err := database.Open(path)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer db.Close() //nolint:errcheck

	if err := database.Migrate(db); err != nil {
		return fmt.Errorf("initial migrate: %w", err)
	}
	if err := database.EnableForeignKeys(db); err != nil {
		return fmt.Errorf("foreign keys: %w", err)
	}

	ctx := context.Background()
	if err := seed(ctx, db); err != nil {
		return fmt.Errorf("seed: %w", err)
	}

	if err := report(ctx, db); err != nil {
		return fmt.Errorf("report: %w", err)
	}

	fmt.Fprintf(os.Stderr, "\nNext: start Stillwater with this DB so the issue #1004 migration collapses the duplicates:\n")
	fmt.Fprintf(os.Stderr, "  STILLWATER_DB=%s bash scripts/dev-restart.sh\n", path)
	fmt.Fprintf(os.Stderr, "Then open http://localhost:1973/artists and verify one row per artist.\n")
	return nil
}

func seed(ctx context.Context, db *sql.DB) error {
	stmts := []string{
		// Connections.
		`INSERT INTO connections (id, name, type, url, encrypted_api_key, enabled, status, created_at, updated_at)
		 VALUES ('conn-emby', 'Emby (UAT)', 'emby', 'http://uat-emby.local',
		         'k', 1, 'ok', datetime('now'), datetime('now'))`,
		`INSERT INTO connections (id, name, type, url, encrypted_api_key, enabled, status, created_at, updated_at)
		 VALUES ('conn-jelly', 'Jellyfin (UAT)', 'jellyfin', 'http://uat-jellyfin.local',
		         'k', 1, 'ok', datetime('now'), datetime('now'))`,

		// Libraries: one filesystem, one each per connection.
		`INSERT INTO libraries (id, name, path, type, source, created_at, updated_at)
		 VALUES ('lib-fs', 'Filesystem Music', '/tmp/uat-music', 'regular', 'filesystem',
		         datetime('now'), datetime('now'))`,
		`INSERT INTO libraries (id, name, path, type, source, connection_id, external_id, created_at, updated_at)
		 VALUES ('lib-emby', 'Emby Music', '/tmp/uat-music', 'regular', 'import',
		         'conn-emby', 'emby-ext', datetime('now'), datetime('now'))`,
		`INSERT INTO libraries (id, name, path, type, source, connection_id, external_id, created_at, updated_at)
		 VALUES ('lib-jelly', 'Jellyfin Music', '/tmp/uat-music', 'regular', 'import',
		         'conn-jelly', 'jelly-ext', datetime('now'), datetime('now'))`,

		// MBID-grouped trio: 12 Stones across all three libraries with
		// the same MBID. Collapse should pick the filesystem row as
		// canonical and re-point everything onto it.
		`INSERT INTO artists (id, name, sort_name, path, library_id, biography, created_at, updated_at)
		 VALUES ('a-12s-fs', '12 Stones', '12 Stones', '/tmp/uat-music/12 Stones',
		         'lib-fs', 'American rock band (filesystem row).',
		         '2026-01-01T00:00:00Z', datetime('now'))`,
		`INSERT INTO artist_provider_ids (artist_id, provider, provider_id, fetched_at)
		 VALUES ('a-12s-fs', 'musicbrainz', 'mbid-12-stones', datetime('now'))`,

		`INSERT INTO artists (id, name, sort_name, path, library_id, biography, created_at, updated_at)
		 VALUES ('a-12s-emby', '12 Stones', '12 Stones', '/tmp/uat-music/12 Stones',
		         'lib-emby', 'American rock band (Emby row).',
		         '2026-01-02T00:00:00Z', datetime('now'))`,
		`INSERT INTO artist_provider_ids (artist_id, provider, provider_id, fetched_at)
		 VALUES ('a-12s-emby', 'musicbrainz', 'mbid-12-stones', datetime('now'))`,
		`INSERT INTO artist_platform_ids (artist_id, connection_id, platform_artist_id, created_at, updated_at)
		 VALUES ('a-12s-emby', 'conn-emby', 'emby-12-stones-id',
		         strftime('%Y-%m-%dT%H:%M:%SZ', 'now'), strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))`,

		`INSERT INTO artists (id, name, sort_name, path, library_id, biography, created_at, updated_at)
		 VALUES ('a-12s-jelly', '12 Stones', '12 Stones', '/tmp/uat-music/12 Stones',
		         'lib-jelly', 'American rock band (Jellyfin row).',
		         '2026-01-03T00:00:00Z', datetime('now'))`,
		`INSERT INTO artist_provider_ids (artist_id, provider, provider_id, fetched_at)
		 VALUES ('a-12s-jelly', 'musicbrainz', 'mbid-12-stones', datetime('now'))`,
		`INSERT INTO artist_platform_ids (artist_id, connection_id, platform_artist_id, created_at, updated_at)
		 VALUES ('a-12s-jelly', 'conn-jelly', 'jelly-12-stones-id',
		         strftime('%Y-%m-%dT%H:%M:%SZ', 'now'), strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))`,

		// Name-grouped pair: VERIDIA without MBIDs, case-insensitive
		// match across filesystem and Jellyfin. Filesystem wins canonical.
		`INSERT INTO artists (id, name, sort_name, path, library_id, biography, created_at, updated_at)
		 VALUES ('a-veridia-fs', 'Veridia', 'Veridia', '/tmp/uat-music/Veridia',
		         'lib-fs', 'American rock band (filesystem row, mixed case).',
		         '2026-01-04T00:00:00Z', datetime('now'))`,
		`INSERT INTO artists (id, name, sort_name, path, library_id, biography, created_at, updated_at)
		 VALUES ('a-veridia-jelly', 'VERIDIA', 'VERIDIA', '/tmp/uat-music/VERIDIA',
		         'lib-jelly', 'American rock band (Jellyfin row, all caps).',
		         '2026-01-05T00:00:00Z', datetime('now'))`,

		// Clean control: Skillet exists only in the Emby library; should
		// remain as one row with a single Emby membership.
		`INSERT INTO artists (id, name, sort_name, path, library_id, biography, created_at, updated_at)
		 VALUES ('a-skillet-emby', 'Skillet', 'Skillet', '/tmp/uat-music/Skillet',
		         'lib-emby', 'Christian rock band, Emby-only control.',
		         '2026-01-06T00:00:00Z', datetime('now'))`,
		`INSERT INTO artist_platform_ids (artist_id, connection_id, platform_artist_id, created_at, updated_at)
		 VALUES ('a-skillet-emby', 'conn-emby', 'emby-skillet-id',
		         strftime('%Y-%m-%dT%H:%M:%SZ', 'now'), strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))`,
	}
	for i, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("statement %d: %w\n%s", i, err, s)
		}
	}
	return nil
}

// report prints the pre-collapse counts so the user can compare against
// what the migration produces on the next Stillwater startup.
func report(ctx context.Context, db *sql.DB) error {
	var artists int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM artists`).Scan(&artists); err != nil {
		return err
	}
	var memberships int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM artist_libraries`).Scan(&memberships); err != nil {
		return err
	}
	var twelveStones int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artists WHERE name = '12 Stones'`).Scan(&twelveStones); err != nil {
		return err
	}
	var veridia int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artists WHERE LOWER(name) = 'veridia'`).Scan(&veridia); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "seeded:\n")
	fmt.Fprintf(os.Stderr, "  total artists:           %d (expect 6 pre-collapse, 3 post-collapse: 12 Stones, Veridia, Skillet)\n", artists)
	fmt.Fprintf(os.Stderr, "  artist_libraries rows:   %d (expect 0 pre-collapse, 4 post-collapse: 12s/fs, 12s/emby, 12s/jelly, veridia/fs, veridia/jelly, skillet/emby)\n", memberships)
	fmt.Fprintf(os.Stderr, "  '12 Stones' rows:        %d (expect 3 pre-collapse, 1 post-collapse)\n", twelveStones)
	fmt.Fprintf(os.Stderr, "  'Veridia' rows (case-i): %d (expect 2 pre-collapse, 1 post-collapse)\n", veridia)
	return nil
}
