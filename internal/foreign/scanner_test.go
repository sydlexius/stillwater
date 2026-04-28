package foreign

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/sydlexius/stillwater/internal/artist"
)

// stubArtistLister returns a fixed artist list to the scanner, paginated to
// match the artist.ListParams interface used by Scanner.Scan.
type stubArtistLister struct{ artists []artist.Artist }

func (s stubArtistLister) List(_ context.Context, params artist.ListParams) ([]artist.Artist, int, error) {
	if params.Page > 1 {
		return nil, len(s.artists), nil
	}
	return s.artists, len(s.artists), nil
}

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	// Minimal schema needed by the tests: artists + foreign_files +
	// foreign_file_allowlist mirrored from 001_initial_schema.sql.
	stmts := []string{
		`CREATE TABLE artists (id TEXT PRIMARY KEY, name TEXT NOT NULL DEFAULT '', path TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE foreign_files (
			id TEXT PRIMARY KEY,
			artist_id TEXT NOT NULL,
			file_path TEXT NOT NULL,
			file_name TEXT NOT NULL,
			size_bytes INTEGER NOT NULL DEFAULT 0,
			detected_at TEXT NOT NULL DEFAULT (datetime('now')),
			UNIQUE(artist_id, file_path))`,
		`CREATE TABLE foreign_file_allowlist (
			id TEXT PRIMARY KEY,
			scope TEXT NOT NULL,
			artist_id TEXT,
			file_name TEXT NOT NULL,
			note TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL DEFAULT (datetime('now')))`,
		`CREATE UNIQUE INDEX idx_foreign_allowlist_global
			ON foreign_file_allowlist(file_name) WHERE scope = 'global'`,
		`CREATE UNIQUE INDEX idx_foreign_allowlist_artist
			ON foreign_file_allowlist(artist_id, file_name) WHERE scope = 'artist'`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("create test schema: %v", err)
		}
	}
	return db
}

func TestIsForeignCandidate(t *testing.T) {
	cases := map[string]bool{
		"backdrop.jpg":  true,
		"BACKDROP.JPG":  true,
		"fanart.jpg":    true,
		"fanart1.jpg":   true,
		"backdrop2.png": true,
		"poster.png":    true,
		"clearart.png":  true,
		"folder.jpg":    false, // not in the list
		"artist.jpg":    false, // Stillwater's own canonical name not flagged
		"random.txt":    false, // wrong extension
		"thumb.jpg":     true,
		"backdrop.tiff": false, // unsupported extension
	}
	for name, want := range cases {
		got := isForeignCandidate(name)
		if got != want {
			t.Errorf("isForeignCandidate(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestScanner_RecordsForeignFiles(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepository(db)

	dir := t.TempDir()
	// Write a fake foreign file (no EXIF) and a non-foreign file we should
	// ignore. Using raw bytes that lack the JPEG/PNG magic so ReadProvenance
	// returns (nil, nil) -- our "no provenance" path. The scanner records
	// any file matching the name pattern that lacks Stillwater EXIF; we
	// only need ReadProvenance to NOT return an error.
	mustWrite(t, filepath.Join(dir, "backdrop.jpg"), []byte("not a real image"))
	mustWrite(t, filepath.Join(dir, "folder.jpg"), []byte("not flagged"))

	// Insert a corresponding artist row so ON DELETE CASCADE wiring is
	// realistic and the scanner has an artist to walk.
	if _, err := db.Exec(`INSERT INTO artists (id, name, path) VALUES (?, ?, ?)`, "a1", "Test Artist", dir); err != nil {
		t.Fatalf("insert artist: %v", err)
	}
	listing := stubArtistLister{artists: []artist.Artist{{ID: "a1", Name: "Test Artist", Path: dir}}}
	scanner := NewScanner(repo, listing, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	if err := scanner.Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	got, err := repo.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 entry, got %d: %#v", len(got), got)
	}
	if got[0].FileName != "backdrop.jpg" {
		t.Errorf("expected backdrop.jpg, got %q", got[0].FileName)
	}
}

func TestScanner_RespectsAllowlist(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepository(db)

	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "fanart.jpg"), []byte("not a real image"))

	if _, err := db.Exec(`INSERT INTO artists (id, name, path) VALUES (?, ?, ?)`, "a1", "Test Artist", dir); err != nil {
		t.Fatalf("insert artist: %v", err)
	}
	if err := repo.AddAllowlist(context.Background(), AllowlistEntry{
		Scope: ScopeGlobal, FileName: "fanart.jpg",
	}); err != nil {
		t.Fatalf("AddAllowlist: %v", err)
	}

	listing := stubArtistLister{artists: []artist.Artist{{ID: "a1", Name: "Test Artist", Path: dir}}}
	scanner := NewScanner(repo, listing, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err := scanner.Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	got, err := repo.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected allowlisted file to be skipped; got %d entries", len(got))
	}
}

func TestScanner_ClearsRowsWhenFileGone(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepository(db)

	dir := t.TempDir()
	if _, err := db.Exec(`INSERT INTO artists (id, name, path) VALUES (?, ?, ?)`, "a1", "Test Artist", dir); err != nil {
		t.Fatalf("insert artist: %v", err)
	}
	// Pre-seed a stale ledger row whose underlying file does not exist.
	if err := repo.Upsert(context.Background(), Entry{
		ArtistID: "a1", FilePath: filepath.Join(dir, "backdrop.jpg"), FileName: "backdrop.jpg",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	listing := stubArtistLister{artists: []artist.Artist{{ID: "a1", Name: "Test Artist", Path: dir}}}
	scanner := NewScanner(repo, listing, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err := scanner.Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	got, err := repo.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected stale row to be cleared; got %d entries", len(got))
	}
}

func TestRepository_AllowlistScopes(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepository(db)
	ctx := context.Background()

	// global rejects artist_id.
	if err := repo.AddAllowlist(ctx, AllowlistEntry{Scope: ScopeGlobal, ArtistID: "a1", FileName: "x.jpg"}); err == nil {
		t.Error("expected global+artist_id to be rejected")
	}
	// artist requires artist_id.
	if err := repo.AddAllowlist(ctx, AllowlistEntry{Scope: ScopeArtist, FileName: "x.jpg"}); err == nil {
		t.Error("expected artist scope without artist_id to be rejected")
	}
	if err := repo.AddAllowlist(ctx, AllowlistEntry{Scope: ScopeArtist, ArtistID: "a1", FileName: "Backdrop.JPG"}); err != nil {
		t.Fatalf("valid artist allowlist: %v", err)
	}
	allowed, err := repo.IsAllowlisted(ctx, "a1", "backdrop.jpg")
	if err != nil {
		t.Fatalf("IsAllowlisted: %v", err)
	}
	if !allowed {
		t.Errorf("expected case-insensitive match on file_name to be allowlisted")
	}
	// Wrong artist must not match an artist-scoped row.
	allowed, err = repo.IsAllowlisted(ctx, "other", "backdrop.jpg")
	if err != nil {
		t.Fatalf("IsAllowlisted other: %v", err)
	}
	if allowed {
		t.Errorf("artist-scoped allowlist must not match a different artist")
	}
	// Replaying the same allowlist row is a no-op (unique-constraint
	// collisions are swallowed by the writer).
	if err := repo.AddAllowlist(ctx, AllowlistEntry{Scope: ScopeArtist, ArtistID: "a1", FileName: "backdrop.jpg"}); err != nil {
		t.Fatalf("idempotent allowlist insert: %v", err)
	}
}

func TestRepository_CRUD(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepository(db)
	ctx := context.Background()

	if _, err := db.Exec(`INSERT INTO artists (id, name, path) VALUES ('a1','Aretha','/m/Aretha')`); err != nil {
		t.Fatalf("insert artist: %v", err)
	}

	e := Entry{ArtistID: "a1", FilePath: "/m/Aretha/backdrop.jpg", FileName: "backdrop.jpg", SizeBytes: 4242}
	if err := repo.Upsert(ctx, e); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	// Idempotent: replaying does not create a duplicate.
	if err := repo.Upsert(ctx, e); err != nil {
		t.Fatalf("Upsert again: %v", err)
	}
	got, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 row, got %d", len(got))
	}
	if got[0].ArtistName != "Aretha" {
		t.Errorf("expected artist name joined, got %q", got[0].ArtistName)
	}

	c, err := repo.Count(ctx)
	if err != nil || c != 1 {
		t.Errorf("Count = %d, %v; want 1", c, err)
	}

	id := got[0].ID
	fetched, err := repo.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if fetched.FilePath != e.FilePath {
		t.Errorf("GetByID path: got %q want %q", fetched.FilePath, e.FilePath)
	}

	if err := repo.DeleteByID(ctx, id); err != nil {
		t.Fatalf("DeleteByID: %v", err)
	}
	if err := repo.DeleteByID(ctx, id); err == nil {
		t.Error("expected ErrNotFound on second delete")
	}

	if _, err := repo.GetByID(ctx, "missing"); err == nil {
		t.Error("expected ErrNotFound on missing id")
	}

	// DeleteByPath errors when nothing matches.
	if err := repo.DeleteByPath(ctx, "a1", "/nope"); err == nil {
		t.Error("expected ErrNotFound from DeleteByPath")
	}
}

func TestRepository_AllowlistList(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepository(db)
	ctx := context.Background()

	if _, err := db.Exec(`INSERT INTO artists (id, name, path) VALUES ('a1','Aretha','/m/Aretha')`); err != nil {
		t.Fatalf("insert artist: %v", err)
	}
	if err := repo.AddAllowlist(ctx, AllowlistEntry{Scope: ScopeGlobal, FileName: "fanart.jpg", Note: "stock"}); err != nil {
		t.Fatalf("AddAllowlist: %v", err)
	}
	if err := repo.AddAllowlist(ctx, AllowlistEntry{Scope: ScopeArtist, ArtistID: "a1", FileName: "backdrop.jpg"}); err != nil {
		t.Fatalf("AddAllowlist artist: %v", err)
	}
	rows, err := repo.ListAllowlist(ctx)
	if err != nil {
		t.Fatalf("ListAllowlist: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}
	// Removing a row makes it disappear from the list.
	id := rows[0].ID
	if err := repo.RemoveAllowlist(ctx, id); err != nil {
		t.Fatalf("RemoveAllowlist: %v", err)
	}
	if err := repo.RemoveAllowlist(ctx, id); err == nil {
		t.Error("expected ErrNotFound on second remove")
	}
}

func TestScanner_PathlessArtistsSkipped(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepository(db)
	listing := stubArtistLister{artists: []artist.Artist{
		{ID: "a1", Name: "no path"}, // no Path -> skipped
	}}
	scanner := NewScanner(repo, listing, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err := scanner.Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	got, err := repo.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("pathless artist should not produce ledger rows; got %d", len(got))
	}
}

func TestScanner_UnreadableDirSkippedNotCleared(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepository(db)
	if _, err := db.Exec(`INSERT INTO artists (id, name, path) VALUES ('a1','x','/no/such/dir')`); err != nil {
		t.Fatalf("insert artist: %v", err)
	}
	// Pre-seed a ledger row for the artist; the scan must NOT remove it
	// when the dir is unreadable (skip-don't-clear).
	if err := repo.Upsert(context.Background(), Entry{
		ArtistID: "a1", FilePath: "/no/such/dir/backdrop.jpg", FileName: "backdrop.jpg",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	listing := stubArtistLister{artists: []artist.Artist{{ID: "a1", Name: "x", Path: "/no/such/dir"}}}
	scanner := NewScanner(repo, listing, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err := scanner.Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	got, err := repo.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("ledger row must persist when dir is unreadable; got %d", len(got))
	}
}

func TestScanner_StartSchedulerStopsOnCancel(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepository(db)
	listing := stubArtistLister{}
	scanner := NewScanner(repo, listing, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		scanner.StartScheduler(ctx, 10*time.Millisecond, time.Millisecond)
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler did not stop within 2s")
	}
}

func TestScanner_StartSchedulerStopsBeforeFirstRun(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepository(db)
	scanner := NewScanner(repo, stubArtistLister{}, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		// Long startup delay so the cancel-before-first-run branch fires.
		scanner.StartScheduler(ctx, time.Hour, time.Hour)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler should exit immediately on cancel")
	}
}

func TestRepository_UpsertValidation(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepository(db)
	if err := repo.Upsert(context.Background(), Entry{}); err == nil {
		t.Error("expected validation error on empty entry")
	}
}

func mustWrite(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o644); err != nil { //nolint:gosec // test fixture
		t.Fatalf("WriteFile %s: %v", path, err)
	}
}
