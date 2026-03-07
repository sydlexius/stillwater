package api

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/platform"
)

// testRouterWithPlatform returns a Router that includes a platform service,
// required by setArtistImageFlag for naming-config lookup.
func testRouterWithPlatform(t *testing.T) (*Router, *artist.Service) {
	t.Helper()
	r, artistSvc := testRouter(t)
	r.platformService = platform.NewService(r.db)
	return r, artistSvc
}

// writeJPEG creates a JPEG file at path with the given dimensions.
// The active platform profile (Kodi) uses folder.jpg as the thumb filename,
// so tests write JPEG files to ensure findExistingImage locates them.
func writeJPEG(t *testing.T, path string, w, h int) {
	t.Helper()
	m := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			m.Set(x, y, color.RGBA{R: 128, G: 128, B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, m, nil); err != nil {
		t.Fatalf("encoding JPEG: %v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

func TestSetArtistImageFlag_LowRes(t *testing.T) {
	r, artistSvc := testRouterWithPlatform(t)
	dir := t.TempDir()
	a := &artist.Artist{Name: "Test", SortName: "Test", Path: dir}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// Write a 300x300 thumb (below 500x500 threshold).
	writeJPEG(t, filepath.Join(dir, "folder.jpg"), 300, 300)

	r.setArtistImageFlag(context.Background(), a, "thumb", true)

	if !a.ThumbExists {
		t.Error("ThumbExists should be true")
	}
	if !a.ThumbLowRes {
		t.Error("ThumbLowRes should be true for 300x300 thumb")
	}
	if !strings.HasPrefix(a.ThumbPlaceholder, "data:image/jpeg;base64,") {
		t.Errorf("ThumbPlaceholder should be a JPEG data URI, got prefix %q", a.ThumbPlaceholder[:min(len(a.ThumbPlaceholder), 30)])
	}
}

func TestSetArtistImageFlag_GoodRes(t *testing.T) {
	r, artistSvc := testRouterWithPlatform(t)
	dir := t.TempDir()
	a := &artist.Artist{Name: "Test", SortName: "Test", Path: dir}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// Write a 1000x1000 thumb (above 500x500 threshold).
	writeJPEG(t, filepath.Join(dir, "folder.jpg"), 1000, 1000)

	r.setArtistImageFlag(context.Background(), a, "thumb", true)

	if !a.ThumbExists {
		t.Error("ThumbExists should be true")
	}
	if a.ThumbLowRes {
		t.Error("ThumbLowRes should be false for 1000x1000 thumb")
	}
	if !strings.HasPrefix(a.ThumbPlaceholder, "data:image/jpeg;base64,") {
		t.Errorf("ThumbPlaceholder should be a JPEG data URI, got prefix %q", a.ThumbPlaceholder[:min(len(a.ThumbPlaceholder), 30)])
	}
}

func TestSetArtistImageFlag_Delete(t *testing.T) {
	r, artistSvc := testRouterWithPlatform(t)
	dir := t.TempDir()
	a := &artist.Artist{
		Name: "Test", SortName: "Test", Path: dir,
		ThumbExists: true, ThumbLowRes: true,
	}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	r.setArtistImageFlag(context.Background(), a, "thumb", false)

	if a.ThumbExists {
		t.Error("ThumbExists should be false after deletion")
	}
	if a.ThumbLowRes {
		t.Error("ThumbLowRes should be false after deletion")
	}
	if a.ThumbPlaceholder != "" {
		t.Errorf("ThumbPlaceholder should be empty after deletion, got %q", a.ThumbPlaceholder[:min(len(a.ThumbPlaceholder), 30)])
	}
}

func TestRequireArtistPath_Degraded(t *testing.T) {
	r, _ := testRouterWithPlatform(t)

	// Artist with empty path (degraded library)
	a := &artist.Artist{Name: "API Only Artist", SortName: "API Only Artist", Path: ""}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/test/images", nil)
	w := httptest.NewRecorder()
	ok := r.requireArtistPath(w, req, a)
	if ok {
		t.Fatal("expected requireArtistPath to return false for empty path")
	}
	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want %d", w.Code, http.StatusUnprocessableEntity)
	}

	// Artist with a path should pass
	a.Path = "/music/some-artist"
	w = httptest.NewRecorder()
	ok = r.requireArtistPath(w, req, a)
	if !ok {
		t.Fatal("expected requireArtistPath to return true for non-empty path")
	}
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestRequireImageDir_WithCacheDir(t *testing.T) {
	r, _ := testRouterWithPlatform(t)
	cacheDir := t.TempDir()
	r.imageCacheDir = cacheDir

	// Pathless artist with cache configured should pass.
	a := &artist.Artist{ID: "abc123", Name: "Cached Artist", SortName: "Cached Artist", Path: ""}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/abc123/images", nil)
	w := httptest.NewRecorder()
	ok := r.requireImageDir(w, req, a)
	if !ok {
		t.Fatal("expected requireImageDir to return true when cache is configured")
	}
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	// Verify cache directory was created.
	expected := filepath.Join(cacheDir, a.ID)
	info, err := os.Stat(expected)
	if err != nil {
		t.Fatalf("expected cache dir %s to exist: %v", expected, err)
	}
	if !info.IsDir() {
		t.Fatalf("expected %s to be a directory", expected)
	}
}

func TestRequireImageDir_RejectsNoCacheNoPath(t *testing.T) {
	r, _ := testRouterWithPlatform(t)
	// No cache dir, no artist path.
	r.imageCacheDir = ""

	a := &artist.Artist{ID: "abc123", Name: "No Path Artist", SortName: "No Path Artist", Path: ""}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/abc123/images", nil)
	w := httptest.NewRecorder()
	ok := r.requireImageDir(w, req, a)
	if ok {
		t.Fatal("expected requireImageDir to return false when no cache and no path")
	}
	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want %d", w.Code, http.StatusUnprocessableEntity)
	}
}

func TestIsPrivateURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want bool
	}{
		{"loopback ipv4", "http://127.0.0.1/image.jpg", true},
		{"loopback ipv6", "http://[::1]/image.jpg", true},
		{"private 10.x", "http://10.0.0.1/image.jpg", true},
		{"private 172.16.x", "http://172.16.0.1/image.jpg", true},
		{"private 192.168.x", "http://192.168.1.1/image.jpg", true},
		{"link-local", "http://169.254.1.1/image.jpg", true},
		{"unspecified ipv4", "http://0.0.0.0/image.jpg", true},
		{"invalid url", "://bad", true},
		{"public ipv4", "http://8.8.8.8/image.jpg", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isPrivateURL(context.Background(), tt.url)
			if got != tt.want {
				t.Errorf("isPrivateURL(%q) = %v, want %v", tt.url, got, tt.want)
			}
		})
	}
}

func TestSSRFSafeTransport_PreservesDefaults(t *testing.T) {
	transport := ssrfSafeTransport()
	if transport.DialContext == nil {
		t.Fatal("DialContext should be set")
	}
	if transport.TLSHandshakeTimeout == 0 {
		t.Error("TLSHandshakeTimeout should be non-zero (inherited from DefaultTransport)")
	}
	if transport.IdleConnTimeout == 0 {
		t.Error("IdleConnTimeout should be non-zero (inherited from DefaultTransport)")
	}
	// Verify TLS config is present (HTTP/2 support via Clone)
	if transport.TLSClientConfig == nil {
		// Clone may or may not set TLSClientConfig depending on DefaultTransport,
		// but ForceAttemptHTTP2 should be inherited.
		if !transport.ForceAttemptHTTP2 {
			t.Log("ForceAttemptHTTP2 not set; HTTP/2 may not be available")
		}
	}
}

func TestSSRFSafeTransport_BlocksPrivateIP(t *testing.T) {
	transport := ssrfSafeTransport()
	client := &http.Client{Transport: transport}

	// Attempt to connect to a loopback address -- should be rejected.
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://127.0.0.1:1/test", nil)
	resp, err := client.Do(req) //nolint:bodyclose // err expected
	if err == nil {
		resp.Body.Close()
		t.Fatal("expected error connecting to loopback address")
	}
}

func TestSSRFSafeTransport_EmptyDNS(t *testing.T) {
	// The empty-DNS guard is exercised when a hostname resolves to zero addresses.
	// We cannot easily force that in a unit test (net.DefaultResolver is global),
	// but we verify the guard exists by reading the function.
	// Instead, test that a non-existent host returns an error (not a panic).
	transport := ssrfSafeTransport()
	// Disable TLS to avoid handshake errors on non-existent hosts.
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // test only
	client := &http.Client{Transport: transport}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://this-host-does-not-exist-abc123xyz.invalid/test", nil)
	resp, err := client.Do(req) //nolint:bodyclose // err expected
	if err == nil {
		resp.Body.Close()
		t.Fatal("expected error for non-existent host")
	}
}

func TestSetArtistImageFlag_UnreadableFile(t *testing.T) {
	r, artistSvc := testRouterWithPlatform(t)
	dir := t.TempDir()
	a := &artist.Artist{Name: "Test", SortName: "Test", Path: dir}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// Write a file with invalid image data (cannot be decoded).
	if err := os.WriteFile(filepath.Join(dir, "folder.jpg"), []byte("not an image"), 0o644); err != nil {
		t.Fatalf("writing bad file: %v", err)
	}

	// Should not panic; ThumbExists is set, ThumbLowRes stays false on decode error.
	r.setArtistImageFlag(context.Background(), a, "thumb", true)

	if !a.ThumbExists {
		t.Error("ThumbExists should be true even when dimensions cannot be read")
	}
	if a.ThumbLowRes {
		t.Error("ThumbLowRes should be false when dimensions cannot be read")
	}
	if a.ThumbPlaceholder != "" {
		t.Errorf("ThumbPlaceholder should be empty for unreadable image, got %q", a.ThumbPlaceholder[:min(len(a.ThumbPlaceholder), 30)])
	}
}

func TestSetArtistImageFlag_TransientPreservesPlaceholder(t *testing.T) {
	r, artistSvc := testRouterWithPlatform(t)
	dir := t.TempDir()

	existingPH := "data:image/jpeg;base64,EXISTING"
	a := &artist.Artist{
		Name: "Test", SortName: "Test", Path: dir,
		ThumbExists: true, ThumbPlaceholder: existingPH,
	}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// Write a corrupted file: image exists on disk but decode will fail.
	if err := os.WriteFile(filepath.Join(dir, "folder.jpg"), []byte("corrupted"), 0o644); err != nil {
		t.Fatalf("writing bad file: %v", err)
	}

	r.setArtistImageFlag(context.Background(), a, "thumb", true)

	if !a.ThumbExists {
		t.Error("ThumbExists should be true")
	}
	// Existing placeholder should be preserved when generation fails transiently.
	if a.ThumbPlaceholder != existingPH {
		t.Errorf("ThumbPlaceholder = %q, want %q (should be preserved on transient failure)",
			a.ThumbPlaceholder[:min(len(a.ThumbPlaceholder), 30)], existingPH)
	}
}

func TestHandleImageUpload_SyncsToPlatform(t *testing.T) {
	type syncCapture struct {
		contentLength int64
		contentType   string
	}
	uploadedCh := make(chan syncCapture, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost && strings.Contains(req.URL.Path, "/Images/Primary") {
			select {
			case uploadedCh <- syncCapture{req.ContentLength, req.Header.Get("Content-Type")}:
			default:
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	r, artistSvc := testRouterWithPlatform(t)
	r.imageCacheDir = t.TempDir()
	addTestConnectionWithURL(t, r, "conn-emby", "Emby", "emby", srv.URL)

	// Platform artist: no local path, images stored in cache dir.
	a := &artist.Artist{Name: "Platform Artist", SortName: "Platform Artist", Path: ""}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	if err := artistSvc.SetPlatformID(context.Background(), a.ID, "conn-emby", "emby-artist-1"); err != nil {
		t.Fatalf("SetPlatformID: %v", err)
	}

	// Build a multipart upload request with a small JPEG.
	// Use CreatePart (not CreateFormFile) to set Content-Type: image/jpeg,
	// because CreateFormFile defaults to application/octet-stream.
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if err := mw.WriteField("type", "thumb"); err != nil {
		t.Fatalf("writing multipart field: %v", err)
	}
	partHeader := make(map[string][]string)
	partHeader["Content-Disposition"] = []string{`form-data; name="file"; filename="thumb.jpg"`}
	partHeader["Content-Type"] = []string{"image/jpeg"}
	fw, err := mw.CreatePart(partHeader)
	if err != nil {
		t.Fatalf("creating multipart part: %v", err)
	}
	testImg := image.NewRGBA(image.Rect(0, 0, 100, 100))
	if err := jpeg.Encode(fw, testImg, nil); err != nil {
		t.Fatalf("encoding JPEG: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("closing multipart writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/upload", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleImageUpload(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	select {
	case got := <-uploadedCh:
		if got.contentLength <= 0 {
			t.Error("expected non-empty image body in platform sync request")
		}
		if got.contentType != "image/jpeg" {
			t.Errorf("Content-Type = %q, want image/jpeg", got.contentType)
		}
	case <-time.After(5 * time.Second):
		t.Error("expected platform sync upload call, but mock server received none")
	}
}

func TestHandleDeleteImage_SyncsToPlatform(t *testing.T) {
	deletedCh := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodDelete && strings.Contains(req.URL.Path, "/Images/Primary") {
			select {
			case deletedCh <- struct{}{}:
			default:
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	r, artistSvc := testRouterWithPlatform(t)
	cacheDir := t.TempDir()
	r.imageCacheDir = cacheDir
	addTestConnectionWithURL(t, r, "conn-emby", "Emby", "emby", srv.URL)

	// Platform artist: no local path, images stored in cache dir.
	a := &artist.Artist{
		Name: "Platform Artist", SortName: "Platform Artist", Path: "",
		ThumbExists: true,
	}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	if err := artistSvc.SetPlatformID(context.Background(), a.ID, "conn-emby", "emby-artist-1"); err != nil {
		t.Fatalf("SetPlatformID: %v", err)
	}

	// Write a thumb image to the cache dir so the delete has a file to remove.
	artistCacheDir := filepath.Join(cacheDir, a.ID)
	if err := os.MkdirAll(artistCacheDir, 0o755); err != nil {
		t.Fatalf("creating cache dir: %v", err)
	}
	writeJPEG(t, filepath.Join(artistCacheDir, "folder.jpg"), 100, 100)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/artists/"+a.ID+"/images/thumb", nil)
	req.SetPathValue("id", a.ID)
	req.SetPathValue("type", "thumb")
	w := httptest.NewRecorder()

	r.handleDeleteImage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	select {
	case <-deletedCh:
	case <-time.After(5 * time.Second):
		t.Error("expected platform sync delete call, but mock server received none")
	}
}

// buildThumbUploadRequest constructs a multipart POST for a small JPEG thumb upload.
func buildThumbUploadRequest(t *testing.T, artistID string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if err := mw.WriteField("type", "thumb"); err != nil {
		t.Fatalf("writing multipart field: %v", err)
	}
	partHeader := make(map[string][]string)
	partHeader["Content-Disposition"] = []string{`form-data; name="file"; filename="thumb.jpg"`}
	partHeader["Content-Type"] = []string{"image/jpeg"}
	fw, err := mw.CreatePart(partHeader)
	if err != nil {
		t.Fatalf("creating multipart part: %v", err)
	}
	testImg := image.NewRGBA(image.Rect(0, 0, 100, 100))
	if err := jpeg.Encode(fw, testImg, nil); err != nil {
		t.Fatalf("encoding JPEG: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("closing multipart writer: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+artistID+"/images/upload", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.SetPathValue("id", artistID)
	return req
}

func TestHandleImageUpload_SyncWarnings_NoPlatforms(t *testing.T) {
	r, artistSvc := testRouterWithPlatform(t)
	dir := t.TempDir()
	a := &artist.Artist{Name: "Local Artist", SortName: "Local Artist", Path: dir}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	req := buildThumbUploadRequest(t, a.ID)
	w := httptest.NewRecorder()
	r.handleImageUpload(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	raw, ok := resp["sync_warnings"]
	if !ok {
		t.Fatal("response missing sync_warnings field")
	}
	var warnings []string
	if err := json.Unmarshal(raw, &warnings); err != nil {
		t.Fatalf("decoding sync_warnings: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("expected empty sync_warnings, got %v", warnings)
	}
}

func TestHandleImageUpload_SyncWarnings_PlatformFailure(t *testing.T) {
	// Mock server that returns 500 for every request, simulating a broken platform.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	r, artistSvc := testRouterWithPlatform(t)
	r.imageCacheDir = t.TempDir()
	addTestConnectionWithURL(t, r, "conn-emby", "Emby", "emby", srv.URL)

	a := &artist.Artist{Name: "Platform Artist", SortName: "Platform Artist", Path: ""}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	if err := artistSvc.SetPlatformID(context.Background(), a.ID, "conn-emby", "emby-artist-1"); err != nil {
		t.Fatalf("SetPlatformID: %v", err)
	}

	req := buildThumbUploadRequest(t, a.ID)
	w := httptest.NewRecorder()
	r.handleImageUpload(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	raw, ok := resp["sync_warnings"]
	if !ok {
		t.Fatal("response missing sync_warnings field")
	}
	var warnings []string
	if err := json.Unmarshal(raw, &warnings); err != nil {
		t.Fatalf("decoding sync_warnings: %v", err)
	}
	if len(warnings) == 0 {
		t.Fatal("expected non-empty sync_warnings when platform returns 500")
	}
	if !strings.Contains(warnings[0], "Emby") {
		t.Errorf("warning should contain connection name, got %q", warnings[0])
	}
	if !strings.Contains(warnings[0], "image upload failed") {
		t.Errorf("warning should mention upload failure, got %q", warnings[0])
	}
}

func TestHandleDeleteImage_SyncWarnings_PlatformFailure(t *testing.T) {
	// Mock server that returns 500 for every request, simulating a broken platform.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	r, artistSvc := testRouterWithPlatform(t)
	cacheDir := t.TempDir()
	r.imageCacheDir = cacheDir
	addTestConnectionWithURL(t, r, "conn-emby", "Emby", "emby", srv.URL)

	a := &artist.Artist{
		Name: "Platform Artist", SortName: "Platform Artist", Path: "",
		ThumbExists: true,
	}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	if err := artistSvc.SetPlatformID(context.Background(), a.ID, "conn-emby", "emby-artist-1"); err != nil {
		t.Fatalf("SetPlatformID: %v", err)
	}

	artistCacheDir := filepath.Join(cacheDir, a.ID)
	if err := os.MkdirAll(artistCacheDir, 0o755); err != nil {
		t.Fatalf("creating cache dir: %v", err)
	}
	writeJPEG(t, filepath.Join(artistCacheDir, "folder.jpg"), 100, 100)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/artists/"+a.ID+"/images/thumb", nil)
	req.SetPathValue("id", a.ID)
	req.SetPathValue("type", "thumb")
	w := httptest.NewRecorder()
	r.handleDeleteImage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	raw, ok := resp["sync_warnings"]
	if !ok {
		t.Fatal("response missing sync_warnings field")
	}
	var warnings []string
	if err := json.Unmarshal(raw, &warnings); err != nil {
		t.Fatalf("decoding sync_warnings: %v", err)
	}
	if len(warnings) == 0 {
		t.Fatal("expected non-empty sync_warnings when platform returns 500")
	}
	if !strings.Contains(warnings[0], "Emby") {
		t.Errorf("warning should contain connection name, got %q", warnings[0])
	}
	if !strings.Contains(warnings[0], "image delete failed") {
		t.Errorf("warning should mention delete failure, got %q", warnings[0])
	}
}

func TestSyncImageToPlatforms_GetByIDError(t *testing.T) {
	r, artistSvc := testRouterWithPlatform(t)
	dir := t.TempDir()
	a := &artist.Artist{Name: "Test Artist", SortName: "Test Artist", Path: dir}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// Write a JPEG so syncImageToPlatforms can find and read an image to upload.
	writeJPEG(t, filepath.Join(dir, "folder.jpg"), 100, 100)

	// Create a connection and link it as a platform ID.
	addTestConnection(t, r, "conn-orphan", "Orphaned Server", "emby")
	if err := artistSvc.SetPlatformID(context.Background(), a.ID, "conn-orphan", "emby-artist-1"); err != nil {
		t.Fatalf("SetPlatformID: %v", err)
	}

	// Delete the connection with FK enforcement off so the platform ID row
	// remains, simulating an orphaned mapping after a connection is removed.
	if _, err := r.db.ExecContext(context.Background(), "PRAGMA foreign_keys = OFF"); err != nil {
		t.Fatalf("disabling FK: %v", err)
	}
	if _, err := r.db.ExecContext(context.Background(), "DELETE FROM connections WHERE id = 'conn-orphan'"); err != nil {
		t.Fatalf("deleting orphaned connection: %v", err)
	}
	if _, err := r.db.ExecContext(context.Background(), "PRAGMA foreign_keys = ON"); err != nil {
		t.Fatalf("re-enabling FK: %v", err)
	}

	warnings := r.syncImageToPlatforms(context.Background(), a, "thumb")

	if len(warnings) == 0 {
		t.Fatal("expected warning for orphaned connection, got none")
	}
	if !strings.Contains(warnings[0], "failed to load") {
		t.Errorf("warning should mention load failure, got %q", warnings[0])
	}
}

func TestHandleImageUpload_SyncWarnings_UnsupportedConnType(t *testing.T) {
	r, artistSvc := testRouterWithPlatform(t)
	r.imageCacheDir = t.TempDir()

	// A connection whose type is not handled by the image sync switch.
	addTestConnection(t, r, "conn-lidarr", "Lidarr", "lidarr")

	a := &artist.Artist{Name: "Platform Artist", SortName: "Platform Artist", Path: ""}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	if err := artistSvc.SetPlatformID(context.Background(), a.ID, "conn-lidarr", "lidarr-artist-1"); err != nil {
		t.Fatalf("SetPlatformID: %v", err)
	}

	req := buildThumbUploadRequest(t, a.ID)
	w := httptest.NewRecorder()
	r.handleImageUpload(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	raw, ok := resp["sync_warnings"]
	if !ok {
		t.Fatal("response missing sync_warnings field")
	}
	var warnings []string
	if err := json.Unmarshal(raw, &warnings); err != nil {
		t.Fatalf("decoding sync_warnings: %v", err)
	}
	if len(warnings) == 0 {
		t.Fatal("expected warning for unsupported connection type, got none")
	}
	if !strings.Contains(warnings[0], "unsupported connection type") {
		t.Errorf("warning should mention unsupported type, got %q", warnings[0])
	}
}

func TestSetSyncWarningTrigger_Truncation(t *testing.T) {
	t.Run("per-message truncation", func(t *testing.T) {
		// Build a message that exceeds maxWarningLen (200 chars).
		long := strings.Repeat("x", 201)
		w := httptest.NewRecorder()
		setSyncWarningTrigger(w, []string{long})

		hdr := w.Header().Get("HX-Trigger")
		if hdr == "" {
			t.Fatal("HX-Trigger header not set")
		}
		if !strings.Contains(hdr, "(truncated)") {
			t.Errorf("expected truncation marker in header, got: %s", hdr)
		}
		// Original 201-char message must be cut, not present verbatim.
		if strings.Contains(hdr, long) {
			t.Error("full untruncated message should not appear in header")
		}
	})

	t.Run("total payload cap fallback", func(t *testing.T) {
		// Build enough warnings to exceed maxHeaderBytes (1000 bytes) even after
		// per-message truncation. 10 warnings of 200 chars each produce a JSON
		// payload well over 1000 bytes.
		warnings := make([]string, 10)
		for i := range warnings {
			warnings[i] = strings.Repeat("w", 200)
		}
		w := httptest.NewRecorder()
		setSyncWarningTrigger(w, warnings)

		hdr := w.Header().Get("HX-Trigger")
		if hdr == "" {
			t.Fatal("HX-Trigger header not set")
		}
		// Fallback replaces individual messages with a summary count.
		if !strings.Contains(hdr, "platform sync warnings") {
			t.Errorf("expected summary count fallback in header, got: %s", hdr)
		}
	})

	t.Run("no truncation at exact limit", func(t *testing.T) {
		// A message of exactly maxWarningLen (200) chars should not be truncated.
		exact := strings.Repeat("y", 200)
		w := httptest.NewRecorder()
		setSyncWarningTrigger(w, []string{exact})

		hdr := w.Header().Get("HX-Trigger")
		if strings.Contains(hdr, "(truncated)") {
			t.Errorf("message at exact limit should not be truncated, got: %s", hdr)
		}
	})
}
