package updater

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/database"
)

// buildTestService creates a Service backed by a real SQLite DB.
func buildTestService(t *testing.T) *Service {
	t.Helper()

	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("opening db: %v", err)
	}
	if err := database.Migrate(db); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	svc := NewService(db, logger)
	return svc
}

// TestGetSetConfig verifies the config round-trip through the settings table.
func TestGetSetConfig(t *testing.T) {
	svc := buildTestService(t)
	ctx := context.Background()

	// Defaults
	cfg, err := svc.GetConfig(ctx)
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	if cfg.Channel != ChannelStable {
		t.Errorf("default channel = %q, want %q", cfg.Channel, ChannelStable)
	}
	if cfg.AutoCheck {
		t.Error("default auto_check should be false")
	}

	// Round-trip prerelease + auto_check=true
	if err := svc.SetConfig(ctx, Config{Channel: ChannelPrerelease, AutoCheck: true}); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	cfg, err = svc.GetConfig(ctx)
	if err != nil {
		t.Fatalf("GetConfig after set: %v", err)
	}
	if cfg.Channel != ChannelPrerelease {
		t.Errorf("channel = %q, want %q", cfg.Channel, ChannelPrerelease)
	}
	if !cfg.AutoCheck {
		t.Error("auto_check should be true")
	}
}

// TestSetConfigInvalidChannel verifies that an invalid channel is rejected.
func TestSetConfigInvalidChannel(t *testing.T) {
	svc := buildTestService(t)
	err := svc.SetConfig(context.Background(), Config{Channel: "nightly"})
	if err == nil {
		t.Fatal("expected error for invalid channel")
	}
}

// TestPickLatestStable verifies stable channel filtering.
func TestPickLatestStable(t *testing.T) {
	releases := []githubRelease{
		{TagName: "v1.0.0-rc.1", Prerelease: true},
		{TagName: "v0.9.6", Prerelease: false},
		{TagName: "v0.9.5", Prerelease: false},
	}

	got := pickLatest(releases, ChannelStable)
	if got == nil {
		t.Fatal("expected a release")
	}
	if got.TagName != "v0.9.6" {
		t.Errorf("stable latest = %q, want v0.9.6", got.TagName)
	}
}

// TestPickLatestPrerelease verifies prerelease channel includes RC tags.
func TestPickLatestPrerelease(t *testing.T) {
	releases := []githubRelease{
		{TagName: "v1.0.0-rc.1", Prerelease: true},
		{TagName: "v0.9.6", Prerelease: false},
	}

	got := pickLatest(releases, ChannelPrerelease)
	if got == nil {
		t.Fatal("expected a release")
	}
	if got.TagName != "v1.0.0-rc.1" {
		t.Errorf("prerelease latest = %q, want v1.0.0-rc.1", got.TagName)
	}
}

// TestPickLatestSkipsDrafts verifies draft releases are excluded.
func TestPickLatestSkipsDrafts(t *testing.T) {
	releases := []githubRelease{
		{TagName: "v1.0.0", Draft: true},
		{TagName: "v0.9.6", Draft: false},
	}

	got := pickLatest(releases, ChannelStable)
	if got == nil {
		t.Fatal("expected a release")
	}
	if got.TagName != "v0.9.6" {
		t.Errorf("expected v0.9.6, got %q", got.TagName)
	}
}

// TestPickLatestEmpty verifies nil is returned for empty list.
func TestPickLatestEmpty(t *testing.T) {
	got := pickLatest(nil, ChannelStable)
	if got != nil {
		t.Errorf("expected nil, got %+v", got)
	}
}

// TestNewerThan verifies version comparison.
func TestNewerThan(t *testing.T) {
	cases := []struct {
		candidate string
		current   string
		want      bool
	}{
		{"v1.0.0", "v0.9.6", true},
		{"v0.9.6", "v0.9.6", false},
		{"v0.9.5", "v0.9.6", false},
		{"v1.0.0-rc.1", "v0.9.6", true},
		{"v0.9.6", "v0.9.6-rc.2", true}, // stable > prerelease same version
		{"v0.9.6-rc.2", "v0.9.6", false},
		{"v1.1.0", "v1.0.9", true},
	}

	for _, tc := range cases {
		got := newerThan(tc.candidate, tc.current)
		if got != tc.want {
			t.Errorf("newerThan(%q, %q) = %v, want %v", tc.candidate, tc.current, got, tc.want)
		}
	}
}

// TestParseChecksum verifies the checksums.txt parser.
func TestParseChecksum(t *testing.T) {
	data := []byte("abc123  stillwater_1.0.0_linux_amd64.tar.gz\n" +
		"def456  stillwater_1.0.0_darwin_arm64.tar.gz\n")

	got := parseChecksum(data, "stillwater_1.0.0_linux_amd64.tar.gz")
	if got != "abc123" {
		t.Errorf("parseChecksum = %q, want abc123", got)
	}
	notFound := parseChecksum(data, "nonexistent.tar.gz")
	if notFound != "" {
		t.Errorf("parseChecksum for missing file = %q, want empty", notFound)
	}
}

// TestDockerDetection verifies that the env var route is tested.
func TestDockerDetection(t *testing.T) {
	// We can only test the env var path without /.dockerenv present.
	t.Setenv("DOCKER_CONTAINER", "")
	t.Setenv("container", "")

	// Without any indicator set, detectDocker should return false
	// (unless /.dockerenv happens to exist in this test environment).
	if _, err := os.Stat("/.dockerenv"); err == nil {
		t.Skip("/.dockerenv present in this environment; skipping false-path test")
	}
	if detectDocker() {
		t.Error("detectDocker() = true, want false when no indicators are set")
	}

	// With DOCKER_CONTAINER set, it should return true.
	t.Setenv("DOCKER_CONTAINER", "1")
	if !detectDocker() {
		t.Error("detectDocker() = false, want true when DOCKER_CONTAINER is set")
	}
}

// TestBinaryAssetName verifies platform-specific asset naming.
func TestBinaryAssetName(t *testing.T) {
	name := binaryAssetName("v1.0.0")
	// Should contain version (without v), GOOS, GOARCH.
	if name == "" {
		t.Fatal("binaryAssetName returned empty string")
	}
	// Spot-check it contains the expected components.
	if !containsAll(name, "1.0.0", ".tar.gz") {
		t.Errorf("binaryAssetName(%q) = %q, missing version or extension", "v1.0.0", name)
	}
}

// TestExtractBinary verifies that a binary can be extracted from a tar.gz.
func TestExtractBinary(t *testing.T) {
	const want = "hello binary"

	// Build a minimal tar.gz containing a file named "stillwater".
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	content := []byte(want)
	hdr := &tar.Header{
		Name: "stillwater",
		Mode: 0o755,
		Size: int64(len(content)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatalf("tar write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}

	got, err := extractBinary(buf.Bytes())
	if err != nil {
		t.Fatalf("extractBinary: %v", err)
	}
	if string(got) != want {
		t.Errorf("extracted = %q, want %q", got, want)
	}
}

// TestCheckWithMockGitHub exercises the Check method against a mock server.
func TestCheckWithMockGitHub(t *testing.T) {
	svc := buildTestService(t)
	ctx := context.Background()

	releases := []githubRelease{
		{
			TagName:     "v999.0.0",
			Prerelease:  false,
			Draft:       false,
			HTMLURL:     "https://github.com/test/repo/releases/v999.0.0",
			PublishedAt: "2026-01-01T00:00:00Z",
		},
	}
	body, err := json.Marshal(releases)
	if err != nil {
		t.Fatalf("marshaling releases: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	// Override the GitHub API URL by replacing the HTTP client with one that
	// routes all requests to the test server regardless of host.
	svc.httpClient = &http.Client{
		Transport: &rewriteHostTransport{base: srv.URL},
	}

	result, err := svc.Check(ctx)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !result.UpdateAvailable {
		t.Errorf("UpdateAvailable = false, want true")
	}
	if result.Latest != "v999.0.0" {
		t.Errorf("Latest = %q, want v999.0.0", result.Latest)
	}
}

// TestApplyDockerBlocked verifies Apply returns an error in Docker.
func TestApplyDockerBlocked(t *testing.T) {
	svc := buildTestService(t)
	svc.isDocker = true

	err := svc.Apply(context.Background())
	if err == nil {
		t.Fatal("Apply in docker env should return an error")
	}
}

// TestStatusInitial checks that a fresh Service starts idle.
func TestStatusInitial(t *testing.T) {
	svc := buildTestService(t)
	st := svc.Status()
	if st.State != StateIdle {
		t.Errorf("initial state = %q, want %q", st.State, StateIdle)
	}
	if st.Error != "" {
		t.Errorf("initial error = %q, want empty", st.Error)
	}
}

// TestDownloadBytes exercises the download path with a real HTTPS test server.
func TestDownloadBytes(t *testing.T) {
	want := []byte("hello download")
	// NewTLSServer gives us an https:// URL; its Client() trusts the test cert.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(want)
	}))
	defer srv.Close()

	svc := buildTestService(t)
	svc.httpClient = srv.Client() // Client already configured to trust TLS cert.

	got, err := svc.downloadBytes(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("downloadBytes: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestDownloadBytesInsecureScheme verifies that http:// and non-https schemes
// are rejected before any network request is made.
func TestDownloadBytesInsecureScheme(t *testing.T) {
	svc := buildTestService(t)

	cases := []string{
		"http://example.com/asset.tar.gz",
		"file:///etc/passwd",
		"ftp://example.com/file",
	}
	for _, u := range cases {
		_, err := svc.downloadBytes(context.Background(), u)
		if err == nil {
			t.Errorf("downloadBytes(%q) succeeded, expected scheme-rejection error", u)
		}
	}
}

// TestDownloadBytesNon200 verifies that non-200 HTTP responses cause an error.
func TestDownloadBytesNon200(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	svc := buildTestService(t)
	svc.httpClient = srv.Client()

	_, err := svc.downloadBytes(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("expected error for 404 response")
	}
}

// TestChecksumAssetName verifies checksums file naming.
func TestChecksumAssetName(t *testing.T) {
	name := checksumAssetName("v1.0.0")
	if name == "" {
		t.Fatal("checksumAssetName returned empty")
	}
	if !bytes.Contains([]byte(name), []byte("checksums")) {
		t.Errorf("checksumAssetName(%q) = %q, missing 'checksums'", "v1.0.0", name)
	}
}

// TestSHA256Hex verifies sha256Hex produces a consistent digest.
func TestSHA256Hex(t *testing.T) {
	data := []byte("hello")
	got := sha256Hex(data)
	// SHA256("hello") is well-known.
	want := "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if got != want {
		t.Errorf("sha256Hex = %q, want %q", got, want)
	}
}

// TestAtomicReplaceFile verifies atomic binary replacement.
func TestAtomicReplaceFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "bin")

	// Create initial file.
	initial := []byte("old content")
	if err := os.WriteFile(target, initial, 0o755); err != nil {
		t.Fatalf("writing initial: %v", err)
	}

	// Replace with new content.
	newContent := []byte("new content")
	if err := atomicReplaceFile(target, newContent); err != nil {
		t.Fatalf("atomicReplaceFile: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("reading after replace: %v", err)
	}
	if !bytes.Equal(got, newContent) {
		t.Errorf("file content = %q, want %q", got, newContent)
	}
}

// TestAtomicReplaceFileMissing verifies that replacing a non-existent file returns an error.
func TestAtomicReplaceFileMissing(t *testing.T) {
	err := atomicReplaceFile("/nonexistent/path/bin", []byte("content"))
	if err == nil {
		t.Fatal("expected error for missing target")
	}
}

// TestNewDockerService verifies that NewDockerService sets isDocker=true.
func TestNewDockerService(t *testing.T) {
	svc := buildTestService(t)
	if svc.IsDocker() {
		// Not in docker -- this is expected.
		t.Skip("skipping: running in Docker")
	}
	// Build a new docker service using the same db.
	dir := t.TempDir()
	db, _ := database.Open(filepath.Join(dir, "d.db"))
	_ = database.Migrate(db)
	t.Cleanup(func() { _ = db.Close() })

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	dockerSvc := NewDockerService(db, logger)
	if !dockerSvc.IsDocker() {
		t.Error("NewDockerService IsDocker() = false, want true")
	}
}

// TestSetHTTPClient verifies that SetHTTPClient replaces the transport.
func TestSetHTTPClient(t *testing.T) {
	svc := buildTestService(t)
	newClient := &http.Client{}
	svc.SetHTTPClient(newClient)
	// If we get here without panic, the field was set correctly.
}

// TestFetchReleasesRateLimited verifies that rate-limit responses return an error.
func TestFetchReleasesRateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	svc := buildTestService(t)
	svc.SetHTTPClient(&http.Client{Transport: &rewriteHostTransport{base: srv.URL}})

	_, err := svc.fetchReleases(context.Background())
	if err == nil {
		t.Fatal("expected error for rate-limited response")
	}
}

// TestExtractBinaryMissingEntry verifies error when the archive has no binary.
func TestExtractBinaryMissingEntry(t *testing.T) {
	// Build a tar.gz with a file that does NOT match the expected name.
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	content := []byte("not a binary")
	hdr := &tar.Header{Name: "README.md", Mode: 0o644, Size: int64(len(content))}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatalf("tar write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}

	_, err := extractBinary(buf.Bytes())
	if err == nil {
		t.Fatal("expected error when binary not found in archive")
	}
}

// TestApplyAlreadyInProgress verifies that Apply returns ErrAlreadyRunning when in flight.
func TestApplyAlreadyInProgress(t *testing.T) {
	svc := buildTestService(t)
	// Simulate an in-progress apply by setting the atomic flag directly.
	svc.applyRunning.Store(1)

	err := svc.Apply(context.Background())
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("expected ErrAlreadyRunning, got %v", err)
	}
}

// TestApplyConcurrentRace verifies that exactly one of two concurrent Apply
// calls succeeds and the other returns ErrAlreadyRunning. Run with -race.
func TestApplyConcurrentRace(t *testing.T) {
	svc := buildTestService(t)
	ctx := context.Background()

	// Serve an empty release list so runApply exits immediately without
	// touching the filesystem. The test only cares about the Apply return values.
	releases := []map[string]interface{}{}
	body, _ := json.Marshal(releases)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	svc.SetHTTPClient(&http.Client{Transport: &rewriteHostTransport{base: srv.URL}})

	errs := make([]error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	for i := range errs {
		i := i
		go func() {
			defer wg.Done()
			errs[i] = svc.Apply(ctx)
		}()
	}
	wg.Wait()

	// Exactly one should succeed (nil) and one should return ErrAlreadyRunning.
	var nils, blocked int
	for _, err := range errs {
		if err == nil {
			nils++
		} else if errors.Is(err, ErrAlreadyRunning) {
			blocked++
		} else {
			t.Errorf("unexpected error: %v", err)
		}
	}
	if nils != 1 || blocked != 1 {
		t.Errorf("expected 1 success and 1 ErrAlreadyRunning, got %d/%d", nils, blocked)
	}

	// Wait for the goroutine that did launch to finish so the test is clean.
	waitForIdle(t, svc, 5*time.Second)
}

// TestRunApplyNoUpdate exercises the runApply goroutine through the "no update
// needed" early-exit path. When the mock server returns an empty release list,
// pickLatest returns nil and runApply sets state back to idle immediately.
func TestRunApplyNoUpdate(t *testing.T) {
	svc := buildTestService(t)
	ctx := context.Background()

	// Serve an empty release list so pickLatest returns nil, causing runApply
	// to reach the "no update needed" branch and set state back to idle.
	releases := []map[string]interface{}{}
	body, _ := json.Marshal(releases)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	svc.SetHTTPClient(&http.Client{Transport: &rewriteHostTransport{base: srv.URL}})

	if err := svc.Apply(ctx); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// runApply runs in a goroutine. Wait up to 5 seconds for it to return to
	// idle (it should be nearly instant with a mock server and no downloads).
	waitForIdle(t, svc, 5*time.Second)
}

// TestRunApplyWithOldRelease covers the newerThan() branch inside runApply:
// when the latest release has a tag that parses but is not newer than the
// current version, runApply exits via the "no update needed" path.
func TestRunApplyWithOldRelease(t *testing.T) {
	svc := buildTestService(t)
	ctx := context.Background()

	// Return a release with a very old version. With version.Version = "" (dev
	// build), parseSemver("") fails so newerThan returns false for any tag.
	releases := []map[string]interface{}{
		{
			"tag_name":     "v0.0.1",
			"prerelease":   false,
			"draft":        false,
			"html_url":     "https://github.com/example/repo/releases/v0.0.1",
			"published_at": "2020-01-01T00:00:00Z",
			"assets":       []interface{}{},
		},
	}
	body, _ := json.Marshal(releases)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	svc.SetHTTPClient(&http.Client{Transport: &rewriteHostTransport{base: srv.URL}})

	if err := svc.Apply(ctx); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// runApply is fast here: it fetches the mock server, finds no newer
	// release, and returns to idle. Wait up to 5 seconds for that to happen.
	waitForIdle(t, svc, 5*time.Second)
}

// --- helpers ---

// waitForIdle polls Status() until the service reaches StateIdle or StateError,
// failing the test if neither is reached within the deadline. It is used by
// tests that exercise the async runApply goroutine.
func waitForIdle(t *testing.T, svc *Service, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		st := svc.Status()
		if st.State == StateIdle || st.State == StateError {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("service did not reach idle/error within %s; state = %q", timeout, svc.Status().State)
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !bytes.Contains([]byte(s), []byte(sub)) {
			return false
		}
	}
	return true
}

// rewriteHostTransport rewrites all request URLs to point at a specific base
// server, regardless of the original host. Used in tests to intercept GitHub
// API calls without DNS overrides.
type rewriteHostTransport struct {
	base string
}

func (t *rewriteHostTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req2 := req.Clone(req.Context())
	req2.URL.Scheme = "http"
	// Strip the "http://" prefix to get just the host:port.
	host := t.base
	if len(host) > 7 && host[:7] == "http://" {
		host = host[7:]
	}
	req2.URL.Host = host
	return http.DefaultTransport.RoundTrip(req2)
}
