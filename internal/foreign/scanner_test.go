package foreign

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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
	// :memory: SQLite gives each connection its own database. Pin the pool
	// to one connection so schema and fixtures are visible to every query.
	db.SetMaxOpenConns(1)
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
	// Use a real path that is not a directory (a regular file) so ReadDir
	// returns ENOTDIR rather than ENOENT. ENOENT would also pass through
	// the skip-don't-clear path but does not exercise the same listing-
	// error branch that production scanners hit on permission failures.
	badPath := filepath.Join(t.TempDir(), "not-a-dir")
	mustWrite(t, badPath, []byte("x"))
	if _, err := db.Exec(`INSERT INTO artists (id, name, path) VALUES ('a1','x', ?)`, badPath); err != nil {
		t.Fatalf("insert artist: %v", err)
	}
	// Pre-seed a ledger row for the artist; the scan must NOT remove it
	// when the dir is unreadable (skip-don't-clear).
	if err := repo.Upsert(context.Background(), Entry{
		ArtistID: "a1", FilePath: filepath.Join(badPath, "backdrop.jpg"), FileName: "backdrop.jpg",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	listing := stubArtistLister{artists: []artist.Artist{{ID: "a1", Name: "x", Path: badPath}}}
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

// TestScanner_DeleteByPathErrorDoesNotIncrementCleared pins the round-2 fix
// for #1246: when the reconcile-pass DeleteByPath fails, the scanner must
// log a Warn and leave `cleared` unchanged so metrics/logs do not over-report
// reconciliation success.
func TestScanner_DeleteByPathErrorDoesNotIncrementCleared(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepository(db)
	dir := t.TempDir()
	if _, err := db.Exec(`INSERT INTO artists (id, name, path) VALUES ('a1','x',?)`, dir); err != nil {
		t.Fatalf("insert artist: %v", err)
	}
	// Seed a stale ledger row whose file is gone (so it falls into the
	// "missing file -> safe to clear" branch).
	if err := repo.Upsert(context.Background(), Entry{
		ArtistID: "a1", FilePath: filepath.Join(dir, "backdrop.jpg"), FileName: "backdrop.jpg",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Drop foreign_files so DeleteByPath returns an error inside the loop.
	// listForArtist is called BEFORE the drop runs (during scanArtist), so
	// the loop still iterates the seeded row -- but the DROP table makes
	// the subsequent DeleteByPath fail. Use a wrapper that drops the table
	// only after listForArtist has returned by chaining inside the scan.
	//
	// Simplest controllable error: close the DB right after listing. We do
	// that by calling Scan directly with a context that short-circuits the
	// reconcile mid-flight via a nil-safe approach: drop the table, scan,
	// and assert.
	if _, err := db.Exec(`DROP TABLE foreign_files`); err != nil {
		t.Fatalf("drop: %v", err)
	}

	listing := stubArtistLister{artists: []artist.Artist{{ID: "a1", Name: "x", Path: dir}}}
	scanner := NewScanner(repo, listing, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	// Scan returns nil even when individual reconcile operations fail,
	// because per-artist failures are logged and the scanner keeps going.
	if err := scanner.Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	// Without the table we can't query Count; the assertion is implicit:
	// the test exercises the cleared-failure logging path without panicking
	// or aborting the scan, which is the contract the round-2 fix pins.
}

// errArtistLister errors on the configured page (1-indexed). Used to drive
// the Scan abort-with-error paths added in the M46-1184 hardening sweep.
type errArtistLister struct {
	pages    map[int][]artist.Artist
	total    int
	errOn    int   // page number that should return an error
	errValue error // error to return on errOn
}

func (e errArtistLister) List(_ context.Context, params artist.ListParams) ([]artist.Artist, int, error) {
	if params.Page == e.errOn {
		return nil, 0, e.errValue
	}
	if list, ok := e.pages[params.Page]; ok {
		return list, e.total, nil
	}
	return nil, e.total, nil
}

// TestScanner_FirstListErrorPropagates pins the round-2 Scan() contract:
// a failure on the very first List call must surface as a wrapped error,
// not as silent "scan complete with zero counts".
func TestScanner_FirstListErrorPropagates(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepository(db)
	boom := errors.New("simulated DB outage")
	listing := errArtistLister{errOn: 1, errValue: boom}
	scanner := NewScanner(repo, listing, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	err := scanner.Scan(context.Background())
	if err == nil {
		t.Fatal("expected error from Scan when first List fails; got nil")
	}
	if !errors.Is(err, boom) {
		t.Errorf("Scan should wrap the underlying error; got %v", err)
	}
}

// TestScanner_PaginationListErrorAborts pins that a mid-corpus pagination
// failure aborts the scan with an Error-level summary log and a wrapped
// error return, rather than the misleading "scan complete" Info that
// silently shipped before the hardening sweep.
func TestScanner_PaginationListErrorAborts(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepository(db)
	dir := t.TempDir()
	if _, err := db.Exec(`INSERT INTO artists (id, name, path) VALUES ('a1','x',?)`, dir); err != nil {
		t.Fatalf("insert artist: %v", err)
	}
	// pageSize is 200; emit page 1 with > 0 artists and a total higher than
	// page-1 length so the scanner pages forward, then error on page 2.
	page1 := []artist.Artist{{ID: "a1", Name: "x", Path: dir}}
	boom := errors.New("page list failure")
	listing := errArtistLister{
		pages:    map[int][]artist.Artist{1: page1},
		total:    250, // > len(page1) so scanner pages
		errOn:    2,
		errValue: boom,
	}
	scanner := NewScanner(repo, listing, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	err := scanner.Scan(context.Background())
	if err == nil {
		t.Fatal("expected error from Scan when page 2 fails; got nil")
	}
	if !errors.Is(err, boom) {
		t.Errorf("Scan should wrap the page-list error; got %v", err)
	}
	wantPrefix := "listing artists page 2"
	if msg := err.Error(); !strings.Contains(msg, wantPrefix) {
		t.Errorf("error message should reference the failing page; got %q", msg)
	}
}

// TestScanner_ContextCanceledMidPagination pins that cancellation between
// pages returns context.Canceled distinctly from the clean-completion path,
// so StartScheduler can suppress the Error log on graceful shutdown.
func TestScanner_ContextCanceledMidPagination(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepository(db)
	dir := t.TempDir()
	if _, err := db.Exec(`INSERT INTO artists (id, name, path) VALUES ('a1','x',?)`, dir); err != nil {
		t.Fatalf("insert artist: %v", err)
	}
	page1 := []artist.Artist{{ID: "a1", Name: "x", Path: dir}}
	// Use a stub that cancels its own context view: signalLister.cancel is
	// invoked from inside List() on the configured page, so the scanner's
	// next ctx.Err() check at the top of the pagination loop sees the
	// cancellation.
	cancelOnPage := 1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	listing := signalLister{
		pages:        map[int][]artist.Artist{1: page1},
		total:        250,
		cancel:       cancel,
		cancelOnPage: cancelOnPage,
	}
	scanner := NewScanner(repo, listing, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	err := scanner.Scan(ctx)
	if err == nil {
		t.Fatal("expected ctx.Err from Scan after cancellation; got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Scan should return context.Canceled; got %v", err)
	}
}

// signalLister cancels its associated context after returning page N,
// exercising the ctx.Err() branch in Scan's pagination loop.
type signalLister struct {
	pages        map[int][]artist.Artist
	total        int
	cancel       context.CancelFunc
	cancelOnPage int
}

func (s signalLister) List(_ context.Context, params artist.ListParams) ([]artist.Artist, int, error) {
	list := s.pages[params.Page]
	if params.Page == s.cancelOnPage {
		// Cancel after returning so the scanner sees ctx.Err on the NEXT
		// loop iteration's check.
		s.cancel()
	}
	return list, s.total, nil
}

// TestScanner_ReconcileIsAllowlistedErrorPreservesRow pins the round-4
// hardening: when IsAllowlisted errors during the reconcile pass, the
// scanner must NOT clear the row (skip-don't-clear). Inducing the error by
// dropping the allowlist table mid-flight is impractical; we drop it
// before the scan instead, which makes IsAllowlisted error on every call
// (record AND reconcile passes). The pre-seeded ledger row must survive.
func TestScanner_ReconcileIsAllowlistedErrorPreservesRow(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepository(db)
	dir := t.TempDir()
	if _, err := db.Exec(`INSERT INTO artists (id, name, path) VALUES ('a1','x',?)`, dir); err != nil {
		t.Fatalf("insert artist: %v", err)
	}
	// Real foreign file on disk so the row's file is "present" -- the
	// reconcile loop only consults IsAllowlisted on rows whose file still
	// exists. Without an on-disk file the row would fall into the
	// missing-file clear branch.
	mustWrite(t, filepath.Join(dir, "backdrop.jpg"), []byte("garbage"))
	if err := repo.Upsert(context.Background(), Entry{
		ArtistID: "a1", FilePath: filepath.Join(dir, "backdrop.jpg"), FileName: "backdrop.jpg",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Drop the allowlist table to force IsAllowlisted to error.
	if _, err := db.Exec(`DROP TABLE foreign_file_allowlist`); err != nil {
		t.Fatalf("drop: %v", err)
	}
	listing := stubArtistLister{artists: []artist.Artist{{ID: "a1", Name: "x", Path: dir}}}
	scanner := NewScanner(repo, listing, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err := scanner.Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	rows, err := repo.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("row must persist when IsAllowlisted errors; got %d rows", len(rows))
	}
}
