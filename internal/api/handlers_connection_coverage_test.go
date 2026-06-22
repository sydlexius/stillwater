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
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/auth"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/encryption"
	"github.com/sydlexius/stillwater/internal/library"
	"github.com/sydlexius/stillwater/internal/nfo"
	"github.com/sydlexius/stillwater/internal/platform"
	"github.com/sydlexius/stillwater/internal/rule"
)

// newConnectionTestRouter wires a minimal Router for connection-handler tests.
// The helper is prefixed to avoid colliding with sibling M49 W5 agents.
func newConnectionTestRouter(t *testing.T) *Router {
	t.Helper()

	db := newTestDB(t)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	enc, _, err := encryption.NewEncryptor("")
	if err != nil {
		t.Fatalf("encryptor: %v", err)
	}

	libSvc := library.NewService(db)
	artistSvc := artist.NewService(db)
	authSvc := auth.NewService(db)
	connSvc := connection.NewService(db, enc)
	ruleSvc := rule.NewService(db)
	if err := ruleSvc.SeedDefaults(context.Background()); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}
	nfoSnapSvc := nfo.NewSnapshotService(db)
	platformSvc := platform.NewService(db)

	cacheDir := filepath.Join(t.TempDir(), "cache", "images")

	return NewRouter(RouterDeps{
		SessionSecret:      testSessionSecret,
		AuthService:        authSvc,
		ArtistService:      artistSvc,
		ConnectionService:  connSvc,
		LibraryService:     libSvc,
		PlatformService:    platformSvc,
		RuleService:        ruleSvc,
		NFOSnapshotService: nfoSnapSvc,
		DB:                 db,
		Logger:             logger,
		StaticFS:           os.DirFS("../../web/static"),
		ImageCacheDir:      cacheDir,
	})
}

// newConnectionStubEmbyServer returns an httptest.Server that satisfies the
// minimum set of Emby endpoints the connection handlers exercise. The optional
// failPath causes that path to return 500 to drive the error branch in tests.
//
// The handler validates the inbound request contract (GET-only, auth material
// present) so a regression that drops the auth header or sends the wrong verb
// fails fast across every test that uses this stub.
func newConnectionStubEmbyServer(t *testing.T, failPath string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Emby/Jellyfin accept any one of three auth-bearing forms; require
		// at least one. Stillwater's client wraps the api key into X-Emby-Token
		// (or the legacy api_key query param) — a regression that strips both
		// surfaces as 401 here.
		if req.Header.Get("Authorization") == "" &&
			req.Header.Get("X-Emby-Token") == "" &&
			req.URL.Query().Get("api_key") == "" {
			http.Error(w, "missing auth material", http.StatusUnauthorized)
			return
		}
		if failPath != "" && strings.HasPrefix(req.URL.Path, failPath) {
			http.Error(w, "stub failure", http.StatusInternalServerError)
			return
		}
		switch {
		case strings.HasPrefix(req.URL.Path, "/System/Info"):
			_ = json.NewEncoder(w).Encode(map[string]any{"ServerName": "test-emby", "Version": "1.0", "Id": "server-1"})
		case strings.HasPrefix(req.URL.Path, "/Users"):
			_ = json.NewEncoder(w).Encode([]map[string]any{{"Id": "user-1", "Name": "admin"}})
		case strings.HasPrefix(req.URL.Path, "/Library/VirtualFolders"):
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"Name": "Music", "CollectionType": "music", "ItemId": "lib-music",
					"LibraryOptions": map[string]any{"SaveLocalMetadata": false, "MetadataSavers": []string{}}},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newConnectionTestConn persists a fixture connection record directly so the
// expensive test-before-save flow is not required during coverage tests.
func newConnectionTestConn(t *testing.T, r *Router, c *connection.Connection) {
	t.Helper()
	if err := r.connectionService.Create(context.Background(), c); err != nil {
		t.Fatalf("seeding connection: %v", err)
	}
}

// assertLidarrContract checks that an outbound request from the Stillwater
// Lidarr client carries the expected HTTP method and the X-Api-Key header
// the client sets at lidarr/client.go:491. Used at the top of every Lidarr
// test stub so a regression that drops the API-key header or sends the
// wrong verb fails the test directly instead of silently returning success.
func assertLidarrContract(t *testing.T, req *http.Request, wantMethod string) {
	t.Helper()
	if req.Method != wantMethod {
		t.Errorf("Lidarr mock: method = %s, want %s (path %s)", req.Method, wantMethod, req.URL.Path)
	}
	if req.Header.Get("X-Api-Key") == "" && req.URL.Query().Get("apikey") == "" {
		t.Errorf("Lidarr mock: missing API key on %s %s", req.Method, req.URL.Path)
	}
}

// --- handleCreateConnection ---------------------------------------------------

// TestHandleCreateConnection_JSON_SkipTest_Lidarr exercises the JSON request
// branch, with skip_test=true so no outbound network call is made. Lidarr
// connections default the library/nfo/image-write feature flags to false,
// which is asserted from the persisted record.
func TestHandleCreateConnection_JSON_SkipTest_Lidarr(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)

	body := strings.NewReader(`{"name":"Lidarr1","type":"lidarr","url":"http://lidarr.local:8686","api_key":"k","enabled":true,"skip_test":true}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/connections", body)
	req.Header.Set("Content-Type", "application/json")

	w := serveValidated(t, http.HandlerFunc(r.handleCreateConnection), req)

	// New connection (no dedupe) on the JSON path: handler at
	// handlers_connection.go:315 returns 201 Created.
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", w.Code, w.Body.String())
	}

	conns, err := r.connectionService.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(conns) != 1 {
		t.Fatalf("len(conns) = %d, want 1", len(conns))
	}
	got := conns[0]
	if got.Type != connection.TypeLidarr {
		t.Errorf("type = %q, want lidarr", got.Type)
	}
	if got.GetFeatureLibraryImport() || got.GetFeatureNFOWrite() || got.GetFeatureImageWrite() {
		t.Errorf("lidarr features should default false: import=%v nfo=%v image=%v",
			got.GetFeatureLibraryImport(), got.GetFeatureNFOWrite(), got.GetFeatureImageWrite())
	}
}

// TestHandleCreateConnection_FormSkipTest covers the form-encoded branch and
// asserts that skip_test=true bypasses the live test call.
func TestHandleCreateConnection_FormSkipTest(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)

	form := url.Values{}
	form.Set("name", "Emby Skip")
	form.Set("type", connection.TypeEmby)
	form.Set("url", "http://nonexistent.invalid:9999")
	form.Set("api_key", "k")
	form.Set("skip_test", "true")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/connections", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	w := serveValidated(t, http.HandlerFunc(r.handleCreateConnection), req)

	// Form-encoded new connection on the non-HTMX path: 201 Created.
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleCreateConnection_BadJSON checks the malformed-JSON branch returns
// 400 without persisting a record.
func TestHandleCreateConnection_BadJSON(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/connections", strings.NewReader("{not json"))
	req.Header.Set("Content-Type", "application/json")
	// NOT wrapped with serveValidated: the openapi request validator would
	// reject malformed JSON before the handler runs, but the purpose of this
	// test is to verify the handler's own 400 branch on a parse error.
	w := httptest.NewRecorder()

	r.handleCreateConnection(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	conns, err := r.connectionService.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(conns) != 0 {
		t.Errorf("len(conns) = %d, want 0", len(conns))
	}
}

// TestHandleCreateConnection_TestSuccess_WithStub uses a stub Emby server so
// the test-before-save call succeeds. This exercises the resolvePlatformUserID
// + UpdateStatus("ok") branch.
func TestHandleCreateConnection_TestSuccess_WithStub(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)
	srv := newConnectionStubEmbyServer(t, "")

	payload := map[string]any{
		"name":    "Emby Live",
		"type":    connection.TypeEmby,
		"url":     srv.URL,
		"api_key": "k",
		"enabled": true,
	}
	buf, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/connections", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")

	w := serveValidated(t, http.HandlerFunc(r.handleCreateConnection), req)

	// Live-test success creates a new row: 201 Created.
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", w.Code, w.Body.String())
	}

	conns, err := r.connectionService.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(conns) != 1 || conns[0].Status != "ok" {
		t.Errorf("conn = %+v, want one with status=ok", conns)
	}
}

// TestHandleCreateConnection_TestFailure_JSON forces the test-before-save call
// to fail by pointing at a closed loopback port. The handler must reply with
// 422 and NOT persist the connection.
func TestHandleCreateConnection_TestFailure_JSON(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)

	payload := map[string]any{
		"name":    "Emby Fail",
		"type":    connection.TypeEmby,
		"url":     "http://127.0.0.1:1",
		"api_key": "k",
	}
	buf, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/connections", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")

	w := serveValidated(t, http.HandlerFunc(r.handleCreateConnection), req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", w.Code, w.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode resp: %v", err)
	}
	if resp["status"] != "test_failed" {
		t.Errorf("status field = %q, want test_failed", resp["status"])
	}
}

// TestHandleCreateConnection_DuplicateUpdatesExisting verifies the dedupe-by
// (type, url) branch that converts a duplicate POST into an UPDATE.
func TestHandleCreateConnection_DuplicateUpdatesExisting(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)

	c := &connection.Connection{
		Name: "Existing", Type: connection.TypeLidarr,
		URL: "http://dup.local:8686", APIKey: "old", Enabled: true,
	}
	newConnectionTestConn(t, r, c)

	body := strings.NewReader(`{"name":"Renamed","type":"lidarr","url":"http://dup.local:8686","api_key":"new","enabled":true,"skip_test":true}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/connections", body)
	req.Header.Set("Content-Type", "application/json")

	w := serveValidated(t, http.HandlerFunc(r.handleCreateConnection), req)

	// Duplicate POST on (type, url) takes the dedupe-update branch at
	// handlers_connection.go:317 (`status = http.StatusOK`).
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (dedupe-update); body=%s", w.Code, w.Body.String())
	}
	conns, err := r.connectionService.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(conns) != 1 || conns[0].Name != "Renamed" {
		t.Errorf("conns = %+v, want one renamed", conns)
	}
}

// --- handleUpdateConnection ---------------------------------------------------

func TestHandleUpdateConnection_PatchesFields(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)

	c := &connection.Connection{
		Name: "Before", Type: connection.TypeEmby,
		URL: "http://emby.local:8096", APIKey: "k", Enabled: true,
	}
	newConnectionTestConn(t, r, c)

	enabled := false
	feat := true
	body, _ := json.Marshal(map[string]any{
		"name":                    "After",
		"enabled":                 enabled,
		"feature_library_import":  feat,
		"feature_nfo_write":       feat,
		"feature_image_write":     feat,
		"feature_metadata_push":   feat,
		"feature_trigger_refresh": feat,
	})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/connections/"+c.ID, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", c.ID)

	w := serveValidated(t, http.HandlerFunc(r.handleUpdateConnection), req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	got, err := r.connectionService.GetByID(context.Background(), c.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "After" || got.Enabled {
		t.Errorf("got = %+v, want name=After, enabled=false", got)
	}
	if !got.GetFeatureLibraryImport() || !got.GetFeatureNFOWrite() {
		t.Errorf("feature flags not flipped: %+v", got)
	}
}

func TestHandleUpdateConnection_NotFound(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/connections/missing",
		strings.NewReader(`{"name":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", "missing")

	w := serveValidated(t, http.HandlerFunc(r.handleUpdateConnection), req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestHandleUpdateConnection_BadJSON(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)

	c := &connection.Connection{
		Name: "Existing", Type: connection.TypeEmby,
		URL: "http://emby.local:8096", APIKey: "k", Enabled: true,
	}
	newConnectionTestConn(t, r, c)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/connections/"+c.ID, strings.NewReader("garbage"))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", c.ID)
	// NOT wrapped with serveValidated: testing the handler's parse-error 400.
	w := httptest.NewRecorder()

	r.handleUpdateConnection(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// --- handleDeleteConnection ---------------------------------------------------

func TestHandleDeleteConnection_DefaultClearsLibraryRefs(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)

	c := &connection.Connection{
		Name: "DelClear", Type: connection.TypeEmby,
		URL: "http://e.local:8096", APIKey: "k", Enabled: true,
	}
	newConnectionTestConn(t, r, c)

	lib := &library.Library{
		Name: "DelLib", Path: "", Type: library.TypeRegular,
		Source: connection.TypeEmby, ConnectionID: c.ID, ExternalID: "ext-1",
	}
	if err := r.libraryService.Create(context.Background(), lib); err != nil {
		t.Fatalf("create library: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/connections/"+c.ID, nil)
	req.SetPathValue("id", c.ID)

	w := serveValidated(t, http.HandlerFunc(r.handleDeleteConnection), req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	// The connection is gone, but the library row survives with a cleared FK.
	if _, err := r.connectionService.GetByID(context.Background(), c.ID); err == nil {
		t.Error("connection still exists after delete")
	}
	got, err := r.libraryService.GetByID(context.Background(), lib.ID)
	if err != nil {
		t.Fatalf("library GetByID: %v", err)
	}
	if got.ConnectionID != "" {
		t.Errorf("library ConnectionID = %q, want empty", got.ConnectionID)
	}
}

func TestHandleDeleteConnection_DeleteLibrariesQueryParam(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)

	c := &connection.Connection{
		Name: "DelHard", Type: connection.TypeEmby,
		URL: "http://e2.local:8096", APIKey: "k", Enabled: true,
	}
	newConnectionTestConn(t, r, c)

	lib := &library.Library{
		Name: "DelHardLib", Path: "", Type: library.TypeRegular,
		Source: connection.TypeEmby, ConnectionID: c.ID, ExternalID: "ext-2",
	}
	if err := r.libraryService.Create(context.Background(), lib); err != nil {
		t.Fatalf("create library: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete,
		"/api/v1/connections/"+c.ID+"?deleteLibraries=true", nil)
	req.SetPathValue("id", c.ID)

	w := serveValidated(t, http.HandlerFunc(r.handleDeleteConnection), req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if _, err := r.libraryService.GetByID(context.Background(), lib.ID); err == nil {
		t.Error("library still exists after deleteLibraries=true")
	}
}

func TestHandleDeleteConnection_NotFound(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/connections/no-such-id", nil)
	req.SetPathValue("id", "no-such-id")
	// NOT wrapped with serveValidated: kept as a direct invocation so the
	// operationId-coverage ratchet (which detects CallExpr selectors)
	// continues to see deleteConnection as covered. Other tests in this file
	// exercise the spec-validation surface via serveValidated.
	w := httptest.NewRecorder()

	r.handleDeleteConnection(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

// --- handleTestConnection -----------------------------------------------------

// TestHandleTestConnection_SSRFRFC1918Rejected validates that pointing
// handleTestConnection at RFC1918 addresses produces an error status.
//
// SCOPE TODAY: there is no SSRF allowlist yet (that lands in M49.5). The
// rejection observed below is produced by dial timeout / connection refused
// because nothing is listening on those networks in the test environment.
// This test therefore proves "unreachable upstream surfaces as status=error",
// which is still load-bearing for the handler contract; the genuine
// SSRF-blocking proof (a hit counter on a real loopback server) needs the
// allowlist to exist and will be added with M49.5.
//
// An earlier revision of this test included a loopback httptest server and
// asserted hit_count == 0, but that proof was bogus on two counts: (a) the
// subtests ran in parallel and the parent post-loop assertion fired BEFORE
// any subtest reached the stub, so the zero-hit count was vacuous; (b) the
// SSRF guard does not actually block loopback today, so removing the
// parallel ordering exposed the assertion as a falsehood. Loopback is
// intentionally not covered until the real guard exists.
func TestHandleTestConnection_SSRFRFC1918Rejected(t *testing.T) {
	t.Parallel()

	rfc1918Targets := []string{
		"http://10.0.0.1:1",
		"http://192.168.0.1:1",
		"http://172.16.0.1:1",
	}
	for _, target := range rfc1918Targets {
		target := target
		t.Run(target, func(t *testing.T) {
			t.Parallel()
			r := newConnectionTestRouter(t)
			c := &connection.Connection{
				Name: "SSRF " + target, Type: connection.TypeEmby,
				URL: target, APIKey: "k", Enabled: true,
			}
			newConnectionTestConn(t, r, c)

			req := httptest.NewRequest(http.MethodPost,
				"/api/v1/connections/"+c.ID+"/test", nil)
			req.SetPathValue("id", c.ID)

			w := serveValidated(t, http.HandlerFunc(r.handleTestConnection), req)

			if w.Code != http.StatusOK {
				t.Fatalf("status code = %d, want 200 (handler returns body status field)", w.Code)
			}
			var resp map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode resp: %v body=%s", err, w.Body.String())
			}
			if resp["status"] != "error" {
				t.Errorf("status = %v, want error (RFC1918 target must not report ok)", resp["status"])
			}
		})
	}
}

// TestHandleTestConnection_NotFound covers the connection-missing branch.
func TestHandleTestConnection_NotFound(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/connections/none/test", nil)
	req.SetPathValue("id", "none")
	// NOT wrapped with serveValidated: kept as a direct invocation so the
	// operationId-coverage ratchet sees testConnection as covered.
	w := httptest.NewRecorder()

	r.handleTestConnection(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

// TestHandleTestConnection_UnsupportedType covers the default branch (an
// unrecognized connection type). The test fixture writes a row with type
// "unknown" via direct DB INSERT because the service validator rejects bad
// types -- this is the only way to exercise the handler's switch default
// without smuggling the value past validation.
func TestHandleTestConnection_UnsupportedType(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)

	c := &connection.Connection{
		Name: "GoodEmby", Type: connection.TypeEmby,
		URL: "http://e.local:8096", APIKey: "k", Enabled: true,
	}
	newConnectionTestConn(t, r, c)
	// Corrupt the type after persistence -- the handler reads it via GetByID.
	if _, err := r.db.ExecContext(context.Background(),
		`UPDATE connections SET type = 'unknown' WHERE id = ?`, c.ID); err != nil {
		t.Fatalf("corrupting connection type: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/connections/"+c.ID+"/test", nil)
	req.SetPathValue("id", c.ID)

	w := serveValidated(t, http.HandlerFunc(r.handleTestConnection), req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// TestHandleTestConnection_EmbyOK uses a stub server so the success branch
// runs end-to-end, including drift detection (no conflicts on the stub).
func TestHandleTestConnection_EmbyOK(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)
	srv := newConnectionStubEmbyServer(t, "")

	c := &connection.Connection{
		Name: "OK Emby", Type: connection.TypeEmby,
		URL: srv.URL, APIKey: "k", Enabled: true,
	}
	newConnectionTestConn(t, r, c)

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/connections/"+c.ID+"/test", nil)
	req.SetPathValue("id", c.ID)

	w := serveValidated(t, http.HandlerFunc(r.handleTestConnection), req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["status"] != "ok" {
		t.Errorf("status = %v, want ok", resp["status"])
	}
}

// --- handleUpdateConnectionFeatures -------------------------------------------

func TestHandleUpdateConnectionFeatures_Patches(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)

	c := &connection.Connection{
		Name: "FeatCon", Type: connection.TypeEmby,
		URL: "http://e.local:8096", APIKey: "k", Enabled: true,
	}
	newConnectionTestConn(t, r, c)

	tr := true
	body, _ := json.Marshal(map[string]any{
		"feature_library_import":  tr,
		"feature_metadata_push":   tr,
		"feature_trigger_refresh": tr,
	})
	req := httptest.NewRequest(http.MethodPatch,
		"/api/v1/connections/"+c.ID+"/features", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", c.ID)

	w := serveValidated(t, http.HandlerFunc(r.handleUpdateConnectionFeatures), req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	got, err := r.connectionService.GetByID(context.Background(), c.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if !got.GetFeatureLibraryImport() || !got.GetFeatureMetadataPush() || !got.GetFeatureTriggerRefresh() {
		t.Errorf("features not toggled: %+v", got)
	}
}

func TestHandleUpdateConnectionFeatures_NotFound(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)

	req := httptest.NewRequest(http.MethodPatch,
		"/api/v1/connections/nope/features", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", "nope")
	// NOT wrapped with serveValidated: kept as a direct invocation so the
	// operationId-coverage ratchet sees updateConnectionFeatures as covered.
	w := httptest.NewRecorder()

	r.handleUpdateConnectionFeatures(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

// --- handleGetPlatformSettings ------------------------------------------------

func TestHandleGetPlatformSettings_Emby(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)
	srv := newConnectionStubEmbyServer(t, "")

	c := &connection.Connection{
		Name: "PSEmby", Type: connection.TypeEmby,
		URL: srv.URL, APIKey: "k", Enabled: true,
	}
	newConnectionTestConn(t, r, c)

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/connections/"+c.ID+"/platform-settings", nil)
	req.SetPathValue("id", c.ID)

	w := serveValidated(t, http.HandlerFunc(r.handleGetPlatformSettings), req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
}

func TestHandleGetPlatformSettings_NotFound(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/connections/missing/platform-settings", nil)
	req.SetPathValue("id", "missing")
	// NOT wrapped with serveValidated: kept as a direct invocation so the
	// operationId-coverage ratchet sees getPlatformSettings as covered.
	w := httptest.NewRecorder()

	r.handleGetPlatformSettings(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestHandleGetPlatformSettings_UpstreamError(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)
	// Make every Emby endpoint return 500.
	srv := newConnectionStubEmbyServer(t, "/")

	c := &connection.Connection{
		Name: "PSErr", Type: connection.TypeEmby,
		URL: srv.URL, APIKey: "k", Enabled: true,
	}
	newConnectionTestConn(t, r, c)

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/connections/"+c.ID+"/platform-settings", nil)
	req.SetPathValue("id", c.ID)

	w := serveValidated(t, http.HandlerFunc(r.handleGetPlatformSettings), req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
}

// --- handleDisablePlatformSettings --------------------------------------------

func TestHandleDisablePlatformSettings_EmbyMissingLibraryID(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)

	c := &connection.Connection{
		Name: "DisEmby", Type: connection.TypeEmby,
		URL: "http://e.local:8096", APIKey: "k", Enabled: true,
	}
	newConnectionTestConn(t, r, c)

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/connections/"+c.ID+"/platform-settings/disable",
		strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", c.ID)

	w := serveValidated(t, http.HandlerFunc(r.handleDisablePlatformSettings), req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestHandleDisablePlatformSettings_LidarrMissingConsumerID(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)

	c := &connection.Connection{
		Name: "DisLidarr", Type: connection.TypeLidarr,
		URL: "http://l.local:8686", APIKey: "k", Enabled: true,
	}
	newConnectionTestConn(t, r, c)

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/connections/"+c.ID+"/platform-settings/disable",
		strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", c.ID)

	w := serveValidated(t, http.HandlerFunc(r.handleDisablePlatformSettings), req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestHandleDisablePlatformSettings_NotFound(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/connections/missing/platform-settings/disable",
		strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", "missing")
	// NOT wrapped with serveValidated: kept as a direct invocation so the
	// operationId-coverage ratchet sees disablePlatformSettings as covered.
	w := httptest.NewRecorder()

	r.handleDisablePlatformSettings(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

// --- handleGetPlatformSummary -------------------------------------------------

func TestHandleGetPlatformSummary_Emby(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)
	srv := newConnectionStubEmbyServer(t, "")

	c := &connection.Connection{
		Name: "SumEmby", Type: connection.TypeEmby,
		URL: srv.URL, APIKey: "k", Enabled: true,
	}
	newConnectionTestConn(t, r, c)

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/connections/"+c.ID+"/platform-summary", nil)
	req.SetPathValue("id", c.ID)

	w := serveValidated(t, http.HandlerFunc(r.handleGetPlatformSummary), req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := resp["total_libraries"]; !ok {
		t.Errorf("missing total_libraries: %v", resp)
	}
}

func TestHandleGetPlatformSummary_NotFound(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/connections/missing/platform-summary", nil)
	req.SetPathValue("id", "missing")
	// NOT wrapped with serveValidated: kept as a direct invocation so the
	// operationId-coverage ratchet sees getPlatformSummary as covered.
	w := httptest.NewRecorder()

	r.handleGetPlatformSummary(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

// --- handleListConnections / handleGetConnection ------------------------------

func TestHandleListConnections_Empty(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/connections", nil)
	// NOT wrapped with serveValidated: kept as a direct invocation so the
	// operationId-coverage ratchet sees listConnections as covered.
	w := httptest.NewRecorder()
	r.handleListConnections(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var resp []connectionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp) != 0 {
		t.Errorf("len = %d, want 0", len(resp))
	}
}

func TestHandleListConnections_TwoEntries(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)

	for i, ct := range []string{connection.TypeEmby, connection.TypeLidarr} {
		c := &connection.Connection{
			Name: "Conn", Type: ct,
			URL: "http://h" + string(rune('a'+i)) + ":1", APIKey: "k", Enabled: true,
		}
		newConnectionTestConn(t, r, c)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/connections", nil)
	w := serveValidated(t, http.HandlerFunc(r.handleListConnections), req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp []connectionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp) != 2 {
		t.Errorf("len = %d, want 2", len(resp))
	}
}

func TestHandleGetConnection_OK(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)

	c := &connection.Connection{
		Name: "GetMe", Type: connection.TypeEmby,
		URL: "http://e.local:8096", APIKey: "k", Enabled: true,
	}
	newConnectionTestConn(t, r, c)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/connections/"+c.ID, nil)
	req.SetPathValue("id", c.ID)
	w := serveValidated(t, http.HandlerFunc(r.handleGetConnection), req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["id"] != c.ID || resp["name"] != "GetMe" {
		t.Errorf("resp = %+v", resp)
	}
}

func TestHandleGetConnection_NotFound(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/connections/missing", nil)
	req.SetPathValue("id", "missing")
	// NOT wrapped with serveValidated: kept as a direct invocation so the
	// operationId-coverage ratchet sees getConnection as covered.
	w := httptest.NewRecorder()
	r.handleGetConnection(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

// TestHandleCreateConnection_HTMXFailureRendersTemplate covers the HTMX
// retry-rendering branch (lines 206-213). The HX-Request header forces the
// templ render path.
func TestHandleCreateConnection_HTMXFailureRendersTemplate(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)

	payload := map[string]any{
		"name": "HTMXFail", "type": connection.TypeEmby,
		"url": "http://127.0.0.1:1", "api_key": "k",
	}
	buf, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/connections", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("HX-Request", "true")
	// NOT wrapped with serveValidated: the HTMX failure path emits a
	// text/html error fragment and kin-openapi's body decoder registry has
	// no text/html decoder, so the wrapper would fail to validate even a
	// correctly-declared response. The HTMX surface is still documented in
	// openapi.yaml as text/html content under the 422 response.
	w := httptest.NewRecorder()

	r.handleCreateConnection(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", w.Code)
	}
}

// TestHandleCreateConnection_HTMXSuccessTriggersRefresh covers the Settings
// HTMX success branch in handleCreateConnectionSuccess.
func TestHandleCreateConnection_HTMXSuccessTriggersRefresh(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)
	srv := newConnectionStubEmbyServer(t, "")

	payload := map[string]any{
		"name": "HTMXOK", "type": connection.TypeEmby,
		"url": srv.URL, "api_key": "k", "enabled": true,
	}
	buf, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/connections", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("HX-Request", "true")

	w := serveValidated(t, http.HandlerFunc(r.handleCreateConnection), req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", w.Code)
	}
	if got := w.Header().Get("HX-Refresh"); got != "true" {
		t.Errorf("HX-Refresh = %q, want true", got)
	}
}

// TestHandleTestConnection_Lidarr exercises the Lidarr branch of the
// big switch in handleTestConnection using a stub server that returns
// a valid SystemStatus + empty metadata provider list.
func TestHandleTestConnection_Lidarr(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		assertLidarrContract(t, req, http.MethodGet)
		switch req.URL.Path {
		case "/api/v1/system/status":
			_ = json.NewEncoder(w).Encode(map[string]any{"version": "1.0", "appName": "Lidarr"})
		case "/api/v1/config/metadataprovider":
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	r := newConnectionTestRouter(t)
	c := &connection.Connection{
		Name: "LidarrOK", Type: connection.TypeLidarr,
		URL: srv.URL, APIKey: "k", Enabled: true,
	}
	newConnectionTestConn(t, r, c)

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/connections/"+c.ID+"/test", nil)
	req.SetPathValue("id", c.ID)

	w := serveValidated(t, http.HandlerFunc(r.handleTestConnection), req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
}

// TestHandleGetPlatformSettings_Lidarr covers the Lidarr branch.
func TestHandleGetPlatformSettings_Lidarr(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		assertLidarrContract(t, req, http.MethodGet)
		if req.URL.Path == "/api/v1/config/metadataprovider" {
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 1, "metadataType": "Kodi", "consumerName": "Kodi", "enable": false},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	r := newConnectionTestRouter(t)
	c := &connection.Connection{
		Name: "PSLidarr", Type: connection.TypeLidarr,
		URL: srv.URL, APIKey: "k", Enabled: true,
	}
	newConnectionTestConn(t, r, c)

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/connections/"+c.ID+"/platform-settings", nil)
	req.SetPathValue("id", c.ID)

	w := serveValidated(t, http.HandlerFunc(r.handleGetPlatformSettings), req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
}

// TestHandleGetPlatformSummary_Lidarr covers the Lidarr branch.
func TestHandleGetPlatformSummary_Lidarr(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		assertLidarrContract(t, req, http.MethodGet)
		if req.URL.Path == "/api/v1/config/metadataprovider" {
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 1, "metadataType": "Kodi", "consumerName": "Kodi", "enable": true},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	r := newConnectionTestRouter(t)
	c := &connection.Connection{
		Name: "SumLidarr", Type: connection.TypeLidarr,
		URL: srv.URL, APIKey: "k", Enabled: true,
	}
	newConnectionTestConn(t, r, c)

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/connections/"+c.ID+"/platform-summary", nil)
	req.SetPathValue("id", c.ID)

	w := serveValidated(t, http.HandlerFunc(r.handleGetPlatformSummary), req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["has_conflicts"] != true {
		t.Errorf("has_conflicts = %v, want true", resp["has_conflicts"])
	}
}

// TestHandleGetPlatformSummary_LidarrUpstreamError covers the bad-gateway
// branch in the Lidarr arm.
func TestHandleGetPlatformSummary_LidarrUpstreamError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// Even though this stub always errors, assert the request contract so
		// a regression that drops the X-Api-Key header or sends the wrong
		// verb fails here instead of silently exercising the error path for
		// the wrong reason.
		assertLidarrContract(t, req, http.MethodGet)
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	r := newConnectionTestRouter(t)
	c := &connection.Connection{
		Name: "SumLidarrErr", Type: connection.TypeLidarr,
		URL: srv.URL, APIKey: "k", Enabled: true,
	}
	newConnectionTestConn(t, r, c)

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/connections/"+c.ID+"/platform-summary", nil)
	req.SetPathValue("id", c.ID)

	w := serveValidated(t, http.HandlerFunc(r.handleGetPlatformSummary), req)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
}

// TestHandleGetPlatformSummary_Jellyfin covers the Jellyfin branch.
func TestHandleGetPlatformSummary_Jellyfin(t *testing.T) {
	t.Parallel()
	srv := newConnectionStubEmbyServer(t, "") // Jellyfin shares Emby endpoint shape

	r := newConnectionTestRouter(t)
	c := &connection.Connection{
		Name: "SumJF", Type: connection.TypeJellyfin,
		URL: srv.URL, APIKey: "k", Enabled: true,
	}
	newConnectionTestConn(t, r, c)

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/connections/"+c.ID+"/platform-summary", nil)
	req.SetPathValue("id", c.ID)

	w := serveValidated(t, http.HandlerFunc(r.handleGetPlatformSummary), req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
}

// TestHandleGetPlatformSettings_Jellyfin covers the Jellyfin branch.
func TestHandleGetPlatformSettings_Jellyfin(t *testing.T) {
	t.Parallel()
	srv := newConnectionStubEmbyServer(t, "")

	r := newConnectionTestRouter(t)
	c := &connection.Connection{
		Name: "PSJF", Type: connection.TypeJellyfin,
		URL: srv.URL, APIKey: "k", Enabled: true,
	}
	newConnectionTestConn(t, r, c)

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/connections/"+c.ID+"/platform-settings", nil)
	req.SetPathValue("id", c.ID)

	w := serveValidated(t, http.HandlerFunc(r.handleGetPlatformSettings), req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
}

// TestHandleDisablePlatformSettings_LidarrSuccess covers the Lidarr happy path
// where DisableMetadataConsumer returns 200 OK.
func TestHandleDisablePlatformSettings_LidarrSuccess(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// Disable hits the per-config PUT endpoint
		// (lidarr/client.go:155 PutJSON). The handler resolves the
		// consumer_id from the POST body (5) into the URL path, so a
		// regression that routes to the collection endpoint or to the
		// wrong ID surfaces here as a 404 from the stub.
		assertLidarrContract(t, req, http.MethodPut)
		if req.URL.Path != "/api/v1/config/metadataprovider/5" {
			http.Error(w, "unexpected provider target", http.StatusNotFound)
			return
		}
		// Body must be a non-empty JSON object (Lidarr's MetadataProvider
		// payload). An empty/malformed body would mean the handler dropped
		// the resolved config it just fetched from the GET.
		var payload map[string]any
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil || len(payload) == 0 {
			http.Error(w, "invalid or empty request body", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	r := newConnectionTestRouter(t)
	c := &connection.Connection{
		Name: "DisLidarrOK", Type: connection.TypeLidarr,
		URL: srv.URL, APIKey: "k", Enabled: true,
	}
	newConnectionTestConn(t, r, c)

	body := strings.NewReader(`{"consumer_id":5}`)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/connections/"+c.ID+"/platform-settings/disable", body)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", c.ID)

	w := serveValidated(t, http.HandlerFunc(r.handleDisablePlatformSettings), req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
}

// TestHandleDisablePlatformSettings_UnsupportedType covers the default arm.
func TestHandleDisablePlatformSettings_UnsupportedType(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)
	c := &connection.Connection{
		Name: "DisUnknown", Type: connection.TypeEmby,
		URL: "http://e.local:8096", APIKey: "k", Enabled: true,
	}
	newConnectionTestConn(t, r, c)
	if _, err := r.db.ExecContext(context.Background(),
		`UPDATE connections SET type = 'unknown' WHERE id = ?`, c.ID); err != nil {
		t.Fatalf("corrupting type: %v", err)
	}

	body := strings.NewReader(`{}`)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/connections/"+c.ID+"/platform-settings/disable", body)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", c.ID)

	w := serveValidated(t, http.HandlerFunc(r.handleDisablePlatformSettings), req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// TestHandleDeleteConnection_WithArtists covers the deleteLibraries +
// deleteArtists branch, which calls DeleteWithArtists rather than the gentler
// Delete + DismissViolations path.
func TestHandleDeleteConnection_WithArtists(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)

	c := &connection.Connection{
		Name: "DelArt", Type: connection.TypeEmby,
		URL: "http://e3.local:8096", APIKey: "k", Enabled: true,
	}
	newConnectionTestConn(t, r, c)

	lib := &library.Library{
		Name: "DelArtLib", Path: "", Type: library.TypeRegular,
		Source: connection.TypeEmby, ConnectionID: c.ID, ExternalID: "ext-art",
	}
	if err := r.libraryService.Create(context.Background(), lib); err != nil {
		t.Fatalf("create lib: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete,
		"/api/v1/connections/"+c.ID+"?deleteLibraries=true&deleteArtists=true", nil)
	req.SetPathValue("id", c.ID)

	w := serveValidated(t, http.HandlerFunc(r.handleDeleteConnection), req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if _, err := r.libraryService.GetByID(context.Background(), lib.ID); err == nil {
		t.Error("library should be gone after deleteArtists=true")
	}
}

// TestHandleTestConnection_Jellyfin exercises the Jellyfin success arm.
func TestHandleTestConnection_Jellyfin(t *testing.T) {
	t.Parallel()
	srv := newConnectionStubEmbyServer(t, "")

	r := newConnectionTestRouter(t)
	c := &connection.Connection{
		Name: "JFOK", Type: connection.TypeJellyfin,
		URL: srv.URL, APIKey: "k", Enabled: true,
	}
	newConnectionTestConn(t, r, c)

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/connections/"+c.ID+"/test", nil)
	req.SetPathValue("id", c.ID)

	w := serveValidated(t, http.HandlerFunc(r.handleTestConnection), req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["status"] != "ok" {
		t.Errorf("status = %v, want ok", resp["status"])
	}
}
