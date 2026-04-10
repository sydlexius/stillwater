package api

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
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
	img "github.com/sydlexius/stillwater/internal/image"
	"github.com/sydlexius/stillwater/internal/platform"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/internal/publish"
)

// testRouterWithPlatform returns a Router that includes a platform service,
// required by setArtistImageFlag for naming-config lookup.
func testRouterWithPlatform(t *testing.T) (*Router, *artist.Service) {
	t.Helper()
	r, artistSvc := testRouter(t)
	platSvc := platform.NewService(r.db)
	r.platformService = platSvc
	// Rebuild publisher with the platform service so image sync tests work.
	r.publisher = publish.New(publish.Deps{
		ArtistService:      r.artistService,
		ConnectionService:  r.connectionService,
		NFOSnapshotService: r.nfoSnapshotService,
		PlatformService:    platSvc,
		ExpectedWrites:     r.expectedWrites,
		ImageCacheDir:      r.imageCacheDir,
		Logger:             r.logger,
	})
	return r, artistSvc
}

// setImageCacheDir sets both the Router's and Publisher's image cache dir.
func setImageCacheDir(r *Router, dir string) {
	r.imageCacheDir = dir
	if r.publisher != nil {
		r.publisher.SetImageCacheDir(dir)
	}
}

// failingRemover is a FileRemover stub that always returns an error.
type failingRemover struct{ err error }

func (f failingRemover) Remove(_ string) error { return f.err }

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

// writeJPEGWithProvenance creates a JPEG file at path with the given dimensions
// and injects Stillwater EXIF provenance metadata. This allows tests to verify
// that setArtistImageFlag reads back provenance from saved image files.
func writeJPEGWithProvenance(t *testing.T, path string, w, h int, meta *img.ExifMeta) {
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

	data := buf.Bytes()
	if meta != nil {
		injected, err := img.InjectMeta(data, meta)
		if err != nil {
			t.Fatalf("injecting EXIF meta: %v", err)
		}
		data = injected
	}

	if err := os.WriteFile(path, data, 0o644); err != nil {
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

func TestRequireArtistPath_Pathless(t *testing.T) {
	r, _ := testRouterWithPlatform(t)

	// Artist with empty path (pathless library)
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
	setImageCacheDir(r, cacheDir)

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
	setImageCacheDir(r, "")

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
	// Verify HTTP/2 support is preserved from DefaultTransport.Clone().
	if !transport.ForceAttemptHTTP2 {
		t.Error("ForceAttemptHTTP2 should be true (inherited from DefaultTransport)")
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
	uploadedCh := make(chan syncCapture, 3)
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
	setImageCacheDir(r, t.TempDir())
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
	// Drain check: verify no unexpected extra uploads.
	select {
	case <-uploadedCh:
		t.Error("unexpected extra platform sync upload")
	default:
	}
}

func TestHandleDeleteImage_SyncsToPlatform(t *testing.T) {
	deletedCh := make(chan struct{}, 3)
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
	setImageCacheDir(r, cacheDir)
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
	// Drain check: verify no unexpected extra deletes.
	select {
	case <-deletedCh:
		t.Error("unexpected extra platform sync delete")
	default:
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
	setImageCacheDir(r, t.TempDir())
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

	hxTrigger := w.Header().Get("HX-Trigger")
	if hxTrigger == "" {
		t.Fatal("HX-Trigger header not set when sync warnings present")
	}
	var triggerPayload map[string][]string
	if err := json.Unmarshal([]byte(hxTrigger), &triggerPayload); err != nil {
		t.Fatalf("HX-Trigger is not valid JSON: %v -- value: %s", err, hxTrigger)
	}
	if len(triggerPayload["syncWarning"]) == 0 {
		t.Error("HX-Trigger syncWarning payload is empty")
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
	setImageCacheDir(r, cacheDir)
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

	warnings := r.publisher.SyncImageToPlatforms(context.Background(), a, "thumb")

	if len(warnings) == 0 {
		t.Fatal("expected warning for orphaned connection, got none")
	}
	if !strings.Contains(warnings[0], "failed to load") {
		t.Errorf("warning should mention load failure, got %q", warnings[0])
	}
}

func TestHandleImageUpload_SyncWarnings_UnsupportedConnType(t *testing.T) {
	r, artistSvc := testRouterWithPlatform(t)
	setImageCacheDir(r, t.TempDir())

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

func TestDeleteImageFromPlatforms_GetByIDError(t *testing.T) {
	r, artistSvc := testRouterWithPlatform(t)
	dir := t.TempDir()
	a := &artist.Artist{Name: "Test Artist", SortName: "Test Artist", Path: dir}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

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

	warnings := r.deleteImageFromPlatforms(context.Background(), a, "thumb")

	if len(warnings) == 0 {
		t.Fatal("expected warning for orphaned connection, got none")
	}
	if !strings.Contains(warnings[0], "failed to load") {
		t.Errorf("warning should mention load failure, got %q", warnings[0])
	}
}

func TestHandleDeleteImage_SyncWarnings_UnsupportedConnType(t *testing.T) {
	r, artistSvc := testRouterWithPlatform(t)
	cacheDir := t.TempDir()
	setImageCacheDir(r, cacheDir)

	addTestConnection(t, r, "conn-lidarr", "Lidarr", "lidarr")

	a := &artist.Artist{Name: "Platform Artist", SortName: "Platform Artist", Path: "", ThumbExists: true}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	if err := artistSvc.SetPlatformID(context.Background(), a.ID, "conn-lidarr", "lidarr-artist-1"); err != nil {
		t.Fatalf("SetPlatformID: %v", err)
	}

	// Create a thumb file so deleteImageFiles finds something to delete,
	// which triggers the platform sync (and its warning).
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
		t.Fatal("expected warning for unsupported connection type, got none")
	}
	if !strings.Contains(warnings[0], "unsupported connection type") {
		t.Errorf("warning should mention unsupported type, got %q", warnings[0])
	}
}

func TestSetSyncWarningTrigger_Truncation(t *testing.T) {
	// assertTruncated decodes the HX-Trigger header, verifies the first warning
	// was cut to exactly maxWarningRunes runes with the "(truncated)" suffix, and
	// confirms the header is valid JSON.
	assertTruncated := func(t *testing.T, hdr string) {
		t.Helper()
		if !strings.Contains(hdr, "(truncated)") {
			t.Fatalf("expected truncation marker in header, got: %s", hdr)
		}
		var payload map[string][]string
		if err := json.Unmarshal([]byte(hdr), &payload); err != nil {
			t.Fatalf("HX-Trigger is not valid JSON: %v -- value: %s", err, hdr)
		}
		msgs := payload["syncWarning"]
		if len(msgs) == 0 {
			t.Fatal("syncWarning missing from header payload")
		}
		preserved := strings.TrimSuffix(msgs[0], " (truncated)")
		if got := len([]rune(preserved)); got != maxWarningRunes {
			t.Errorf("preserved rune count = %d, want %d", got, maxWarningRunes)
		}
	}

	t.Run("per-message truncation ASCII", func(t *testing.T) {
		// 201 ASCII chars: byte and rune counts are identical.
		long := strings.Repeat("x", maxWarningRunes+1)
		w := httptest.NewRecorder()
		setSyncWarningTrigger(w, []string{long})

		hdr := w.Header().Get("HX-Trigger")
		if hdr == "" {
			t.Fatal("HX-Trigger header not set")
		}
		assertTruncated(t, hdr)
		if strings.Contains(hdr, long) {
			t.Error("full untruncated message should not appear in header")
		}
	})

	t.Run("per-message truncation 2-byte rune", func(t *testing.T) {
		// 201 two-byte runes (é = U+00E9). Byte-based slicing at byte 200 would
		// split the 200th rune; rune-based truncation must preserve exactly 200 runes.
		long := strings.Repeat("\u00e9", maxWarningRunes+1)
		w := httptest.NewRecorder()
		setSyncWarningTrigger(w, []string{long})

		hdr := w.Header().Get("HX-Trigger")
		if hdr == "" {
			t.Fatal("HX-Trigger header not set")
		}
		assertTruncated(t, hdr)
	})

	t.Run("per-message truncation 4-byte rune", func(t *testing.T) {
		// 201 four-byte runes (emoji). Verifies all Unicode planes are handled,
		// not just the basic multilingual plane.
		long := strings.Repeat("\U0001F600", maxWarningRunes+1)
		w := httptest.NewRecorder()
		setSyncWarningTrigger(w, []string{long})

		hdr := w.Header().Get("HX-Trigger")
		if hdr == "" {
			t.Fatal("HX-Trigger header not set")
		}
		assertTruncated(t, hdr)
	})

	t.Run("per-message truncation mixed ASCII and multibyte", func(t *testing.T) {
		// ASCII prefix followed by multibyte runes: models real platform names like
		// "My Emby Server" with occasional accented characters. Byte position 200
		// falls inside a multibyte rune; must not split it.
		long := strings.Repeat("a", maxWarningRunes-2) + strings.Repeat("\u00e9", 3)
		w := httptest.NewRecorder()
		setSyncWarningTrigger(w, []string{long})

		hdr := w.Header().Get("HX-Trigger")
		if hdr == "" {
			t.Fatal("HX-Trigger header not set")
		}
		assertTruncated(t, hdr)
	})

	t.Run("no truncation at exact limit ASCII", func(t *testing.T) {
		// Exactly maxWarningRunes ASCII chars: must pass through unchanged.
		exact := strings.Repeat("y", maxWarningRunes)
		w := httptest.NewRecorder()
		setSyncWarningTrigger(w, []string{exact})

		hdr := w.Header().Get("HX-Trigger")
		if hdr == "" {
			t.Fatal("HX-Trigger header not set")
		}
		if strings.Contains(hdr, "(truncated)") {
			t.Errorf("message at exact ASCII limit should not be truncated, got: %s", hdr)
		}
	})

	t.Run("no truncation at exact limit multibyte", func(t *testing.T) {
		// Exactly maxWarningRunes two-byte runes = 400 bytes. The old byte-based
		// check (len(msg) > 200) would incorrectly truncate this.
		exact := strings.Repeat("\u00e9", maxWarningRunes)
		w := httptest.NewRecorder()
		setSyncWarningTrigger(w, []string{exact})

		hdr := w.Header().Get("HX-Trigger")
		if hdr == "" {
			t.Fatal("HX-Trigger header not set")
		}
		if strings.Contains(hdr, "(truncated)") {
			t.Errorf("200-rune multibyte message should not be truncated, got: %s", hdr)
		}
	})

	t.Run("total payload cap fallback", func(t *testing.T) {
		// 10 warnings of maxWarningRunes runes each: after per-rune truncation the
		// JSON payload exceeds maxHeaderBytes, triggering the summary fallback.
		warnings := make([]string, 10)
		for i := range warnings {
			warnings[i] = strings.Repeat("w", maxWarningRunes)
		}
		w := httptest.NewRecorder()
		setSyncWarningTrigger(w, warnings)

		hdr := w.Header().Get("HX-Trigger")
		if hdr == "" {
			t.Fatal("HX-Trigger header not set")
		}
		// Fallback replaces individual messages with a single summary count.
		want := fmt.Sprintf("%d platform sync warnings", len(warnings))
		if !strings.Contains(hdr, want) {
			t.Errorf("expected %q in header, got: %s", want, hdr)
		}
	})
}

func TestHandleImageCrop_SyncWarnings_NoPlatforms(t *testing.T) {
	r, artistSvc := testRouterWithPlatform(t)
	dir := t.TempDir()
	a := &artist.Artist{Name: "Crop Artist", SortName: "Crop Artist", Path: dir}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// Encode a small JPEG as base64 for the crop request body.
	testImg := image.NewRGBA(image.Rect(0, 0, 100, 100))
	var jpegBuf bytes.Buffer
	if err := jpeg.Encode(&jpegBuf, testImg, nil); err != nil {
		t.Fatalf("encoding JPEG: %v", err)
	}
	b64Data := base64.StdEncoding.EncodeToString(jpegBuf.Bytes())

	reqBody, err := json.Marshal(map[string]string{
		"image_data": b64Data,
		"type":       "thumb",
	})
	if err != nil {
		t.Fatalf("marshaling request body: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/crop", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()
	r.handleImageCrop(w, req)

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

func TestHandleImageCrop_SyncWarnings_PlatformFailure(t *testing.T) {
	// Mock server that returns 500 for every request, simulating a broken platform.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	r, artistSvc := testRouterWithPlatform(t)
	setImageCacheDir(r, t.TempDir())
	addTestConnectionWithURL(t, r, "conn-emby", "Emby", "emby", srv.URL)

	a := &artist.Artist{Name: "Platform Crop Artist", SortName: "Platform Crop Artist", Path: ""}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	if err := artistSvc.SetPlatformID(context.Background(), a.ID, "conn-emby", "emby-artist-1"); err != nil {
		t.Fatalf("SetPlatformID: %v", err)
	}

	// Encode a small JPEG as base64 for the crop request body.
	testImg := image.NewRGBA(image.Rect(0, 0, 100, 100))
	var jpegBuf bytes.Buffer
	if err := jpeg.Encode(&jpegBuf, testImg, nil); err != nil {
		t.Fatalf("encoding JPEG: %v", err)
	}
	b64Data := base64.StdEncoding.EncodeToString(jpegBuf.Bytes())

	reqBody, err := json.Marshal(map[string]string{
		"image_data": b64Data,
		"type":       "thumb",
	})
	if err != nil {
		t.Fatalf("marshaling request body: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/crop", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()
	r.handleImageCrop(w, req)

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

func TestExtractImageFetchParams_MalformedJSON(t *testing.T) {
	body := strings.NewReader(`{bad json`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/x/images/fetch", body)
	req.Header.Set("Content-Type", "application/json")

	_, _, err := extractImageFetchParams(req)
	if err == nil {
		t.Fatal("expected error for malformed JSON, got nil")
	}
	if !strings.Contains(err.Error(), "invalid request body") {
		t.Errorf("error should mention invalid request body, got %q", err.Error())
	}
}

func TestExtractImageFetchParams_ValidJSON(t *testing.T) {
	body := strings.NewReader(`{"url":"https://example.com/img.jpg","type":"thumb"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/x/images/fetch", body)
	req.Header.Set("Content-Type", "application/json")

	u, it, err := extractImageFetchParams(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u != "https://example.com/img.jpg" {
		t.Errorf("url = %q, want %q", u, "https://example.com/img.jpg")
	}
	if it != "thumb" {
		t.Errorf("type = %q, want %q", it, "thumb")
	}
}

func TestHandleDeleteImage_FanartRemoveFailure(t *testing.T) {
	// Mock server that records whether a DELETE was received.
	// Use a channel (not a bare bool) to avoid an unprotected cross-goroutine write
	// if the production guard is ever relaxed.
	deletedCh := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodDelete {
			select {
			case deletedCh <- struct{}{}:
			default:
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	r, artistSvc := testRouterWithPlatform(t)
	dir := t.TempDir()
	addTestConnectionWithURL(t, r, "conn-emby", "Emby", "emby", srv.URL)

	// Inject a remover that always fails, simulating a permission error.
	r.fileRemover = failingRemover{err: fmt.Errorf("permission denied")}

	a := &artist.Artist{
		Name: "Fanart Artist", SortName: "Fanart Artist", Path: dir,
		FanartExists: true,
	}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	if err := artistSvc.SetPlatformID(context.Background(), a.ID, "conn-emby", "emby-artist-1"); err != nil {
		t.Fatalf("SetPlatformID: %v", err)
	}

	// Create two fanart files so DiscoverFanart finds them.
	writeJPEG(t, filepath.Join(dir, "fanart.jpg"), 100, 100)
	writeJPEG(t, filepath.Join(dir, "fanart2.jpg"), 100, 100)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/artists/"+a.ID+"/images/fanart", nil)
	req.SetPathValue("id", a.ID)
	req.SetPathValue("type", "fanart")
	w := httptest.NewRecorder()
	r.handleDeleteImage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	// Verify warning is surfaced about failed removals.
	raw, ok := resp["sync_warnings"]
	if !ok {
		t.Fatal("response missing sync_warnings field")
	}
	var warnings []string
	if err := json.Unmarshal(raw, &warnings); err != nil {
		t.Fatalf("decoding sync_warnings: %v", err)
	}
	found := false
	for _, msg := range warnings {
		if strings.Contains(msg, "could not be deleted") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected warning about failed deletion, got %v", warnings)
	}

	// Platform delete must NOT be called when local removal failed.
	select {
	case <-deletedCh:
		t.Error("platform delete was called despite local removal failure")
	default:
	}
}

func TestHandleFanartBatchDelete_SyncCalledAfterDelete(t *testing.T) {
	// Track uploads to the mock platform server. Use a channel so the test
	// goroutine can wait for the uploads to arrive without polling.
	type uploadCapture struct {
		index string
	}
	uploadCh := make(chan uploadCapture, 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost && strings.Contains(req.URL.Path, "/Images/Backdrop/") {
			parts := strings.Split(req.URL.Path, "/")
			idx := parts[len(parts)-1]
			select {
			case uploadCh <- uploadCapture{index: idx}:
			default:
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	r, artistSvc := testRouterWithPlatform(t)
	dir := t.TempDir()
	addTestConnectionWithURL(t, r, "conn-emby", "Emby", "emby", srv.URL)

	a := &artist.Artist{
		Name: "Batch Delete Sync", SortName: "Batch Delete Sync", Path: dir,
		FanartExists: true, FanartCount: 3,
	}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	if err := artistSvc.SetPlatformID(context.Background(), a.ID, "conn-emby", "emby-bd-1"); err != nil {
		t.Fatalf("SetPlatformID: %v", err)
	}

	// Create 3 fanart files (default primary = fanart.jpg).
	for _, name := range []string{"fanart.jpg", "fanart2.jpg", "fanart3.jpg"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("img-"+name), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Delete index 1 (fanart2.jpg). Survivors: fanart.jpg and fanart3.jpg.
	body := `{"indices": [1]}`
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/artists/"+a.ID+"/images/fanart/batch",
		strings.NewReader(body))
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleFanartBatchDelete(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp struct {
		Status       string   `json:"status"`
		Deleted      []string `json:"deleted"`
		Count        int      `json:"count"`
		SyncWarnings []string `json:"sync_warnings"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	if resp.Status != "deleted" {
		t.Errorf("status = %q, want deleted", resp.Status)
	}
	if len(resp.Deleted) != 1 {
		t.Errorf("expected 1 deleted, got %d: %v", len(resp.Deleted), resp.Deleted)
	}
	if resp.Count != 2 {
		t.Errorf("count = %d, want 2", resp.Count)
	}

	// Verify the mock platform received sync uploads for the 2 surviving fanart files.
	received := 0
	for range 2 {
		select {
		case <-uploadCh:
			received++
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for platform sync upload %d", received+1)
		}
	}
	if received != 2 {
		t.Errorf("expected 2 platform sync uploads, got %d", received)
	}
	// Drain check: verify no unexpected extra uploads.
	select {
	case <-uploadCh:
		t.Error("unexpected extra platform sync upload beyond expected 2")
	default:
	}

	if len(resp.SyncWarnings) != 0 {
		t.Errorf("expected no sync warnings, got %v", resp.SyncWarnings)
	}
}

func TestHandleFanartBatchDelete_RenumberFailureSkipsSync(t *testing.T) {
	// Track whether any upload reaches the mock platform server.
	uploadCh := make(chan struct{}, 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost && strings.Contains(req.URL.Path, "/Images/Backdrop/") {
			select {
			case uploadCh <- struct{}{}:
			default:
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	r, artistSvc := testRouterWithPlatform(t)
	dir := t.TempDir()
	addTestConnectionWithURL(t, r, "conn-emby", "Emby", "emby", srv.URL)

	a := &artist.Artist{
		Name: "Renumber Fail", SortName: "Renumber Fail", Path: dir,
		FanartExists: true, FanartCount: 3,
	}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	if err := artistSvc.SetPlatformID(context.Background(), a.ID, "conn-emby", "emby-rf-1"); err != nil {
		t.Fatalf("SetPlatformID: %v", err)
	}

	// Create 3 fanart files.
	for _, name := range []string{"fanart.jpg", "fanart2.jpg", "fanart3.jpg"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("img-"+name), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Create a non-empty directory at the temp file path that RenumberFanart
	// uses during Phase 1. os.Remove on a non-empty directory returns ENOTEMPTY,
	// which RenumberFanart treats as a fatal error (not ErrNotExist).
	blocker := filepath.Join(dir, "fanart_renumber_0.jpg.tmp")
	if err := os.MkdirAll(blocker, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blocker, "stale"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	body := `{"indices": [1]}`
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/artists/"+a.ID+"/images/fanart/batch",
		strings.NewReader(body))
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleFanartBatchDelete(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp struct {
		SyncWarnings []string `json:"sync_warnings"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	// Verify the renumber warning is present.
	found := false
	for _, msg := range resp.SyncWarnings {
		if strings.Contains(msg, "could not be renumbered") && strings.Contains(msg, "platform sync skipped") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected renumber failure warning, got %v", resp.SyncWarnings)
	}

	// No uploads should have reached the platform.
	select {
	case <-uploadCh:
		t.Error("platform sync upload was called despite renumber failure")
	default:
	}
}

func TestHandleFanartBatchDelete_SyncWarningsPropagated(t *testing.T) {
	// Mock platform server that returns 500, simulating sync failure.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	r, artistSvc := testRouterWithPlatform(t)
	dir := t.TempDir()
	addTestConnectionWithURL(t, r, "conn-emby", "Emby", "emby", srv.URL)

	a := &artist.Artist{
		Name: "Sync Warning", SortName: "Sync Warning", Path: dir,
		FanartExists: true, FanartCount: 2,
	}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	if err := artistSvc.SetPlatformID(context.Background(), a.ID, "conn-emby", "emby-sw-1"); err != nil {
		t.Fatalf("SetPlatformID: %v", err)
	}

	// Create 2 fanart files.
	for _, name := range []string{"fanart.jpg", "fanart2.jpg"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("img-"+name), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Delete index 0. After renumber, fanart2.jpg becomes fanart.jpg.
	body := `{"indices": [0]}`
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/artists/"+a.ID+"/images/fanart/batch",
		strings.NewReader(body))
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleFanartBatchDelete(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp struct {
		SyncWarnings []string `json:"sync_warnings"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	if len(resp.SyncWarnings) == 0 {
		t.Fatal("expected sync warnings when platform returns 500")
	}
	found := false
	for _, w := range resp.SyncWarnings {
		if strings.Contains(w, "Emby") && strings.Contains(w, "upload failed") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected warning mentioning 'Emby' and 'upload failed', got %v", resp.SyncWarnings)
	}
}

func TestHandleDeleteImage_ThumbRemoveFailure(t *testing.T) {
	// Mock server that records whether a DELETE was received.
	deletedCh := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodDelete {
			select {
			case deletedCh <- struct{}{}:
			default:
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	r, artistSvc := testRouterWithPlatform(t)
	dir := t.TempDir()
	addTestConnectionWithURL(t, r, "conn-emby", "Emby", "emby", srv.URL)

	// Inject a remover that always fails, simulating a permission error.
	r.fileRemover = failingRemover{err: fmt.Errorf("permission denied")}

	a := &artist.Artist{
		Name: "Thumb Artist", SortName: "Thumb Artist", Path: dir,
		ThumbExists: true,
	}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	if err := artistSvc.SetPlatformID(context.Background(), a.ID, "conn-emby", "emby-artist-1"); err != nil {
		t.Fatalf("SetPlatformID: %v", err)
	}

	writeJPEG(t, filepath.Join(dir, "folder.jpg"), 100, 100)

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
	found := false
	for _, msg := range warnings {
		if strings.Contains(msg, "could not be deleted") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected warning about failed deletion, got %v", warnings)
	}

	// Platform delete must NOT be called when local removal failed.
	select {
	case <-deletedCh:
		t.Error("platform delete was called despite local removal failure")
	default:
	}
}

func TestSetArtistImageFlag_RecordsProvenance(t *testing.T) {
	r, artistSvc := testRouterWithPlatform(t)
	dir := t.TempDir()
	a := &artist.Artist{Name: "Provenance Test", SortName: "Provenance Test", Path: dir}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// Write a JPEG with embedded Stillwater provenance metadata.
	meta := &img.ExifMeta{
		Source:  "fanarttv",
		DHash:   "abcd1234abcd1234",
		Fetched: time.Now().UTC(),
		Mode:    "auto",
	}
	writeJPEGWithProvenance(t, filepath.Join(dir, "folder.jpg"), 800, 800, meta)

	// Call setArtistImageFlag which should now read provenance and record it.
	r.setArtistImageFlag(context.Background(), a, "thumb", true)

	// Verify the image exists flag was set.
	if !a.ThumbExists {
		t.Fatal("ThumbExists should be true")
	}

	// Retrieve the image metadata from the database and verify provenance fields.
	images, err := artistSvc.GetImagesForArtist(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("GetImagesForArtist: %v", err)
	}

	var thumbImg *artist.ArtistImage
	for i := range images {
		if images[i].ImageType == "thumb" && images[i].SlotIndex == 0 {
			thumbImg = &images[i]
			break
		}
	}
	if thumbImg == nil {
		t.Fatal("no thumb image found in database")
	}

	if thumbImg.PHash != "abcd1234abcd1234" {
		t.Errorf("PHash = %q, want %q", thumbImg.PHash, "abcd1234abcd1234")
	}
	if thumbImg.Source != "fanarttv" {
		t.Errorf("Source = %q, want %q", thumbImg.Source, "fanarttv")
	}
	if thumbImg.FileFormat != "jpeg" {
		t.Errorf("FileFormat = %q, want %q", thumbImg.FileFormat, "jpeg")
	}
	if thumbImg.LastWrittenAt == "" {
		t.Error("LastWrittenAt should be populated with the file mtime")
	}
}

func TestSetArtistImageFlag_NoProvenance_StillWorks(t *testing.T) {
	r, artistSvc := testRouterWithPlatform(t)
	dir := t.TempDir()
	a := &artist.Artist{Name: "No Provenance", SortName: "No Provenance", Path: dir}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// Write a plain JPEG without Stillwater provenance metadata.
	writeJPEG(t, filepath.Join(dir, "folder.jpg"), 600, 600)

	// setArtistImageFlag should still work -- provenance is supplementary.
	r.setArtistImageFlag(context.Background(), a, "thumb", true)

	if !a.ThumbExists {
		t.Fatal("ThumbExists should be true even without provenance")
	}

	// Verify the image row exists but provenance fields are empty
	// (ReadProvenance returns nil for images without the Stillwater tag).
	images, err := artistSvc.GetImagesForArtist(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("GetImagesForArtist: %v", err)
	}

	var thumbImg *artist.ArtistImage
	for i := range images {
		if images[i].ImageType == "thumb" && images[i].SlotIndex == 0 {
			thumbImg = &images[i]
			break
		}
	}
	if thumbImg == nil {
		t.Fatal("no thumb image found in database")
	}

	if thumbImg.PHash != "" {
		t.Errorf("PHash should be empty for image without provenance, got %q", thumbImg.PHash)
	}
	if thumbImg.Source != "" {
		t.Errorf("Source should be empty for image without provenance, got %q", thumbImg.Source)
	}
	// FileFormat should still be detected from extension.
	if thumbImg.FileFormat != "jpeg" {
		t.Errorf("FileFormat = %q, want %q (detected from extension)", thumbImg.FileFormat, "jpeg")
	}
	// LastWrittenAt should still be populated from file mtime.
	if thumbImg.LastWrittenAt == "" {
		t.Error("LastWrittenAt should be populated even without EXIF provenance")
	}
}

func TestSetArtistImageFlag_ClearsProvenance_OnDelete(t *testing.T) {
	r, artistSvc := testRouterWithPlatform(t)
	dir := t.TempDir()
	a := &artist.Artist{Name: "Delete Provenance", SortName: "Delete Provenance", Path: dir}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// Write a JPEG with provenance and set the flag.
	meta := &img.ExifMeta{Source: "user", DHash: "ffff0000ffff0000"}
	writeJPEGWithProvenance(t, filepath.Join(dir, "folder.jpg"), 500, 500, meta)
	r.setArtistImageFlag(context.Background(), a, "thumb", true)

	// Verify provenance was recorded.
	images, err := artistSvc.GetImagesForArtist(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("GetImagesForArtist: %v", err)
	}
	if len(images) == 0 {
		t.Fatal("expected at least one image after setting flag")
	}

	// Now remove the image file and clear the flag.
	if err := os.Remove(filepath.Join(dir, "folder.jpg")); err != nil {
		t.Fatalf("removing image: %v", err)
	}
	r.setArtistImageFlag(context.Background(), a, "thumb", false)

	if a.ThumbExists {
		t.Error("ThumbExists should be false after clearing")
	}

	// Verify provenance was cleared in the database, not just on the struct.
	// UpsertAll (via Update) deletes rows for image types that no longer exist,
	// so the thumb row should be gone entirely.
	imagesAfter, err := artistSvc.GetImagesForArtist(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("GetImagesForArtist after clear: %v", err)
	}
	for _, im := range imagesAfter {
		if im.ImageType == "thumb" && im.SlotIndex == 0 && im.Exists {
			t.Errorf("thumb row should not exist after clearing the flag")
		}
	}
}

// TestHandleServeImage_ClearsStaleFlag verifies that when the DB says an image
// exists but the file is missing on disk, the serve endpoint returns 404 and
// asynchronously clears the stale exists flag so subsequent UI renders show a
// placeholder instead of a broken image.
func TestHandleServeImage_ClearsStaleFlag(t *testing.T) {
	r, artistSvc := testRouterWithPlatform(t)
	ctx := context.Background()
	dir := t.TempDir()

	a := &artist.Artist{Name: "Stale Flag", SortName: "Stale Flag", Path: dir}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// Write a thumb so the flag is set, then delete the file.
	writeJPEG(t, filepath.Join(dir, "folder.jpg"), 500, 500)
	r.setArtistImageFlag(ctx, a, "thumb", true)
	if !a.ThumbExists {
		t.Fatal("ThumbExists should be true after setting flag")
	}

	// Verify the flag was persisted to the DB before testing its cleanup.
	// Checking only the in-memory field would let this test pass even if the
	// DB write regressed, since the poll loop below checks DB state.
	preImages, err := artistSvc.GetImagesForArtist(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetImagesForArtist (precondition): %v", err)
	}
	thumbPersisted := false
	for _, im := range preImages {
		if im.ImageType == "thumb" && im.SlotIndex == 0 && im.Exists {
			thumbPersisted = true
			break
		}
	}
	if !thumbPersisted {
		t.Fatal("precondition: thumb image row Exists not persisted to DB before file removal")
	}
	preReloaded, err := artistSvc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetByID (precondition): %v", err)
	}
	if !preReloaded.ThumbExists {
		t.Fatal("precondition: artist.ThumbExists not persisted to DB before file removal")
	}

	if err := os.Remove(filepath.Join(dir, "folder.jpg")); err != nil {
		t.Fatalf("removing image: %v", err)
	}

	// Request the image file via the serve endpoint.
	url := fmt.Sprintf("/api/v1/artists/%s/images/thumb/file", a.ID)
	req := httptest.NewRequest("GET", url, nil)
	req.SetPathValue("id", a.ID)
	req.SetPathValue("type", "thumb")
	w := httptest.NewRecorder()

	r.handleServeImage(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}

	// The flag clearing happens asynchronously; poll until it takes effect.
	// The background goroutine uses a 5s context timeout, so allow 6s here.
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		images, err := artistSvc.GetImagesForArtist(ctx, a.ID)
		if err != nil {
			t.Fatalf("GetImagesForArtist: %v", err)
		}
		cleared := true
		for _, im := range images {
			if im.ImageType == "thumb" && im.SlotIndex == 0 && im.Exists {
				cleared = false
				break
			}
		}
		if cleared {
			// Also verify the model-level flag reflects the cleared state.
			updated, err := artistSvc.GetByID(ctx, a.ID)
			if err != nil {
				t.Fatalf("reloading artist: %v", err)
			}
			if updated.ThumbExists {
				t.Error("artist.ThumbExists should be false after flag clear")
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("thumb exists_flag should have been cleared after serving a missing file (timed out)")
}

// TestHandleRandomBackdrop_ServesValidFile verifies that the endpoint serves
// an existing fanart file and returns 200.
func TestHandleRandomBackdrop_ServesValidFile(t *testing.T) {
	r, artistSvc := testRouterWithPlatform(t)
	ctx := context.Background()
	dir := t.TempDir()

	a := &artist.Artist{Name: "Backdrop Artist", SortName: "Backdrop Artist", Path: dir}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	writeJPEG(t, filepath.Join(dir, "fanart.jpg"), 100, 56)

	// Insert the artist_images row directly to avoid the full placeholder
	// generation pipeline that setArtistImageFlag runs.
	if _, err := r.db.ExecContext(ctx,
		`INSERT INTO artist_images (id, artist_id, image_type, slot_index, exists_flag)
		 VALUES (lower(hex(randomblob(16))), ?, 'fanart', 0, 1)
		 ON CONFLICT (artist_id, image_type, slot_index) DO UPDATE SET exists_flag = 1`,
		a.ID); err != nil {
		t.Fatalf("seeding artist_images: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/v1/images/random-backdrop", nil)
	w := httptest.NewRecorder()
	r.handleRandomBackdrop(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

// TestHandleRandomBackdrop_ClearsStaleFlag verifies that when the DB says fanart
// exists but the file is missing, the endpoint returns 404 and synchronously
// clears the stale exists_flag so the entry is not returned again.
func TestHandleRandomBackdrop_ClearsStaleFlag(t *testing.T) {
	r, artistSvc := testRouterWithPlatform(t)
	ctx := context.Background()
	dir := t.TempDir()

	a := &artist.Artist{Name: "Stale Backdrop", SortName: "Stale Backdrop", Path: dir}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// Seed the flag without creating a file so it is immediately stale.
	if _, err := r.db.ExecContext(ctx,
		`INSERT INTO artist_images (id, artist_id, image_type, slot_index, exists_flag)
		 VALUES (lower(hex(randomblob(16))), ?, 'fanart', 0, 1)
		 ON CONFLICT (artist_id, image_type, slot_index) DO UPDATE SET exists_flag = 1`,
		a.ID); err != nil {
		t.Fatalf("seeding artist_images: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/v1/images/random-backdrop", nil)
	w := httptest.NewRecorder()
	r.handleRandomBackdrop(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}

	images, err := artistSvc.GetImagesForArtist(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetImagesForArtist: %v", err)
	}
	for _, im := range images {
		if im.ImageType == "fanart" && im.SlotIndex == 0 && im.Exists {
			t.Error("fanart exists_flag should have been cleared after file was missing")
		}
	}
}

// TestHandleRandomBackdrop_EmptyPool verifies that the endpoint returns 404
// when no artists have fanart flagged as existing.
func TestHandleRandomBackdrop_EmptyPool(t *testing.T) {
	r, _ := testRouterWithPlatform(t)

	req := httptest.NewRequest("GET", "/api/v1/images/random-backdrop", nil)
	w := httptest.NewRecorder()
	r.handleRandomBackdrop(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestSortImageResults(t *testing.T) {
	images := []provider.ImageResult{
		{URL: "a.jpg", Likes: 5, Width: 100, Height: 100},   // area=10000
		{URL: "b.jpg", Likes: 10, Width: 50, Height: 50},    // area=2500
		{URL: "c.jpg", Likes: 0, Width: 1000, Height: 1000}, // area=1000000
		{URL: "d.jpg", Likes: 10, Width: 200, Height: 200},  // area=40000
	}

	t.Run("default sorts by likes descending then resolution", func(t *testing.T) {
		imgs := make([]provider.ImageResult, len(images))
		copy(imgs, images)
		sortImageResults(imgs, "")
		// Likes 10 (area 40000) > Likes 10 (area 2500) > Likes 5 > Likes 0
		if imgs[0].URL != "d.jpg" {
			t.Errorf("expected d.jpg first, got %s", imgs[0].URL)
		}
		if imgs[1].URL != "b.jpg" {
			t.Errorf("expected b.jpg second, got %s", imgs[1].URL)
		}
		if imgs[2].URL != "a.jpg" {
			t.Errorf("expected a.jpg third, got %s", imgs[2].URL)
		}
		if imgs[3].URL != "c.jpg" {
			t.Errorf("expected c.jpg fourth, got %s", imgs[3].URL)
		}
	})

	t.Run("likes sorts by likes descending", func(t *testing.T) {
		imgs := make([]provider.ImageResult, len(images))
		copy(imgs, images)
		sortImageResults(imgs, "likes")
		if imgs[0].URL != "d.jpg" {
			t.Errorf("expected d.jpg first, got %s", imgs[0].URL)
		}
	})

	t.Run("resolution sorts by area descending then likes", func(t *testing.T) {
		imgs := make([]provider.ImageResult, len(images))
		copy(imgs, images)
		sortImageResults(imgs, "resolution")
		// area 1000000 > area 40000 > area 10000 > area 2500
		if imgs[0].URL != "c.jpg" {
			t.Errorf("expected c.jpg first (largest area), got %s", imgs[0].URL)
		}
		if imgs[1].URL != "d.jpg" {
			t.Errorf("expected d.jpg second, got %s", imgs[1].URL)
		}
		if imgs[2].URL != "a.jpg" {
			t.Errorf("expected a.jpg third, got %s", imgs[2].URL)
		}
		if imgs[3].URL != "b.jpg" {
			t.Errorf("expected b.jpg fourth (smallest area), got %s", imgs[3].URL)
		}
	})
}
