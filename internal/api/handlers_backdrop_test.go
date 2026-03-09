package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/auth"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/database"
	"github.com/sydlexius/stillwater/internal/encryption"
	img "github.com/sydlexius/stillwater/internal/image"
	"github.com/sydlexius/stillwater/internal/nfo"
	"github.com/sydlexius/stillwater/internal/platform"
	"github.com/sydlexius/stillwater/internal/rule"
)

// testRouterForBackdrops creates a Router suitable for backdrop handler tests.
// It includes PlatformService and ImageCacheDir.
func testRouterForBackdrops(t *testing.T) (*Router, *artist.Service) {
	t.Helper()

	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	if err := database.Migrate(db); err != nil {
		t.Fatalf("running migrations: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	enc, _, err := encryption.NewEncryptor("")
	if err != nil {
		t.Fatalf("creating encryptor: %v", err)
	}

	authSvc := auth.NewService(db)
	artistSvc := artist.NewService(db)
	connSvc := connection.NewService(db, enc)
	ruleSvc := rule.NewService(db)
	if err := ruleSvc.SeedDefaults(context.Background()); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}
	nfoSnapSvc := nfo.NewSnapshotService(db)
	platformSvc := platform.NewService(db)

	cacheDir := filepath.Join(t.TempDir(), "cache", "images")

	r := NewRouter(RouterDeps{
		AuthService:        authSvc,
		ArtistService:      artistSvc,
		ConnectionService:  connSvc,
		PlatformService:    platformSvc,
		RuleService:        ruleSvc,
		NFOSnapshotService: nfoSnapSvc,
		DB:                 db,
		Logger:             logger,
		StaticDir:          "../../web/static",
		ImageCacheDir:      cacheDir,
	})

	return r, artistSvc
}

// createTestJPEGForBackdrop generates a small test JPEG.
func createTestJPEGForBackdrop(t *testing.T) []byte {
	t.Helper()
	im := image.NewRGBA(image.Rect(0, 0, 2, 2))
	im.Set(0, 0, color.RGBA{R: 255, G: 0, B: 0, A: 255})
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, im, nil); err != nil {
		t.Fatalf("encoding test jpeg: %v", err)
	}
	return buf.Bytes()
}

// addTestConnectionWithURLForBackdrop is a test helper for creating a connection
// with a specific URL (for httptest servers).
func addTestConnectionWithURLForBackdrop(t *testing.T, r *Router, id, name, connType, url string) {
	t.Helper()
	c := &connection.Connection{
		ID:             id,
		Name:           name,
		Type:           connType,
		URL:            url,
		APIKey:         "test-key",
		Enabled:        true,
		Status:         "ok",
		PlatformUserID: "test-user-1",
	}
	if err := r.connectionService.Create(context.Background(), c); err != nil {
		t.Fatalf("creating test connection %s: %v", id, err)
	}
}

func TestHandlePlatformBackdrops_NoConnections(t *testing.T) {
	r, artistSvc := testRouterForBackdrops(t)
	a := addTestArtist(t, artistSvc, "TestArtist")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/"+a.ID+"/platform-backdrops", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handlePlatformBackdrops(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp struct {
		Connections []platformBackdropConnection `json:"connections"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(resp.Connections) != 0 {
		t.Errorf("got %d connections, want 0", len(resp.Connections))
	}
}

func TestHandlePlatformBackdrops_WithBackdrops(t *testing.T) {
	r, artistSvc := testRouterForBackdrops(t)

	// Mock Emby server that returns an artist with 3 backdrops.
	embySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/Users/test-user-1/Items/emby-artist-1" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{
				"Name": "TestArtist",
				"Id": "emby-artist-1",
				"SortName": "TestArtist",
				"ImageTags": {"Primary": "abc"},
				"BackdropImageTags": ["tag0", "tag1", "tag2"],
				"ProviderIds": {},
				"Overview": "",
				"Genres": [],
				"Tags": [],
				"PremiereDate": "",
				"EndDate": "",
				"LockedFields": [],
				"LockData": false
			}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer embySrv.Close()

	a := addTestArtist(t, artistSvc, "TestArtist")
	addTestConnectionWithURLForBackdrop(t, r, "conn-emby", "My Emby", "emby", embySrv.URL)
	if err := artistSvc.SetPlatformID(context.Background(), a.ID, "conn-emby", "emby-artist-1"); err != nil {
		t.Fatalf("setting platform ID: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/"+a.ID+"/platform-backdrops", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handlePlatformBackdrops(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp struct {
		Connections []platformBackdropConnection `json:"connections"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(resp.Connections) != 1 {
		t.Fatalf("got %d connections, want 1", len(resp.Connections))
	}
	if len(resp.Connections[0].Backdrops) != 3 {
		t.Errorf("got %d backdrops, want 3", len(resp.Connections[0].Backdrops))
	}
	if resp.Connections[0].ConnectionName != "My Emby" {
		t.Errorf("connection name = %q, want %q", resp.Connections[0].ConnectionName, "My Emby")
	}
	// Verify thumbnail URLs are correctly formed.
	for i, bd := range resp.Connections[0].Backdrops {
		expected := fmt.Sprintf("/api/v1/artists/%s/platform-backdrops/conn-emby/%d/thumbnail", a.ID, i)
		if bd.ThumbnailURL != expected {
			t.Errorf("backdrop[%d] thumbnail_url = %q, want %q", i, bd.ThumbnailURL, expected)
		}
	}
}

func TestHandlePlatformBackdropThumbnail(t *testing.T) {
	jpegData := createTestJPEGForBackdrop(t)

	r, artistSvc := testRouterForBackdrops(t)

	embySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/Items/emby-artist-1/Images/Backdrop/1" {
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(jpegData)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer embySrv.Close()

	a := addTestArtist(t, artistSvc, "TestArtist")
	addTestConnectionWithURLForBackdrop(t, r, "conn-emby", "My Emby", "emby", embySrv.URL)
	if err := artistSvc.SetPlatformID(context.Background(), a.ID, "conn-emby", "emby-artist-1"); err != nil {
		t.Fatalf("setting platform ID: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/"+a.ID+"/platform-backdrops/conn-emby/1/thumbnail", nil)
	req.SetPathValue("id", a.ID)
	req.SetPathValue("connectionId", "conn-emby")
	req.SetPathValue("index", "1")
	w := httptest.NewRecorder()

	r.handlePlatformBackdropThumbnail(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("content-type = %q, want image/jpeg", ct)
	}
	if !bytes.Equal(w.Body.Bytes(), jpegData) {
		t.Error("response body does not match source image data")
	}
}

func TestHandleFanartSlotAssign(t *testing.T) {
	jpegData := createTestJPEGForBackdrop(t)
	artistDir := t.TempDir()

	r, artistSvc := testRouterForBackdrops(t)

	embySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/Items/emby-artist-1/Images/Backdrop/0" {
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(jpegData)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer embySrv.Close()

	a := &artist.Artist{
		Name:     "TestArtist",
		SortName: "TestArtist",
		Type:     "group",
		Path:     artistDir,
	}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	addTestConnectionWithURLForBackdrop(t, r, "conn-emby", "My Emby", "emby", embySrv.URL)
	if err := artistSvc.SetPlatformID(context.Background(), a.ID, "conn-emby", "emby-artist-1"); err != nil {
		t.Fatalf("setting platform ID: %v", err)
	}

	body := `{"connection_id":"conn-emby","platform_index":0}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/fanart/0/assign", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", a.ID)
	req.SetPathValue("slot", "0")
	w := httptest.NewRecorder()

	r.handleFanartSlotAssign(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	// Verify the file was saved -- default primary is "fanart.jpg".
	primary := r.getActiveFanartPrimary(context.Background())
	paths := img.DiscoverFanart(artistDir, primary)
	if len(paths) != 1 {
		t.Fatalf("got %d fanart files, want 1", len(paths))
	}
}

func TestHandleFanartSlotAssign_GapRejected(t *testing.T) {
	r, artistSvc := testRouterForBackdrops(t)
	artistDir := t.TempDir()

	a := &artist.Artist{
		Name:     "TestArtist",
		SortName: "TestArtist",
		Type:     "group",
		Path:     artistDir,
	}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	addTestConnectionWithURLForBackdrop(t, r, "conn-emby", "My Emby", "emby", "http://unused:8096")
	if err := artistSvc.SetPlatformID(context.Background(), a.ID, "conn-emby", "emby-artist-1"); err != nil {
		t.Fatalf("setting platform ID: %v", err)
	}

	// Try to assign to slot 5 when no fanart exists (gap).
	body := `{"connection_id":"conn-emby","platform_index":0}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/fanart/5/assign", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", a.ID)
	req.SetPathValue("slot", "5")
	w := httptest.NewRecorder()

	r.handleFanartSlotAssign(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

func TestHandleFanartSlotDelete(t *testing.T) {
	artistDir := t.TempDir()

	r, artistSvc := testRouterForBackdrops(t)

	a := &artist.Artist{
		Name:     "TestArtist",
		SortName: "TestArtist",
		Type:     "group",
		Path:     artistDir,
	}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// Create 3 fanart files manually.
	primary := r.getActiveFanartPrimary(context.Background())
	kodi := r.isKodiNumbering(context.Background())
	for i := 0; i < 3; i++ {
		name := img.FanartFilename(primary, i, kodi)
		if err := os.WriteFile(filepath.Join(artistDir, name), []byte("fake-image"), 0o644); err != nil {
			t.Fatalf("writing test fanart %d: %v", i, err)
		}
	}

	// Delete slot 1 (middle).
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/artists/"+a.ID+"/images/fanart/1", nil)
	req.SetPathValue("id", a.ID)
	req.SetPathValue("slot", "1")
	w := httptest.NewRecorder()

	r.handleFanartSlotDelete(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	// Should have 2 remaining files, renumbered.
	remaining := img.DiscoverFanart(artistDir, primary)
	if len(remaining) != 2 {
		t.Fatalf("got %d remaining fanart, want 2", len(remaining))
	}
}

func TestHandleFanartSlotDelete_OnlySlot(t *testing.T) {
	artistDir := t.TempDir()
	r, artistSvc := testRouterForBackdrops(t)

	a := &artist.Artist{
		Name:     "TestArtist",
		SortName: "TestArtist",
		Type:     "group",
		Path:     artistDir,
	}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	primary := r.getActiveFanartPrimary(context.Background())
	name := img.FanartFilename(primary, 0, false)
	if err := os.WriteFile(filepath.Join(artistDir, name), []byte("fake"), 0o644); err != nil {
		t.Fatalf("writing test fanart: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/artists/"+a.ID+"/images/fanart/0", nil)
	req.SetPathValue("id", a.ID)
	req.SetPathValue("slot", "0")
	w := httptest.NewRecorder()

	r.handleFanartSlotDelete(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	remaining := img.DiscoverFanart(artistDir, primary)
	if len(remaining) != 0 {
		t.Fatalf("got %d remaining fanart, want 0", len(remaining))
	}

	// Reload artist to check FanartExists is false.
	updated, err := artistSvc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("reloading artist: %v", err)
	}
	if updated.FanartExists {
		t.Error("FanartExists should be false after deleting the only fanart")
	}
}

func TestHandleFanartReorder(t *testing.T) {
	artistDir := t.TempDir()
	r, artistSvc := testRouterForBackdrops(t)

	a := &artist.Artist{
		Name:     "TestArtist",
		SortName: "TestArtist",
		Type:     "group",
		Path:     artistDir,
	}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	primary := r.getActiveFanartPrimary(context.Background())
	kodi := r.isKodiNumbering(context.Background())
	// Create 3 fanart files with distinct content.
	contents := []string{"AAA", "BBB", "CCC"}
	for i, c := range contents {
		name := img.FanartFilename(primary, i, kodi)
		if err := os.WriteFile(filepath.Join(artistDir, name), []byte(c), 0o644); err != nil {
			t.Fatalf("writing test fanart %d: %v", i, err)
		}
	}

	// Reorder: [2, 0, 1] means slot 0 gets old slot 2, slot 1 gets old slot 0, slot 2 gets old slot 1.
	body := `{"order":[2,0,1]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/fanart/reorder", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleFanartReorder(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	// Verify content moved correctly.
	paths := img.DiscoverFanart(artistDir, primary)
	if len(paths) != 3 {
		t.Fatalf("got %d fanart files, want 3", len(paths))
	}
	expected := []string{"CCC", "AAA", "BBB"}
	for i, p := range paths {
		data, err := os.ReadFile(p) //nolint:gosec // test code
		if err != nil {
			t.Fatalf("reading fanart %d: %v", i, err)
		}
		if string(data) != expected[i] {
			t.Errorf("fanart[%d] content = %q, want %q", i, string(data), expected[i])
		}
	}
}

func TestHandleFanartReorder_InvalidPermutation(t *testing.T) {
	artistDir := t.TempDir()
	r, artistSvc := testRouterForBackdrops(t)

	a := &artist.Artist{
		Name:     "TestArtist",
		SortName: "TestArtist",
		Type:     "group",
		Path:     artistDir,
	}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	primary := r.getActiveFanartPrimary(context.Background())
	for i := 0; i < 3; i++ {
		name := img.FanartFilename(primary, i, false)
		if err := os.WriteFile(filepath.Join(artistDir, name), []byte("data"), 0o644); err != nil {
			t.Fatalf("writing test fanart %d: %v", i, err)
		}
	}

	tests := []struct {
		name string
		body string
	}{
		{"duplicate indices", `{"order":[0,0,1]}`},
		{"wrong length", `{"order":[1,0]}`},
		{"out of range", `{"order":[0,1,5]}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/fanart/reorder", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			req.SetPathValue("id", a.ID)
			w := httptest.NewRecorder()

			r.handleFanartReorder(w, req)

			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
			}
		})
	}
}

func TestHandleFanartSyncState_NoConnections(t *testing.T) {
	r, artistSvc := testRouterForBackdrops(t)
	a := addTestArtist(t, artistSvc, "TestArtist")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/"+a.ID+"/fanart-sync-state", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleFanartSyncState(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp struct {
		Slots []fanartSyncSlot `json:"slots"`
		State string           `json:"state"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if resp.State != "no_connections" {
		t.Errorf("state = %q, want %q", resp.State, "no_connections")
	}
	if len(resp.Slots) != 0 {
		t.Errorf("got %d slots, want 0", len(resp.Slots))
	}
}

func TestHandleFanartSyncState_AllSynced(t *testing.T) {
	r, artistSvc := testRouterForBackdrops(t)
	artistDir := t.TempDir()

	// Mock Emby server with 3 backdrops.
	embySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasPrefix(req.URL.Path, "/Users/test-user-1/Items/emby-artist-1") {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{
				"Name": "TestArtist",
				"Id": "emby-artist-1",
				"SortName": "TestArtist",
				"ImageTags": {"Primary": "abc"},
				"BackdropImageTags": ["tag0", "tag1", "tag2"],
				"ProviderIds": {},
				"Overview": "",
				"Genres": [],
				"Tags": [],
				"PremiereDate": "",
				"EndDate": "",
				"LockedFields": [],
				"LockData": false
			}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer embySrv.Close()

	a := &artist.Artist{
		Name:     "TestArtist",
		SortName: "TestArtist",
		Type:     "group",
		Path:     artistDir,
	}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	addTestConnectionWithURLForBackdrop(t, r, "conn-emby", "My Emby", "emby", embySrv.URL)
	if err := artistSvc.SetPlatformID(context.Background(), a.ID, "conn-emby", "emby-artist-1"); err != nil {
		t.Fatalf("setting platform ID: %v", err)
	}

	// Create 3 local fanart files.
	primary := r.getActiveFanartPrimary(context.Background())
	kodi := r.isKodiNumbering(context.Background())
	for i := 0; i < 3; i++ {
		name := img.FanartFilename(primary, i, kodi)
		if err := os.WriteFile(filepath.Join(artistDir, name), []byte("fake-image"), 0o644); err != nil {
			t.Fatalf("writing test fanart %d: %v", i, err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/"+a.ID+"/fanart-sync-state", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleFanartSyncState(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp struct {
		Slots []fanartSyncSlot `json:"slots"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(resp.Slots) != 3 {
		t.Fatalf("got %d slots, want 3", len(resp.Slots))
	}
	for i, s := range resp.Slots {
		if s.State != "synced" {
			t.Errorf("slot[%d] state = %q, want %q", i, s.State, "synced")
		}
		if s.Index != i {
			t.Errorf("slot[%d] index = %d, want %d", i, s.Index, i)
		}
	}
}

func TestHandleFanartSyncState_Partial(t *testing.T) {
	r, artistSvc := testRouterForBackdrops(t)
	artistDir := t.TempDir()

	// Mock Emby server with only 2 backdrops but we have 3 local.
	embySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasPrefix(req.URL.Path, "/Users/test-user-1/Items/emby-artist-1") {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{
				"Name": "TestArtist",
				"Id": "emby-artist-1",
				"SortName": "TestArtist",
				"ImageTags": {"Primary": "abc"},
				"BackdropImageTags": ["tag0", "tag1"],
				"ProviderIds": {},
				"Overview": "",
				"Genres": [],
				"Tags": [],
				"PremiereDate": "",
				"EndDate": "",
				"LockedFields": [],
				"LockData": false
			}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer embySrv.Close()

	a := &artist.Artist{
		Name:     "TestArtist",
		SortName: "TestArtist",
		Type:     "group",
		Path:     artistDir,
	}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	addTestConnectionWithURLForBackdrop(t, r, "conn-emby", "My Emby", "emby", embySrv.URL)
	if err := artistSvc.SetPlatformID(context.Background(), a.ID, "conn-emby", "emby-artist-1"); err != nil {
		t.Fatalf("setting platform ID: %v", err)
	}

	// Create 3 local fanart files.
	primary := r.getActiveFanartPrimary(context.Background())
	kodi := r.isKodiNumbering(context.Background())
	for i := 0; i < 3; i++ {
		name := img.FanartFilename(primary, i, kodi)
		if err := os.WriteFile(filepath.Join(artistDir, name), []byte("fake-image"), 0o644); err != nil {
			t.Fatalf("writing test fanart %d: %v", i, err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/"+a.ID+"/fanart-sync-state", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleFanartSyncState(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp struct {
		Slots []fanartSyncSlot `json:"slots"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(resp.Slots) != 3 {
		t.Fatalf("got %d slots, want 3", len(resp.Slots))
	}
	// Slots 0 and 1 should be synced (BackdropCount=2 > index).
	for i := 0; i < 2; i++ {
		if resp.Slots[i].State != "synced" {
			t.Errorf("slot[%d] state = %q, want %q", i, resp.Slots[i].State, "synced")
		}
	}
	// Slot 2 should be unsynced.
	if resp.Slots[2].State != "unsynced" {
		t.Errorf("slot[2] state = %q, want %q", resp.Slots[2].State, "unsynced")
	}
}

func TestHandleFanartSyncState_MultipleConnections(t *testing.T) {
	r, artistSvc := testRouterForBackdrops(t)
	artistDir := t.TempDir()

	// Mock Emby with 3 backdrops, Jellyfin with 1 backdrop.
	embySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasPrefix(req.URL.Path, "/Users/test-user-1/Items/emby-artist-1") {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{
				"Name": "TestArtist",
				"Id": "emby-artist-1",
				"SortName": "TestArtist",
				"ImageTags": {"Primary": "abc"},
				"BackdropImageTags": ["tag0", "tag1", "tag2"],
				"ProviderIds": {},
				"Overview": "",
				"Genres": [],
				"Tags": [],
				"PremiereDate": "",
				"EndDate": "",
				"LockedFields": [],
				"LockData": false
			}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer embySrv.Close()

	jellyfinSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasPrefix(req.URL.Path, "/Users/test-user-1/Items/jf-artist-1") {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{
				"Name": "TestArtist",
				"Id": "jf-artist-1",
				"SortName": "TestArtist",
				"ImageTags": {"Primary": "abc"},
				"BackdropImageTags": ["tag0"],
				"ProviderIds": {},
				"Overview": "",
				"Genres": [],
				"Tags": [],
				"PremiereDate": "",
				"EndDate": "",
				"LockedFields": [],
				"LockData": false
			}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer jellyfinSrv.Close()

	a := &artist.Artist{
		Name:     "TestArtist",
		SortName: "TestArtist",
		Type:     "group",
		Path:     artistDir,
	}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	addTestConnectionWithURLForBackdrop(t, r, "conn-emby", "My Emby", "emby", embySrv.URL)
	addTestConnectionWithURLForBackdrop(t, r, "conn-jf", "My Jellyfin", "jellyfin", jellyfinSrv.URL)
	if err := artistSvc.SetPlatformID(context.Background(), a.ID, "conn-emby", "emby-artist-1"); err != nil {
		t.Fatalf("setting emby platform ID: %v", err)
	}
	if err := artistSvc.SetPlatformID(context.Background(), a.ID, "conn-jf", "jf-artist-1"); err != nil {
		t.Fatalf("setting jellyfin platform ID: %v", err)
	}

	// Create 3 local fanart files.
	primary := r.getActiveFanartPrimary(context.Background())
	kodi := r.isKodiNumbering(context.Background())
	for i := 0; i < 3; i++ {
		name := img.FanartFilename(primary, i, kodi)
		if err := os.WriteFile(filepath.Join(artistDir, name), []byte("fake-image"), 0o644); err != nil {
			t.Fatalf("writing test fanart %d: %v", i, err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/"+a.ID+"/fanart-sync-state", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleFanartSyncState(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp struct {
		Slots []fanartSyncSlot `json:"slots"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(resp.Slots) != 3 {
		t.Fatalf("got %d slots, want 3", len(resp.Slots))
	}

	// Slot 0: both have it -> synced
	if resp.Slots[0].State != "synced" {
		t.Errorf("slot[0] state = %q, want %q", resp.Slots[0].State, "synced")
	}
	if len(resp.Slots[0].Connections) != 2 {
		t.Errorf("slot[0] connections = %d, want 2", len(resp.Slots[0].Connections))
	}

	// Slot 1: Emby has it (3 backdrops > 1), Jellyfin does not (1 backdrop <= 1) -> partial
	if resp.Slots[1].State != "partial" {
		t.Errorf("slot[1] state = %q, want %q", resp.Slots[1].State, "partial")
	}

	// Slot 2: Emby has it (3 > 2), Jellyfin does not (1 <= 2) -> partial
	if resp.Slots[2].State != "partial" {
		t.Errorf("slot[2] state = %q, want %q", resp.Slots[2].State, "partial")
	}

	// Verify per-connection detail on slot 1.
	for _, c := range resp.Slots[1].Connections {
		if c.ConnectionType == "emby" && !c.Synced {
			t.Error("slot[1] emby connection should be synced")
		}
		if c.ConnectionType == "jellyfin" && c.Synced {
			t.Error("slot[1] jellyfin connection should not be synced")
		}
	}
}

func TestIsValidPermutation(t *testing.T) {
	tests := []struct {
		order []int
		want  bool
	}{
		{[]int{0, 1, 2}, true},
		{[]int{2, 0, 1}, true},
		{[]int{0}, true},
		{[]int{}, false},
		{[]int{0, 0}, false},
		{[]int{1, 2}, false},
		{[]int{-1, 0}, false},
	}
	for _, tc := range tests {
		got := isValidPermutation(tc.order)
		if got != tc.want {
			t.Errorf("isValidPermutation(%v) = %v, want %v", tc.order, got, tc.want)
		}
	}
}
