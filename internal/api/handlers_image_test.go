package api

import (
	"bytes"
	"context"
	"crypto/tls"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

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
	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want %d", w.Code, http.StatusConflict)
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
}
