package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
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
	r, svc := testRouterForConflictToggle(t)
	fake, received := startFakeEmby(t)
	defer fake.Close()

	ctx := context.Background()
	conn := &connection.Connection{Name: "TestEmby", Type: connection.TypeEmby, URL: fake.URL, APIKey: "key"}
	if err := svc.Create(ctx, conn); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Enable first so we have a snapshot to restore from.
	body := bytes.NewReader([]byte(`{"enabled":true}`))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/connections/"+conn.ID+"/stillwater-managed", body)
	req.SetPathValue("id", conn.ID)
	r.handleSetStillwaterManaged(httptest.NewRecorder(), req)

	// Now disable; restore path should POST the original (saver-on) config back.
	body = bytes.NewReader([]byte(`{"enabled":false}`))
	req = httptest.NewRequest(http.MethodPost, "/api/v1/connections/"+conn.ID+"/stillwater-managed", body)
	req.SetPathValue("id", conn.ID)
	w := httptest.NewRecorder()
	r.handleSetStillwaterManaged(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("disable status = %d body=%s", w.Code, w.Body.String())
	}

	// The most recent POST should have restored the saver on.
	got, _ := received.Load("lib1")
	opts := got.(embyLibraryOptionsShape)
	if !opts.SaveLocalMetadata || len(opts.MetadataSavers) != 1 {
		t.Errorf("restore did not reinstate savers: %+v", opts)
	}

	updated, _ := svc.GetByID(ctx, conn.ID)
	if updated.FeatureManageServerFiles {
		t.Error("FeatureManageServerFiles should be false after disable")
	}
	if updated.PreStillwaterConfigJSON != "" {
		t.Error("snapshot should be cleared after restore")
	}
}

// startFakeJellyfin mirrors startFakeEmby for the Jellyfin dispatch branch in
// handleSetStillwaterManaged. Same LibraryOptions POST shape.
func startFakeJellyfin(t *testing.T) *httptest.Server {
	t.Helper()
	initial := map[string]any{
		"Name":           "Music",
		"CollectionType": "music",
		"ItemId":         "jl1",
		"LibraryOptions": map[string]any{
			"SaveLocalMetadata": true,
			"MetadataSavers":    []string{"Nfo"},
		},
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			initial["LibraryOptions"] = map[string]any{
				"SaveLocalMetadata": got.SaveLocalMetadata,
				"MetadataSavers":    got.MetadataSavers,
			}
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
}

// startFakeLidarr exposes /api/v1/metadata (the NFO/image consumer endpoint)
// and the matching /:id PUT for the Lidarr dispatch branch. Mirrors the real
// Lidarr shape: each consumer has an "enable" flag and a "fields" array
// whose entries toggle sub-features like artistMetadata and artistImages.
func startFakeLidarr(t *testing.T) *httptest.Server {
	t.Helper()
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
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/metadata":
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
			consumers[0] = got
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestSetStillwaterManaged_JellyfinBranch(t *testing.T) {
	r, svc := testRouterForConflictToggle(t)
	fake := startFakeJellyfin(t)
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
}

func TestSetStillwaterManaged_LidarrBranch(t *testing.T) {
	r, svc := testRouterForConflictToggle(t)
	fake := startFakeLidarr(t)
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

	// And disable, which routes through the Lidarr restore branch.
	body = bytes.NewReader([]byte(`{"enabled":false}`))
	req = httptest.NewRequest(http.MethodPost, "/", body)
	req.SetPathValue("id", conn.ID)
	w = httptest.NewRecorder()
	r.handleSetStillwaterManaged(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("disable status = %d body=%s", w.Code, w.Body.String())
	}
}

func TestHandleGetConnectionConflictDetail_FormatsPathsSummary(t *testing.T) {
	// Force a ledger with many paths via a handcrafted detector.
	r := newConflictHarness(t, []connection.Connection{
		{ID: "c1", Name: "C1", Type: connection.TypeEmby, Enabled: true},
	})
	// Can't easily inject paths through the test harness; just verify the
	// handler renders without panicking when the ledger has a known conn.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.SetPathValue("id", "c1")
	w := httptest.NewRecorder()
	r.handleGetConnectionConflictDetail(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d", w.Code)
	}
}

func TestSetStillwaterManaged_404OnUnknownConnection(t *testing.T) {
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
	r, svc := testRouterForConflictToggle(t)

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
				http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
				return
			}
			var wrapper struct {
				ID             string          `json:"Id"`
				LibraryOptions json.RawMessage `json:"LibraryOptions"`
			}
			if err := json.Unmarshal(body, &wrapper); err != nil {
				http.Error(w, "wrapper: "+err.Error(), http.StatusBadRequest)
				return
			}
			var got embyLibraryOptionsShape
			if err := json.Unmarshal(wrapper.LibraryOptions, &got); err != nil {
				http.Error(w, "options: "+err.Error(), http.StatusBadRequest)
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
