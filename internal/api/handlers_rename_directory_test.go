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
	"github.com/sydlexius/stillwater/internal/connection"
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
	// Use map[string]any because the response now carries a platforms
	// slice alongside the two string fields (#1222, #1231). The slice is
	// empty on this fixture (no platform mappings seeded) but the field
	// is always emitted so clients can range over it unconditionally.
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	wantPath := filepath.Join(root, "Renamed")
	if got, _ := resp["new_path"].(string); got != wantPath {
		t.Errorf("new_path = %q, want %q", got, wantPath)
	}
	if got, _ := resp["status"].(string); got != "renamed" {
		t.Errorf("status = %q, want \"renamed\"", got)
	}
	platforms, ok := resp["platforms"].([]any)
	if !ok {
		t.Fatalf("platforms key missing or wrong type: %v", resp["platforms"])
	}
	if len(platforms) != 0 {
		t.Errorf("platforms = %v, want empty slice (no mappings seeded)", platforms)
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

// TestHandleArtistRenameDirectory_PlatformsInResponse drives the full
// happy-path through the publish.Publisher.SyncRename hook with a real
// httptest Emby peer + a Lidarr peer that simulates a 500. Confirms the
// handler emits one entry per artist_platform_ids row with the right
// (connection_id, result, error) shape and that a single platform failure
// does NOT cause the rename itself to fail or roll back (#1222, #1231).
func TestHandleArtistRenameDirectory_PlatformsInResponse(t *testing.T) {
	t.Parallel()
	r, a, _ := renameHandlerFixture(t)
	ctx := context.Background()

	// Emby stub: GET /Users/{u}/Items/emby-pid returns minimal item;
	// POST /Items/emby-pid returns 204 (success).
	embySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Id":"emby-pid","Name":"X","Path":"/old"}`))
		case http.MethodPost:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer embySrv.Close()

	// Lidarr stub: GET /api/v1/artist/lid-42 returns minimal artist; PUT
	// returns 500 so the per-platform failure path is exercised end-to-end.
	lidarrSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":42,"artistName":"X","path":"/old"}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer lidarrSrv.Close()

	// Seed both connections and the platform-id mappings. The connection
	// service decrypts api_key at read time, so use Create which encrypts.
	for _, c := range []*connection.Connection{
		{ID: "c-emby", Name: "emby", Type: connection.TypeEmby, URL: embySrv.URL, APIKey: "k", Enabled: true, PlatformUserID: "u1"},
		{ID: "c-lid", Name: "lid", Type: connection.TypeLidarr, URL: lidarrSrv.URL, APIKey: "k", Enabled: true},
	} {
		if err := r.connectionService.Create(ctx, c); err != nil {
			t.Fatalf("seed connection %s: %v", c.ID, err)
		}
	}
	if err := r.artistService.SetPlatformID(ctx, a.ID, "c-emby", "emby-pid"); err != nil {
		t.Fatalf("set emby platform id: %v", err)
	}
	if err := r.artistService.SetPlatformID(ctx, a.ID, "c-lid", "42"); err != nil {
		t.Fatalf("set lidarr platform id: %v", err)
	}

	req, w := renameRequest(t, a.ID, "Renamed With Platforms")
	r.handleArtistRenameDirectory(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp struct {
		Status    string                       `json:"status"`
		NewPath   string                       `json:"new_path"`
		Platforms []artist.PlatformRemapResult `json:"platforms"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "renamed" {
		t.Errorf("status = %q, want \"renamed\"", resp.Status)
	}
	if len(resp.Platforms) != 2 {
		t.Fatalf("platforms: got %d, want 2 (one per seeded mapping)", len(resp.Platforms))
	}
	byConn := map[string]artist.PlatformRemapResult{}
	for _, p := range resp.Platforms {
		byConn[p.ConnectionID] = p
	}
	if byConn["c-emby"].Result != artist.PlatformRemapOK {
		t.Errorf("emby: got %q (err=%q), want ok", byConn["c-emby"].Result, byConn["c-emby"].Error)
	}
	if byConn["c-lid"].Result != artist.PlatformRemapFailed {
		t.Errorf("lidarr: got %q, want failed (peer returned 500)", byConn["c-lid"].Result)
	}
	if byConn["c-lid"].Error == "" {
		t.Error("lidarr Error empty; expected wrapped 500 message")
	}

	// Lock the omitempty contract: a successful (Emby) entry's JSON must
	// have NO `error` field. We re-decode the raw body into map[string]any
	// because the typed decode above can't distinguish "absent" from
	// "present but empty"; the OpenAPI contract is the former.
	var raw map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("re-decode raw: %v", err)
	}
	rawPlatforms, _ := raw["platforms"].([]any)
	for _, p := range rawPlatforms {
		entry, _ := p.(map[string]any)
		if entry["connection_id"] == "c-emby" {
			if _, present := entry["error"]; present {
				t.Errorf("emby entry has error field present; want absent on ok result (omitempty contract): %v", entry)
			}
		}
	}
}

// TestHandleArtistRenameDirectory_DisabledConnectionOmitsError seeds a
// disabled connection mapped to the artist, exercises the rename, and
// asserts the response (HTTP 200) lists the disabled connection with
// result=ok AND no `error` field. The raw map decode (vs typed
// PlatformRemapResult struct) is load-bearing: the OpenAPI contract is
// "error present only when result is failed", so an empty-string `error`
// would be a regression. omitempty on the struct tag enforces this, and
// this test guards the wire contract against a future tag drop.
func TestHandleArtistRenameDirectory_DisabledConnectionOmitsError(t *testing.T) {
	t.Parallel()
	r, a, _ := renameHandlerFixture(t)
	ctx := context.Background()

	// Seed a single disabled Emby connection mapped to the artist. The
	// peer URL is intentionally bogus: the disabled-skip branch returns
	// before any HTTP call so the URL never matters.
	c := &connection.Connection{
		ID: "c-off", Name: "off", Type: connection.TypeEmby,
		URL: "http://disabled.invalid", APIKey: "k",
		Enabled: false, PlatformUserID: "u1",
	}
	if err := r.connectionService.Create(ctx, c); err != nil {
		t.Fatalf("seed disabled connection: %v", err)
	}
	if err := r.artistService.SetPlatformID(ctx, a.ID, "c-off", "emby-pid"); err != nil {
		t.Fatalf("set platform id: %v", err)
	}

	req, w := renameRequest(t, a.ID, "Renamed With Disabled Peer")
	r.handleArtistRenameDirectory(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	// Raw decode (not typed) so we can distinguish absent vs empty-string
	// for the `error` field; the typed PlatformRemapResult always
	// materializes Error as "" even when the JSON omits it.
	var raw map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	rawPlatforms, ok := raw["platforms"].([]any)
	if !ok {
		t.Fatalf("platforms key missing or wrong type: %v", raw["platforms"])
	}
	if len(rawPlatforms) != 1 {
		t.Fatalf("platforms: got %d, want 1 (single disabled mapping)", len(rawPlatforms))
	}
	entry, _ := rawPlatforms[0].(map[string]any)
	if got := entry["connection_id"]; got != "c-off" {
		t.Errorf("connection_id = %v, want c-off", got)
	}
	if got := entry["result"]; got != "ok" {
		t.Errorf("result = %v, want ok (disabled connection is a no-op success)", got)
	}
	if _, present := entry["error"]; present {
		t.Errorf("error field present on disabled-skip entry; want absent per OpenAPI \"present only when result is failed\": %v", entry)
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
