package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"

	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/database"
	"github.com/sydlexius/stillwater/internal/encryption"
)

// testRouterForConflictToggle stands up a minimal Router with a real
// connection service + in-memory DB so the handleSetStillwaterManaged
// flow runs through snapshot / disable / restore against a fake Emby
// server. Unlike testRouterForLibraryOps this keeps setup narrow -- it
// only includes the fields the conflict handlers consult.
func testRouterForConflictToggle(t *testing.T) (*Router, *connection.Service) {
	t.Helper()
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("opening db: %v", err)
	}
	if err := database.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	enc, _, err := encryption.NewEncryptor("")
	if err != nil {
		t.Fatalf("encryptor: %v", err)
	}
	connSvc := connection.NewService(db, enc)
	r := NewRouter(RouterDeps{
		ConnectionService: connSvc,
		DB:                db,
		Logger:            slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		StaticFS:          os.DirFS("../../web/static"),
	})
	return r, connSvc
}

// assertSetManagedResponse decodes the 200 body from POST
// /connections/{id}/stillwater-managed and asserts the contract advertised
// in openapi.yaml (connection_id + feature_manage_server_files). The
// handler at handlers_conflict.go:284 builds this response from a
// map[string]any literal, so neither the Go type system nor
// TestOpenAPIConsistency (a name-presence-only spec-drift detector) catches
// regressions in the field name, value, or type. This helper is the only
// place that does.
func assertSetManagedResponse(t *testing.T, w *httptest.ResponseRecorder, wantConnID string, wantEnabled bool) {
	t.Helper()
	var resp struct {
		ConnectionID             string `json:"connection_id"`
		FeatureManageServerFiles bool   `json:"feature_manage_server_files"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v body=%s", err, w.Body.String())
	}
	if resp.ConnectionID != wantConnID {
		t.Fatalf("connection_id = %q, want %q", resp.ConnectionID, wantConnID)
	}
	if resp.FeatureManageServerFiles != wantEnabled {
		t.Fatalf("feature_manage_server_files = %v, want %v", resp.FeatureManageServerFiles, wantEnabled)
	}
}

// embyLibraryOptionsShape mirrors the minimal shape Stillwater sends to
// /Library/VirtualFolders/LibraryOptions so the test fake can assert what
// it received.
type embyLibraryOptionsShape struct {
	SaveLocalMetadata bool     `json:"SaveLocalMetadata"`
	MetadataSavers    []string `json:"MetadataSavers"`
}

// startFakeEmby stands up an httptest server that serves a single music
// library and records POSTs so the test can assert both Snapshot and
// DisableFileWriteBack went through.
func startFakeEmby(t *testing.T) (*httptest.Server, *sync.Map) {
	t.Helper()
	received := &sync.Map{}
	initial := map[string]any{
		"Name":           "Music",
		"CollectionType": "music",
		"ItemId":         "lib1",
		"LibraryOptions": map[string]any{
			"SaveLocalMetadata": true,
			"MetadataSavers":    []string{"Nfo"},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Library/VirtualFolders":
			_ = json.NewEncoder(w).Encode([]any{initial})
		case "/Library/VirtualFolders/LibraryOptions":
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("fake emby: read body err = %v", err)
				http.Error(w, "read body failed", http.StatusBadRequest)
				return
			}
			// Unwrap the LibraryOptionsInfo envelope; see production
			// client for why the peer requires it.
			var wrapper struct {
				ID             string          `json:"Id"`
				LibraryOptions json.RawMessage `json:"LibraryOptions"`
			}
			if err := json.Unmarshal(body, &wrapper); err != nil {
				t.Errorf("fake emby: decode wrapper err = %v body=%s", err, body)
				http.Error(w, "decode wrapper failed", http.StatusBadRequest)
				return
			}
			var got embyLibraryOptionsShape
			if err := json.Unmarshal(wrapper.LibraryOptions, &got); err != nil {
				t.Errorf("fake emby: decode library options err = %v body=%s", err, wrapper.LibraryOptions)
				http.Error(w, "decode library options failed", http.StatusBadRequest)
				return
			}
			received.Store(r.URL.Query().Get("Id"), got)
			// Reflect the post into subsequent GETs so the detector sees
			// the peer's updated state.
			initial["LibraryOptions"] = map[string]any{
				"SaveLocalMetadata": got.SaveLocalMetadata,
				"MetadataSavers":    got.MetadataSavers,
			}
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	return srv, received
}

func TestSetStillwaterManaged_EnableSnapshotsAndDisablesPeer(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForConflictToggle(t)
	fake, received := startFakeEmby(t)
	defer fake.Close()

	ctx := context.Background()
	conn := &connection.Connection{
		Name:   "TestEmby",
		Type:   connection.TypeEmby,
		URL:    fake.URL,
		APIKey: "key",
	}
	if err := svc.Create(ctx, conn); err != nil {
		t.Fatalf("create conn: %v", err)
	}

	// POST enable=true
	body := bytes.NewReader([]byte(`{"enabled":true}`))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/connections/"+conn.ID+"/stillwater-managed", body)
	req.SetPathValue("id", conn.ID)
	w := httptest.NewRecorder()
	r.handleSetStillwaterManaged(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	assertSetManagedResponse(t, w, conn.ID, true)

	// Verify the peer was patched to disable savers.
	got, ok := received.Load("lib1")
	if !ok {
		t.Fatal("no POST to peer recorded")
	}
	opts := got.(embyLibraryOptionsShape)
	// SaveLocalMetadata=false is the single master switch that stops
	// Emby/Jellyfin from persisting artwork OR NFO to disk. We intentionally
	// leave MetadataSavers alone because mutating it alongside the flag
	// triggered a NullReferenceException on real peer builds.
	if opts.SaveLocalMetadata {
		t.Errorf("peer SaveLocalMetadata should be off: %+v", opts)
	}

	// Verify the DB now reflects the toggle state + a non-empty snapshot.
	updated, err := svc.GetByID(ctx, conn.ID)
	if err != nil {
		t.Fatalf("reload conn: %v", err)
	}
	if !updated.FeatureManageServerFiles {
		t.Error("FeatureManageServerFiles should be true")
	}
	if updated.PreStillwaterConfigJSON == "" {
		t.Error("snapshot should be populated")
	}
}

func TestSetStillwaterManaged_DisableRestoresSnapshot(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForConflictToggle(t)
	fake, received := startFakeEmby(t)
	defer fake.Close()

	ctx := context.Background()
	conn := &connection.Connection{Name: "TestEmby", Type: connection.TypeEmby, URL: fake.URL, APIKey: "key"}
	if err := svc.Create(ctx, conn); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Enable first so we have a snapshot to restore from. If this setup
	// step regresses (e.g. snapshot path stops returning 200), we want a
	// clear failure here rather than a confusing assertion miss further
	// down -- otherwise a broken enable masquerades as a broken restore.
	body := bytes.NewReader([]byte(`{"enabled":true}`))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/connections/"+conn.ID+"/stillwater-managed", body)
	req.SetPathValue("id", conn.ID)
	enableW := httptest.NewRecorder()
	r.handleSetStillwaterManaged(enableW, req)
	if enableW.Code != http.StatusOK {
		t.Fatalf("enable status = %d body=%s", enableW.Code, enableW.Body.String())
	}
	assertSetManagedResponse(t, enableW, conn.ID, true)

	// Now disable; restore path should POST the original (saver-on) config back.
	body = bytes.NewReader([]byte(`{"enabled":false}`))
	req = httptest.NewRequest(http.MethodPost, "/api/v1/connections/"+conn.ID+"/stillwater-managed", body)
	req.SetPathValue("id", conn.ID)
	w := httptest.NewRecorder()
	r.handleSetStillwaterManaged(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("disable status = %d body=%s", w.Code, w.Body.String())
	}
	assertSetManagedResponse(t, w, conn.ID, false)

	// The most recent POST should have restored the saver on. Check the
	// ok flag so a regression that stops POSTing entirely fails loudly
	// instead of silently passing zero-value assertions.
	got, ok := received.Load("lib1")
	if !ok {
		t.Fatal("no restore POST to peer recorded")
	}
	opts := got.(embyLibraryOptionsShape)
	if !opts.SaveLocalMetadata || len(opts.MetadataSavers) != 1 {
		t.Errorf("restore did not reinstate savers: %+v", opts)
	}

	updated, err := svc.GetByID(ctx, conn.ID)
	if err != nil {
		t.Fatalf("reload conn: %v", err)
	}
	if updated.FeatureManageServerFiles {
		t.Error("FeatureManageServerFiles should be false after disable")
	}
	if updated.PreStillwaterConfigJSON != "" {
		t.Error("snapshot should be cleared after restore")
	}
}

// startFakeJellyfin mirrors startFakeEmby for the Jellyfin dispatch branch in
// handleSetStillwaterManaged. Same LibraryOptions POST shape. The snapshot
// closure exposes the most recent decoded LibraryOptions so tests can assert
// the production client actually flipped SaveLocalMetadata to false; without
// that the JellyfinBranch test would only verify HTTP 200 and miss a no-op
// regression in the dispatch branch.
func startFakeJellyfin(t *testing.T) (*httptest.Server, func() (embyLibraryOptionsShape, bool)) {
	t.Helper()
	var (
		mu   sync.Mutex
		last embyLibraryOptionsShape
		seen bool
	)
	initial := map[string]any{
		"Name":           "Music",
		"CollectionType": "music",
		"ItemId":         "jl1",
		"LibraryOptions": map[string]any{
			"SaveLocalMetadata": true,
			"MetadataSavers":    []string{"Nfo"},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Library/VirtualFolders":
			_ = json.NewEncoder(w).Encode([]any{initial})
		case "/Library/VirtualFolders/LibraryOptions":
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("fake jellyfin: read body err = %v", err)
				http.Error(w, "read body failed", http.StatusBadRequest)
				return
			}
			// Unwrap the LibraryOptionsInfo envelope; see production
			// client for why the peer requires it.
			var wrapper struct {
				ID             string          `json:"Id"`
				LibraryOptions json.RawMessage `json:"LibraryOptions"`
			}
			if err := json.Unmarshal(body, &wrapper); err != nil {
				t.Errorf("fake jellyfin: decode wrapper err = %v body=%s", err, body)
				http.Error(w, "decode wrapper failed", http.StatusBadRequest)
				return
			}
			var got embyLibraryOptionsShape
			if err := json.Unmarshal(wrapper.LibraryOptions, &got); err != nil {
				t.Errorf("fake jellyfin: decode library options err = %v body=%s", err, wrapper.LibraryOptions)
				http.Error(w, "decode library options failed", http.StatusBadRequest)
				return
			}
			mu.Lock()
			last = got
			seen = true
			mu.Unlock()
			initial["LibraryOptions"] = map[string]any{
				"SaveLocalMetadata": got.SaveLocalMetadata,
				"MetadataSavers":    got.MetadataSavers,
			}
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	snapshot := func() (embyLibraryOptionsShape, bool) {
		mu.Lock()
		defer mu.Unlock()
		return last, seen
	}
	return srv, snapshot
}

// startFakeLidarr exposes /api/v1/metadata (the NFO/image consumer endpoint)
// and the matching /:id PUT for the Lidarr dispatch branch. Mirrors the real
// Lidarr shape: each consumer has an "enable" flag and a "fields" array
// whose entries toggle sub-features like artistMetadata and artistImages.
func startFakeLidarr(t *testing.T) (*httptest.Server, func() map[string]any) {
	t.Helper()
	var mu sync.Mutex
	consumers := []map[string]any{
		{
			"id":     float64(1),
			"name":   "Kodi (XBMC) / Emby",
			"enable": true,
			"fields": []any{
				map[string]any{"name": "artistMetadata", "value": true},
				map[string]any{"name": "artistImages", "value": true},
			},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/metadata":
			mu.Lock()
			defer mu.Unlock()
			_ = json.NewEncoder(w).Encode(consumers)
		case r.Method == http.MethodPut && r.URL.Path == "/api/v1/metadata/1":
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("fake lidarr: read body err = %v", err)
				http.Error(w, "read body failed", http.StatusBadRequest)
				return
			}
			var got map[string]any
			if err := json.Unmarshal(body, &got); err != nil {
				t.Errorf("fake lidarr: decode metadata err = %v body=%s", err, body)
				http.Error(w, "decode metadata failed", http.StatusBadRequest)
				return
			}
			mu.Lock()
			consumers[0] = got
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	// Snapshot returns a copy of the latest consumer payload so the test can
	// assert which fields the production client actually flipped (enable +
	// the artistMetadata / artistImages entries inside fields). Without this
	// the LidarrBranch test would only verify HTTP 200, missing no-op or
	// wrong-field regressions in the dispatch branch.
	snapshot := func() map[string]any {
		mu.Lock()
		defer mu.Unlock()
		out := make(map[string]any, len(consumers[0]))
		maps.Copy(out, consumers[0])
		return out
	}
	return srv, snapshot
}

func TestSetStillwaterManaged_JellyfinBranch(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForConflictToggle(t)
	fake, snapshot := startFakeJellyfin(t)
	defer fake.Close()

	conn := &connection.Connection{Name: "TestJF", Type: connection.TypeJellyfin, URL: fake.URL, APIKey: "k"}
	if err := svc.Create(context.Background(), conn); err != nil {
		t.Fatalf("create: %v", err)
	}

	body := bytes.NewReader([]byte(`{"enabled":true}`))
	req := httptest.NewRequest(http.MethodPost, "/", body)
	req.SetPathValue("id", conn.ID)
	w := httptest.NewRecorder()
	r.handleSetStillwaterManaged(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	assertSetManagedResponse(t, w, conn.ID, true)

	// SaveLocalMetadata=false is the single master switch that stops Jellyfin
	// from persisting artwork OR NFO to disk. A 200-only check would let a
	// regression slip where the dispatch branch routes correctly but never
	// actually flips the flag on the peer payload.
	got, ok := snapshot()
	if !ok {
		t.Fatal("no LibraryOptions POST captured for Jellyfin")
	}
	if got.SaveLocalMetadata {
		t.Errorf("SaveLocalMetadata: want false after enable, got %+v", got)
	}
}

func TestSetStillwaterManaged_LidarrBranch(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForConflictToggle(t)
	fake, snapshot := startFakeLidarr(t)
	defer fake.Close()

	conn := &connection.Connection{Name: "TestLid", Type: connection.TypeLidarr, URL: fake.URL, APIKey: "k"}
	if err := svc.Create(context.Background(), conn); err != nil {
		t.Fatalf("create: %v", err)
	}

	body := bytes.NewReader([]byte(`{"enabled":true}`))
	req := httptest.NewRequest(http.MethodPost, "/", body)
	req.SetPathValue("id", conn.ID)
	w := httptest.NewRecorder()
	r.handleSetStillwaterManaged(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	assertSetManagedResponse(t, w, conn.ID, true)

	// Stillwater-managed flips the per-field flags artistMetadata and
	// artistImages to false; the top-level "enable" stays true (the
	// consumer remains registered, just with its writers gated). Asserting
	// the field-level flips is what catches no-op or wrong-field
	// regressions that a 200-only check would let through. See
	// internal/connection/lidarr/writeback_test.go for the same contract
	// at the client layer.
	got := snapshot()
	if enable, _ := got["enable"].(bool); !enable {
		t.Errorf("enable: want true after enable (consumer stays registered), got %v (full=%+v)", got["enable"], got)
	}
	if v := lidarrField(got, "artistMetadata"); v != false {
		t.Errorf("artistMetadata: want false after enable, got %v", v)
	}
	if v := lidarrField(got, "artistImages"); v != false {
		t.Errorf("artistImages: want false after enable, got %v", v)
	}

	// And disable, which routes through the Lidarr restore branch.
	body = bytes.NewReader([]byte(`{"enabled":false}`))
	req = httptest.NewRequest(http.MethodPost, "/", body)
	req.SetPathValue("id", conn.ID)
	w = httptest.NewRecorder()
	r.handleSetStillwaterManaged(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("disable status = %d body=%s", w.Code, w.Body.String())
	}
	assertSetManagedResponse(t, w, conn.ID, false)

	// Restore should put the original (true/true/true) back; the snapshot
	// captured pre-enable values match the seed in startFakeLidarr.
	got = snapshot()
	if enable, _ := got["enable"].(bool); !enable {
		t.Errorf("enable: want true after restore, got %v (full=%+v)", got["enable"], got)
	}
	if v := lidarrField(got, "artistMetadata"); v != true {
		t.Errorf("artistMetadata: want true after restore, got %v", v)
	}
	if v := lidarrField(got, "artistImages"); v != true {
		t.Errorf("artistImages: want true after restore, got %v", v)
	}
}

// lidarrField walks the {fields:[{name,value}]} shape used by Lidarr metadata
// consumers and returns the value for the named entry, or nil if absent. The
// production code stores per-field flags in this nested array (not flat top
// level), so assertions need a small helper to keep the test readable.
func lidarrField(consumer map[string]any, name string) any {
	fields, ok := consumer["fields"].([]any)
	if !ok {
		return nil
	}
	for _, f := range fields {
		fm, ok := f.(map[string]any)
		if !ok {
			continue
		}
		if fm["name"] == name {
			return fm["value"]
		}
	}
	return nil
}

func TestSetStillwaterManaged_404OnUnknownConnection(t *testing.T) {
	t.Parallel()
	r, _ := testRouterForConflictToggle(t)
	body := bytes.NewReader([]byte(`{"enabled":true}`))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/connections/ghost/stillwater-managed", body)
	req.SetPathValue("id", "ghost")
	w := httptest.NewRecorder()
	r.handleSetStillwaterManaged(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

// TestSetStillwaterManaged_DisableReturns502OnPeerRestoreFailure covers the
// clear path: when the user toggles managed-mode off but the peer rejects
// the restore POST, the handler must surface 502 (peer side) rather than
// 500 (local side). This is the inverse of the apply-side rollback test
// and pins the symmetric behavior across both directions.
func TestSetStillwaterManaged_DisableReturns502OnPeerRestoreFailure(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForConflictToggle(t)

	var (
		mu        sync.Mutex
		postCount int
	)
	initial := map[string]any{
		"Name":           "Music",
		"CollectionType": "music",
		"ItemId":         "lib1",
		"LibraryOptions": map[string]any{
			"SaveLocalMetadata": true,
			"MetadataSavers":    []string{"Nfo"},
		},
	}
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/Library/VirtualFolders":
			_ = json.NewEncoder(w).Encode([]any{initial})
		case "/Library/VirtualFolders/LibraryOptions":
			mu.Lock()
			postCount++
			n := postCount
			mu.Unlock()
			// First POST is the enable-disable (Stillwater clearing savers);
			// let it succeed so a snapshot is persisted in the DB. The
			// second POST is the disable-restore the test wants to fail
			// (peer rejects the restore, so the handler must return 502).
			if n == 1 {
				w.WriteHeader(http.StatusOK)
				return
			}
			http.Error(w, "peer restore failed", http.StatusInternalServerError)
		default:
			http.NotFound(w, req)
		}
	}))
	defer fake.Close()

	ctx := context.Background()
	conn := &connection.Connection{Name: "TestEmby", Type: connection.TypeEmby, URL: fake.URL, APIKey: "key"}
	if err := svc.Create(ctx, conn); err != nil {
		t.Fatalf("create: %v", err)
	}

	enableReq := httptest.NewRequest(http.MethodPost, "/api/v1/connections/"+conn.ID+"/stillwater-managed", bytes.NewReader([]byte(`{"enabled":true}`)))
	enableReq.SetPathValue("id", conn.ID)
	enableW := httptest.NewRecorder()
	r.handleSetStillwaterManaged(enableW, enableReq)
	if enableW.Code != http.StatusOK {
		t.Fatalf("enable status = %d body=%s", enableW.Code, enableW.Body.String())
	}
	assertSetManagedResponse(t, enableW, conn.ID, true)

	disableReq := httptest.NewRequest(http.MethodPost, "/api/v1/connections/"+conn.ID+"/stillwater-managed", bytes.NewReader([]byte(`{"enabled":false}`)))
	disableReq.SetPathValue("id", conn.ID)
	disableW := httptest.NewRecorder()
	r.handleSetStillwaterManaged(disableW, disableReq)
	if disableW.Code != http.StatusBadGateway {
		t.Fatalf("disable status = %d body=%s, want 502", disableW.Code, disableW.Body.String())
	}

	// clearStillwaterManaged flips SetManageServerFiles(false) before
	// attempting the peer restore, so a peer-restore failure leaves the
	// DB flag off. The cached ledger must reflect that immediately --
	// without the error-path refresh the banner and write gate would
	// keep treating the connection as managed (ManageServerFiles=true)
	// until the 5-minute TTL expires. Querying Current here returns the
	// in-memory cache directly; if refreshConflictState ran on the error
	// path it should now show ManageServerFiles=false.
	if r.conflictDetector == nil {
		t.Fatal("test harness should have wired conflictDetector")
	}
	ledger := r.conflictDetector.Current(ctx)
	var found bool
	for _, c := range ledger.Connections {
		if c.ConnectionID != conn.ID {
			continue
		}
		found = true
		if c.ManageServerFiles {
			t.Errorf("cached ledger still shows ManageServerFiles=true after failed disable; error path did not refresh detector cache: %+v", c)
		}
	}
	if !found {
		t.Errorf("connection %s missing from cached ledger after disable", conn.ID)
	}
}

// TestSetStillwaterManaged_RollbackRestoreFailureLogged covers the rollback
// path's logging branch: when applyStillwaterManaged fails AND the rollback
// restoreLibraryOptions also fails (peer is fully broken), the handler must
// still surface the original 502 to the caller and the snapshot row must
// still be cleared. The rollback restore error is logged but not returned.
func TestSetStillwaterManaged_RollbackRestoreFailureLogged(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForConflictToggle(t)

	// postCount tracks how many LibraryOptions POSTs the fake peer received.
	// The handler's apply path issues one POST to disable savers (which
	// returns 500 here, driving rollback) and the rollback path then issues
	// a second POST to restore the original config (which also 500s,
	// driving the restoreLibraryOptions error branch). A regression that
	// silently skips the restore call would leave postCount at 1, but the
	// outer effects (502 + cleared snapshot) would still match. Without
	// this counter the test cannot distinguish "rollback ran and failed"
	// from "rollback was never attempted".
	var (
		mu        sync.Mutex
		postCount int
	)
	initial := map[string]any{
		"Name":           "Music",
		"CollectionType": "music",
		"ItemId":         "lib1",
		"LibraryOptions": map[string]any{
			"SaveLocalMetadata": true,
			"MetadataSavers":    []string{"Nfo"},
		},
	}
	// Every POST fails. Both the disable and the rollback restore POST 500,
	// driving the rollbackStillwaterManaged restoreLibraryOptions error
	// branch.
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/Library/VirtualFolders":
			_ = json.NewEncoder(w).Encode([]any{initial})
		case "/Library/VirtualFolders/LibraryOptions":
			mu.Lock()
			postCount++
			mu.Unlock()
			http.Error(w, "peer broken", http.StatusInternalServerError)
		default:
			http.NotFound(w, req)
		}
	}))
	defer fake.Close()

	ctx := context.Background()
	conn := &connection.Connection{Name: "TestEmby", Type: connection.TypeEmby, URL: fake.URL, APIKey: "key"}
	if err := svc.Create(ctx, conn); err != nil {
		t.Fatalf("create: %v", err)
	}

	body := bytes.NewReader([]byte(`{"enabled":true}`))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/connections/"+conn.ID+"/stillwater-managed", body)
	req.SetPathValue("id", conn.ID)
	w := httptest.NewRecorder()
	r.handleSetStillwaterManaged(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d body=%s, want 502", w.Code, w.Body.String())
	}

	mu.Lock()
	gotPostCount := postCount
	mu.Unlock()
	if gotPostCount < 2 {
		t.Fatalf("rollback restore was not attempted; LibraryOptions POST count = %d, want >= 2 (disable + restore)", gotPostCount)
	}

	updated, err := svc.GetByID(ctx, conn.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if updated.PreStillwaterConfigJSON != "" {
		t.Errorf("snapshot should be cleared even when rollback restore fails, got %q", updated.PreStillwaterConfigJSON)
	}
}

// TestSetStillwaterManaged_RollsBackWhenPeerDisableFails verifies that a
// failure in the peer disable step (after the snapshot is persisted) drives
// applyStillwaterManaged through its rollback path. The handler must return
// 502, the rollback must POST the original (savers-on) shape back to the
// peer, and the snapshot column must be cleared so a retry does not snapshot
// the already-mutated peer state. Without rollback, a second enable attempt
// resnaps the savers-off peer and overwrites the real pre-Stillwater config,
// breaking opt-out forever.
func TestSetStillwaterManaged_RollsBackWhenPeerDisableFails(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForConflictToggle(t)

	var (
		mu        sync.Mutex
		postCount int
		posts     []embyLibraryOptionsShape
	)
	initial := map[string]any{
		"Name":           "Music",
		"CollectionType": "music",
		"ItemId":         "lib1",
		"LibraryOptions": map[string]any{
			"SaveLocalMetadata": true,
			"MetadataSavers":    []string{"Nfo"},
		},
	}
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/Library/VirtualFolders":
			_ = json.NewEncoder(w).Encode([]any{initial})
		case "/Library/VirtualFolders/LibraryOptions":
			body, err := io.ReadAll(req.Body)
			if err != nil {
				t.Errorf("rollback fake: read body err = %v", err)
				http.Error(w, "read body failed", http.StatusBadRequest)
				return
			}
			var wrapper struct {
				ID             string          `json:"Id"`
				LibraryOptions json.RawMessage `json:"LibraryOptions"`
			}
			if err := json.Unmarshal(body, &wrapper); err != nil {
				t.Errorf("rollback fake: decode wrapper err = %v body=%s", err, body)
				http.Error(w, "decode wrapper failed", http.StatusBadRequest)
				return
			}
			var got embyLibraryOptionsShape
			if err := json.Unmarshal(wrapper.LibraryOptions, &got); err != nil {
				t.Errorf("rollback fake: decode library options err = %v body=%s", err, wrapper.LibraryOptions)
				http.Error(w, "decode library options failed", http.StatusBadRequest)
				return
			}
			mu.Lock()
			postCount++
			posts = append(posts, got)
			n := postCount
			mu.Unlock()
			// First POST is Stillwater's disable attempt -- fail with 500
			// to trigger the rollback path. Subsequent POSTs (rollback
			// restore) succeed so we can confirm the restore was attempted
			// and what it sent.
			if n == 1 {
				http.Error(w, "peer disable failed", http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, req)
		}
	}))
	defer fake.Close()

	ctx := context.Background()
	conn := &connection.Connection{Name: "TestEmby", Type: connection.TypeEmby, URL: fake.URL, APIKey: "key"}
	if err := svc.Create(ctx, conn); err != nil {
		t.Fatalf("create: %v", err)
	}

	body := bytes.NewReader([]byte(`{"enabled":true}`))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/connections/"+conn.ID+"/stillwater-managed", body)
	req.SetPathValue("id", conn.ID)
	w := httptest.NewRecorder()
	r.handleSetStillwaterManaged(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d body=%s, want 502", w.Code, w.Body.String())
	}

	mu.Lock()
	gotCount := postCount
	gotPosts := append([]embyLibraryOptionsShape(nil), posts...)
	mu.Unlock()
	if gotCount != 2 {
		t.Fatalf("post count = %d, want 2 (failed disable + rollback restore)", gotCount)
	}
	if gotPosts[0].SaveLocalMetadata {
		t.Errorf("first POST (disable) should send SaveLocalMetadata=false, got %+v", gotPosts[0])
	}
	if !gotPosts[1].SaveLocalMetadata {
		t.Errorf("second POST (rollback restore) should send SaveLocalMetadata=true, got %+v", gotPosts[1])
	}

	updated, err := svc.GetByID(ctx, conn.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if updated.FeatureManageServerFiles {
		t.Error("FeatureManageServerFiles should remain false after rollback")
	}
	if updated.PreStillwaterConfigJSON != "" {
		t.Errorf("snapshot should be cleared after rollback, got %q", updated.PreStillwaterConfigJSON)
	}
}

// TestSetStillwaterManaged_IdempotentEnablePreservesSnapshot pins the
// idempotency contract from issue #1190. When a client sends enabled:true
// twice in a row (stale HTMX hx-vals payload, repeated curl, concurrent
// clicks), the second call must NOT re-snapshot the peer. If it did, the
// fresh snapshot would capture the post-managed (savers-off) state and
// overwrite pre_stillwater_config_json, so a future "disable" would replay
// Stillwater's own settings instead of the user's original config. The
// regression assertion is byte-equal: the JSON stored after call #1 must
// match what is stored after call #2.
func TestSetStillwaterManaged_IdempotentEnablePreservesSnapshot(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForConflictToggle(t)
	fake, _ := startFakeEmby(t)
	defer fake.Close()

	ctx := context.Background()
	conn := &connection.Connection{Name: "TestEmby", Type: connection.TypeEmby, URL: fake.URL, APIKey: "key"}
	if err := svc.Create(ctx, conn); err != nil {
		t.Fatalf("create conn: %v", err)
	}

	// First enable: stores the genuine pre-Stillwater snapshot (savers on).
	body := bytes.NewReader([]byte(`{"enabled":true}`))
	req := httptest.NewRequest(http.MethodPost, "/", body)
	req.SetPathValue("id", conn.ID)
	w := httptest.NewRecorder()
	r.handleSetStillwaterManaged(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("first enable status = %d body=%s", w.Code, w.Body.String())
	}

	afterFirst, err := svc.GetByID(ctx, conn.ID)
	if err != nil {
		t.Fatalf("reload after first enable: %v", err)
	}
	originalSnapshot := afterFirst.PreStillwaterConfigJSON
	if originalSnapshot == "" {
		t.Fatal("first enable should have populated pre_stillwater_config_json")
	}

	// Second enable: must be a no-op. The handler should detect the
	// already-managed state and skip applyStillwaterManaged entirely.
	body = bytes.NewReader([]byte(`{"enabled":true}`))
	req = httptest.NewRequest(http.MethodPost, "/", body)
	req.SetPathValue("id", conn.ID)
	w = httptest.NewRecorder()
	r.handleSetStillwaterManaged(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("second enable status = %d body=%s", w.Code, w.Body.String())
	}
	assertSetManagedResponse(t, w, conn.ID, true)

	// Snapshot column must be byte-equal to what call #1 wrote. Pre-fix,
	// the second call would re-snapshot the peer's now-disabled
	// LibraryOptions and SetPreStillwaterConfig would clobber this.
	afterSecond, err := svc.GetByID(ctx, conn.ID)
	if err != nil {
		t.Fatalf("reload after second enable: %v", err)
	}
	if afterSecond.PreStillwaterConfigJSON != originalSnapshot {
		t.Errorf("idempotent enable clobbered snapshot:\nfirst:  %s\nsecond: %s", originalSnapshot, afterSecond.PreStillwaterConfigJSON)
	}
	if !afterSecond.FeatureManageServerFiles {
		t.Error("FeatureManageServerFiles should remain true after idempotent enable")
	}
}

// TestSetStillwaterManaged_IdempotentDisableSkipsPeerCall pins the disable-side
// idempotency contract from issue #1190. The sequence enable -> disable ->
// disable must hit the peer LibraryOptions endpoint exactly twice (the disable
// PATCH on the first disable plus the restore PATCH that follows it); the
// second disable, against the now-cleared snapshot, must short-circuit before
// reaching the peer. Earlier shape of this test seeded an already-unmanaged
// connection with no snapshot, so it would have passed even if the no-op
// guard were removed; this version exercises the actual regression path.
func TestSetStillwaterManaged_IdempotentDisableSkipsPeerCall(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForConflictToggle(t)

	var (
		mu        sync.Mutex
		postCount int
	)
	initial := map[string]any{
		"Name":           "Music",
		"CollectionType": "music",
		"ItemId":         "lib1",
		"LibraryOptions": map[string]any{
			"SaveLocalMetadata": true,
			"MetadataSavers":    []string{"Nfo"},
		},
	}
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/Library/VirtualFolders":
			_ = json.NewEncoder(w).Encode([]any{initial})
		case "/Library/VirtualFolders/LibraryOptions":
			mu.Lock()
			postCount++
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, req)
		}
	}))
	defer fake.Close()

	ctx := context.Background()
	conn := &connection.Connection{Name: "TestEmby", Type: connection.TypeEmby, URL: fake.URL, APIKey: "key"}
	if err := svc.Create(ctx, conn); err != nil {
		t.Fatalf("create conn: %v", err)
	}

	post := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte(body)))
		req.SetPathValue("id", conn.ID)
		w := httptest.NewRecorder()
		r.handleSetStillwaterManaged(w, req)
		return w
	}

	// Step 1: enable. This is one peer POST (the disable PATCH issued by
	// applyStillwaterManaged) and writes a non-empty snapshot.
	if w := post(`{"enabled":true}`); w.Code != http.StatusOK {
		t.Fatalf("enable status = %d body=%s", w.Code, w.Body.String())
	}

	// Step 2: first disable. This is one more peer POST (the restore PATCH
	// issued by clearStillwaterManaged) and clears the snapshot.
	if w := post(`{"enabled":false}`); w.Code != http.StatusOK {
		t.Fatalf("first disable status = %d body=%s", w.Code, w.Body.String())
	}

	// Step 3: second disable against the now-cleared snapshot. The
	// idempotency guard has to short-circuit -- otherwise we hit the peer
	// again and gotPosts climbs to 3.
	w := post(`{"enabled":false}`)
	if w.Code != http.StatusOK {
		t.Fatalf("second disable status = %d body=%s", w.Code, w.Body.String())
	}
	assertSetManagedResponse(t, w, conn.ID, false)

	mu.Lock()
	gotPosts := postCount
	mu.Unlock()
	if gotPosts != 2 {
		t.Errorf("peer POST count = %d, want 2 (1 disable PATCH on enable + 1 restore PATCH on first disable; second/third disables must short-circuit)", gotPosts)
	}

	updated, err := svc.GetByID(ctx, conn.ID)
	if err != nil {
		t.Fatalf("reload conn: %v", err)
	}
	if updated.FeatureManageServerFiles {
		t.Error("FeatureManageServerFiles should be false after disable")
	}
	if updated.PreStillwaterConfigJSON != "" {
		t.Errorf("snapshot should be empty after disable, got %q", updated.PreStillwaterConfigJSON)
	}
}
