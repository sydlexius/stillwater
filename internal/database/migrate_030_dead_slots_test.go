package database

// Migration 030 strips provider/field pairs whose adapter cannot populate the
// field from the STORED provider priority rows (issue #2897).
//
// It gets a populated-database test because the defect it fixes is invisible
// to any test that reads DefaultPriorities(): migration 001 seeds the priority
// rows, GetPriorities only ever APPENDS missing defaults to a stored row, and
// so a correction made in Go code alone reaches new installs only. Every
// install that has run 001 keeps the dead slots. That is the same gap
// migration 007 closed for biography/wikidata (#1029/#1577).
//
// The test builds a database at the PRE-030 schema so the seeded rows are the
// real pre-fix shape rather than a hand-written approximation, asserts the
// dead slots ARE present first (without which every assertion below would pass
// vacuously), and only then applies 030.

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
)

// deadSlots is the set migration 030 strips, as (field, provider) pairs.
var deadSlots = []struct {
	field    string
	provider string
}{
	{"genres", "discogs"},
	{"members", "wikidata"},
	{"born", "wikidata"},
	{"died", "wikidata"},
	{"gender", "wikidata"},
	{"type", "wikidata"},
	{"type", "discogs"},
	{"disbanded", "wikipedia"},
	{"years_active", "audiodb"},
	// Not from the default chains: migration 001 seeds MusicBrainz first for
	// biography and migration 007 removed only wikidata. #1029 covered this
	// pair with a fieldProviderExclusions entry, which the live scraper path
	// does not consult.
	{"biography", "musicbrainz"},
}

func readPriority(t *testing.T, db *sql.DB, field string) []string {
	t.Helper()
	var raw string
	err := db.QueryRowContext(context.Background(),
		"SELECT value FROM settings WHERE key = ?", "provider.priority."+field).Scan(&raw)
	if err != nil {
		t.Fatalf("reading provider.priority.%s: %v", field, err)
	}
	var providers []string
	if err := json.Unmarshal([]byte(raw), &providers); err != nil {
		t.Fatalf("provider.priority.%s is not a JSON array (%q): %v", field, raw, err)
	}
	return providers
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// applyMigration030 runs goose up from the current version through 030.
func applyMigration030(t *testing.T, dbPath string) {
	t.Helper()
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("opening db: %v", err)
	}
	defer db.Close()

	goose.SetBaseFS(migrations)
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatalf("setting goose dialect: %v", err)
	}
	if err := goose.UpTo(db, "migrations", 30); err != nil {
		t.Fatalf("migrating up to 30: %v", err)
	}
}

func TestMigration030_StripsDeadSlotsFromStoredPriorities(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "priorities.db")
	ctx := context.Background()

	// 1. A database at the schema that shipped BEFORE this change, so the
	//    priority rows are migration 001's real seed.
	migrateUpTo(t, dbPath, 29)

	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopening db: %v", err)
	}

	// 2. PRECONDITION. Every dead slot must actually be present in the stored
	//    rows before the migration runs. Without this the post-migration
	//    assertions would pass against a database that never had the defect.
	for _, slot := range deadSlots {
		got := readPriority(t, db, slot.field)
		if !contains(got, slot.provider) {
			t.Fatalf("precondition failed: provider.priority.%s = %v does not contain %q, so this test cannot prove migration 030 removes it",
				slot.field, got, slot.provider)
		}
	}

	// 3. Seed one row with an operator's DELIBERATE ordering of providers that
	//    all survive, and one where a dead slot sits in the middle of a
	//    hand-picked order. The migration must strip the dead entry without
	//    disturbing the relative order of what remains, and must not rewrite a
	//    row that has no dead slot at all.
	// styles carries no dead slot, so 030 must leave it byte-identical.
	untouched := `["discogs","audiodb","lastfm","musicbrainz"]`
	if _, err := db.ExecContext(ctx,
		"UPDATE settings SET value = ? WHERE key = 'provider.priority.styles'", untouched); err != nil {
		t.Fatalf("seeding operator-ordered styles row: %v", err)
	}
	// wikipedia and lastfm both survive the genres strip; discogs sits between
	// them, so a naive rebuild that reorders would be caught.
	if _, err := db.ExecContext(ctx,
		"UPDATE settings SET value = ? WHERE key = 'provider.priority.genres'",
		`["wikipedia","discogs","lastfm","musicbrainz"]`); err != nil {
		t.Fatalf("seeding operator-ordered genres row: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("closing db before migrating: %v", err)
	}

	// 4. Apply migration 030.
	applyMigration030(t, dbPath)

	db, err = Open(dbPath)
	if err != nil {
		t.Fatalf("reopening db after migration: %v", err)
	}
	defer db.Close()

	// 5. Every dead slot is gone.
	for _, slot := range deadSlots {
		got := readPriority(t, db, slot.field)
		if contains(got, slot.provider) {
			t.Errorf("provider.priority.%s = %v still contains %q; migration 030 did not strip it",
				slot.field, got, slot.provider)
		}
		if len(got) == 0 {
			t.Errorf("provider.priority.%s is now empty; stripping a dead slot must never leave a field with no provider", slot.field)
		}
	}

	// 6. The operator's ordering of the surviving providers is preserved, and
	//    a row with no dead slot is left byte-identical.
	if got, want := readPriority(t, db, "genres"), []string{"wikipedia", "lastfm", "musicbrainz"}; !reflect.DeepEqual(got, want) {
		t.Errorf("provider.priority.genres = %v, want %v (operator ordering around the stripped entry must survive)", got, want)
	}
	var styles string
	if err := db.QueryRowContext(ctx,
		"SELECT value FROM settings WHERE key = 'provider.priority.styles'").Scan(&styles); err != nil {
		t.Fatalf("reading styles row: %v", err)
	}
	if styles != untouched {
		t.Errorf("provider.priority.styles = %s, want %s unchanged (the row carries no dead slot, so 030 must not rewrite it)", styles, untouched)
	}
}

// TestMigration030_IsIdempotent proves re-running the migration is a no-op.
// The EXISTS guard is what makes each statement skip an already-clean row; if
// it were dropped the UPDATE would still produce the same JSON, so the only
// way to see the difference is to compare full state across two runs.
func TestMigration030_IsIdempotent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "idempotent.db")
	ctx := context.Background()

	migrateUpTo(t, dbPath, 29)
	applyMigration030(t, dbPath)

	snapshot := func() map[string]string {
		db, err := Open(dbPath)
		if err != nil {
			t.Fatalf("opening db: %v", err)
		}
		defer db.Close()
		rows, err := db.QueryContext(ctx,
			"SELECT key, value FROM settings WHERE key LIKE 'provider.priority.%' ORDER BY key")
		if err != nil {
			t.Fatalf("reading priority rows: %v", err)
		}
		defer rows.Close()
		out := make(map[string]string)
		for rows.Next() {
			var k, v string
			if err := rows.Scan(&k, &v); err != nil {
				t.Fatalf("scanning priority row: %v", err)
			}
			out[k] = v
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterating priority rows: %v", err)
		}
		return out
	}

	first := snapshot()
	if len(first) == 0 {
		t.Fatal("no provider.priority.* rows after the first migration -- the comparison below would be vacuous")
	}

	// Re-run every statement in migration 030 against the already-migrated
	// database. goose will not re-apply a recorded version, so the second run
	// has to execute the file's statements directly to prove the SQL itself is
	// safe to repeat.
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopening db: %v", err)
	}
	body, err := migrations.ReadFile("migrations/030_remove_dead_provider_priority_slots.sql")
	if err != nil {
		t.Fatalf("reading migration 030: %v", err)
	}
	stmts := upStatements(t, string(body))
	if len(stmts) != 9 {
		t.Fatalf("parsed %d UPDATE statements from migration 030, want 9 -- the re-run would not exercise the real migration", len(stmts))
	}
	for _, stmt := range stmts {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("re-running migration 030 statement: %v\n%s", err, stmt)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("closing db: %v", err)
	}

	if second := snapshot(); !reflect.DeepEqual(first, second) {
		for k, v := range first {
			if second[k] != v {
				t.Errorf("re-running migration 030 changed %s: %q -> %q", k, v, second[k])
			}
		}
	}
}

// upStatements extracts the UPDATE statements from the +goose Up section of a
// migration file, skipping comments and the Down section.
func upStatements(t *testing.T, body string) []string {
	t.Helper()
	var out []string
	var current []string
	inUp := false
	for _, line := range strings.Split(body, "\n") {
		if line == "-- +goose Up" {
			inUp = true
			continue
		}
		if line == "-- +goose Down" {
			break
		}
		if !inUp || line == "" || strings.HasPrefix(line, "--") {
			continue
		}
		current = append(current, line)
		if strings.HasSuffix(line, ";") {
			out = append(out, strings.Join(current, "\n"))
			current = nil
		}
	}
	return out
}
