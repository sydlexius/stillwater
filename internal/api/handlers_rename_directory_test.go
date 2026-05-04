package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/event"
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	r, _, _ := renameHandlerFixture(t)
	req, w := renameRequest(t, "no-such-artist", "Anything")
	r.handleArtistRenameDirectory(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (body: %s)", w.Code, w.Body.String())
	}
}

func TestHandleArtistRenameDirectory_Locked(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// TestHandleArtistRenameDirectory_MalformedFormBody covers the ParseForm
// error path in extractRenameDirname. A literal "%" with no following hex
// digits is invalid percent-encoding and trips ParseForm before the
// PostForm lookup, which the handler maps to HTTP 400. Without this case
// the err != nil branch returning "invalid form body" is unreachable in
// tests.
func TestHandleArtistRenameDirectory_MalformedFormBody(t *testing.T) {
	t.Parallel()
	r, a, _ := renameHandlerFixture(t)
	// "%" is an incomplete percent-escape; ParseForm rejects with
	// "invalid URL escape".
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/artists/"+a.ID+"/rename-directory", strings.NewReader("new_dirname=%"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()
	r.handleArtistRenameDirectory(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (body: %s)", w.Code, w.Body.String())
	}
}

// TestHandleArtistRenameDirectory_PublishesEvent verifies the eventBus.Publish
// branch on a successful rename. testRouter does not wire an event bus by
// default, leaving the publish path unexecuted by every other test in this
// file. Here we attach a real bus, subscribe to ArtistUpdated, drive a
// rename, and confirm the subscriber observed the event with the right
// artist_id payload. Covers both the nil-check and the Publish call site.
func TestHandleArtistRenameDirectory_PublishesEvent(t *testing.T) {
	t.Parallel()
	r, a, _ := renameHandlerFixture(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	bus := event.NewBus(logger, 4)
	r.eventBus = bus

	received := make(chan event.Event, 1)
	bus.Subscribe(event.ArtistUpdated, func(e event.Event) {
		// Non-blocking send: we only care about the first event.
		select {
		case received <- e:
		default:
		}
	})

	// Bus.Start blocks until Stop. Run it in a goroutine for the duration
	// of the test so Publish actually dispatches to subscribers.
	go bus.Start()
	t.Cleanup(bus.Stop)

	req, w := renameRequest(t, a.ID, "Bus Renamed")
	r.handleArtistRenameDirectory(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	select {
	case e := <-received:
		if e.Data["artist_id"] != a.ID {
			t.Errorf("event artist_id = %v, want %q", e.Data["artist_id"], a.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for ArtistUpdated event after successful rename")
	}
}

// TestHandleArtistRenameDirectory_FilesystemError500 covers the handler's
// default error branch (not one of the named sentinels). With the parent
// directory mode 0500, the service's RenameDirAtomic fails with EACCES,
// which the service wraps as a generic error. The handler must map that to
// HTTP 500, log the failure, and surface a non-empty body. We use atomic
// counters off the slog handler call to assert the log fired so a future
// refactor that drops the slog.Error in the default branch is caught.
func TestHandleArtistRenameDirectory_FilesystemError500(t *testing.T) {
	t.Parallel()
	// Root bypasses POSIX permission bits, so the chmod 0500 below would
	// not produce EACCES and the default 500 branch we want to exercise
	// would never fire. Same skip pattern used elsewhere in the repo.
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permission bits; cannot trigger EACCES")
	}

	r, a, root := renameHandlerFixture(t)

	// Strip write permission so any rename or copy under the parent fails.
	if err := os.Chmod(root, 0o500); err != nil {
		t.Fatalf("chmod parent ro: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })

	var logged atomic.Int32
	r.logger = slog.New(countingHandler{count: &logged})

	req, w := renameRequest(t, a.ID, "Cannot Rename")
	r.handleArtistRenameDirectory(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body: %s)", w.Code, w.Body.String())
	}
	if logged.Load() == 0 {
		t.Errorf("expected handler to log unmapped error in the default 500 branch")
	}
}

// countingHandler is a minimal slog.Handler that tallies records at Error
// level. Used by FilesystemError500 to confirm the handler's default branch
// emitted the diagnostic log.
type countingHandler struct {
	count *atomic.Int32
}

func (h countingHandler) Enabled(_ context.Context, lvl slog.Level) bool {
	return lvl >= slog.LevelError
}
func (h countingHandler) Handle(_ context.Context, _ slog.Record) error {
	h.count.Add(1)
	return nil
}
func (h countingHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h countingHandler) WithGroup(_ string) slog.Handler      { return h }
