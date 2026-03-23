package database

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestQueryPlans runs EXPLAIN QUERY PLAN on the most performance-critical
// SQL queries in the application to verify that indexes are being used and
// no full table scans occur on large tables.
//
// This test uses a real SQLite database with the full schema applied via
// migrations. It does not insert data -- EXPLAIN QUERY PLAN analyzes the
// query plan statically based on available indexes.
func TestQueryPlans(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create a temporary database with full schema
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test_query_plans.db")

	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	defer db.Close() //nolint:errcheck

	if err := Migrate(db); err != nil {
		t.Fatalf("running migrations: %v", err)
	}

	// criticalQueries maps a descriptive name to a SQL query.
	// Each query uses placeholder values for parameters.
	criticalQueries := map[string]struct {
		query string
		args  []any
	}{
		"artist_by_id": {
			query: "SELECT id, name FROM artists WHERE id = ?",
			args:  []any{"test-id"},
		},
		"artist_by_path": {
			query: "SELECT id, name FROM artists WHERE path = ?",
			args:  []any{"/music/test"},
		},
		"artist_by_name": {
			query: "SELECT id, name FROM artists WHERE name = ?",
			args:  []any{"Test Artist"},
		},
		"artist_list_by_library": {
			query: "SELECT id, name FROM artists WHERE library_id = ? ORDER BY name ASC LIMIT ? OFFSET ?",
			args:  []any{"lib-1", 50, 0},
		},
		"artist_list_with_search": {
			query: "SELECT id, name FROM artists WHERE name LIKE ? ORDER BY name ASC LIMIT ? OFFSET ?",
			args:  []any{"%test%", 50, 0},
		},
		"artist_count_by_library": {
			query: "SELECT COUNT(*) FROM artists WHERE library_id = ?",
			args:  []any{"lib-1"},
		},
		"artist_search": {
			query: "SELECT id, name FROM artists WHERE name LIKE ? ORDER BY name LIMIT 20",
			args:  []any{"%test%"},
		},
		"provider_id_lookup": {
			query: "SELECT artist_id FROM artist_provider_ids WHERE provider = ? AND provider_id = ?",
			args:  []any{"musicbrainz", "5b11f4ce-a62d-471e-81fc-a69a8278c7da"},
		},
		"artist_by_mbid": {
			query: `SELECT id FROM artists
				WHERE id = (SELECT artist_id FROM artist_provider_ids WHERE provider = 'musicbrainz' AND provider_id = ? LIMIT 1)`,
			args: []any{"5b11f4ce-a62d-471e-81fc-a69a8278c7da"},
		},
		"artist_by_mbid_and_library": {
			query: `SELECT id FROM artists
				WHERE id IN (SELECT artist_id FROM artist_provider_ids WHERE provider = 'musicbrainz' AND provider_id = ?)
				AND library_id = ?`,
			args: []any{"5b11f4ce-a62d-471e-81fc-a69a8278c7da", "lib-1"},
		},
		"images_for_artist": {
			query: "SELECT id, image_type, exists_flag FROM artist_images WHERE artist_id = ?",
			args:  []any{"test-id"},
		},
		"missing_thumb_filter": {
			query: `SELECT id FROM artists WHERE NOT EXISTS (
				SELECT 1 FROM artist_images WHERE artist_id = artists.id AND image_type = 'thumb' AND exists_flag = 1
			)`,
			args: nil,
		},
		"missing_mbid_filter": {
			query: `SELECT id FROM artists WHERE NOT EXISTS (
				SELECT 1 FROM artist_provider_ids WHERE artist_id = artists.id AND provider = 'musicbrainz'
			)`,
			args: nil,
		},
		"rule_violations_by_artist": {
			query: "SELECT id, rule_id FROM rule_violations WHERE artist_id = ?",
			args:  []any{"test-id"},
		},
		"rule_violations_by_status": {
			query: "SELECT id, rule_id FROM rule_violations WHERE status = ?",
			args:  []any{"open"},
		},
		"rule_violations_by_rule": {
			query: "SELECT id, artist_id FROM rule_violations WHERE rule_id = ?",
			args:  []any{"test-rule"},
		},
		"band_members_by_artist": {
			query: "SELECT id, member_name FROM band_members WHERE artist_id = ?",
			args:  []any{"test-id"},
		},
		"aliases_by_artist": {
			query: "SELECT id, alias FROM artist_aliases WHERE artist_id = ?",
			args:  []any{"test-id"},
		},
		"nfo_snapshots_by_artist": {
			query: "SELECT id, content FROM nfo_snapshots WHERE artist_id = ?",
			args:  []any{"test-id"},
		},
		"sessions_by_expiry": {
			query: "SELECT id FROM sessions WHERE expires_at < ?",
			args:  []any{"2024-01-01T00:00:00Z"},
		},
		"api_token_lookup": {
			query: "SELECT id, name FROM api_tokens WHERE token_hash = ?",
			args:  []any{"abc123hash"},
		},
		"health_history_range": {
			query: "SELECT id, score FROM health_history WHERE recorded_at >= ? ORDER BY recorded_at",
			args:  []any{"2024-01-01"},
		},
		"artists_by_path_for_library": {
			query: "SELECT id, path FROM artists WHERE library_id = ? AND path != ''",
			args:  []any{"lib-1"},
		},
		"locked_artists": {
			query: "SELECT id, name FROM artists WHERE locked = 1",
			args:  nil,
		},

		// --- Alias search and duplicate detection (sqlite_alias.go) ---

		"search_with_aliases": {
			query: `SELECT id FROM artists WHERE id IN (
				SELECT artists.id FROM artists
				LEFT JOIN artist_aliases ON artists.id = artist_aliases.artist_id
				WHERE LOWER(artists.name) LIKE ? OR LOWER(artist_aliases.alias) LIKE ?
			) ORDER BY name`,
			args: []any{"%test%", "%test%"},
		},
		"find_mbid_duplicates": {
			query: `SELECT a.id, p.provider_id FROM artists a
				JOIN artist_provider_ids p ON p.artist_id = a.id AND p.provider = 'musicbrainz'
				WHERE a.is_excluded = 0
				AND p.provider_id IN (
					SELECT provider_id FROM artist_provider_ids
					WHERE provider = 'musicbrainz'
					GROUP BY provider_id HAVING COUNT(*) > 1
				) ORDER BY p.provider_id, a.name`,
			args: nil,
		},
		"find_alias_duplicates": {
			query: `SELECT artists.id, aa.alias FROM artists
				JOIN artist_aliases aa ON artists.id = aa.artist_id
				WHERE LOWER(aa.alias) IN (
					SELECT LOWER(alias) FROM artist_aliases
					GROUP BY LOWER(alias) HAVING COUNT(DISTINCT artist_id) > 1
				) ORDER BY LOWER(aa.alias), artists.name`,
			args: nil,
		},

		// --- Violation trend and severity queries (rule/service.go) ---

		"violation_trend_created": {
			query: `SELECT date(created_at) AS day, COUNT(*) AS cnt
				FROM rule_violations
				WHERE created_at >= ? AND created_at < ?
				GROUP BY day`,
			args: []any{"2024-01-01T00:00:00Z", "2024-02-01T00:00:00Z"},
		},
		"violation_trend_resolved": {
			query: `SELECT date(resolved_at) AS day, COUNT(*) AS cnt
				FROM rule_violations
				WHERE resolved_at IS NOT NULL
				  AND resolved_at >= ? AND resolved_at < ?
				GROUP BY day`,
			args: []any{"2024-01-01T00:00:00Z", "2024-02-01T00:00:00Z"},
		},
		"violation_count_by_severity": {
			query: `SELECT severity, COUNT(*) FROM rule_violations
				WHERE status IN (?, ?)
				GROUP BY severity`,
			args: []any{"open", "pending_choice"},
		},

		// --- Image write tracking (sqlite_image.go) ---

		"newest_write_times_by_artist": {
			query: `SELECT a.id, MAX(ai.last_written_at)
				FROM artist_images ai
				JOIN artists a ON ai.artist_id = a.id
				WHERE a.library_id = ? AND ai.last_written_at != ''
				GROUP BY a.id`,
			args: []any{"lib-1"},
		},

		// --- Violation cleanup (rule/service.go) ---

		"cleanup_old_violations": {
			query: `DELETE FROM rule_violations WHERE status = ? AND resolved_at < ?`,
			args:  []any{"resolved", "2024-01-01T00:00:00Z"},
		},
	}

	// mustIndex lists queries that must use an index -- a full table scan on
	// any of these is a performance regression and fails the test.
	mustIndex := map[string]bool{
		"artist_by_id":              true,
		"artist_by_path":            true,
		"artist_by_name":            true,
		"provider_id_lookup":        true,
		"api_token_lookup":          true,
		"images_for_artist":         true,
		"rule_violations_by_artist": true,
		"band_members_by_artist":    true,
		"aliases_by_artist":         true,
		"violation_trend_created":   true,
		"violation_trend_resolved":  true,
		"cleanup_old_violations":    true,
	}

	// Collect results for summary output
	var findings []string
	scanIssues := 0

	for name, tc := range criticalQueries {
		t.Run(name, func(t *testing.T) {
			plan := explainQueryPlan(t, ctx, db, tc.query, tc.args...)
			t.Logf("Query plan for %s:\n%s", name, plan)

			// Check for full table scans on the primary table.
			// SQLite may emit "SCAN <name>" or "SCAN TABLE <name>"; check each line
			// individually to avoid masking per-step issues in multi-step plans.
			hasUnindexedScan := false
			for _, line := range strings.Split(plan, "\n") {
				if strings.Contains(line, "SCAN ") &&
					!strings.Contains(line, "USING INDEX") &&
					!strings.Contains(line, "USING COVERING INDEX") {
					hasUnindexedScan = true
					break
				}
			}
			if hasUnindexedScan {
				msg := fmt.Sprintf("SCAN (no index): %s", name)
				findings = append(findings, msg)
				scanIssues++
				if mustIndex[name] {
					t.Errorf("REGRESSION: %s does a full table scan; expected index usage", name)
				} else {
					t.Logf("WARNING: %s does a full table scan (may be expected for this query pattern)", name)
				}
			}
		})
	}

	// Print summary
	if len(findings) > 0 {
		t.Logf("\n=== Query Plan Findings ===")
		for _, f := range findings {
			t.Logf("  - %s", f)
		}
		t.Logf("Total scan-without-index issues: %d / %d queries", scanIssues, len(criticalQueries))
	} else {
		t.Logf("\nAll %d critical queries use indexes appropriately.", len(criticalQueries))
	}
}

// TestQueryPlanSummary provides a human-readable summary of all query plans
// for documentation purposes. Set SW_QUERY_PLAN_REPORT=1 to enable verbose
// output that can be captured for performance analysis reports.
func TestQueryPlanSummary(t *testing.T) {
	if os.Getenv("SW_QUERY_PLAN_REPORT") != "1" {
		t.Skip("set SW_QUERY_PLAN_REPORT=1 to generate verbose query plan report")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test_query_report.db")

	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	defer db.Close() //nolint:errcheck

	if err := Migrate(db); err != nil {
		t.Fatalf("running migrations: %v", err)
	}

	// List all indexes in the database
	t.Log("=== Database Indexes ===")
	rows, err := db.QueryContext(ctx, "SELECT name, tbl_name FROM sqlite_master WHERE type = 'index' AND name NOT LIKE 'sqlite_%' ORDER BY tbl_name, name")
	if err != nil {
		t.Fatalf("listing indexes: %v", err)
	}
	defer rows.Close() //nolint:errcheck

	for rows.Next() {
		var name, table string
		if err := rows.Scan(&name, &table); err != nil {
			t.Fatalf("scanning index row: %v", err)
		}
		t.Logf("  %s -> %s", table, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating indexes: %v", err)
	}

	// Report table stats
	t.Log("\n=== Table Schema Analysis ===")
	tables := []string{"artists", "artist_provider_ids", "artist_images", "artist_aliases",
		"artist_platform_ids", "band_members", "rule_violations", "nfo_snapshots",
		"sessions", "api_tokens", "health_history"}

	for _, table := range tables {
		var createSQL string
		err := db.QueryRowContext(ctx, "SELECT sql FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&createSQL)
		if err != nil {
			t.Logf("  %s: could not read schema: %v", table, err)
			continue
		}

		// Count indexes for this table
		var indexCount int
		err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND tbl_name=? AND name NOT LIKE 'sqlite_%'",
			table).Scan(&indexCount)
		if err != nil {
			t.Logf("  %s: could not count indexes: %v", table, err)
			continue
		}

		t.Logf("  %s: %d indexes", table, indexCount)
	}
}

// explainQueryPlan runs EXPLAIN QUERY PLAN and returns the output as a string.
func explainQueryPlan(t *testing.T, ctx context.Context, db *sql.DB, query string, args ...any) string {
	t.Helper()

	rows, err := db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN failed: %v\nQuery: %s", err, query)
	}
	defer rows.Close() //nolint:errcheck

	var lines []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scanning EXPLAIN result: %v", err)
		}
		lines = append(lines, fmt.Sprintf("  %d | %d | %s", id, parent, detail))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating EXPLAIN results: %v", err)
	}

	return strings.Join(lines, "\n")
}
