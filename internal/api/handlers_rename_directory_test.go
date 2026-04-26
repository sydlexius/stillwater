package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
)

// renameHandlerFixture seeds an artist whose directory exists on a temp
// filesystem path, then returns the router, the artist row, and the path
// root so individual cases can construct collision targets.
func renameHandlerFixture(t *testing.T) (*Router, *artist.Artist, string) {
	t.Helper()
	r, svc := testRouter(t)
	ctx := context.Background()
	root := t.TempDir()

	dir := filepath.Join(root, "Rename Me")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("creating artist dir: %v", err)
	}

	a := &artist.Artist{Name: "Rename Me", Path: dir}
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}
	return r, a, root
}

func renameRequest(t *testing.T, artistID, newDirname string) (*http.Request, *httptest.ResponseRecorder) {
	t.Helper()
	body, err := json.Marshal(map[string]string{"new_dirname": newDirname})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/artists/"+artistID+"/rename-directory", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", artistID)
	return req, httptest.NewRecorder()
}

func TestHandleArtistRenameDirectory_Happy(t *testing.T) {
	r, a, root := renameHandlerFixture(t)
	req, w := renameRequest(t, a.ID, "Renamed")
	r.handleArtistRenameDirectory(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	wantPath := filepath.Join(root, "Renamed")
	if resp["new_path"] != wantPath {
		t.Errorf("new_path = %q, want %q", resp["new_path"], wantPath)
	}
	if resp["status"] != "renamed" {
		t.Errorf("status = %q, want \"renamed\"", resp["status"])
	}
}

func TestHandleArtistRenameDirectory_BadInput(t *testing.T) {
	r, a, _ := renameHandlerFixture(t)
	cases := map[string]string{
		"empty":         "",
		"dot":           ".",
		"dotdot":        "..",
		"forward-slash": "with/slash",
		"back-slash":    "with\\back",
	}
	for label, input := range cases {
		t.Run(label, func(t *testing.T) {
			req, w := renameRequest(t, a.ID, input)
			r.handleArtistRenameDirectory(w, req)
			if w.Code != http.StatusBadRequest {
				t.Errorf("input %q: status = %d, want 400 (body: %s)", input, w.Code, w.Body.String())
			}
		})
	}
}

func TestHandleArtistRenameDirectory_NotFound(t *testing.T) {
	r, _, _ := renameHandlerFixture(t)
	req, w := renameRequest(t, "no-such-artist", "Anything")
	r.handleArtistRenameDirectory(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (body: %s)", w.Code, w.Body.String())
	}
}

func TestHandleArtistRenameDirectory_Locked(t *testing.T) {
	r, a, _ := renameHandlerFixture(t)
	if err := r.artistService.Lock(context.Background(), a.ID, "user"); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	req, w := renameRequest(t, a.ID, "New Name")
	r.handleArtistRenameDirectory(w, req)
	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409 (body: %s)", w.Code, w.Body.String())
	}
}

func TestHandleArtistRenameDirectory_DestExists(t *testing.T) {
	r, a, root := renameHandlerFixture(t)
	if err := os.Mkdir(filepath.Join(root, "Already Here"), 0o755); err != nil {
		t.Fatalf("creating collision target: %v", err)
	}
	req, w := renameRequest(t, a.ID, "Already Here")
	r.handleArtistRenameDirectory(w, req)
	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409 (body: %s)", w.Code, w.Body.String())
	}
}

func TestHandleArtistRenameDirectory_NoChange(t *testing.T) {
	r, a, _ := renameHandlerFixture(t)
	req, w := renameRequest(t, a.ID, filepath.Base(a.Path))
	r.handleArtistRenameDirectory(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (body: %s)", w.Code, w.Body.String())
	}
}

// TestHandleArtistRenameDirectory_FormBody covers the
// application/x-www-form-urlencoded branch of extractRenameDirname. The
// handler godoc documents both JSON and form bodies as supported, but
// every other test in this file uses JSON, leaving the form path
// uncovered. A regression that breaks ParseForm or the PostForm lookup
// would silently pass without this case.
func TestHandleArtistRenameDirectory_FormBody(t *testing.T) {
	r, a, root := renameHandlerFixture(t)
	form := url.Values{}
	form.Set("new_dirname", "Form Renamed")
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/artists/"+a.ID+"/rename-directory", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()
	r.handleArtistRenameDirectory(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	wantPath := filepath.Join(root, "Form Renamed")
	if _, err := os.Stat(wantPath); err != nil {
		t.Errorf("expected new directory on disk: %v", err)
	}
}

func TestHandleArtistRenameDirectory_InvalidJSON(t *testing.T) {
	r, a, _ := renameHandlerFixture(t)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/artists/"+a.ID+"/rename-directory", bytes.NewReader([]byte(`{not json`)))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()
	r.handleArtistRenameDirectory(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (body: %s)", w.Code, w.Body.String())
	}
}
