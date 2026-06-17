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
	"github.com/sydlexius/stillwater/internal/conflict"
	"github.com/sydlexius/stillwater/internal/connection"
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

	db := newTestDB(t)

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
		SessionSecret:      testSessionSecret,
		AuthService:        authSvc,
		ArtistService:      artistSvc,
		ConnectionService:  connSvc,
		PlatformService:    platformSvc,
		RuleService:        ruleSvc,
		NFOSnapshotService: nfoSnapSvc,
		DB:                 db,
		Logger:             logger,
		StaticFS:           os.DirFS("../../web/static"),
		ImageCacheDir:      cacheDir,
	})

	// Override the auto-wired conflict detector with the no-op NewForTest
	// variant so handler tests that create connection fixtures (without
	// standing up real peer stubs) do not trip the fail-closed CheckErr
	// contract in ledger.AnyImageConflict / AnyNFOConflict. Tests that
	// exercise the gate itself build their own router.
	r.conflictDetector = conflict.NewForTest(connSvc, logger)
	r.conflictGate = conflict.NewGate(r.conflictDetector)

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

// addTestConnectionWithURLForBackdrop is a test helper for creating a single
// "My Emby" connection pointed at a specific URL (an httptest server). The ID,
// name, and type are fixed since every caller uses the same single connection.
func addTestConnectionWithURLForBackdrop(t *testing.T, r *Router, url string) {
	t.Helper()
	const id = "conn-emby"
	c := &connection.Connection{
		ID:             id,
		Name:           "My Emby",
		Type:           "emby",
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
	t.Parallel()
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
	t.Parallel()
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
	addTestConnectionWithURLForBackdrop(t, r, embySrv.URL)
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
	t.Parallel()
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
	addTestConnectionWithURLForBackdrop(t, r, embySrv.URL)
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
	t.Parallel()
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
	addTestConnectionWithURLForBackdrop(t, r, embySrv.URL)
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
	paths, _ := img.DiscoverFanart(artistDir, primary)
	if len(paths) != 1 {
		t.Fatalf("got %d fanart files, want 1", len(paths))
	}
}

func TestHandleFanartSlotAssign_GapRejected(t *testing.T) {
	t.Parallel()
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
	addTestConnectionWithURLForBackdrop(t, r, "http://unused:8096")
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
	t.Parallel()
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
	remaining, _ := img.DiscoverFanart(artistDir, primary)
	if len(remaining) != 2 {
		t.Fatalf("got %d remaining fanart, want 2", len(remaining))
	}
}

func TestHandleFanartSlotDelete_OnlySlot(t *testing.T) {
	t.Parallel()
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

	remaining, _ := img.DiscoverFanart(artistDir, primary)
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
	t.Parallel()
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
	paths, _ := img.DiscoverFanart(artistDir, primary)
	if len(paths) != 3 {
		t.Fatalf("got %d fanart files, want 3", len(paths))
	}
	expected := []string{"CCC", "AAA", "BBB"}
	for i, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("reading fanart %d: %v", i, err)
		}
		if string(data) != expected[i] {
			t.Errorf("fanart[%d] content = %q, want %q", i, string(data), expected[i])
		}
	}
}

func TestHandleFanartReorder_InvalidPermutation(t *testing.T) {
	t.Parallel()
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

func TestHandlePlatformBackdropThumbnail_InvalidIndex(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouterForBackdrops(t)
	a := addTestArtist(t, artistSvc, "TestArtist")
	addTestConnectionWithURLForBackdrop(t, r, "http://unused:8096")
	if err := artistSvc.SetPlatformID(context.Background(), a.ID, "conn-emby", "emby-artist-1"); err != nil {
		t.Fatalf("setting platform ID: %v", err)
	}

	tests := []struct {
		name  string
		index string
	}{
		{"non-numeric index", "abc"},
		{"negative index", "-1"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/"+a.ID+"/platform-backdrops/conn-emby/"+tc.index+"/thumbnail", nil)
			req.SetPathValue("id", a.ID)
			req.SetPathValue("connectionId", "conn-emby")
			req.SetPathValue("index", tc.index)
			w := httptest.NewRecorder()

			r.handlePlatformBackdropThumbnail(w, req)

			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
			}
		})
	}
}

func TestHandleFanartSlotDelete_OutOfRange(t *testing.T) {
	t.Parallel()
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

	// Create 2 fanart files.
	primary := r.getActiveFanartPrimary(context.Background())
	kodi := r.isKodiNumbering(context.Background())
	for i := 0; i < 2; i++ {
		name := img.FanartFilename(primary, i, kodi)
		if err := os.WriteFile(filepath.Join(artistDir, name), []byte("fake-image"), 0o644); err != nil {
			t.Fatalf("writing test fanart %d: %v", i, err)
		}
	}

	// Try to delete slot 5 (out of range for 2 files).
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/artists/"+a.ID+"/images/fanart/5", nil)
	req.SetPathValue("id", a.ID)
	req.SetPathValue("slot", "5")
	w := httptest.NewRecorder()

	r.handleFanartSlotDelete(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}

	// Verify the error message mentions the slot being out of range.
	var resp struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding error response: %v", err)
	}
	if !strings.Contains(resp.Error, "out of range") {
		t.Errorf("error = %q, want it to contain %q", resp.Error, "out of range")
	}
}

func TestIsValidPermutation(t *testing.T) {
	t.Parallel()
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

// seedBlockedFanartArtist creates an artist rooted at a temp dir with two seeded
// fanart slots and returns the artist plus the slot paths, so a blocked
// destructive op can assert the bytes were left untouched.
func seedBlockedFanartArtist(t *testing.T, artistSvc *artist.Service, name string) (*artist.Artist, string, string) {
	t.Helper()
	dir := t.TempDir()
	a := &artist.Artist{Name: name, SortName: name, Type: "group", Path: dir}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist %s: %v", name, err)
	}
	p0 := filepath.Join(dir, "fanart.jpg")
	p1 := filepath.Join(dir, "fanart1.jpg")
	writeJPEG(t, p0, 1920, 1080)
	writeJPEG(t, p1, 1920, 1080)
	return a, p0, p1
}

// assertSlotsUnchanged asserts both seeded slot files still exist after a
// blocked destructive op.
func assertSlotsUnchanged(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("blocked op must leave %s on disk: %v", filepath.Base(p), err)
		}
	}
}

// assertConflictWriteBlock asserts a gated 409 body carries the shared
// ConflictWriteBlock payload (error/axis/reason/ledger) rather than a bare
// error, so a regression dropping the conflict fields fails the test (CR #1839).
func assertConflictWriteBlock(t *testing.T, body []byte) {
	t.Helper()
	var blocked map[string]any
	if err := json.Unmarshal(body, &blocked); err != nil {
		t.Fatalf("decoding 409 body: %v; body = %s", err, string(body))
	}
	if blocked["axis"] != "image" {
		t.Errorf("409 axis = %v, want image", blocked["axis"])
	}
	if s, _ := blocked["error"].(string); s == "" {
		t.Errorf("409 error code must be non-empty, got %v", blocked["error"])
	}
	if s, _ := blocked["reason"].(string); s == "" {
		t.Errorf("409 reason must be non-empty, got %v", blocked["reason"])
	}
	if _, ok := blocked["ledger"].(map[string]any); !ok {
		t.Errorf("409 ledger should be a JSON object, got %T (%v)", blocked["ledger"], blocked["ledger"])
	}
}

func TestHandleFanartSlotDelete_Blocked409(t *testing.T) {
	t.Parallel()
	d := conflict.NewBlockingForTest(testDiscardLogger())
	r, artistSvc := testRouterForBackdrops(t)
	r.conflictDetector = d
	r.conflictGate = conflict.NewGate(d)
	a, p0, p1 := seedBlockedFanartArtist(t, artistSvc, "Blocked Slot")
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/artists/"+a.ID+"/images/fanart/0", nil)
	req.SetPathValue("id", a.ID)
	req.SetPathValue("slot", "0")
	w := httptest.NewRecorder()
	r.handleFanartSlotDelete(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body: %s", w.Code, w.Body.String())
	}
	assertConflictWriteBlock(t, w.Body.Bytes()) // 409 must carry the conflict payload
	assertSlotsUnchanged(t, p0, p1)             // T1: blocked delete must not remove bytes
}

// TestHandleFanartSlotDelete_RemoveFailureKeepsSlots proves T5: when the file
// remover fails, slot-delete returns 500 and BOTH seeded slots remain on disk.
func TestHandleFanartSlotDelete_RemoveFailureKeepsSlots(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouterForBackdrops(t)
	r.fileRemover = failingRemover{err: fmt.Errorf("permission denied")}
	a, p0, p1 := seedBlockedFanartArtist(t, artistSvc, "Remove Fail Slot")
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/artists/"+a.ID+"/images/fanart/0", nil)
	req.SetPathValue("id", a.ID)
	req.SetPathValue("slot", "0")
	w := httptest.NewRecorder()
	r.handleFanartSlotDelete(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body: %s", w.Code, w.Body.String())
	}
	assertSlotsUnchanged(t, p0, p1) // remove failed -> nothing deleted
}

func TestHandleFanartReorder_Blocked409(t *testing.T) {
	t.Parallel()
	d := conflict.NewBlockingForTest(testDiscardLogger())
	r, artistSvc := testRouterForBackdrops(t)
	r.conflictDetector = d
	r.conflictGate = conflict.NewGate(d)
	a, p0, p1 := seedBlockedFanartArtist(t, artistSvc, "Blocked Reorder")
	// Distinct per-slot contents so a blocked reorder that nevertheless swapped
	// bytes before returning 409 would be caught (matching filenames alone is not
	// enough - both slots keep the same names through a reorder). The gate blocks
	// before any file read, so non-image marker bytes are fine here.
	if err := os.WriteFile(p0, []byte("SLOT0-CONTENT"), 0o644); err != nil {
		t.Fatalf("seeding slot 0 content: %v", err)
	}
	if err := os.WriteFile(p1, []byte("SLOT1-CONTENT"), 0o644); err != nil {
		t.Fatalf("seeding slot 1 content: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/fanart/reorder",
		bytes.NewReader([]byte(`{"order":[1,0]}`)))
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()
	r.handleFanartReorder(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body: %s", w.Code, w.Body.String())
	}
	assertConflictWriteBlock(t, w.Body.Bytes()) // 409 must carry the conflict payload
	assertSlotsUnchanged(t, p0, p1)             // T1: blocked reorder must not rename files
	// Bytes must stay put per slot: a blocked reorder must not swap contents.
	if got, _ := os.ReadFile(p0); string(got) != "SLOT0-CONTENT" {
		t.Errorf("slot 0 content changed after blocked reorder: %q", got)
	}
	if got, _ := os.ReadFile(p1); string(got) != "SLOT1-CONTENT" {
		t.Errorf("slot 1 content changed after blocked reorder: %q", got)
	}
}

func TestHandleFanartBatchDelete_Blocked409(t *testing.T) {
	t.Parallel()
	d := conflict.NewBlockingForTest(testDiscardLogger())
	r, artistSvc := testRouterForBackdrops(t)
	r.conflictDetector = d
	r.conflictGate = conflict.NewGate(d)
	a, p0, p1 := seedBlockedFanartArtist(t, artistSvc, "Blocked Batch")
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/artists/"+a.ID+"/images/fanart/batch",
		bytes.NewReader([]byte(`{"indices":[0]}`)))
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()
	r.handleFanartBatchDelete(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body: %s", w.Code, w.Body.String())
	}
	assertConflictWriteBlock(t, w.Body.Bytes()) // 409 must carry the conflict payload
	assertSlotsUnchanged(t, p0, p1)             // T1: blocked batch-delete must not remove bytes
}
