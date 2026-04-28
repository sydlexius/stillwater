package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/sydlexius/stillwater/internal/foreign"
)

// newTestRouterWithForeign builds a Router with an initialized foreign-file
// repo backed by an in-memory SQLite. Auth is intentionally omitted -- the
// handlers themselves do not depend on r.authService for the foreign-file
// API endpoints (route-level middleware handles auth), so direct invocation
// of the handler method exercises the business logic.
func newTestRouterWithForeign(t *testing.T) (*Router, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	// :memory: SQLite gives each connection its own database. Pin the pool
	// to one connection so schema and fixtures are visible to every query.
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	for _, s := range []string{
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
		`CREATE UNIQUE INDEX idx_foreign_allowlist_global ON foreign_file_allowlist(file_name) WHERE scope = 'global'`,
		`CREATE UNIQUE INDEX idx_foreign_allowlist_artist ON foreign_file_allowlist(artist_id, file_name) WHERE scope = 'artist'`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	r := &Router{
		logger:      slog.Default(),
		foreignRepo: foreign.NewRepository(db),
	}
	return r, db
}

func TestHandleForeignFilesList(t *testing.T) {
	r, db := newTestRouterWithForeign(t)
	mustExec(t, db, `INSERT INTO artists (id, name, path) VALUES ('a1','Aretha','/m/Aretha')`)
	if err := r.foreignRepo.Upsert(context.Background(), foreign.Entry{
		ArtistID: "a1", FilePath: "/m/Aretha/backdrop.jpg", FileName: "backdrop.jpg",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/foreign-files", nil)
	rec := httptest.NewRecorder()
	r.handleForeignFilesList(rec, req)
	res := rec.Result()
	defer res.Body.Close() //nolint:errcheck
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d", res.StatusCode)
	}
	var body struct {
		Count int             `json:"count"`
		Items []foreign.Entry `json:"items"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Count != 1 || len(body.Items) != 1 {
		t.Errorf("unexpected response body: %+v", body)
	}
}

func TestHandleForeignFileAllowlist(t *testing.T) {
	r, db := newTestRouterWithForeign(t)
	mustExec(t, db, `INSERT INTO artists (id, name, path) VALUES ('a1','Aretha','/m/Aretha')`)
	if err := r.foreignRepo.Upsert(context.Background(), foreign.Entry{
		ArtistID: "a1", FilePath: "/m/Aretha/backdrop.jpg", FileName: "backdrop.jpg",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	all, _ := r.foreignRepo.List(context.Background())
	id := all[0].ID

	req := httptest.NewRequest(http.MethodPost, "/api/v1/foreign-files/"+id+"/allowlist", nil)
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	r.handleForeignFileAllowlist(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d", rec.Code)
	}
	// Ledger row should be gone, allowlist should have the entry.
	count, _ := r.foreignRepo.Count(context.Background())
	if count != 0 {
		t.Errorf("ledger row should be cleared after allowlist; got %d", count)
	}
	allowed, _ := r.foreignRepo.IsAllowlisted(context.Background(), "a1", "backdrop.jpg")
	if !allowed {
		t.Errorf("expected entry to be allowlisted")
	}

	// 404 path: missing id.
	req = httptest.NewRequest(http.MethodPost, "/api/v1/foreign-files/missing/allowlist", nil)
	req.SetPathValue("id", "missing")
	rec = httptest.NewRecorder()
	r.handleForeignFileAllowlist(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}

func TestHandleForeignFileDelete(t *testing.T) {
	r, db := newTestRouterWithForeign(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "backdrop.jpg")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil { //nolint:gosec
		t.Fatalf("write: %v", err)
	}
	mustExec(t, db, `INSERT INTO artists (id, name, path) VALUES ('a1','Aretha',?)`, dir)
	if err := r.foreignRepo.Upsert(context.Background(), foreign.Entry{
		ArtistID: "a1", FilePath: target, FileName: "backdrop.jpg",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	all, _ := r.foreignRepo.List(context.Background())
	id := all[0].ID

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/foreign-files/"+id+"/file", nil)
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	r.handleForeignFileDelete(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d", rec.Code)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("file should be removed: stat err = %v", err)
	}
	count, _ := r.foreignRepo.Count(context.Background())
	if count != 0 {
		t.Errorf("ledger row should be cleared; got %d", count)
	}
}

func TestHandleForeignAllowlistRemove(t *testing.T) {
	r, db := newTestRouterWithForeign(t)
	mustExec(t, db, `INSERT INTO artists (id, name, path) VALUES ('a1','Aretha','/m/Aretha')`)
	if err := r.foreignRepo.AddAllowlist(context.Background(), foreign.AllowlistEntry{
		Scope: foreign.ScopeGlobal, FileName: "fanart.jpg",
	}); err != nil {
		t.Fatalf("AddAllowlist: %v", err)
	}
	rows, _ := r.foreignRepo.ListAllowlist(context.Background())
	id := rows[0].ID

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/foreign-file-allowlist/"+id, nil)
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	r.handleForeignAllowlistRemove(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d", rec.Code)
	}
	rows, _ = r.foreignRepo.ListAllowlist(context.Background())
	if len(rows) != 0 {
		t.Errorf("expected allowlist empty; got %d rows", len(rows))
	}

	// 404: removing again.
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/foreign-file-allowlist/"+id, nil)
	req.SetPathValue("id", id)
	rec = httptest.NewRecorder()
	r.handleForeignAllowlistRemove(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}

func TestHandleForeignAllowlistList(t *testing.T) {
	r, _ := newTestRouterWithForeign(t)
	if err := r.foreignRepo.AddAllowlist(context.Background(), foreign.AllowlistEntry{
		Scope: foreign.ScopeGlobal, FileName: "fanart.jpg",
	}); err != nil {
		t.Fatalf("AddAllowlist: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/foreign-file-allowlist", nil)
	rec := httptest.NewRecorder()
	r.handleForeignAllowlistList(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d", rec.Code)
	}
	var body struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Count != 1 {
		t.Errorf("expected 1 row, got %d", body.Count)
	}
}

func TestForeignSummaryForBanner(t *testing.T) {
	r, db := newTestRouterWithForeign(t)
	mustExec(t, db, `INSERT INTO artists (id, name, path) VALUES ('a1','x','/x')`)
	if err := r.foreignRepo.Upsert(context.Background(), foreign.Entry{
		ArtistID: "a1", FilePath: "/x/fanart.jpg", FileName: "fanart.jpg",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if got := r.foreignSummaryForBanner(context.Background()); got != 1 {
		t.Errorf("foreignSummaryForBanner = %d, want 1", got)
	}

	// nil-repo path returns zero count.
	r2 := &Router{logger: slog.Default()}
	if got := r2.foreignSummaryForBanner(context.Background()); got != 0 {
		t.Errorf("nil-repo summary should be 0; got %d", got)
	}
}

func TestHandleForeignFilesDismiss(t *testing.T) {
	r, db := newTestRouterWithForeign(t)
	mustExec(t, db, `INSERT INTO artists (id, name, path) VALUES ('a1','x','/x')`)
	for _, fn := range []string{"backdrop.jpg", "fanart.jpg"} {
		if err := r.foreignRepo.Upsert(context.Background(), foreign.Entry{
			ArtistID: "a1", FilePath: "/x/" + fn, FileName: fn,
		}); err != nil {
			t.Fatalf("seed %s: %v", fn, err)
		}
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/foreign-files/dismiss", nil)
	rec := httptest.NewRecorder()
	r.handleForeignFilesDismiss(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("dismiss should return 200 with the empty table partial; got %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Errorf("dismiss should render HTML; got Content-Type=%q", got)
	}
	if !strings.Contains(rec.Body.String(), "foreign-files-table") {
		t.Errorf("dismiss body should swap the foreign-files table; body=%q", rec.Body.String())
	}

	count, _ := r.foreignRepo.Count(context.Background())
	if count != 0 {
		t.Errorf("ledger should be cleared after dismiss; got %d", count)
	}
	rows, _ := r.foreignRepo.ListAllowlist(context.Background())
	if len(rows) != 2 {
		t.Errorf("expected 2 global allowlist rows after dismiss; got %d", len(rows))
	}
	for _, row := range rows {
		if row.Scope != foreign.ScopeGlobal {
			t.Errorf("expected global scope; got %q", row.Scope)
		}
	}
}

func TestForeignHandlers_NoRepoServiceUnavailable(t *testing.T) {
	r := &Router{logger: slog.Default()}
	cases := []struct {
		name    string
		invoke  func(http.ResponseWriter, *http.Request)
		method  string
		path    string
		pathKey string
		pathVal string
	}{
		{"list", r.handleForeignFilesList, http.MethodGet, "/api/v1/foreign-files", "", ""},
		{"allowlist", r.handleForeignFileAllowlist, http.MethodPost, "/api/v1/foreign-files/x/allowlist", "id", "x"},
		{"delete", r.handleForeignFileDelete, http.MethodDelete, "/api/v1/foreign-files/x/file", "id", "x"},
		{"dismiss", r.handleForeignFilesDismiss, http.MethodPost, "/api/v1/foreign-files/dismiss", "", ""},
		{"allowlist-list", r.handleForeignAllowlistList, http.MethodGet, "/api/v1/foreign-file-allowlist", "", ""},
		{"allowlist-remove", r.handleForeignAllowlistRemove, http.MethodDelete, "/api/v1/foreign-file-allowlist/x", "id", "x"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(c.method, c.path, nil)
			if c.pathKey != "" {
				req.SetPathValue(c.pathKey, c.pathVal)
			}
			rec := httptest.NewRecorder()
			c.invoke(rec, req)
			if rec.Code != http.StatusServiceUnavailable {
				t.Errorf("expected 503, got %d", rec.Code)
			}
		})
	}
}

func TestHandleForeignFile_MissingID(t *testing.T) {
	r, _ := newTestRouterWithForeign(t)
	cases := []func(http.ResponseWriter, *http.Request){
		r.handleForeignFileAllowlist,
		r.handleForeignFileDelete,
		r.handleForeignAllowlistRemove,
	}
	for _, h := range cases {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("missing id should return 400; got %d", rec.Code)
		}
	}
}

func TestHandleForeignFileDelete_MissingFileStillSucceeds(t *testing.T) {
	r, db := newTestRouterWithForeign(t)
	mustExec(t, db, `INSERT INTO artists (id, name, path) VALUES ('a1','x','/x')`)
	// Ledger row points at a non-existent path; the handler should still
	// clear the ledger row because the user wanted the file gone.
	if err := r.foreignRepo.Upsert(context.Background(), foreign.Entry{
		ArtistID: "a1", FilePath: "/no/such/path/backdrop.jpg", FileName: "backdrop.jpg",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	all, _ := r.foreignRepo.List(context.Background())
	id := all[0].ID

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/foreign-files/"+id+"/file", nil)
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	r.handleForeignFileDelete(rec, req)
	// Treat as success: file is already gone.
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 for already-gone file; got %d", rec.Code)
	}
}

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}
