package publish

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/platform"
)

// --- truncateWarning ---

// TestTruncateWarning covers both the no-op and the truncation branches.
func TestTruncateWarning(t *testing.T) {
	t.Run("short message returned unchanged", func(t *testing.T) {
		if got := truncateWarning("hello"); got != "hello" {
			t.Errorf("expected unchanged, got %q", got)
		}
	})
	t.Run("over cap is truncated with suffix", func(t *testing.T) {
		long := strings.Repeat("x", maxWarningRunes+50)
		got := truncateWarning(long)
		if !strings.HasSuffix(got, " (truncated)") {
			t.Errorf("expected truncated suffix, got %q", got)
		}
		// Body before suffix should be exactly maxWarningRunes runes.
		body := strings.TrimSuffix(got, " (truncated)")
		if len([]rune(body)) != maxWarningRunes {
			t.Errorf("expected %d runes before suffix, got %d", maxWarningRunes, len([]rune(body)))
		}
	})
}

// --- SetImageCacheDir / ImageDir ---

// TestImageDir covers the three resolution branches: artist path, cache
// fallback, and the empty fallthrough.
func TestImageDir(t *testing.T) {
	t.Run("uses artist path when set", func(t *testing.T) {
		p := New(Deps{Logger: silentLogger()})
		got := p.ImageDir(&artist.Artist{ID: "a1", Path: "/music/A"})
		if got != "/music/A" {
			t.Errorf("expected artist path; got %q", got)
		}
	})

	t.Run("falls back to cache dir + id when path empty", func(t *testing.T) {
		p := New(Deps{Logger: silentLogger(), ImageCacheDir: "/var/cache"})
		got := p.ImageDir(&artist.Artist{ID: "a1"})
		if got != filepath.Join("/var/cache", "a1") {
			t.Errorf("expected cache path; got %q", got)
		}
	})

	t.Run("empty when no path and no cache dir", func(t *testing.T) {
		p := New(Deps{Logger: silentLogger()})
		if got := p.ImageDir(&artist.Artist{ID: "a1"}); got != "" {
			t.Errorf("expected empty string; got %q", got)
		}
	})

	t.Run("empty when path empty and id empty even with cache dir", func(t *testing.T) {
		p := New(Deps{Logger: silentLogger(), ImageCacheDir: "/var/cache"})
		if got := p.ImageDir(&artist.Artist{}); got != "" {
			t.Errorf("expected empty string; got %q", got)
		}
	})
}

// TestSetImageCacheDir verifies the setter updates the publisher's dir.
func TestSetImageCacheDir(t *testing.T) {
	p := New(Deps{Logger: silentLogger()})
	p.SetImageCacheDir("/new/cache")
	got := p.ImageDir(&artist.Artist{ID: "a1"})
	if got != filepath.Join("/new/cache", "a1") {
		t.Errorf("expected /new/cache/a1; got %q", got)
	}

	// nil receiver must not panic.
	var nilP *Publisher
	nilP.SetImageCacheDir("/whatever")
}

// --- getActiveNamingConfig / getActiveFanartPrimary ---

// fakePlatformProvider returns a fixed profile or an error.
type fakePlatformProvider struct {
	profile *platform.Profile
	err     error
}

func (f *fakePlatformProvider) GetActive(_ context.Context) (*platform.Profile, error) {
	return f.profile, f.err
}

// TestGetActiveNamingConfig covers: nil service, provider error, nil profile,
// empty names, and the populated success path.
func TestGetActiveNamingConfig(t *testing.T) {
	t.Run("nil service falls back to defaults", func(t *testing.T) {
		p := New(Deps{Logger: silentLogger()})
		got := p.getActiveNamingConfig(context.Background(), "thumb")
		if len(got) == 0 {
			t.Errorf("expected default names; got %v", got)
		}
	})

	t.Run("provider error falls back to defaults", func(t *testing.T) {
		p := New(Deps{Logger: silentLogger(), PlatformService: &fakePlatformProvider{err: errors.New("boom")}})
		got := p.getActiveNamingConfig(context.Background(), "thumb")
		if len(got) == 0 {
			t.Errorf("expected default names; got %v", got)
		}
	})

	t.Run("nil profile falls back to defaults", func(t *testing.T) {
		p := New(Deps{Logger: silentLogger(), PlatformService: &fakePlatformProvider{}})
		got := p.getActiveNamingConfig(context.Background(), "thumb")
		if len(got) == 0 {
			t.Errorf("expected default names; got %v", got)
		}
	})

	t.Run("empty names list falls back to defaults", func(t *testing.T) {
		profile := &platform.Profile{ImageNaming: platform.ImageNaming{}}
		p := New(Deps{Logger: silentLogger(), PlatformService: &fakePlatformProvider{profile: profile}})
		got := p.getActiveNamingConfig(context.Background(), "thumb")
		if len(got) == 0 {
			t.Errorf("expected default names fallback; got %v", got)
		}
	})

	t.Run("populated profile returns configured names", func(t *testing.T) {
		profile := &platform.Profile{ImageNaming: platform.ImageNaming{
			Thumb: []string{"poster.jpg"},
		}}
		p := New(Deps{Logger: silentLogger(), PlatformService: &fakePlatformProvider{profile: profile}})
		got := p.getActiveNamingConfig(context.Background(), "thumb")
		if len(got) != 1 || got[0] != "poster.jpg" {
			t.Errorf("expected [poster.jpg]; got %v", got)
		}
	})
}

// TestGetActiveFanartPrimary mirrors the naming-config branches for the
// fanart-primary helper.
func TestGetActiveFanartPrimary(t *testing.T) {
	t.Run("nil service -> default", func(t *testing.T) {
		p := New(Deps{Logger: silentLogger()})
		if got := p.getActiveFanartPrimary(context.Background()); got == "" {
			t.Errorf("expected default name; got empty")
		}
	})

	t.Run("provider error -> default", func(t *testing.T) {
		p := New(Deps{Logger: silentLogger(), PlatformService: &fakePlatformProvider{err: errors.New("boom")}})
		if got := p.getActiveFanartPrimary(context.Background()); got == "" {
			t.Errorf("expected default name; got empty")
		}
	})

	t.Run("nil profile -> default", func(t *testing.T) {
		p := New(Deps{Logger: silentLogger(), PlatformService: &fakePlatformProvider{}})
		if got := p.getActiveFanartPrimary(context.Background()); got == "" {
			t.Errorf("expected default name; got empty")
		}
	})

	t.Run("empty PrimaryName -> default", func(t *testing.T) {
		profile := &platform.Profile{ImageNaming: platform.ImageNaming{}}
		p := New(Deps{Logger: silentLogger(), PlatformService: &fakePlatformProvider{profile: profile}})
		if got := p.getActiveFanartPrimary(context.Background()); got == "" {
			t.Errorf("expected default name fallback; got empty")
		}
	})

	t.Run("populated profile returns first fanart name", func(t *testing.T) {
		profile := &platform.Profile{ImageNaming: platform.ImageNaming{
			Fanart: []string{"backdrop.jpg", "fanart.jpg"},
		}}
		p := New(Deps{Logger: silentLogger(), PlatformService: &fakePlatformProvider{profile: profile}})
		if got := p.getActiveFanartPrimary(context.Background()); got != "backdrop.jpg" {
			t.Errorf("expected backdrop.jpg; got %q", got)
		}
	})
}

// --- newImageUploader / newIndexedImageUploader / NewMetadataPusher ---

// TestUploaderConstructors covers the type switches for the three
// connection-typed constructors.
func TestUploaderConstructors(t *testing.T) {
	logger := silentLogger()

	cases := []struct {
		name      string
		conn      *connection.Connection
		wantImage bool
		wantPush  bool
	}{
		{"emby", &connection.Connection{Type: connection.TypeEmby}, true, true},
		{"jellyfin", &connection.Connection{Type: connection.TypeJellyfin}, true, true},
		{"lidarr", &connection.Connection{Type: connection.TypeLidarr}, false, false},
		{"unknown", &connection.Connection{Type: "other"}, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			img := newImageUploader(c.conn, logger)
			if (img != nil) != c.wantImage {
				t.Errorf("newImageUploader: gotNil=%v, wantSupported=%v", img == nil, c.wantImage)
			}
			idx := newIndexedImageUploader(c.conn, logger)
			if (idx != nil) != c.wantImage {
				t.Errorf("newIndexedImageUploader: gotNil=%v, wantSupported=%v", idx == nil, c.wantImage)
			}
			_, ok := NewMetadataPusher(c.conn, logger)
			if ok != c.wantPush {
				t.Errorf("NewMetadataPusher: ok=%v want %v", ok, c.wantPush)
			}
		})
	}
}

// --- SyncImageToPlatforms ---

// uploadHits records image upload arrivals so tests can assert. Per-upload
// Content-Type and body size are captured under a mutex so tests can verify
//   - the uploader's extension-based dispatch (image/jpeg for .jpg,
//     image/png for .png) — a regression that always sent image/jpeg would
//     otherwise still pass an upload-counter-only assertion;
//   - that the request body actually carried payload bytes — a regression
//     that drops the file body entirely would otherwise still register as
//     an "upload" because the path-match alone is enough.
type uploadHits struct {
	uploads      atomic.Int32
	mu           sync.Mutex
	contentTypes []string
	bodySizes    []int
}

// lastContentType returns the Content-Type header recorded on the most
// recent upload, or "" if none has been observed.
func (h *uploadHits) lastContentType() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.contentTypes) == 0 {
		return ""
	}
	return h.contentTypes[len(h.contentTypes)-1]
}

// lastBodySize returns the body byte-length recorded on the most recent
// upload, or -1 if none has been observed. -1 distinguishes "no upload
// yet" from "upload with empty body".
func (h *uploadHits) lastBodySize() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.bodySizes) == 0 {
		return -1
	}
	return h.bodySizes[len(h.bodySizes)-1]
}

func newImageUploadServer(hits *uploadHits) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/Images/") {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			ct := r.Header.Get("Content-Type")
			hits.mu.Lock()
			hits.contentTypes = append(hits.contentTypes, ct)
			hits.bodySizes = append(hits.bodySizes, len(body))
			hits.mu.Unlock()
			hits.uploads.Add(1)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}

// seedJPG writes a tiny valid-ish JPEG byte stream to dir/name.
func seedJPG(t *testing.T, dir, name string) {
	t.Helper()
	// Minimal SOI+EOI marker; FindExistingImage only checks for file existence,
	// not signature validity, so the content does not need to be a real JPEG.
	data := []byte{0xff, 0xd8, 0xff, 0xd9}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
		t.Fatalf("seeding %s: %v", name, err)
	}
}

// waitForUploads spins up to 2s for the expected number of image uploads.
func waitForUploads(t *testing.T, hits *uploadHits, want int32) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if hits.uploads.Load() >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected %d uploads, got %d", want, hits.uploads.Load())
}

// TestSyncImageToPlatforms_NilReceiver covers the nil guard.
func TestSyncImageToPlatforms_NilReceiver(t *testing.T) {
	var p *Publisher
	if got := p.SyncImageToPlatforms(context.Background(), &artist.Artist{}, "thumb"); got != nil {
		t.Errorf("nil publisher should return nil warnings; got %v", got)
	}
}

// TestSyncImageToPlatforms_ListerErrorReturnsWarning verifies the early-return
// warning path when GetPlatformIDs fails.
func TestSyncImageToPlatforms_ListerErrorReturnsWarning(t *testing.T) {
	p := New(Deps{
		Logger:            silentLogger(),
		ArtistService:     errPlatformLister{},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{}},
	})
	warnings := p.SyncImageToPlatforms(context.Background(), &artist.Artist{ID: "a1"}, "thumb")
	if len(warnings) != 1 || !strings.Contains(warnings[0], "failed to load platform mappings") {
		t.Errorf("unexpected warnings: %v", warnings)
	}
}

// TestSyncImageToPlatforms_NoPlatformIDsEmptyWarnings verifies the empty
// platforms early-return: zero-warning, non-nil slice.
func TestSyncImageToPlatforms_NoPlatformIDsEmptyWarnings(t *testing.T) {
	p := New(Deps{
		Logger:            silentLogger(),
		ArtistService:     &fakePlatformLister{ids: nil},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{}},
	})
	got := p.SyncImageToPlatforms(context.Background(), &artist.Artist{ID: "a1"}, "thumb")
	if got == nil || len(got) != 0 {
		t.Errorf("expected non-nil empty slice; got %v", got)
	}
}

// TestSyncImageToPlatforms_NoImageDirWarning verifies the "no image directory"
// warning branch.
func TestSyncImageToPlatforms_NoImageDirWarning(t *testing.T) {
	p := New(Deps{
		Logger: silentLogger(),
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c", PlatformArtistID: "p1"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c": {ID: "c", Type: connection.TypeEmby, URL: "http://example.invalid", Enabled: true},
		}},
	})
	// Artist with no path and no cache dir -> ImageDir returns "".
	warnings := p.SyncImageToPlatforms(context.Background(), &artist.Artist{ID: "a1"}, "thumb")
	if len(warnings) != 1 || !strings.Contains(warnings[0], "no image directory configured") {
		t.Errorf("unexpected warnings: %v", warnings)
	}
}

// TestSyncImageToPlatforms_NoImageFileWarning verifies the warning when the
// directory exists but no image file matches the naming patterns.
func TestSyncImageToPlatforms_NoImageFileWarning(t *testing.T) {
	dir := t.TempDir() // empty
	p := New(Deps{
		Logger: silentLogger(),
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c", PlatformArtistID: "p1"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c": {ID: "c", Type: connection.TypeEmby, URL: "http://example.invalid", Enabled: true},
		}},
	})
	warnings := p.SyncImageToPlatforms(context.Background(), &artist.Artist{ID: "a1", Path: dir}, "thumb")
	if len(warnings) != 1 || !strings.Contains(warnings[0], "no local image found") {
		t.Errorf("unexpected warnings: %v", warnings)
	}
}

// TestSyncImageToPlatforms_HappyPath spins a fake Emby server and asserts the
// image upload arrives.
func TestSyncImageToPlatforms_HappyPath(t *testing.T) {
	dir := t.TempDir()
	seedJPG(t, dir, "folder.jpg")

	hits := &uploadHits{}
	srv := newImageUploadServer(hits)
	defer srv.Close()

	p := New(Deps{
		Logger: silentLogger(),
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c-emby", PlatformArtistID: "p1"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c-emby": {ID: "c-emby", Type: connection.TypeEmby, URL: srv.URL, Enabled: true, Status: "ok", Emby: &connection.EmbyConfig{PlatformUserID: "u1", FeatureImageWrite: true}},
		}},
	})
	warnings := p.SyncImageToPlatforms(context.Background(), &artist.Artist{ID: "a1", Name: "X", Path: dir}, "thumb")
	if len(warnings) != 0 {
		t.Errorf("expected no warnings; got %v", warnings)
	}
	waitForUploads(t, hits, 1)
}

// TestSyncImageToPlatforms_ConnectionErrorAndDisabledAndUnsupported covers the
// per-connection branches: lookup error appends warning; Status!="ok" or
// disabled silently skipped; unsupported type appends warning.
func TestSyncImageToPlatforms_PerConnectionBranches(t *testing.T) {
	dir := t.TempDir()
	seedJPG(t, dir, "folder.jpg")

	hits := &uploadHits{}
	srv := newImageUploadServer(hits)
	defer srv.Close()

	conns := &fakeConnectionGetter{conns: map[string]*connection.Connection{
		// Disabled emby connection (skipped silently, no warning).
		"c-off": {ID: "c-off", Type: connection.TypeEmby, URL: srv.URL, Enabled: false, Status: "ok"},
		// Non-"ok" status (skipped silently).
		"c-bad": {ID: "c-bad", Type: connection.TypeEmby, URL: srv.URL, Enabled: true, Status: "error"},
		// Lidarr: GetFeatureImageWrite()=false -> silently skipped before uploader lookup.
		"c-lid": {ID: "c-lid", Type: connection.TypeLidarr, URL: srv.URL, Enabled: true, Status: "ok"},
		// Happy path (counts as 1 upload).
		"c-ok": {ID: "c-ok", Type: connection.TypeEmby, URL: srv.URL, Enabled: true, Status: "ok", Emby: &connection.EmbyConfig{PlatformUserID: "u1", FeatureImageWrite: true}},
	}}

	p := New(Deps{
		Logger:            silentLogger(),
		ArtistService:     &fakePlatformLister{ids: []artist.PlatformID{{ConnectionID: "c-off", PlatformArtistID: "p1"}, {ConnectionID: "c-bad", PlatformArtistID: "p1"}, {ConnectionID: "c-lid", PlatformArtistID: "p1"}, {ConnectionID: "c-missing", PlatformArtistID: "p1"}, {ConnectionID: "c-ok", PlatformArtistID: "p1"}}},
		ConnectionService: conns,
	})
	warnings := p.SyncImageToPlatforms(context.Background(), &artist.Artist{ID: "a1", Path: dir, Name: "X"}, "thumb")

	// c-lid (Lidarr) is now silently filtered by !GetFeatureImageWrite() before
	// reaching newImageUploader, so "unsupported connection type" is no longer
	// emitted. Only c-missing produces a warning (lookup failure).
	if len(warnings) != 1 {
		t.Errorf("expected 1 warning (lookup-error only); got %d: %v", len(warnings), warnings)
	}
	hasLookup := false
	for _, w := range warnings {
		if strings.Contains(w, "failed to load") {
			hasLookup = true
		}
	}
	if !hasLookup {
		t.Errorf("expected lookup-error warning; got %v", warnings)
	}

	// Exactly one upload arrived (c-ok).
	waitForUploads(t, hits, 1)
}

// TestSyncImageToPlatforms_ReadErrorWarning verifies the read-error warning
// when the resolved file is unreadable. Chmod 0o000 produces a deterministic
// permission error on os.ReadFile across macOS and Linux non-root users; root
// (typical CI container) ignores permissions, so the test skips there rather
// than misreport a silent pass.
func TestSyncImageToPlatforms_ReadErrorWarning(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; chmod 0o000 does not block reads and the read-error branch cannot be exercised on this runner")
	}
	dir := t.TempDir()
	seedJPG(t, dir, "folder.jpg")
	bad := filepath.Join(dir, "folder.jpg")
	if err := os.Chmod(bad, 0o000); err != nil {
		t.Fatalf("chmod 000 folder.jpg: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(bad, 0o644) })

	hits := &uploadHits{}
	srv := newImageUploadServer(hits)
	defer srv.Close()

	p := New(Deps{
		Logger: silentLogger(),
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ConnectionID: "c", PlatformArtistID: "p1"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c": {ID: "c", Type: connection.TypeEmby, URL: srv.URL, Enabled: true, Status: "ok"},
		}},
	})
	warnings := p.SyncImageToPlatforms(context.Background(), &artist.Artist{ID: "a1", Path: dir, Name: "X"}, "thumb")

	// FindExistingImage uses os.Stat which succeeds on directories; the
	// subsequent ReadFile fails. The function returns a "failed to read image"
	// warning. Some platforms might still let ReadFile return data for a dir
	// (rare), but the EISDIR / EISDIR-like error is the universal case.
	foundReadErr := false
	for _, w := range warnings {
		if strings.Contains(w, "failed to read image") {
			foundReadErr = true
		}
	}
	if !foundReadErr {
		t.Errorf("expected 'failed to read image' warning; got %v", warnings)
	}
	// No upload should have arrived.
	time.Sleep(150 * time.Millisecond)
	if got := hits.uploads.Load(); got != 0 {
		t.Errorf("no upload expected; got %d", got)
	}
}

// TestSyncImageToPlatforms_PNGContentTypeDetection verifies the PNG content
// type branch by seeding a .png file and asserting the upload arrives.
func TestSyncImageToPlatforms_PNGContentType(t *testing.T) {
	dir := t.TempDir()
	// poster.png matches no default thumb pattern; use a profile that names
	// it as the thumb so the file is matched.
	if err := os.WriteFile(filepath.Join(dir, "poster.png"), []byte{0x89, 0x50, 0x4e, 0x47}, 0o644); err != nil {
		t.Fatalf("seeding png: %v", err)
	}

	hits := &uploadHits{}
	srv := newImageUploadServer(hits)
	defer srv.Close()

	profile := &platform.Profile{ImageNaming: platform.ImageNaming{
		Thumb: []string{"poster.png"},
	}}

	p := New(Deps{
		Logger:          silentLogger(),
		PlatformService: &fakePlatformProvider{profile: profile},
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ConnectionID: "c-emby", PlatformArtistID: "p1"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c-emby": {ID: "c-emby", Type: connection.TypeEmby, URL: srv.URL, Enabled: true, Status: "ok", Emby: &connection.EmbyConfig{PlatformUserID: "u1", FeatureImageWrite: true}},
		}},
	})
	warnings := p.SyncImageToPlatforms(context.Background(), &artist.Artist{ID: "a1", Path: dir, Name: "X"}, "thumb")
	if len(warnings) != 0 {
		t.Errorf("expected no warnings; got %v", warnings)
	}
	waitForUploads(t, hits, 1)
	// Verify the upload actually carried bytes. A regression that drops
	// the file payload would still match the path-prefix + counter check
	// above without this assertion.
	if got := hits.lastBodySize(); got <= 0 {
		t.Errorf("uploaded body size = %d, want >0 (payload missing)", got)
	}
	// A regression that hard-codes image/jpeg would otherwise pass the
	// upload-count assertion. Verify the uploader actually picked image/png
	// from the .png extension.
	if got := hits.lastContentType(); got != "image/png" {
		t.Errorf("Content-Type: got %q, want image/png", got)
	}
}

// --- SyncAllFanartToPlatforms ---

// TestSyncAllFanartToPlatforms_NilReceiver covers the nil guard.
func TestSyncAllFanartToPlatforms_NilReceiver(t *testing.T) {
	var p *Publisher
	if got := p.SyncAllFanartToPlatforms(context.Background(), &artist.Artist{}); got != nil {
		t.Errorf("nil publisher should return nil; got %v", got)
	}
}

// TestSyncAllFanartToPlatforms_ListerErrorWarning verifies the early-return
// warning path.
func TestSyncAllFanartToPlatforms_ListerErrorWarning(t *testing.T) {
	p := New(Deps{
		Logger:            silentLogger(),
		ArtistService:     errPlatformLister{},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{}},
	})
	warnings := p.SyncAllFanartToPlatforms(context.Background(), &artist.Artist{ID: "a1"})
	if len(warnings) != 1 || !strings.Contains(warnings[0], "failed to load platform mappings") {
		t.Errorf("unexpected warnings: %v", warnings)
	}
}

// TestSyncAllFanartToPlatforms_NoIDsEmpty verifies the no-mappings path.
func TestSyncAllFanartToPlatforms_NoIDsEmpty(t *testing.T) {
	p := New(Deps{
		Logger:            silentLogger(),
		ArtistService:     &fakePlatformLister{ids: nil},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{}},
	})
	got := p.SyncAllFanartToPlatforms(context.Background(), &artist.Artist{ID: "a1"})
	if len(got) != 0 {
		t.Errorf("expected no warnings; got %v", got)
	}
}

// TestSyncAllFanartToPlatforms_NoImageDir verifies the "no image directory"
// warning.
func TestSyncAllFanartToPlatforms_NoImageDir(t *testing.T) {
	p := New(Deps{
		Logger: silentLogger(),
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ConnectionID: "c", PlatformArtistID: "p1"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c": {ID: "c", Type: connection.TypeEmby, Enabled: true, Status: "ok"},
		}},
	})
	warnings := p.SyncAllFanartToPlatforms(context.Background(), &artist.Artist{ID: "a1"})
	if len(warnings) != 1 || !strings.Contains(warnings[0], "no image directory configured") {
		t.Errorf("unexpected warnings: %v", warnings)
	}
}

// TestSyncAllFanartToPlatforms_NoFanartFilesEmpty verifies that an artist
// directory with no fanart returns zero warnings (silent success).
func TestSyncAllFanartToPlatforms_NoFanartFilesEmpty(t *testing.T) {
	dir := t.TempDir() // empty
	p := New(Deps{
		Logger: silentLogger(),
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ConnectionID: "c", PlatformArtistID: "p1"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c": {ID: "c", Type: connection.TypeEmby, Enabled: true, Status: "ok"},
		}},
	})
	warnings := p.SyncAllFanartToPlatforms(context.Background(), &artist.Artist{ID: "a1", Path: dir, Name: "X"})
	if len(warnings) != 0 {
		t.Errorf("expected no warnings; got %v", warnings)
	}
}

// TestSyncAllFanartToPlatforms_HappyPath seeds primary + indexed fanart files
// and verifies multiple uploads arrive.
func TestSyncAllFanartToPlatforms_HappyPath(t *testing.T) {
	dir := t.TempDir()
	seedJPG(t, dir, "fanart.jpg")  // primary
	seedJPG(t, dir, "fanart1.jpg") // index 1
	seedJPG(t, dir, "fanart2.jpg") // index 2

	hits := &uploadHits{}
	srv := newImageUploadServer(hits)
	defer srv.Close()

	p := New(Deps{
		Logger: silentLogger(),
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ConnectionID: "c", PlatformArtistID: "p1"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c": {ID: "c", Type: connection.TypeEmby, URL: srv.URL, Enabled: true, Status: "ok", Emby: &connection.EmbyConfig{PlatformUserID: "u1", FeatureImageWrite: true}},
		}},
	})
	warnings := p.SyncAllFanartToPlatforms(context.Background(), &artist.Artist{ID: "a1", Path: dir, Name: "X"})
	if len(warnings) != 0 {
		t.Errorf("expected no warnings; got %v", warnings)
	}
	waitForUploads(t, hits, 3)
}

// TestSyncAllFanartToPlatforms_PerConnectionBranches covers the disabled /
// bad-status / unsupported / lookup-error connection branches mirroring the
// image-sync tests.
func TestSyncAllFanartToPlatforms_PerConnectionBranches(t *testing.T) {
	dir := t.TempDir()
	seedJPG(t, dir, "fanart.jpg")
	// Naming this file fanart.png drives the publisher's extension-based
	// Content-Type detection (filepath.Ext -> image/png), even though the
	// bytes are still JPEG. Production never sniffs magic bytes, so the
	// mismatch is intentional and exercises only the dispatch path.
	seedJPG(t, dir, "fanart.png")

	hits := &uploadHits{}
	srv := newImageUploadServer(hits)
	defer srv.Close()

	conns := &fakeConnectionGetter{conns: map[string]*connection.Connection{
		"c-off": {ID: "c-off", Type: connection.TypeEmby, URL: srv.URL, Enabled: false, Status: "ok"},
		"c-bad": {ID: "c-bad", Type: connection.TypeEmby, URL: srv.URL, Enabled: true, Status: "error"},
		"c-lid": {ID: "c-lid", Type: connection.TypeLidarr, URL: srv.URL, Enabled: true, Status: "ok"},
		"c-ok":  {ID: "c-ok", Type: connection.TypeEmby, URL: srv.URL, Enabled: true, Status: "ok", Emby: &connection.EmbyConfig{PlatformUserID: "u1", FeatureImageWrite: true}},
	}}

	p := New(Deps{
		Logger: silentLogger(),
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ConnectionID: "c-off", PlatformArtistID: "p1"},
			{ConnectionID: "c-bad", PlatformArtistID: "p1"},
			{ConnectionID: "c-lid", PlatformArtistID: "p1"},
			{ConnectionID: "c-missing", PlatformArtistID: "p1"},
			{ConnectionID: "c-ok", PlatformArtistID: "p1"},
		}},
		ConnectionService: conns,
	})
	warnings := p.SyncAllFanartToPlatforms(context.Background(), &artist.Artist{ID: "a1", Path: dir, Name: "X"})

	// c-lid (Lidarr) is now silently filtered by !GetFeatureImageWrite() before
	// reaching newIndexedImageUploader, so "unsupported connection type" is no
	// longer emitted. Only c-missing produces a warning (lookup failure).
	hasLookup := false
	for _, w := range warnings {
		if strings.Contains(w, "failed to load") {
			hasLookup = true
		}
	}
	if !hasLookup {
		t.Errorf("expected lookup-error warning; got %v", warnings)
	}
	// The c-ok connection is the readable/supported path. Assert that the
	// warning-emitting branches do not short-circuit the entire sync.
	waitForUploads(t, hits, 1)
}

// TestSyncAllFanartToPlatforms_ReadErrorWarning verifies the per-file read
// error path: making a discovered fanart file unreadable (chmod 0000) makes
// os.ReadFile fail and the function emits a "failed to read fanart N"
// warning while well-formed files still upload. Skips when running as root
// (chmod 0o000 does not block root reads on the runner).
func TestSyncAllFanartToPlatforms_ReadErrorWarning(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; chmod 0o000 does not block reads and the read-error branch cannot be exercised on this runner")
	}
	dir := t.TempDir()
	// fanart.jpg is primary (index 0) and a real readable file.
	seedJPG(t, dir, "fanart.jpg")
	// fanart2.jpg exists but cannot be read.
	bad := filepath.Join(dir, "fanart2.jpg")
	seedJPG(t, dir, "fanart2.jpg")
	if err := os.Chmod(bad, 0o000); err != nil {
		t.Fatalf("chmod 000 fanart2.jpg: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(bad, 0o644) })

	hits := &uploadHits{}
	srv := newImageUploadServer(hits)
	defer srv.Close()

	p := New(Deps{
		Logger: silentLogger(),
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ConnectionID: "c-emby", PlatformArtistID: "p1"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c-emby": {ID: "c-emby", Type: connection.TypeEmby, URL: srv.URL, Enabled: true, Status: "ok", Emby: &connection.EmbyConfig{PlatformUserID: "u1", FeatureImageWrite: true}},
		}},
	})
	warnings := p.SyncAllFanartToPlatforms(context.Background(), &artist.Artist{ID: "a1", Path: dir, Name: "X"})

	foundReadErr := false
	for _, w := range warnings {
		if strings.Contains(w, "failed to read fanart") {
			foundReadErr = true
		}
	}
	if !foundReadErr {
		t.Errorf("expected 'failed to read fanart' warning; got %v", warnings)
	}
	// fanart.jpg is readable and primary; it must still upload despite
	// the per-file read error on fanart2.jpg. A regression that aborts
	// the whole sync on the first read error would slip past the warning
	// assertion alone.
	waitForUploads(t, hits, 1)
}

// errFanartDiscovery: not directly invocable from outside the package, but we
// can simulate the discover-error branch by making the directory unreadable.
// On macOS, removing read perms on a tempdir is reliable.
func TestSyncAllFanartToPlatforms_DiscoverError(t *testing.T) {
	dir := t.TempDir()
	// Strip read+execute permissions so DiscoverFanart's ReadDir fails.
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatalf("chmod 000: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	p := New(Deps{
		Logger: silentLogger(),
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ConnectionID: "c", PlatformArtistID: "p1"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c": {ID: "c", Type: connection.TypeEmby, Enabled: true, Status: "ok"},
		}},
	})
	warnings := p.SyncAllFanartToPlatforms(context.Background(), &artist.Artist{ID: "a1", Path: dir, Name: "X"})

	// One of these early-return warnings must surface; otherwise the
	// discover-error branch was never exercised and the test name is a
	// lie. We accept either form because io/fs may swallow chmod 0o000
	// behavior in some sandboxed environments (CI containers run as
	// root, for example, where 0o000 still permits reads).
	for _, w := range warnings {
		if strings.Contains(w, "failed to read fanart directory") ||
			strings.Contains(w, "no image directory configured") {
			return
		}
	}
	// If we're running as root (CI container, common case), chmod 0o000
	// does not block reads and the discover-error branch genuinely cannot
	// be reached from this test. t.Skip with a structured reason so the
	// CI shows the skip explicitly instead of an opaque pass.
	if os.Geteuid() == 0 {
		t.Skip("running as root; chmod 0o000 does not block reads and the discover-error branch cannot be exercised on this runner")
	}
	t.Fatalf("discover-error branch was not exercised; got warnings: %v", warnings)
}
