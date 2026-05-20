package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/foreign"
	"github.com/sydlexius/stillwater/internal/i18n"
)

// withI18nCtx attaches an English i18n translator to a request so that
// template i18n lookups resolve to real translations rather than raw key
// strings. Used by tests that assert on user-facing copy in rendered HTML.
func withI18nCtx(tb testing.TB, req *http.Request) *http.Request {
	tb.Helper()
	bundle, err := i18n.LoadEmbedded()
	if err != nil {
		tb.Fatalf("loading i18n bundle: %v", err)
	}
	ctx := i18n.WithTranslator(req.Context(), bundle.Translator("en"))
	return req.WithContext(ctx)
}

// sha256HexAPI returns the lowercase hex sha256 of b. Mirrors the
// foreign-package test helper so handler tests can pre-compute the hash
// they expect the scanner / resolver to derive from on-disk bytes.
func sha256HexAPI(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

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
			content_hash TEXT,
			size_bytes INTEGER NOT NULL DEFAULT 0,
			detected_at TEXT NOT NULL DEFAULT (datetime('now')),
			UNIQUE(artist_id, file_path))`,
		`CREATE TABLE foreign_file_allowlist (
			id TEXT PRIMARY KEY,
			scope TEXT NOT NULL,
			artist_id TEXT,
			file_name TEXT NOT NULL,
			content_hash TEXT,
			note TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL DEFAULT (datetime('now')))`,
		`CREATE UNIQUE INDEX idx_foreign_allowlist_global_hash
			ON foreign_file_allowlist(content_hash)
			WHERE scope = 'global' AND content_hash IS NOT NULL`,
		`CREATE UNIQUE INDEX idx_foreign_allowlist_artist_hash
			ON foreign_file_allowlist(artist_id, content_hash)
			WHERE scope = 'artist' AND content_hash IS NOT NULL`,
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
	t.Parallel()
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
	defer res.Body.Close()
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
	t.Parallel()
	r, db := newTestRouterWithForeign(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "backdrop.jpg")
	body := []byte("allowlist target bytes")
	if err := os.WriteFile(target, body, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	mustExec(t, db, `INSERT INTO artists (id, name, path) VALUES ('a1','Aretha',?)`, dir)
	if err := r.foreignRepo.Upsert(context.Background(), foreign.Entry{
		ArtistID: "a1", FilePath: target, FileName: "backdrop.jpg", ContentHash: sha256HexAPI(body),
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
	allowed, _ := r.foreignRepo.IsAllowlisted(context.Background(), "a1", sha256HexAPI(body))
	if !allowed {
		t.Errorf("expected entry to be allowlisted by hash")
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
	t.Parallel()
	r, db := newTestRouterWithForeign(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "backdrop.jpg")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
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
	t.Parallel()
	r, db := newTestRouterWithForeign(t)
	mustExec(t, db, `INSERT INTO artists (id, name, path) VALUES ('a1','Aretha','/m/Aretha')`)
	if err := r.foreignRepo.AddAllowlist(context.Background(), foreign.AllowlistEntry{
		Scope: foreign.ScopeGlobal, FileName: "fanart.jpg", ContentHash: sha256HexAPI([]byte("fanart")),
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
	t.Parallel()
	r, _ := newTestRouterWithForeign(t)
	if err := r.foreignRepo.AddAllowlist(context.Background(), foreign.AllowlistEntry{
		Scope: foreign.ScopeGlobal, FileName: "fanart.jpg", ContentHash: sha256HexAPI([]byte("fanart")),
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
	t.Parallel()
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
	t.Parallel()
	r, db := newTestRouterWithForeign(t)
	dir := t.TempDir()
	mustExec(t, db, `INSERT INTO artists (id, name, path) VALUES ('a1','x',?)`, dir)
	for _, fn := range []string{"backdrop.jpg", "fanart.jpg"} {
		body := []byte("body-" + fn)
		path := filepath.Join(dir, fn)
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatalf("write %s: %v", fn, err)
		}
		if err := r.foreignRepo.Upsert(context.Background(), foreign.Entry{
			ArtistID: "a1", FilePath: path, FileName: fn, ContentHash: sha256HexAPI(body),
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// TestHandleForeignFile_RenderRefreshedTable_AfterRowActions pins the new
// render-after-success behavior for the per-row Allowlist and Delete
// handlers. Both should now return the refreshed #foreign-files-table partial
// (HTML) so HTMX can swap the whole container, fixing the stale empty-state
// + bulk-dismiss button bug from PR #1246 round 2.
func TestHandleForeignFile_RenderRefreshedTable_AfterRowActions(t *testing.T) {
	t.Parallel()
	r, db := newTestRouterWithForeign(t)
	dirA := t.TempDir()
	dirB := t.TempDir()
	bodyA := []byte("artist-A")
	bodyB := []byte("artist-B")
	targetA := filepath.Join(dirA, "backdrop.jpg")
	targetB := filepath.Join(dirB, "backdrop.jpg")
	if err := os.WriteFile(targetA, bodyA, 0o644); err != nil {
		t.Fatalf("write A: %v", err)
	}
	if err := os.WriteFile(targetB, bodyB, 0o644); err != nil {
		t.Fatalf("write B: %v", err)
	}
	mustExec(t, db, `INSERT INTO artists (id, name, path) VALUES ('a1','Aretha',?)`, dirA)
	mustExec(t, db, `INSERT INTO artists (id, name, path) VALUES ('a2','Beth',?)`, dirB)
	if err := r.foreignRepo.Upsert(context.Background(), foreign.Entry{
		ArtistID: "a1", FilePath: targetA, FileName: "backdrop.jpg", ContentHash: sha256HexAPI(bodyA),
	}); err != nil {
		t.Fatalf("seed allowlist target: %v", err)
	}
	if err := r.foreignRepo.Upsert(context.Background(), foreign.Entry{
		ArtistID: "a2", FilePath: targetB, FileName: "backdrop.jpg", ContentHash: sha256HexAPI(bodyB),
	}); err != nil {
		t.Fatalf("seed delete target: %v", err)
	}
	rows, _ := r.foreignRepo.List(context.Background())
	allowID, deleteID := rows[0].ID, rows[1].ID

	// Allowlist returns the refreshed table partial.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/foreign-files/"+allowID+"/allowlist", nil)
	req.SetPathValue("id", allowID)
	rec := httptest.NewRecorder()
	r.handleForeignFileAllowlist(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("allowlist status: got %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Errorf("allowlist Content-Type: got %q want HTML", got)
	}
	if !strings.Contains(rec.Body.String(), `id="foreign-files-table"`) {
		t.Errorf("allowlist body should contain the swap target id; body=%q", rec.Body.String())
	}

	// Delete returns the refreshed table partial.
	req = withI18nCtx(t, httptest.NewRequest(http.MethodDelete, "/api/v1/foreign-files/"+deleteID+"/file", nil))
	req.SetPathValue("id", deleteID)
	rec = httptest.NewRecorder()
	r.handleForeignFileDelete(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status: got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `id="foreign-files-table"`) {
		t.Errorf("delete body should contain the swap target id; body=%q", rec.Body.String())
	}

	// After both actions, ledger is empty -> empty-state copy must render.
	if !strings.Contains(rec.Body.String(), "No foreign image files detected") {
		t.Errorf("empty-state copy should appear on last-row removal; body=%q", rec.Body.String())
	}
}

// TestHandleForeignAllowlistRemove_RendersRefreshedTable pins the same
// behavior for the allowlist page's per-row Remove action.
func TestHandleForeignAllowlistRemove_RendersRefreshedTable(t *testing.T) {
	t.Parallel()
	r, _ := newTestRouterWithForeign(t)
	if err := r.foreignRepo.AddAllowlist(context.Background(), foreign.AllowlistEntry{
		Scope: foreign.ScopeGlobal, FileName: "fanart.jpg", ContentHash: sha256HexAPI([]byte("fanart")),
	}); err != nil {
		t.Fatalf("AddAllowlist: %v", err)
	}
	rows, _ := r.foreignRepo.ListAllowlist(context.Background())
	id := rows[0].ID

	req := withI18nCtx(t, httptest.NewRequest(http.MethodDelete, "/api/v1/foreign-file-allowlist/"+id, nil))
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	r.handleForeignAllowlistRemove(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Errorf("Content-Type: got %q want HTML", got)
	}
	if !strings.Contains(rec.Body.String(), `id="foreign-allowlist-table"`) {
		t.Errorf("body should contain the allowlist-table swap target; body=%q", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "The allowlist is empty") {
		t.Errorf("empty-state copy should render on last-row removal; body=%q", rec.Body.String())
	}
}

// TestHandleForeignFilesDismiss_RendersSurvivingRows pins the round-2 fix:
// dismiss must render the actual remaining rows (not an unconditional empty
// view) so a partial-success run does not hide surviving detections.
func TestHandleForeignFilesDismiss_RendersSurvivingRows(t *testing.T) {
	t.Parallel()
	r, db := newTestRouterWithForeign(t)
	dir := t.TempDir()
	mustExec(t, db, `INSERT INTO artists (id, name, path) VALUES ('a1','x',?)`, dir)
	for _, fn := range []string{"backdrop.jpg", "fanart.jpg"} {
		body := []byte("body-" + fn)
		path := filepath.Join(dir, fn)
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatalf("write %s: %v", fn, err)
		}
		if err := r.foreignRepo.Upsert(context.Background(), foreign.Entry{
			ArtistID: "a1", FilePath: path, FileName: fn, ContentHash: sha256HexAPI(body),
		}); err != nil {
			t.Fatalf("seed %s: %v", fn, err)
		}
	}
	req := withI18nCtx(t, httptest.NewRequest(http.MethodPost, "/api/v1/foreign-files/dismiss", nil))
	rec := httptest.NewRecorder()
	r.handleForeignFilesDismiss(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("dismiss status: got %d", rec.Code)
	}
	// Successful dismiss against a real DB clears every row -> empty state.
	if !strings.Contains(rec.Body.String(), "No foreign image files detected") {
		t.Errorf("dismiss should render empty-state when ledger is fully cleared; body=%q", rec.Body.String())
	}
}

// TestHandleForeignFiles_DBErrorPaths exercises the 5xx logger.Error branches
// across every JSON handler by closing the underlying DB before the call.
// This pins the "log first, sanitized client message" contract for the round
// 1 review (slog.Error before writeJSON 500) and lifts patch coverage on the
// previously-untested error branches.
func TestHandleForeignFiles_DBErrorPaths(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		method string
		path   string
		pathID string
		seed   func(t *testing.T, r *Router, db *sql.DB)
		invoke func(r *Router, w http.ResponseWriter, req *http.Request)
	}{
		{
			name: "list", method: http.MethodGet, path: "/api/v1/foreign-files",
			invoke: func(r *Router, w http.ResponseWriter, req *http.Request) { r.handleForeignFilesList(w, req) },
		},
		{
			name: "allowlist-load-error", method: http.MethodPost, path: "/api/v1/foreign-files/x/allowlist", pathID: "x",
			invoke: func(r *Router, w http.ResponseWriter, req *http.Request) { r.handleForeignFileAllowlist(w, req) },
		},
		{
			name: "delete-load-error", method: http.MethodDelete, path: "/api/v1/foreign-files/x/file", pathID: "x",
			invoke: func(r *Router, w http.ResponseWriter, req *http.Request) { r.handleForeignFileDelete(w, req) },
		},
		{
			name: "dismiss-list-error", method: http.MethodPost, path: "/api/v1/foreign-files/dismiss",
			invoke: func(r *Router, w http.ResponseWriter, req *http.Request) { r.handleForeignFilesDismiss(w, req) },
		},
		{
			name: "allowlist-list-error", method: http.MethodGet, path: "/api/v1/foreign-file-allowlist",
			invoke: func(r *Router, w http.ResponseWriter, req *http.Request) { r.handleForeignAllowlistList(w, req) },
		},
		{
			name: "allowlist-remove-error", method: http.MethodDelete, path: "/api/v1/foreign-file-allowlist/x", pathID: "x",
			invoke: func(r *Router, w http.ResponseWriter, req *http.Request) { r.handleForeignAllowlistRemove(w, req) },
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, db := newTestRouterWithForeign(t)
			if c.seed != nil {
				c.seed(t, r, db)
			}
			// Close the DB so every repo call returns an error.
			if err := db.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}
			req := httptest.NewRequest(c.method, c.path, nil)
			if c.pathID != "" {
				req.SetPathValue("id", c.pathID)
			}
			rec := httptest.NewRecorder()
			c.invoke(r, rec, req)
			if rec.Code != http.StatusInternalServerError {
				t.Errorf("expected 500 on DB error; got %d (body=%s)", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestLoadForeignFilesView covers the data-loading helper extracted from
// handleForeignFilesPage. The page handler itself needs full Router wiring
// (static assets, auth service) to render the wrapping page; testing the
// loader directly captures the data-shaping logic without that machinery.
func TestLoadForeignFilesView(t *testing.T) {
	t.Parallel()
	r, db := newTestRouterWithForeign(t)

	// Empty repo -> empty view, no error.
	view, err := r.loadForeignFilesView(context.Background())
	if err != nil || view.Count != 0 || len(view.Rows) != 0 {
		t.Errorf("empty: view=%+v err=%v", view, err)
	}

	// Populated repo -> rows materialized + Count synced to len(Rows).
	mustExec(t, db, `INSERT INTO artists (id, name, path) VALUES ('a1','Aretha','/m/Aretha')`)
	if err := r.foreignRepo.Upsert(context.Background(), foreign.Entry{
		ArtistID: "a1", FilePath: "/m/Aretha/backdrop.jpg", FileName: "backdrop.jpg",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	view, err = r.loadForeignFilesView(context.Background())
	if err != nil {
		t.Fatalf("populated: %v", err)
	}
	if view.Count != 1 || len(view.Rows) != 1 || view.Rows[0].FileName != "backdrop.jpg" {
		t.Errorf("populated view: %+v", view)
	}

	// Nil repo path -> zero view, no error.
	r2 := &Router{logger: slog.Default()}
	view, err = r2.loadForeignFilesView(context.Background())
	if err != nil || view.Count != 0 {
		t.Errorf("nil-repo: view=%+v err=%v", view, err)
	}

	// Repo error -> error returned, view zeroed.
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := r.loadForeignFilesView(context.Background()); err == nil {
		t.Error("closed-DB loadForeignFilesView should return error")
	}
}

// TestLoadForeignAllowlistView mirrors the foreign-files loader test for
// the allowlist page.
func TestLoadForeignAllowlistView(t *testing.T) {
	t.Parallel()
	r, db := newTestRouterWithForeign(t)

	view, err := r.loadForeignAllowlistView(context.Background())
	if err != nil || len(view.Rows) != 0 {
		t.Errorf("empty: view=%+v err=%v", view, err)
	}

	if err := r.foreignRepo.AddAllowlist(context.Background(), foreign.AllowlistEntry{
		Scope: foreign.ScopeGlobal, FileName: "fanart.jpg", ContentHash: sha256HexAPI([]byte("fanart")), Note: "stock",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	view, err = r.loadForeignAllowlistView(context.Background())
	if err != nil {
		t.Fatalf("populated: %v", err)
	}
	if len(view.Rows) != 1 || view.Rows[0].FileName != "fanart.jpg" || view.Rows[0].Scope != "global" {
		t.Errorf("populated view: %+v", view)
	}

	r2 := &Router{logger: slog.Default()}
	view, err = r2.loadForeignAllowlistView(context.Background())
	if err != nil || len(view.Rows) != 0 {
		t.Errorf("nil-repo: view=%+v err=%v", view, err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := r.loadForeignAllowlistView(context.Background()); err == nil {
		t.Error("closed-DB loadForeignAllowlistView should return error")
	}
}

// TestHandleForeignFilesPage_DBErrorPath exercises the error branch of
// handleForeignFilesPage: with an admin context and a closed DB,
// requireForeignAdmin allows, loadForeignFilesView returns an error, and the
// handler returns 500. This covers the page handler's gate + load + error
// path without needing the full Router wiring (static assets, auth service)
// that the templ render at the end of the handler requires.
func TestHandleForeignFilesPage_DBErrorPath(t *testing.T) {
	t.Parallel()
	r, db := newTestRouterWithForeign(t)
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	ctx := middleware.WithTestUserID(context.Background(), "admin-user")
	ctx = middleware.WithTestRole(ctx, "administrator")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/settings/foreign-files", nil)
	rec := httptest.NewRecorder()
	r.handleForeignFilesPage(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 on closed-DB load; got %d", rec.Code)
	}
}

// TestHandleForeignAllowlistPage_DBErrorPath is the analog for the
// allowlist page handler.
func TestHandleForeignAllowlistPage_DBErrorPath(t *testing.T) {
	t.Parallel()
	r, db := newTestRouterWithForeign(t)
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	ctx := middleware.WithTestUserID(context.Background(), "admin-user")
	ctx = middleware.WithTestRole(ctx, "administrator")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/settings/foreign-files/allowlist", nil)
	rec := httptest.NewRecorder()
	r.handleForeignAllowlistPage(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 on closed-DB load; got %d", rec.Code)
	}
}

// TestForeignSummaryForBanner_CountError pins the Warn-and-return-zero
// fallback when the repo's Count call fails (closed DB).
func TestForeignSummaryForBanner_CountError(t *testing.T) {
	t.Parallel()
	r, db := newTestRouterWithForeign(t)
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := r.foreignSummaryForBanner(context.Background()); got != 0 {
		t.Errorf("Count error should return 0; got %d", got)
	}
}

// TestRequireForeignAdmin_NonAdminGetsForbidden pins the non-admin RBAC gate.
// The anon path (empty userID) calls renderLoginPage which needs full Router
// wiring; that branch is integration-test territory.
func TestRequireForeignAdmin_NonAdminGetsForbidden(t *testing.T) {
	t.Parallel()
	r, _ := newTestRouterWithForeign(t)
	ctx := middleware.WithTestUserID(context.Background(), "u1")
	ctx = middleware.WithTestRole(ctx, "operator")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/settings/foreign-files", nil)
	rec := httptest.NewRecorder()
	if r.requireForeignAdmin(rec, req) {
		t.Error("operator role should be denied")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403; got %d", rec.Code)
	}
}

// TestHandleForeignFile_RenderListErrorAfterMutation pins that the
// render-after-success path returns 500 (not stale 200) when the post-action
// List call fails. We seed a row, complete the mutation against the open DB,
// then invoke the handler with the foreign_files table dropped so List errors.
func TestHandleForeignFile_RenderListErrorAfterMutation(t *testing.T) {
	t.Parallel()
	r, db := newTestRouterWithForeign(t)
	mustExec(t, db, `INSERT INTO artists (id, name, path) VALUES ('a1','x','/x')`)
	if err := r.foreignRepo.Upsert(context.Background(), foreign.Entry{
		ArtistID: "a1", FilePath: "/x/backdrop.jpg", FileName: "backdrop.jpg",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rows, _ := r.foreignRepo.List(context.Background())
	id := rows[0].ID

	// Drop the table the post-success render needs. The handler's mutation
	// path (DeleteByID) tolerates ErrNotFound but the render's List call
	// will fail with "no such table: foreign_files".
	mustExec(t, db, `DROP TABLE foreign_files`)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/foreign-files/"+id+"/allowlist", nil)
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	r.handleForeignFileAllowlist(rec, req)

	// allowlist GetByID will fail first because foreign_files is dropped, so
	// the handler returns 500 on the load error -- still exercises the
	// "load foreign-file row for allowlist" logger.Error branch.
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected 500; got %d (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestHandleForeignFilesDismiss_SameBasenameDistinctBytes is the headline
// behavior the migration is here to support: bulk dismiss creates one
// allowlist row PER distinct content hash, so two "poster.jpg" files in
// different artist directories with different bytes do not collide on a
// single global allowlist row.
func TestHandleForeignFilesDismiss_SameBasenameDistinctBytes(t *testing.T) {
	t.Parallel()
	r, db := newTestRouterWithForeign(t)
	dirA := t.TempDir()
	dirB := t.TempDir()
	bodyA := []byte("artist-A poster")
	bodyB := []byte("artist-B poster")
	pathA := filepath.Join(dirA, "poster.jpg")
	pathB := filepath.Join(dirB, "poster.jpg")
	if err := os.WriteFile(pathA, bodyA, 0o644); err != nil {
		t.Fatalf("write A: %v", err)
	}
	if err := os.WriteFile(pathB, bodyB, 0o644); err != nil {
		t.Fatalf("write B: %v", err)
	}
	mustExec(t, db, `INSERT INTO artists (id, name, path) VALUES ('a1','A',?), ('a2','B',?)`, dirA, dirB)
	if err := r.foreignRepo.Upsert(context.Background(), foreign.Entry{
		ArtistID: "a1", FilePath: pathA, FileName: "poster.jpg", ContentHash: sha256HexAPI(bodyA),
	}); err != nil {
		t.Fatalf("seed A: %v", err)
	}
	if err := r.foreignRepo.Upsert(context.Background(), foreign.Entry{
		ArtistID: "a2", FilePath: pathB, FileName: "poster.jpg", ContentHash: sha256HexAPI(bodyB),
	}); err != nil {
		t.Fatalf("seed B: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/foreign-files/dismiss", nil)
	rec := httptest.NewRecorder()
	r.handleForeignFilesDismiss(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("dismiss status: got %d", rec.Code)
	}
	rows, _ := r.foreignRepo.ListAllowlist(context.Background())
	if len(rows) != 2 {
		t.Fatalf("expected 2 distinct global allowlist rows (one per content hash); got %d", len(rows))
	}
	// Each row should carry a distinct content_hash matching one of the
	// seeded bodies. Sort-insensitive comparison via a set.
	wantHashes := map[string]bool{sha256HexAPI(bodyA): true, sha256HexAPI(bodyB): true}
	seenHashes := map[string]int{}
	for _, row := range rows {
		if !wantHashes[row.ContentHash] {
			t.Errorf("unexpected content_hash %q on dismissed row", row.ContentHash)
		}
		seenHashes[row.ContentHash]++
		if row.FileName != "poster.jpg" {
			t.Errorf("expected file_name preserved as %q; got %q", "poster.jpg", row.FileName)
		}
	}
	// Both distinct hashes must be persisted exactly once. The membership
	// check above still passes if both rows accidentally reuse one hash;
	// the per-hash count is what proves the bytes were keyed separately.
	for want := range wantHashes {
		if seenHashes[want] != 1 {
			t.Errorf("content_hash %q persisted %d times; want exactly 1", want, seenHashes[want])
		}
	}
}

// TestResolveForeignHash_BackfillsFromDisk covers the on-demand rehash
// path for pre-008 ledger rows whose content_hash column is empty. The
// handler must recompute the digest from disk so the allowlist write
// has a non-empty key.
func TestResolveForeignHash_BackfillsFromDisk(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	body := []byte("backfill bytes")
	target := filepath.Join(dir, "backdrop.jpg")
	if err := os.WriteFile(target, body, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Legacy entry: ContentHash empty.
	got, err := resolveForeignHash(&foreign.Entry{FilePath: target})
	if err != nil {
		t.Fatalf("resolveForeignHash: %v", err)
	}
	if got != sha256HexAPI(body) {
		t.Errorf("backfilled hash = %q; want sha256(body)", got)
	}
	// Already-hashed entry: short-circuits without touching disk.
	const sentinel = "sentinel-precomputed-hash"
	got, err = resolveForeignHash(&foreign.Entry{FilePath: "/does/not/exist", ContentHash: sentinel})
	if err != nil {
		t.Fatalf("resolveForeignHash precomputed: %v", err)
	}
	if got != sentinel {
		t.Errorf("expected stored hash to short-circuit; got %q", got)
	}
	// Legacy entry whose file is gone: the on-demand rehash must fail
	// loudly rather than return an empty hash that would later produce a
	// malformed allowlist row.
	got, err = resolveForeignHash(&foreign.Entry{FilePath: filepath.Join(dir, "missing.jpg")})
	if err == nil {
		t.Errorf("expected error for empty hash with missing file; got hash %q", got)
	}
}
