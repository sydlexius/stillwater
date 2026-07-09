package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/conflict"
	"github.com/sydlexius/stillwater/internal/httpsafe"
	img "github.com/sydlexius/stillwater/internal/image"
	"github.com/sydlexius/stillwater/internal/platform"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/internal/publish"
	"github.com/sydlexius/stillwater/internal/rule"
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

// writePNG creates a PNG file at path with the given dimensions. Used where the
// image type requires PNG (logos) so tests do not seed mismatched bytes.
func writePNG(t *testing.T, path string, w, h int) {
	t.Helper()
	m := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			m.Set(x, y, color.RGBA{R: 200, G: 100, B: 50, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, m); err != nil {
		t.Fatalf("encoding PNG: %v", err)
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	transport := httpsafe.SafeTransport()
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
	t.Parallel()
	transport := httpsafe.SafeTransport()
	client := &http.Client{Transport: transport}

	// Attempt to connect to a loopback address -- should be rejected.
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://127.0.0.1:1/test", nil)
	resp, err := client.Do(req)
	if err == nil {
		resp.Body.Close()
		t.Fatal("expected error connecting to loopback address")
	}
}

func TestSSRFSafeTransport_EmptyDNS(t *testing.T) {
	t.Parallel()
	// The empty-DNS guard is exercised when a hostname resolves to zero addresses.
	// We cannot easily force that in a unit test (net.DefaultResolver is global),
	// but we verify the guard exists by reading the function.
	// Instead, test that a non-existent host returns an error (not a panic).
	transport := httpsafe.SafeTransport()
	client := &http.Client{Transport: transport}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://this-host-does-not-exist-abc123xyz.invalid/test", nil)
	resp, err := client.Do(req)
	if err == nil {
		resp.Body.Close()
		t.Fatal("expected error for non-existent host")
	}
}

func TestSetArtistImageFlag_UnreadableFile(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	req := httptest.NewRequest(http.MethodGet, url, nil)
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
	t.Parallel()
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

	req := httptest.NewRequest(http.MethodGet, "/api/v1/images/random-backdrop", nil)
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
	t.Parallel()
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

	req := httptest.NewRequest(http.MethodGet, "/api/v1/images/random-backdrop", nil)
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
	t.Parallel()
	r, _ := testRouterWithPlatform(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/images/random-backdrop", nil)
	w := httptest.NewRecorder()
	r.handleRandomBackdrop(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestSortImageResults(t *testing.T) {
	t.Parallel()
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

	t.Run("default sort falls back to resolution when all likes are zero", func(t *testing.T) {
		imgs := []provider.ImageResult{
			{URL: "a.jpg", Likes: 0, Width: 100, Height: 100},   // area=10000
			{URL: "b.jpg", Likes: 0, Width: 50, Height: 50},     // area=2500
			{URL: "c.jpg", Likes: 0, Width: 1000, Height: 1000}, // area=1000000
			{URL: "d.jpg", Likes: 0, Width: 200, Height: 200},   // area=40000
		}

		sortImageResults(imgs, "")

		if imgs[0].URL != "c.jpg" || imgs[1].URL != "d.jpg" || imgs[2].URL != "a.jpg" || imgs[3].URL != "b.jpg" {
			t.Errorf("expected resolution fallback order [c,d,a,b], got [%s, %s, %s, %s]",
				imgs[0].URL, imgs[1].URL, imgs[2].URL, imgs[3].URL)
		}
	})

	t.Run("unknown sort falls back to likes default", func(t *testing.T) {
		imgs := make([]provider.ImageResult, len(images))
		copy(imgs, images)
		sortImageResults(imgs, "bogus")
		// Same expected order as default: likes desc, then area desc
		if imgs[0].URL != "d.jpg" || imgs[1].URL != "b.jpg" || imgs[2].URL != "a.jpg" || imgs[3].URL != "c.jpg" {
			t.Errorf("unexpected fallback order: got [%s, %s, %s, %s]",
				imgs[0].URL, imgs[1].URL, imgs[2].URL, imgs[3].URL)
		}
	})
}

// TestHandleImageUpload_RerunsRulesAfterWrite verifies that a successful image
// upload triggers a best-effort rule re-evaluation so image violations
// (missing thumbnail, etc.) auto-clear without waiting for the next scheduled
// scan. See issue #1028.
func TestHandleImageUpload_RerunsRulesAfterWrite(t *testing.T) {
	t.Parallel()
	called := make(chan string, 1)
	stub := &stubPipeline{
		runForArtistFn: func(_ context.Context, a *artist.Artist) (*rule.RunResult, error) {
			select {
			case called <- a.ID:
			default:
			}
			return &rule.RunResult{}, nil
		},
	}
	r, artistSvc := testRouterWithStubPipeline(t, stub)
	// Wire a platform service + publisher so the upload path resolves naming
	// config and runs to completion.
	platSvc := platform.NewService(r.db)
	r.platformService = platSvc
	r.publisher = publish.New(publish.Deps{
		ArtistService:      r.artistService,
		ConnectionService:  r.connectionService,
		NFOSnapshotService: r.nfoSnapshotService,
		PlatformService:    platSvc,
		ImageCacheDir:      r.imageCacheDir,
		Logger:             r.logger,
	})

	dir := t.TempDir()
	a := &artist.Artist{Name: "Rerun Rules", SortName: "Rerun Rules", Path: dir}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if err := mw.WriteField("type", "thumb"); err != nil {
		t.Fatalf("writing field: %v", err)
	}
	partHeader := make(map[string][]string)
	partHeader["Content-Disposition"] = []string{`form-data; name="file"; filename="thumb.jpg"`}
	partHeader["Content-Type"] = []string{"image/jpeg"}
	fw, err := mw.CreatePart(partHeader)
	if err != nil {
		t.Fatalf("CreatePart: %v", err)
	}
	// Use a roughly-1:1 image so it does not trigger the needs_crop branch.
	testImg := image.NewRGBA(image.Rect(0, 0, 500, 500))
	if err := jpeg.Encode(fw, testImg, nil); err != nil {
		t.Fatalf("encoding JPEG: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("closing multipart writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/upload?skip_crop=true", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleImageUpload(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	select {
	case got := <-called:
		if got != a.ID {
			t.Errorf("RunForArtist called with %q, want %q", got, a.ID)
		}
	case <-time.After(2 * time.Second):
		t.Error("RunForArtist was not invoked after successful image upload")
	}
}

// stubRoundTripper returns a fixed response without touching the network.
// Used to replace Router.ssrfClient.Transport in fetch-path tests so the
// handler runs end-to-end without an actual HTTP request.
type stubRoundTripper struct {
	body []byte
}

func (s *stubRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"image/jpeg"}},
		Body:       io.NopCloser(bytes.NewReader(s.body)),
		Request:    req,
	}, nil
}

// TestHandleImageFetch_RerunsRulesAfterWrite mirrors
// TestHandleImageUpload_RerunsRulesAfterWrite for the fetch path. The PR's
// runRulesAfterRefresh hook is wired into BOTH save paths; covering only one
// would let a regression in the fetch handler's call site land unnoticed.
//
// example.com resolves to a public IP so isPrivateURL passes; the stub
// RoundTripper short-circuits the actual HTTP request so no network is
// involved at test time.
func TestHandleImageFetch_RerunsRulesAfterWrite(t *testing.T) {
	t.Parallel()
	called := make(chan string, 1)
	stub := &stubPipeline{
		runForArtistFn: func(_ context.Context, a *artist.Artist) (*rule.RunResult, error) {
			select {
			case called <- a.ID:
			default:
			}
			return &rule.RunResult{}, nil
		},
	}
	r, artistSvc := testRouterWithStubPipeline(t, stub)
	platSvc := platform.NewService(r.db)
	r.platformService = platSvc
	r.publisher = publish.New(publish.Deps{
		ArtistService:      r.artistService,
		ConnectionService:  r.connectionService,
		NFOSnapshotService: r.nfoSnapshotService,
		PlatformService:    platSvc,
		ImageCacheDir:      r.imageCacheDir,
		Logger:             r.logger,
	})

	// Encode a 1:1 JPEG and serve it via the stubbed Transport so the fetch
	// path runs without a real network round trip.
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 500, 500)), nil); err != nil {
		t.Fatalf("encoding JPEG: %v", err)
	}
	r.ssrfClient = &http.Client{Transport: &stubRoundTripper{body: buf.Bytes()}}

	dir := t.TempDir()
	a := &artist.Artist{Name: "Fetch Rerun Rules", SortName: "Fetch Rerun Rules", Path: dir}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// Use a public IP literal rather than a hostname so isPrivateURL's DNS
	// lookup is skipped: in offline / locked-down CI a hostname lookup can
	// fail and isPrivateURL fails closed (returns true), which would block
	// the fetch path before the stub transport ever runs.
	body := strings.NewReader(`{"url":"https://8.8.8.8/test.jpg","type":"thumb"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/fetch?skip_crop=true", body)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleImageFetch(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	select {
	case got := <-called:
		if got != a.ID {
			t.Errorf("RunForArtist called with %q, want %q", got, a.ID)
		}
	case <-time.After(2 * time.Second):
		t.Error("RunForArtist was not invoked after successful image fetch")
	}
}

// TestHandleServeImage_PreservesFlagOnStatError verifies that when the artist
// directory cannot be stat-walked (EACCES from an unreadable parent dir), the
// serve handler returns 404 but does NOT clear the exists_flag. Without the
// strict variant of FindExistingImage, a single permission-denied directory
// would silently drop every flag for artists under it on each request. See
// issue #1161.
func TestHandleServeImage_PreservesFlagOnStatError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod 0o000 semantics are Unix-specific")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permission bits; cannot trigger EACCES")
	}
	t.Parallel()
	r, artistSvc := testRouterWithPlatform(t)
	ctx := context.Background()

	// Create an artist whose Path is a child of a directory we will make
	// unreadable. The serve handler will then hit EACCES when stat-ing files.
	parent := t.TempDir()
	child := filepath.Join(parent, "stat-error-artist")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	a := &artist.Artist{Name: "Stat Error", SortName: "Stat Error", Path: child}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// Place a real thumb so the flag is set, then drop the parent's exec bit so
	// traversal fails with EACCES (the file is still on disk, but unreachable).
	writeJPEG(t, filepath.Join(child, "folder.jpg"), 500, 500)
	r.setArtistImageFlag(ctx, a, "thumb", true)
	if !a.ThumbExists {
		t.Fatal("ThumbExists should be true after setting flag")
	}
	if err := os.Chmod(parent, 0o000); err != nil {
		t.Fatalf("Chmod parent: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })

	url := fmt.Sprintf("/api/v1/artists/%s/images/thumb/file", a.ID)
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.SetPathValue("id", a.ID)
	req.SetPathValue("type", "thumb")
	w := httptest.NewRecorder()
	r.handleServeImage(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 on stat error, got %d", w.Code)
	}

	// Restore permissions so the DB read can use the path freely (defensive).
	_ = os.Chmod(parent, 0o755)

	// The serve handler clears stale flags from a goroutine with a 5s
	// timeout. A short sleep can let the test pass even if a buggy code path
	// schedules the clear with extra latency, so poll past the goroutine's
	// own deadline and assert the flag stays true the whole window. Any flip
	// to false at any point is a regression.
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		updated, err := artistSvc.GetByID(ctx, a.ID)
		if err != nil {
			t.Fatalf("GetByID: %v", err)
		}
		if !updated.ThumbExists {
			t.Fatal("ThumbExists must remain true after a non-ENOENT stat error; otherwise transient FS hiccups corrupt flags")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestHandleDeleteImage_PreservesFlagOnStatError verifies that when the delete
// path's post-delete probe encounters a non-ENOENT stat error (here EACCES from
// an unreadable parent directory), the exists_flag is NOT cleared. Without the
// strict variant, a transient permission-denied stat would silently drop a
// flag whose underlying file may still be on disk. See issue #1161.
func TestHandleDeleteImage_PreservesFlagOnStatError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod 0o000 semantics are Unix-specific")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permission bits; cannot trigger EACCES")
	}
	t.Parallel()
	r, artistSvc := testRouterWithPlatform(t)
	ctx := context.Background()

	parent := t.TempDir()
	child := filepath.Join(parent, "delete-stat-error")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	a := &artist.Artist{Name: "Delete Stat Error", SortName: "Delete Stat Error", Path: child}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// Place a thumb and set the flag, then drop the parent's exec bit so the
	// post-delete strict probe hits EACCES rather than ENOENT.
	writeJPEG(t, filepath.Join(child, "folder.jpg"), 500, 500)
	r.setArtistImageFlag(ctx, a, "thumb", true)
	if !a.ThumbExists {
		t.Fatal("ThumbExists should be true after setting flag")
	}
	if err := os.Chmod(parent, 0o000); err != nil {
		t.Fatalf("Chmod parent: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })

	url := fmt.Sprintf("/api/v1/artists/%s/images/thumb", a.ID)
	req := httptest.NewRequest(http.MethodDelete, url, nil)
	req.SetPathValue("id", a.ID)
	req.SetPathValue("type", "thumb")
	w := httptest.NewRecorder()
	r.handleDeleteImage(w, req)

	// Pin the handler-contract status so a regression that returns a
	// non-2xx for this EACCES-on-post-probe path is caught here, not just
	// "ThumbExists stayed true." StatusOK is the contract for delete: the
	// remove itself reports success and the flag-preservation is the
	// follow-on probe behavior, not the response.
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	// Restore permissions so subsequent DB reads / cleanup are unaffected.
	_ = os.Chmod(parent, 0o755)

	updated, err := artistSvc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if !updated.ThumbExists {
		t.Error("ThumbExists must remain true after a non-ENOENT post-delete stat error")
	}
}

// TestHandleRandomBackdrop_PreservesFlagOnStatError verifies that the random
// backdrop endpoint does not clear the fanart exists_flag when probing the
// artist directory hits a non-ENOENT stat error (EACCES). Without the strict
// variant, a single unreadable parent dir would silently drop the flag for
// every artist under it on each request. See issue #1161.
func TestHandleRandomBackdrop_PreservesFlagOnStatError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod 0o000 semantics are Unix-specific")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permission bits; cannot trigger EACCES")
	}
	t.Parallel()
	r, artistSvc := testRouterWithPlatform(t)
	ctx := context.Background()

	parent := t.TempDir()
	child := filepath.Join(parent, "backdrop-stat-error")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	a := &artist.Artist{Name: "Backdrop Stat Error", SortName: "Backdrop Stat Error", Path: child}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// Seed the fanart flag (file present on disk) then drop parent permissions.
	writeJPEG(t, filepath.Join(child, "fanart.jpg"), 100, 56)
	if _, err := r.db.ExecContext(ctx,
		`INSERT INTO artist_images (id, artist_id, image_type, slot_index, exists_flag)
		 VALUES (lower(hex(randomblob(16))), ?, 'fanart', 0, 1)
		 ON CONFLICT (artist_id, image_type, slot_index) DO UPDATE SET exists_flag = 1`,
		a.ID); err != nil {
		t.Fatalf("seeding artist_images: %v", err)
	}
	if err := os.Chmod(parent, 0o000); err != nil {
		t.Fatalf("Chmod parent: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })

	req := httptest.NewRequest(http.MethodGet, "/api/v1/images/random-backdrop", nil)
	w := httptest.NewRecorder()
	r.handleRandomBackdrop(w, req)

	// Endpoint returns 404 because all candidates were skipped due to stat error.
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 when all candidates skipped, got %d", w.Code)
	}

	// Restore permissions so subsequent DB queries are unaffected.
	_ = os.Chmod(parent, 0o755)

	images, err := artistSvc.GetImagesForArtist(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetImagesForArtist: %v", err)
	}
	var sawFanart bool
	for _, im := range images {
		if im.ImageType == "fanart" && im.SlotIndex == 0 {
			sawFanart = true
			if !im.Exists {
				t.Error("fanart exists_flag must remain true after a non-ENOENT stat error")
			}
		}
	}
	if !sawFanart {
		t.Error("expected to find fanart row in artist_images")
	}
}

// TestHandleImageUpload_FanartAppend_RerunsRules verifies that the fanart
// append branch (a.FanartExists == true) of handleImageUpload also calls
// RunForArtist after a successful write. The primary path is covered by
// TestHandleImageUpload_RerunsRulesAfterWrite. See #1028.
func TestHandleImageUpload_FanartAppend_RerunsRules(t *testing.T) {
	t.Parallel()
	called := make(chan string, 1)
	stub := &stubPipeline{
		runForArtistFn: func(_ context.Context, a *artist.Artist) (*rule.RunResult, error) {
			select {
			case called <- a.ID:
			default:
			}
			return &rule.RunResult{}, nil
		},
	}
	r, artistSvc := testRouterWithStubPipeline(t, stub)
	platSvc := platform.NewService(r.db)
	r.platformService = platSvc
	r.publisher = publish.New(publish.Deps{
		ArtistService:      r.artistService,
		ConnectionService:  r.connectionService,
		NFOSnapshotService: r.nfoSnapshotService,
		PlatformService:    platSvc,
		ImageCacheDir:      r.imageCacheDir,
		Logger:             r.logger,
	})

	dir := t.TempDir()
	a := &artist.Artist{Name: "Fanart Append", SortName: "Fanart Append", Path: dir, FanartExists: true}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	// Seed a primary fanart so the append branch produces fanart2.jpg.
	writeJPEG(t, filepath.Join(dir, "fanart.jpg"), 1920, 1080)

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if err := mw.WriteField("type", "fanart"); err != nil {
		t.Fatalf("writing field: %v", err)
	}
	partHeader := make(map[string][]string)
	partHeader["Content-Disposition"] = []string{`form-data; name="file"; filename="fanart.jpg"`}
	partHeader["Content-Type"] = []string{"image/jpeg"}
	fw, err := mw.CreatePart(partHeader)
	if err != nil {
		t.Fatalf("CreatePart: %v", err)
	}
	// 16:9 aspect to satisfy fanart geometry without triggering needs_crop.
	testImg := image.NewRGBA(image.Rect(0, 0, 1920, 1080))
	if err := jpeg.Encode(fw, testImg, nil); err != nil {
		t.Fatalf("encoding JPEG: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("closing multipart writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/upload?skip_crop=true", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleImageUpload(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	select {
	case got := <-called:
		if got != a.ID {
			t.Errorf("RunForArtist called with %q, want %q", got, a.ID)
		}
	case <-time.After(2 * time.Second):
		t.Error("RunForArtist was not invoked after successful fanart append")
	}
}

// blockingRoundTripper blocks indefinitely unless its request's context is
// canceled, in which case it returns the context error. It is the inverse of
// stubRoundTripper: where stubRoundTripper proves the happy path, this one
// proves that a caller-supplied context governs request lifetime. Without
// http.NewRequestWithContext, a canceled caller ctx would have no effect on
// the in-flight request and this transport would block until t.Fatal hit the
// outer 1-second deadline.
type blockingRoundTripper struct{}

func (blockingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	<-req.Context().Done()
	return nil, req.Context().Err()
}

// TestFetchImageFromURL_CancelledContext is the regression test for issue
// #1412 D6: fetchImageFromURL must honor its ctx so that a disconnected
// client releases the ssrfClient connection pool slot instead of leaving
// the request in flight until the underlying timeout fires. Before the fix
// the function used http.NewRequest with no ctx and the call site carried a
// suppress-noctx lint exemption; after the fix it uses
// http.NewRequestWithContext and propagates the caller's ctx into client.Do.
//
// The test calls fetchImageFromURL with an already-canceled ctx and asserts
// that the call returns promptly (well under any production fetch timeout)
// with an error chain that includes context.Canceled.
func TestFetchImageFromURL_CancelledContext(t *testing.T) {
	t.Parallel()

	r := &Router{
		ssrfClient: &http.Client{Transport: blockingRoundTripper{}},
		logger:     slog.New(slog.DiscardHandler),
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel so client.Do sees a dead ctx immediately

	done := make(chan error, 1)
	go func() {
		_, err := r.fetchImageFromURL(ctx, "https://8.8.8.8/test.jpg")
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("fetchImageFromURL returned nil error for canceled ctx; want context.Canceled")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v; want chain to include context.Canceled", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("fetchImageFromURL did not return within 1s after ctx cancellation; ctx is not being honored")
	}
}

func TestHandleImageCrop_PreservesBackup(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouterWithPlatform(t)
	dir := t.TempDir()
	a := &artist.Artist{Name: "Backup Artist", SortName: "Backup Artist", Path: dir}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	// Seed an existing canonical thumb (Kodi default profile uses folder.jpg).
	writeJPEG(t, filepath.Join(dir, "folder.jpg"), 600, 600)
	originalBytes, err := os.ReadFile(filepath.Join(dir, "folder.jpg"))
	if err != nil {
		t.Fatalf("reading seed: %v", err)
	}

	// Build a fresh 500x500 JPEG to crop in.
	var imgBuf bytes.Buffer
	if err := jpeg.Encode(&imgBuf, image.NewRGBA(image.Rect(0, 0, 500, 500)), nil); err != nil {
		t.Fatalf("encoding crop input: %v", err)
	}
	reqBody, _ := json.Marshal(map[string]any{
		"image_data": base64.StdEncoding.EncodeToString(imgBuf.Bytes()),
		"type":       "thumb",
		"x":          0, "y": 0, "width": 500, "height": 500,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/crop", bytes.NewReader(reqBody))
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleImageCrop(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	// Backup of the pre-crop original must exist (keyed by image TYPE) and match
	// the seed bytes (#1837: per-type, format-independent backup identity).
	backup := filepath.Join(dir, img.BackupDirName, "thumb", "folder.jpg")
	gotBackup, err := os.ReadFile(backup)
	if err != nil {
		t.Fatalf("expected backup at %s: %v", backup, err)
	}
	if !bytes.Equal(gotBackup, originalBytes) {
		t.Error("backup bytes do not match the pre-crop original")
	}
}

func TestHandleLogoTrim_GatedAndBacksUp(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouterWithPlatform(t)
	dir := t.TempDir()
	a := &artist.Artist{Name: "Logo Artist", SortName: "Logo Artist", Path: dir}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	// Seed a PNG logo with a transparent border so TrimAlpha changes it.
	logoPath := filepath.Join(dir, "logo.png")
	m := image.NewRGBA(image.Rect(0, 0, 200, 100))
	for y := 20; y < 80; y++ {
		for x := 20; x < 180; x++ {
			m.Set(x, y, color.RGBA{R: 10, G: 20, B: 30, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, m); err != nil {
		t.Fatalf("encoding logo: %v", err)
	}
	if err := os.WriteFile(logoPath, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("seeding logo: %v", err)
	}
	originalBytes, _ := os.ReadFile(logoPath)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/logo/trim", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()
	r.handleLogoTrim(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	backup := filepath.Join(dir, img.BackupDirName, "logo", "logo.png")
	gotBackup, err := os.ReadFile(backup)
	if err != nil {
		t.Fatalf("expected logo backup at %s: %v", backup, err)
	}
	if !bytes.Equal(gotBackup, originalBytes) {
		t.Error("logo backup bytes do not match the pre-trim original")
	}
}

func TestHandleLogoTrim_Blocked409(t *testing.T) {
	t.Parallel()
	d := conflict.NewBlockingForTest(testDiscardLogger())
	r, artistSvc := testRouterWithPlatform(t)
	r.conflictDetector = d
	r.conflictGate = conflict.NewGate(d)
	dir := t.TempDir()
	a := &artist.Artist{Name: "Blocked Logo", SortName: "Blocked Logo", Path: dir}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	// T8: seed a real PNG (logos are PNG) so the blocked test cannot pass for
	// the wrong reason.
	logoPath := filepath.Join(dir, "logo.png")
	writePNG(t, logoPath, 200, 100)
	before, _ := os.ReadFile(logoPath)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/logo/trim", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()
	r.handleLogoTrim(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body: %s", w.Code, w.Body.String())
	}
	// CR #1839: the gated 409 must carry the shared ConflictWriteBlock payload
	// (error/axis/reason/ledger), not a bare error. Asserting the shape here
	// catches a regression to a generic 409 that would silently break the
	// UI/OpenAPI contract.
	assertConflictWriteBlock(t, w.Body.Bytes())
	// T1: a blocked trim must not touch disk - no backup, canonical unchanged.
	if img.HasBackup(dir, "logo") {
		t.Error("blocked logo-trim must not create a backup")
	}
	after, _ := os.ReadFile(logoPath)
	if !bytes.Equal(before, after) {
		t.Error("blocked logo-trim must not modify the canonical logo")
	}
}

// TestHandleImageInfo_BackupExistsContract pins the JSON contract for the
// backup_exists field that drives the UI's revert affordance (CR #1839): it is
// true for a single-slot kind (thumb/logo/banner) that has a one-deep
// .sw-backup original, and always false for fanart (multi-slot, no single-slot
// backup) even when an unrelated backup dir is present. This guards against
// OpenAPI/UI drift that the on-disk-only backup tests would not catch.
func TestHandleImageInfo_BackupExistsContract(t *testing.T) {
	t.Parallel()
	r, svc := newImageHandlerTestServer(t)
	dir := t.TempDir()
	a := &artist.Artist{Name: "InfoBackupExists", SortName: "InfoBackupExists", Path: dir}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	// Canonical thumb + fanart + logo on disk so the info handler can stat each.
	writeJPEG(t, filepath.Join(dir, "folder.jpg"), 600, 600)
	writeJPEG(t, filepath.Join(dir, "fanart.jpg"), 1920, 1080)
	writePNG(t, filepath.Join(dir, "logo.png"), 400, 200) // single-slot, NO backup seeded
	// A one-deep backup under .sw-backup/thumb -> img.HasBackup(dir,"thumb") true.
	thumbBackupDir := filepath.Join(dir, img.BackupDirName, "thumb")
	if err := os.MkdirAll(thumbBackupDir, 0o750); err != nil {
		t.Fatalf("creating thumb backup dir: %v", err)
	}
	writeJPEG(t, filepath.Join(thumbBackupDir, "folder.jpg"), 600, 600)

	backupExists := func(t *testing.T, imageType string) bool {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/"+a.ID+"/images/"+imageType+"/info", nil)
		req.SetPathValue("id", a.ID)
		req.SetPathValue("type", imageType)
		w := serveValidated(t, http.HandlerFunc(r.handleImageInfo), req)
		if w.Code != http.StatusOK {
			t.Fatalf("type=%s status = %d, want 200; body: %s", imageType, w.Code, w.Body.String())
		}
		var resp struct {
			BackupExists bool `json:"backup_exists"`
		}
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("type=%s decoding: %v", imageType, err)
		}
		return resp.BackupExists
	}

	if !backupExists(t, "thumb") {
		t.Error("backup_exists must be true for a single-slot type with a one-deep backup")
	}
	if backupExists(t, "fanart") {
		t.Error("backup_exists must always be false for fanart (multi-slot, no single-slot backup)")
	}
	// Single-slot kind present on disk but with NO one-deep backup -> false. Pins
	// the "false unless a backup exists" contract (guards a regression that would
	// return true for every single-slot type).
	if backupExists(t, "logo") {
		t.Error("backup_exists must be false for a single-slot type with no backup present")
	}
}

// TestProcessAndSaveImage_BackupFailureAborts proves F2/T2: when the backup
// cannot be written (here, .sw-backup pre-exists as a FILE so MkdirAll fails),
// the destructive crop save is aborted and the canonical original is preserved.
func TestProcessAndSaveImage_BackupFailureAborts(t *testing.T) {
	t.Parallel()
	r, _ := testRouterWithPlatform(t)
	dir := t.TempDir()
	origPath := filepath.Join(dir, "folder.jpg")
	writeJPEG(t, origPath, 80, 80)
	orig, _ := os.ReadFile(origPath)

	// Block backup-dir creation: .sw-backup is a regular file, so MkdirAll fails.
	if err := os.WriteFile(filepath.Join(dir, img.BackupDirName), []byte("x"), 0o644); err != nil {
		t.Fatalf("seeding .sw-backup as a file: %v", err)
	}

	crop := encodeImageFmt(t, "jpeg", 120, 120)
	if _, err := r.processAndSaveImage(context.Background(), dir, "thumb", crop, nil); err == nil {
		t.Fatal("processAndSaveImage must abort when the backup cannot be written")
	}
	// The original must be intact (the destructive Save never ran).
	after, _ := os.ReadFile(origPath)
	if !bytes.Equal(orig, after) {
		t.Error("canonical original must be preserved when backup fails")
	}
}

// stubWebImageProvider is a minimal WebImageProvider for testing handleWebImageSearch.
// It returns a fixed set of ImageResult values regardless of the query parameters.
type stubWebImageProvider struct {
	name    provider.ProviderName
	results []provider.ImageResult
}

func (s *stubWebImageProvider) Name() provider.ProviderName { return s.name }
func (s *stubWebImageProvider) RequiresAuth() bool          { return false }
func (s *stubWebImageProvider) SearchImages(_ context.Context, _ string, _ provider.ImageType) ([]provider.ImageResult, error) {
	return s.results, nil
}

// TestHandleWebImageSearch_NormalizesHTTPToHTTPS verifies that http:// thumbnail
// URLs returned by a web-search provider are rewritten to https:// before the
// response is sent, so they satisfy the "img-src 'self' data: https:" CSP header.
// An already-https:// URL must remain unchanged.
func TestHandleWebImageSearch_NormalizesHTTPToHTTPS(t *testing.T) {
	t.Parallel()
	r, svc := newImageHandlerTestServer(t)

	a := &artist.Artist{Name: "CSP Artist", SortName: "CSP Artist", Path: t.TempDir()}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// Stub provider returns one http:// URL and one https:// URL.
	stub := &stubWebImageProvider{
		name: provider.NameDuckDuckGo,
		results: []provider.ImageResult{
			{URL: "http://example.com/img.jpg", Type: provider.ImageThumb, Source: "duckduckgo"},
			{URL: "https://example.com/img2.jpg", Type: provider.ImageThumb, Source: "duckduckgo"},
		},
	}
	r.webSearchRegistry.Register(stub)

	if err := r.providerSettings.SetWebSearchEnabled(context.Background(), provider.NameDuckDuckGo, true); err != nil {
		t.Fatalf("enabling provider: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/"+a.ID+"/images/websearch?type=thumb", nil)
	req.SetPathValue("id", a.ID)

	w := serveValidated(t, http.HandlerFunc(r.handleWebImageSearch), req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Images []provider.ImageResult `json:"images"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(resp.Images) != 2 {
		t.Fatalf("images = %d, want 2", len(resp.Images))
	}
	for _, im := range resp.Images {
		if strings.HasPrefix(im.URL, "http://") {
			t.Errorf("URL %q still uses http://, want https://", im.URL)
		}
	}
	// Verify the http:// URL was upgraded and the https:// URL was left alone.
	if want := "https://example.com/img.jpg"; resp.Images[0].URL != want {
		t.Errorf("images[0].URL = %q, want %q", resp.Images[0].URL, want)
	}
	if want := "https://example.com/img2.jpg"; resp.Images[1].URL != want {
		t.Errorf("images[1].URL = %q, want %q", resp.Images[1].URL, want)
	}
}

// TestHandleLogoTrim_BackupFailureAborts proves F3/T2: a backup-write failure
// aborts the trim with 500 and leaves the original logo intact.
func TestHandleLogoTrim_BackupFailureAborts(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouterWithPlatform(t)
	dir := t.TempDir()
	a := &artist.Artist{Name: "Trim Abort", SortName: "Trim Abort", Path: dir}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	logoPath := filepath.Join(dir, "logo.png")
	writePNG(t, logoPath, 200, 100)
	orig, _ := os.ReadFile(logoPath)

	// Block backup-dir creation.
	if err := os.WriteFile(filepath.Join(dir, img.BackupDirName), []byte("x"), 0o644); err != nil {
		t.Fatalf("seeding .sw-backup as a file: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/logo/trim", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()
	r.handleLogoTrim(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body: %s", w.Code, w.Body.String())
	}
	after, _ := os.ReadFile(logoPath)
	if !bytes.Equal(orig, after) {
		t.Error("original logo must be preserved when pre-trim backup fails")
	}
}

// TestHandleImageCrop_AppendFanart_AppendsNewFile proves #2314: cropping a
// newly added fanart (append=true) with a primary already present appends a
// new numbered file rather than overwriting the primary.
func TestHandleImageCrop_AppendFanart_AppendsNewFile(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouterWithPlatform(t)
	dir := t.TempDir()
	a := &artist.Artist{Name: "Crop Append Artist", SortName: "Crop Append Artist", Path: dir}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	writeJPEG(t, filepath.Join(dir, "fanart.jpg"), 1920, 1080)
	// FanartExists is derived from the artist_images side table (hydrated on
	// every GetByID), not the in-memory struct passed to Create -- seed it the
	// way a real prior save would have, via updateArtistImageFlag.
	r.updateArtistImageFlag(context.Background(), a, "fanart")
	primaryBefore, err := os.ReadFile(filepath.Join(dir, "fanart.jpg"))
	if err != nil {
		t.Fatalf("reading seed primary: %v", err)
	}

	var imgBuf bytes.Buffer
	if err := jpeg.Encode(&imgBuf, image.NewRGBA(image.Rect(0, 0, 1280, 720)), nil); err != nil {
		t.Fatalf("encoding crop input: %v", err)
	}
	reqBody, _ := json.Marshal(map[string]any{
		"image_data": base64.StdEncoding.EncodeToString(imgBuf.Bytes()),
		"type":       "fanart",
		"x":          0, "y": 0, "width": 1280, "height": 720,
		"append": true,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/crop", bytes.NewReader(reqBody))
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleImageCrop(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	primaryAfter, err := os.ReadFile(filepath.Join(dir, "fanart.jpg"))
	if err != nil {
		t.Fatalf("reading primary after crop: %v", err)
	}
	if !bytes.Equal(primaryBefore, primaryAfter) {
		t.Error("primary fanart.jpg was overwritten; append=true must leave it untouched")
	}
	// The exact appended filename depends on the active naming convention
	// (fanart1.jpg / fanart2.jpg); assert on the response contract instead of
	// a hardcoded name.
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	saved, _ := resp["saved"].([]any)
	if len(saved) != 1 {
		t.Fatalf("saved = %v, want exactly one newly-appended file", saved)
	}
	if savedName, _ := saved[0].(string); savedName == "fanart.jpg" {
		t.Errorf("saved file %q must not be the primary; append=true must add a new numbered file", savedName)
	}
	if _, err := os.Stat(filepath.Join(dir, saved[0].(string))); err != nil {
		t.Errorf("expected appended file %v to exist on disk: %v", saved[0], err)
	}
	if count, _ := resp["count"].(float64); count != 2 {
		t.Errorf("count = %v, want 2 after appending a second fanart", resp["count"])
	}

	updated, err := artistSvc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("reloading artist: %v", err)
	}
	if updated.FanartCount != 2 {
		t.Errorf("FanartCount = %d, want 2 after append", updated.FanartCount)
	}
}

// TestHandleImageCrop_ReplacePrimary_NoAppend proves the recrop-of-primary
// path (append absent/false) still replaces the primary fanart in place,
// even when a primary already exists -- the counterpart to
// TestHandleImageCrop_AppendFanart_AppendsNewFile.
func TestHandleImageCrop_ReplacePrimary_NoAppend(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouterWithPlatform(t)
	dir := t.TempDir()
	a := &artist.Artist{Name: "Crop Replace Artist", SortName: "Crop Replace Artist", Path: dir}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	writeJPEG(t, filepath.Join(dir, "fanart.jpg"), 1920, 1080)
	r.updateArtistImageFlag(context.Background(), a, "fanart")
	primaryBefore, err := os.ReadFile(filepath.Join(dir, "fanart.jpg"))
	if err != nil {
		t.Fatalf("reading seed primary: %v", err)
	}

	var imgBuf bytes.Buffer
	if err := jpeg.Encode(&imgBuf, image.NewRGBA(image.Rect(0, 0, 1280, 720)), nil); err != nil {
		t.Fatalf("encoding crop input: %v", err)
	}
	// No "append" field: defaults to false (recrop-of-primary).
	reqBody, _ := json.Marshal(map[string]any{
		"image_data": base64.StdEncoding.EncodeToString(imgBuf.Bytes()),
		"type":       "fanart",
		"x":          0, "y": 0, "width": 1280, "height": 720,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/crop", bytes.NewReader(reqBody))
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleImageCrop(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	primaryAfter, err := os.ReadFile(filepath.Join(dir, "fanart.jpg"))
	if err != nil {
		t.Fatalf("reading primary after crop: %v", err)
	}
	if bytes.Equal(primaryBefore, primaryAfter) {
		t.Error("expected fanart.jpg to be overwritten by the recrop")
	}
	if _, err := os.Stat(filepath.Join(dir, "fanart2.jpg")); err == nil {
		t.Error("no-append recrop must not create fanart2.jpg")
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if count, _ := resp["count"].(float64); count != 1 {
		t.Errorf("count = %v, want 1 after replacing the sole primary", resp["count"])
	}
}

// TestHandleImageUpload_NeedsCrop_IncludesAppend proves the needs_crop JSON
// response from handleImageUpload carries the same append decision the
// upload's own fanart-append branch would take, so the client's auto-opened
// crop modal can carry it through to handleImageCrop (#2314).
func TestHandleImageUpload_NeedsCrop_IncludesAppend(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouterWithPlatform(t)
	dir := t.TempDir()
	a := &artist.Artist{Name: "Needs Crop Append", SortName: "Needs Crop Append", Path: dir}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	writeJPEG(t, filepath.Join(dir, "fanart.jpg"), 1920, 1080)
	r.updateArtistImageFlag(context.Background(), a, "fanart")

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if err := mw.WriteField("type", "fanart"); err != nil {
		t.Fatalf("writing field: %v", err)
	}
	partHeader := make(map[string][]string)
	partHeader["Content-Disposition"] = []string{`form-data; name="file"; filename="fanart.jpg"`}
	partHeader["Content-Type"] = []string{"image/jpeg"}
	fw, err := mw.CreatePart(partHeader)
	if err != nil {
		t.Fatalf("CreatePart: %v", err)
	}
	// Mismatched aspect ratio (square) so this hits the needs_crop branch.
	if err := jpeg.Encode(fw, image.NewRGBA(image.Rect(0, 0, 500, 500)), nil); err != nil {
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
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if got, ok := resp["needs_crop"].(bool); !ok || !got {
		t.Fatalf("expected needs_crop=true, got %v", resp["needs_crop"])
	}
	if got, ok := resp["append"].(bool); !ok || !got {
		t.Errorf("expected append=true (fanart already exists), got %v", resp["append"])
	}
}

// TestHandleImageCrop_AppendRequested_NoPrimary_SavesAsPrimary proves the
// append branch requires a.FanartExists, not just the client's append flag:
// append=true with no existing primary must fall through to the single-slot
// save path and create the primary, not a numbered append file.
func TestHandleImageCrop_AppendRequested_NoPrimary_SavesAsPrimary(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouterWithPlatform(t)
	dir := t.TempDir()
	a := &artist.Artist{Name: "Crop Append No Primary", SortName: "Crop Append No Primary", Path: dir}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	// No existing fanart.jpg seeded, and FanartExists is never flagged.

	var imgBuf bytes.Buffer
	if err := jpeg.Encode(&imgBuf, image.NewRGBA(image.Rect(0, 0, 1280, 720)), nil); err != nil {
		t.Fatalf("encoding crop input: %v", err)
	}
	reqBody, _ := json.Marshal(map[string]any{
		"image_data": base64.StdEncoding.EncodeToString(imgBuf.Bytes()),
		"type":       "fanart",
		"x":          0, "y": 0, "width": 1280, "height": 720,
		"append": true,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/crop", bytes.NewReader(reqBody))
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleImageCrop(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	if _, err := os.Stat(filepath.Join(dir, "fanart.jpg")); err != nil {
		t.Errorf("expected fanart.jpg (primary) to be created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "fanart1.jpg")); err == nil {
		t.Error("append=true with no existing primary must not create a numbered append file")
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	saved, _ := resp["saved"].([]any)
	if len(saved) != 1 || saved[0] != "fanart.jpg" {
		t.Errorf("saved = %v, want [\"fanart.jpg\"]", saved)
	}
	if count, _ := resp["count"].(float64); count != 1 {
		t.Errorf("count = %v, want 1 after saving the first fanart as primary", resp["count"])
	}
}

// TestHandleImageCrop_AppendFanart_WriteFailureReturns500 covers the
// processAndAppendFanart failure path: when the artist directory rejects new
// writes, the handler must return 500 with its existing generic error and
// must not disturb the pre-existing primary. Mirrors the write-failure
// injection approach used by TestHandleLogoTrim_BackupFailureAborts (seed a
// filesystem state that blocks the save), adapted to the append branch,
// which has no backup-dir step of its own to target.
func TestHandleImageCrop_AppendFanart_WriteFailureReturns500(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod write-bit semantics are Unix-specific")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permission bits; cannot trigger a write failure")
	}
	t.Parallel()
	r, artistSvc := testRouterWithPlatform(t)
	dir := t.TempDir()
	a := &artist.Artist{Name: "Crop Append Write Failure", SortName: "Crop Append Write Failure", Path: dir}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	writeJPEG(t, filepath.Join(dir, "fanart.jpg"), 1920, 1080)
	r.updateArtistImageFlag(context.Background(), a, "fanart")
	primaryBefore, err := os.ReadFile(filepath.Join(dir, "fanart.jpg"))
	if err != nil {
		t.Fatalf("reading seed primary: %v", err)
	}

	// Drop the write bit so the append save's temp-file creation fails while
	// existing reads (directory scan for the next fanart index) still succeed.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("Chmod dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	var imgBuf bytes.Buffer
	if err := jpeg.Encode(&imgBuf, image.NewRGBA(image.Rect(0, 0, 1280, 720)), nil); err != nil {
		t.Fatalf("encoding crop input: %v", err)
	}
	reqBody, _ := json.Marshal(map[string]any{
		"image_data": base64.StdEncoding.EncodeToString(imgBuf.Bytes()),
		"type":       "fanart",
		"x":          0, "y": 0, "width": 1280, "height": 720,
		"append": true,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/crop", bytes.NewReader(reqBody))
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleImageCrop(w, req)

	_ = os.Chmod(dir, 0o755)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp["error"] != "failed to save image" {
		t.Errorf("error = %q, want %q", resp["error"], "failed to save image")
	}

	primaryAfter, err := os.ReadFile(filepath.Join(dir, "fanart.jpg"))
	if err != nil {
		t.Fatalf("reading primary after failed append: %v", err)
	}
	if !bytes.Equal(primaryBefore, primaryAfter) {
		t.Error("primary fanart.jpg must be untouched when the append save fails")
	}
	if _, err := os.Stat(filepath.Join(dir, "fanart1.jpg")); err == nil {
		t.Error("no appended file should exist after a failed append save")
	}
}
