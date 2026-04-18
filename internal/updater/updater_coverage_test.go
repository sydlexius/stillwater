package updater

// Additional tests to raise patch coverage on updater.go above 80%.
// Focused on: runApply error branches, atomicReplaceFile error paths,
// fetchReleases edge cases, detectDocker env var path, pickLatest edge cases,
// newerThan invalid inputs, Status with lastChecked, Check fetch-failure path.
//
// Note: tests that call buildTestService are NOT run in parallel because
// database.Migrate (via goose) writes to a global dialect variable that causes
// a data race under -race when multiple goroutines call Migrate concurrently.
// Pure-logic tests (no DB, no filesystem) use t.Parallel().

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// buildTarGZ returns a .tar.gz containing a single file named name with the
// given content. Used to synthesize fake release archives in tests.
func buildTarGZ(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	hdr := &tar.Header{
		Name: name,
		Mode: 0o755,
		Size: int64(len(content)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("buildTarGZ WriteHeader: %v", err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatalf("buildTarGZ Write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("buildTarGZ tar close: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("buildTarGZ gzip close: %v", err)
	}
	return buf.Bytes()
}

// TestRunApplyFetchError exercises the runApply branch where fetchReleases
// returns an error (server responds with 500).
func TestRunApplyFetchError(t *testing.T) {
	// Not parallel: buildTestService calls database.Migrate (goose global race).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	svc := buildTestService(t)
	svc.SetHTTPClient(&http.Client{Transport: &rewriteHostTransport{base: srv.URL}})

	if err := svc.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	waitForApplyDone(t, svc)

	// The goroutine must have landed in StateError after the 500 response.
	st := svc.Status()
	if st.State != StateError {
		t.Errorf("state = %q after fetch error, want %q", st.State, StateError)
	}
	if st.Error == "" {
		t.Error("expected non-empty error message after fetch failure")
	}
}

// TestRunApplyNoAssetForPlatform exercises the branch where the release exists
// but has no asset matching the current platform.
func TestRunApplyNoAssetForPlatform(t *testing.T) {
	// Not parallel: buildTestService calls database.Migrate (goose global race).

	// Return a release with an asset that will never match the current GOOS/GOARCH.
	releases := []map[string]interface{}{
		{
			"tag_name":     "v999.0.0",
			"prerelease":   false,
			"draft":        false,
			"html_url":     "https://github.com/example/repo/releases/v999.0.0",
			"published_at": "2030-01-01T00:00:00Z",
			"assets": []map[string]string{
				{
					"name":                 "stillwater_999.0.0_wrongos_wrongarch.tar.gz",
					"browser_download_url": "https://example.com/wrong.tar.gz",
				},
			},
		},
	}
	body, _ := json.Marshal(releases)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	svc := buildTestService(t)
	svc.SetHTTPClient(&http.Client{Transport: &rewriteHostTransport{base: srv.URL}})

	if err := svc.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	st := waitForApplyDone(t, svc)
	if !strings.Contains(st.Error, "no release asset found") {
		t.Errorf("expected 'no release asset found' in error, got %q", st.Error)
	}
}

// TestRunApplyChecksumDownloadError exercises the branch where the checksum
// file download fails.
func TestRunApplyChecksumDownloadError(t *testing.T) {
	// Not parallel: buildTestService calls database.Migrate (goose global race).
	tagName := "v999.0.0"
	assetName := binaryAssetName(tagName)
	checksumName := checksumAssetName(tagName)

	releases := []map[string]interface{}{
		{
			"tag_name":     tagName,
			"prerelease":   false,
			"draft":        false,
			"html_url":     "https://github.com/example/repo/releases/" + tagName,
			"published_at": "2030-01-01T00:00:00Z",
			"assets": []map[string]string{
				{
					"name":                 assetName,
					"browser_download_url": "https://placeholder/binary",
				},
				{
					"name":                 checksumName,
					"browser_download_url": "https://placeholder/checksums",
				},
			},
		},
	}
	body, _ := json.Marshal(releases)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/checksums":
			// Return 500 so downloadBytes returns an error.
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(body)
		}
	}))
	defer srv.Close()

	svc := buildTestService(t)
	svc.SetHTTPClient(&http.Client{Transport: &rewriteHostTransport{base: srv.URL}})

	if err := svc.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	st := waitForApplyDone(t, svc)
	if !strings.Contains(st.Error, "downloading checksums") && !strings.Contains(st.Error, "download returned status") {
		t.Errorf("unexpected error %q", st.Error)
	}
}

// TestRunApplyNoChecksumAsset verifies the fail-closed branch when the release
// ships no checksums.txt. Without a manifest, the updater has no way to verify
// the binary and must refuse to install.
func TestRunApplyNoChecksumAsset(t *testing.T) {
	// Not parallel: buildTestService calls database.Migrate (goose global race).
	tagName := "v999.0.0"
	assetName := binaryAssetName(tagName)

	releases := []map[string]interface{}{
		{
			"tag_name":     tagName,
			"prerelease":   false,
			"draft":        false,
			"html_url":     "https://github.com/example/repo/releases/" + tagName,
			"published_at": "2030-01-01T00:00:00Z",
			"assets": []map[string]string{
				{
					"name":                 assetName,
					"browser_download_url": "https://placeholder/binary",
				},
			},
		},
	}
	body, _ := json.Marshal(releases)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	svc := buildTestService(t)
	svc.SetHTTPClient(&http.Client{Transport: &rewriteHostTransport{base: srv.URL}})

	if err := svc.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	st := waitForApplyDone(t, svc)
	if !strings.Contains(st.Error, "checksum asset") || !strings.Contains(st.Error, "not found in release") {
		t.Errorf("expected 'checksum asset ... not found in release' error, got %q", st.Error)
	}
}

// TestRunApplyChecksumMissingAsset verifies the fail-closed branch when the
// checksums.txt is present but does not list the binary asset we need.
func TestRunApplyChecksumMissingAsset(t *testing.T) {
	// Not parallel: buildTestService calls database.Migrate (goose global race).
	tagName := "v999.0.0"
	assetName := binaryAssetName(tagName)
	checksumName := checksumAssetName(tagName)
	// Checksum file references a different asset name, so parseChecksum
	// returns the empty string for assetName.
	checksumContent := []byte("0000000000000000000000000000000000000000000000000000000000000000  other-asset.tar.gz\n")

	releases := []map[string]interface{}{
		{
			"tag_name":     tagName,
			"prerelease":   false,
			"draft":        false,
			"html_url":     "https://github.com/example/repo/releases/" + tagName,
			"published_at": "2030-01-01T00:00:00Z",
			"assets": []map[string]string{
				{
					"name":                 assetName,
					"browser_download_url": "https://placeholder/binary",
				},
				{
					"name":                 checksumName,
					"browser_download_url": "https://placeholder/checksums",
				},
			},
		},
	}
	body, _ := json.Marshal(releases)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/checksums":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(checksumContent)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(body)
		}
	}))
	defer srv.Close()

	svc := buildTestService(t)
	svc.SetHTTPClient(&http.Client{Transport: &rewriteHostTransport{base: srv.URL}})

	if err := svc.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	st := waitForApplyDone(t, svc)
	if !strings.Contains(st.Error, "checksum for asset") || !strings.Contains(st.Error, "not found") {
		t.Errorf("expected 'checksum for asset ... not found' error, got %q", st.Error)
	}
}

// TestRunApplyBinaryDownloadError exercises the branch where the binary
// download fails. Checksum download must succeed (fail-closed policy), so the
// release ships a valid checksums asset even though the binary URL 410s.
func TestRunApplyBinaryDownloadError(t *testing.T) {
	// Not parallel: buildTestService calls database.Migrate (goose global race).
	tagName := "v999.0.0"
	assetName := binaryAssetName(tagName)
	checksumName := checksumAssetName(tagName)
	checksumContent := []byte("0000000000000000000000000000000000000000000000000000000000000000  " + assetName + "\n")

	releases := []map[string]interface{}{
		{
			"tag_name":     tagName,
			"prerelease":   false,
			"draft":        false,
			"html_url":     "https://github.com/example/repo/releases/" + tagName,
			"published_at": "2030-01-01T00:00:00Z",
			"assets": []map[string]string{
				{
					"name":                 assetName,
					"browser_download_url": "https://placeholder/binary",
				},
				{
					"name":                 checksumName,
					"browser_download_url": "https://placeholder/checksums",
				},
			},
		},
	}
	body, _ := json.Marshal(releases)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/binary":
			w.WriteHeader(http.StatusGone)
		case "/checksums":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(checksumContent)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(body)
		}
	}))
	defer srv.Close()

	svc := buildTestService(t)
	svc.SetHTTPClient(&http.Client{Transport: &rewriteHostTransport{base: srv.URL}})

	if err := svc.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	st := waitForApplyDone(t, svc)
	if !strings.Contains(st.Error, "downloading binary") && !strings.Contains(st.Error, "download returned status") {
		t.Errorf("unexpected error %q", st.Error)
	}
}

// TestRunApplyChecksumMismatch exercises the checksum verification failure
// branch in runApply.
func TestRunApplyChecksumMismatch(t *testing.T) {
	// Not parallel: buildTestService calls database.Migrate (goose global race).
	tagName := "v999.0.0"
	assetName := binaryAssetName(tagName)
	checksumName := checksumAssetName(tagName)

	binaryContent := buildTarGZ(t, "stillwater", []byte("fake binary content"))
	// Provide a checksums file with a wrong hash.
	wrongChecksum := "0000000000000000000000000000000000000000000000000000000000000000"
	checksumContent := []byte(wrongChecksum + "  " + assetName + "\n")

	releases := []map[string]interface{}{
		{
			"tag_name":     tagName,
			"prerelease":   false,
			"draft":        false,
			"html_url":     "https://github.com/example/repo/releases/" + tagName,
			"published_at": "2030-01-01T00:00:00Z",
			"assets": []map[string]string{
				{
					"name":                 assetName,
					"browser_download_url": "https://placeholder/binary",
				},
				{
					"name":                 checksumName,
					"browser_download_url": "https://placeholder/checksums",
				},
			},
		},
	}
	body, _ := json.Marshal(releases)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/binary":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(binaryContent)
		case "/checksums":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(checksumContent)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(body)
		}
	}))
	defer srv.Close()

	svc := buildTestService(t)
	svc.SetHTTPClient(&http.Client{Transport: &rewriteHostTransport{base: srv.URL}})

	if err := svc.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	st := waitForApplyDone(t, svc)
	if !strings.Contains(st.Error, "checksum mismatch") {
		t.Errorf("expected 'checksum mismatch' in error, got %q", st.Error)
	}
}

// TestRunApplyExtractError exercises the extractBinary failure branch
// (binary download succeeds but archive is corrupt / missing the binary entry).
// Checksum verification must succeed so the flow reaches the extract step.
func TestRunApplyExtractError(t *testing.T) {
	// Not parallel: buildTestService calls database.Migrate (goose global race).
	tagName := "v999.0.0"
	assetName := binaryAssetName(tagName)
	checksumName := checksumAssetName(tagName)

	// Build a tar.gz that does NOT contain a file named "stillwater".
	badArchive := buildTarGZ(t, "README.md", []byte("no binary here"))
	// Compute the real checksum so verification passes and we reach extract.
	checksumContent := []byte(sha256Hex(badArchive) + "  " + assetName + "\n")

	releases := []map[string]interface{}{
		{
			"tag_name":     tagName,
			"prerelease":   false,
			"draft":        false,
			"html_url":     "https://github.com/example/repo/releases/" + tagName,
			"published_at": "2030-01-01T00:00:00Z",
			"assets": []map[string]string{
				{
					"name":                 assetName,
					"browser_download_url": "https://placeholder/binary",
				},
				{
					"name":                 checksumName,
					"browser_download_url": "https://placeholder/checksums",
				},
			},
		},
	}
	body, _ := json.Marshal(releases)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/binary":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(badArchive)
		case "/checksums":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(checksumContent)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(body)
		}
	}))
	defer srv.Close()

	svc := buildTestService(t)
	svc.SetHTTPClient(&http.Client{Transport: &rewriteHostTransport{base: srv.URL}})

	if err := svc.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	st := waitForApplyDone(t, svc)
	if !strings.Contains(st.Error, "extracting binary") && !strings.Contains(st.Error, "binary not found") {
		t.Errorf("unexpected error %q", st.Error)
	}
}

// TestRunApplyAtomicReplaceError verifies that atomicReplaceFile surfaces the
// underlying tmp-write failure when the target's parent directory is read-only.
// atomicReplaceFile delegates to filesystem.WriteFileAtomic, which fails at the
// tmp-file write step under those conditions.
func TestRunApplyAtomicReplaceError(t *testing.T) {
	// Not parallel: manipulates filesystem permissions.
	if runtime.GOOS == "windows" {
		t.Skip("chmod-based test not applicable on Windows")
	}
	if os.Getuid() == 0 {
		t.Skip("running as root; permission restrictions do not apply")
	}

	dir := t.TempDir()
	targetPath := filepath.Join(dir, "stillwater-bin")
	if err := os.WriteFile(targetPath, []byte("old binary"), 0o755); err != nil {
		t.Fatalf("writing target: %v", err)
	}

	// Make the directory read-only so the tmp-file write fails.
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}
	t.Cleanup(func() {
		// Restore write permission so TempDir cleanup succeeds.
		_ = os.Chmod(dir, 0o755)
	})

	err := atomicReplaceFile(targetPath, []byte("new content"))
	if err == nil {
		t.Fatal("expected error from atomicReplaceFile when dir is read-only")
	}
	if !strings.Contains(err.Error(), "temp file") {
		t.Errorf("error should mention the temp-file write step, got %q", err.Error())
	}
}

// TestAtomicReplaceFileWriteSucceeds verifies that the write+replace path works
// end-to-end (positive case for the Chmod/Write/Close/Rename code path).
func TestAtomicReplaceFileWriteSucceeds(t *testing.T) {
	// Not parallel: filesystem test.
	dir := t.TempDir()
	target := filepath.Join(dir, "bin")
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	if err := atomicReplaceFile(target, []byte("new")); err != nil {
		t.Fatalf("atomicReplaceFile: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("reading file after replace: %v", err)
	}
	if string(got) != "new" {
		t.Errorf("content = %q, want new", got)
	}
}

// TestAtomicReplaceFileStatFail verifies the stat error branch.
func TestAtomicReplaceFileStatFail(t *testing.T) {
	t.Parallel()

	err := atomicReplaceFile("/no/such/path/bin", []byte("content"))
	if err == nil {
		t.Fatal("expected error for missing target")
	}
	if !strings.Contains(err.Error(), "stat target") {
		t.Errorf("error should mention 'stat target', got %q", err.Error())
	}
}

// TestFetchReleasesTooManyRequests verifies that 429 is treated as rate-limited.
func TestFetchReleasesTooManyRequests(t *testing.T) {
	// Not parallel: buildTestService calls database.Migrate (goose global race).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	svc := buildTestService(t)
	svc.SetHTTPClient(&http.Client{Transport: &rewriteHostTransport{base: srv.URL}})

	_, err := svc.fetchReleases(context.Background())
	if err == nil {
		t.Fatal("expected error for 429")
	}
	if !strings.Contains(err.Error(), "rate limited") {
		t.Errorf("error should mention 'rate limited', got %q", err.Error())
	}
}

// TestFetchReleasesNon200 verifies non-200, non-rate-limit responses return an error.
func TestFetchReleasesNon200(t *testing.T) {
	// Not parallel: buildTestService calls database.Migrate (goose global race).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	svc := buildTestService(t)
	svc.SetHTTPClient(&http.Client{Transport: &rewriteHostTransport{base: srv.URL}})

	_, err := svc.fetchReleases(context.Background())
	if err == nil {
		t.Fatal("expected error for 503")
	}
	if !strings.Contains(err.Error(), "returned status 503") {
		t.Errorf("error should mention status 503, got %q", err.Error())
	}
}

// TestFetchReleasesJSONDecodeError verifies JSON decode errors are surfaced.
func TestFetchReleasesJSONDecodeError(t *testing.T) {
	// Not parallel: buildTestService calls database.Migrate (goose global race).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not json at all }{"))
	}))
	defer srv.Close()

	svc := buildTestService(t)
	svc.SetHTTPClient(&http.Client{Transport: &rewriteHostTransport{base: srv.URL}})

	_, err := svc.fetchReleases(context.Background())
	if err == nil {
		t.Fatal("expected JSON decode error")
	}
	if !strings.Contains(err.Error(), "decoding github response") {
		t.Errorf("error should mention 'decoding github response', got %q", err.Error())
	}
}

// TestDetectDockerContainerEnvVar exercises the container=docker env var path.
func TestDetectDockerContainerEnvVar(t *testing.T) {
	// Not parallel: modifies process env via t.Setenv.
	if _, err := os.Stat("/.dockerenv"); err == nil {
		t.Skip("/.dockerenv present; fires before env var branch")
	}

	t.Setenv("container", "docker")
	t.Setenv("DOCKER_CONTAINER", "")

	if !detectDocker() {
		t.Error("detectDocker() = false, expected true when container=docker")
	}
}

// TestPickLatestAllDrafts verifies nil is returned when all releases are drafts.
func TestPickLatestAllDrafts(t *testing.T) {
	t.Parallel()

	releases := []githubRelease{
		{TagName: "v1.0.0", Draft: true},
		{TagName: "v0.9.6", Draft: true},
	}
	got := pickLatest(releases, ChannelStable)
	if got != nil {
		t.Errorf("expected nil for all-draft list, got %+v", got)
	}
}

// TestPickLatestAllPrereleasesStableChannel verifies nil when all are prereleases
// and the channel is stable.
func TestPickLatestAllPrereleasesStableChannel(t *testing.T) {
	t.Parallel()

	releases := []githubRelease{
		{TagName: "v1.0.0-rc.1", Prerelease: true},
		{TagName: "v0.9.6-rc.2", Prerelease: true},
	}
	got := pickLatest(releases, ChannelStable)
	if got != nil {
		t.Errorf("expected nil for all-prerelease list on stable channel, got %+v", got)
	}
}

// TestPickLatestInvalidSemver verifies that tags failing the semver regex are
// skipped.
func TestPickLatestInvalidSemver(t *testing.T) {
	t.Parallel()

	releases := []githubRelease{
		{TagName: "nightly-20260101", Prerelease: false, Draft: false},
		{TagName: "v1.0.0", Prerelease: false, Draft: false},
	}
	got := pickLatest(releases, ChannelStable)
	if got == nil {
		t.Fatal("expected v1.0.0, got nil")
	}
	if got.TagName != "v1.0.0" {
		t.Errorf("got %q, want v1.0.0", got.TagName)
	}
}

// TestNewerThanInvalidVersions verifies that invalid version strings cause
// newerThan to return false (both parse errors branch).
func TestNewerThanInvalidVersions(t *testing.T) {
	t.Parallel()

	cases := []struct {
		candidate string
		current   string
	}{
		{"not-a-version", "v1.0.0"},
		{"v1.0.0", "also-not-valid"},
		{"", ""},
	}
	for _, tc := range cases {
		if newerThan(tc.candidate, tc.current) {
			t.Errorf("newerThan(%q, %q) = true, want false for invalid inputs", tc.candidate, tc.current)
		}
	}
}

// TestStatusWithLastChecked verifies that Status.LastChecked is populated
// after setState(StateChecking, ...) is called.
func TestStatusWithLastChecked(t *testing.T) {
	// Not parallel: buildTestService calls database.Migrate (goose global race).
	svc := buildTestService(t)
	// Trigger the StateChecking path which sets lastChecked.
	svc.setState(StateChecking, 0, "")
	// Reset to idle but lastChecked should remain set.
	svc.setState(StateIdle, 100, "")

	st := svc.Status()
	if st.LastChecked == "" {
		t.Error("LastChecked should be populated after a check cycle")
	}
}

// TestCheckFetchError verifies that Check propagates a fetchReleases error.
func TestCheckFetchError(t *testing.T) {
	// Not parallel: buildTestService calls database.Migrate (goose global race).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	svc := buildTestService(t)
	svc.SetHTTPClient(&http.Client{Transport: &rewriteHostTransport{base: srv.URL}})

	_, err := svc.Check(context.Background())
	if err == nil {
		t.Fatal("expected error when fetch fails")
	}
	if !strings.Contains(err.Error(), "fetching releases") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "fetching releases")
	}
}

// TestCheckNoReleases verifies that Check returns a result with UpdateAvailable=false
// when the release list is empty (pickLatest returns nil).
func TestCheckNoReleases(t *testing.T) {
	// Not parallel: buildTestService calls database.Migrate (goose global race).
	body, _ := json.Marshal([]githubRelease{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	svc := buildTestService(t)
	svc.SetHTTPClient(&http.Client{Transport: &rewriteHostTransport{base: srv.URL}})

	res, err := svc.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.UpdateAvailable {
		t.Error("expected UpdateAvailable=false for empty release list")
	}
}

// TestCheckClearsStaleUpdateFlag verifies that a Check run that finds no
// matching release clears updateAvailable and latestVersion even when a
// previous check had set them. This prevents /status from advertising a
// stale update after a channel switch.
func TestCheckClearsStaleUpdateFlag(t *testing.T) {
	// Not parallel: buildTestService calls database.Migrate (goose global race).

	// First: serve a release so the service caches updateAvailable=true.
	releasesWithUpdate := []githubRelease{
		{TagName: "v999.0.0", Prerelease: false, Draft: false, HTMLURL: "https://example.com/v999"},
	}
	positiveBody, _ := json.Marshal(releasesWithUpdate)

	// Second: serve an empty list to simulate no matching release on the new channel.
	emptyBody, _ := json.Marshal([]githubRelease{})

	// Use atomic to guard serveEmpty: the test goroutine writes it and the
	// httptest.Server handler goroutine reads it. A plain bool would be a data race.
	var serveEmpty atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if serveEmpty.Load() {
			_, _ = w.Write(emptyBody)
		} else {
			_, _ = w.Write(positiveBody)
		}
	}))
	defer srv.Close()

	svc := buildTestService(t)
	svc.SetHTTPClient(&http.Client{Transport: &rewriteHostTransport{base: srv.URL}})

	// First check: should set updateAvailable.
	_, err := svc.Check(context.Background())
	if err != nil {
		t.Fatalf("first Check: %v", err)
	}
	if st := svc.Status(); !st.UpdateAvailable {
		t.Fatal("expected UpdateAvailable=true after first check")
	}

	// Second check: no releases -- should clear the stale flag.
	serveEmpty.Store(true)
	_, err = svc.Check(context.Background())
	if err != nil {
		t.Fatalf("second Check: %v", err)
	}
	st := svc.Status()
	if st.UpdateAvailable {
		t.Error("UpdateAvailable should be false after check returns no releases")
	}
	if st.Latest != "" {
		t.Errorf("Latest = %q, want empty after stale-clear", st.Latest)
	}
}

// TestExtractBinaryCorruptGzip verifies that a corrupt gzip input returns an error.
func TestExtractBinaryCorruptGzip(t *testing.T) {
	t.Parallel()

	_, err := extractBinary([]byte("not a gzip archive at all"))
	if err == nil {
		t.Fatal("expected error for corrupt gzip")
	}
}

// TestExtractBinaryCorruptTar verifies that a valid gzip but corrupt tar returns an error.
func TestExtractBinaryCorruptTar(t *testing.T) {
	t.Parallel()

	// Write a valid gzip wrapper around garbage tar data.
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	_, _ = gw.Write([]byte("this is not valid tar data at all !@#$"))
	_ = gw.Close()

	_, err := extractBinary(buf.Bytes())
	if err == nil {
		t.Fatal("expected error for corrupt tar data")
	}
}

// TestRunApplyConfigError exercises the runApply branch where GetConfig returns
// an error. We do this by closing the database before calling Apply.
func TestRunApplyConfigError(t *testing.T) {
	// Not parallel: closes a DB that buildTestService opens.
	svc := buildTestService(t)

	// Close the DB directly to make GetConfig fail.
	if err := svc.db.Close(); err != nil {
		t.Fatalf("closing db: %v", err)
	}

	if err := svc.Apply(context.Background()); err != nil {
		t.Fatalf("Apply launch: %v", err)
	}

	st := waitForApplyDone(t, svc)
	if !strings.Contains(st.Error, "reading config") {
		t.Errorf("expected 'reading config' in error, got %q", st.Error)
	}
}

// TestGetConfigUnknownChannel verifies that an unsupported channel value
// stored in the DB is silently ignored and the default (stable) is returned.
func TestGetConfigUnknownChannel(t *testing.T) {
	// Not parallel: buildTestService calls database.Migrate (goose global race).
	svc := buildTestService(t)
	ctx := context.Background()

	// Write an unknown channel value directly to the settings table.
	_, err := svc.db.ExecContext(ctx,
		`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		SettingChannel, "unknown-channel", "2030-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("inserting bad channel: %v", err)
	}

	cfg, err := svc.GetConfig(ctx)
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	// Unknown channel should fall through to default (stable).
	if cfg.Channel != ChannelStable {
		t.Errorf("channel = %q, want %q for unknown value", cfg.Channel, ChannelStable)
	}
}

// TestExtractBinaryWindowsExe verifies that "stillwater.exe" is found in archives.
func TestExtractBinaryWindowsExe(t *testing.T) {
	t.Parallel()

	const want = "windows binary"
	archive := buildTarGZ(t, "stillwater.exe", []byte(want))

	got, err := extractBinary(archive)
	if err != nil {
		t.Fatalf("extractBinary: %v", err)
	}
	if string(got) != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// waitForApplyDone waits up to 5 seconds for the runApply goroutine to finish.
// Apply sets applyRunning=1 before launching the goroutine (via CompareAndSwap),
// so applyRunning==1 is guaranteed immediately after Apply returns. The goroutine
// resets it to 0 via a deferred Store(0). Polling for 0 is therefore reliable
// and avoids the false-idle race that occurs when polling Status() directly.
func waitForApplyDone(t *testing.T, svc *Service) StatusResult {
	t.Helper()
	const timeout = 5 * time.Second
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if svc.applyRunning.Load() == 0 {
			return svc.Status()
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("runApply goroutine did not finish within %s; state = %q", timeout, svc.Status().State)
	return StatusResult{}
}

// TestDownloadBytesConnectionError exercises the "do request fails" branch.
func TestDownloadBytesConnectionError(t *testing.T) {
	// Not parallel: buildTestService calls database.Migrate (goose global race).
	svc := buildTestService(t)
	// Use a TLS server but a plain HTTP client without the cert; TLS handshake
	// fails, exercising the httpClient.Do error branch in downloadBytes.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("data"))
	}))
	defer srv.Close()

	// Use the default (non-TLS) client which won't trust the test cert.
	svc.SetHTTPClient(&http.Client{Timeout: 2 * time.Second})

	_, err := svc.downloadBytes(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("expected TLS error when client doesn't trust test cert")
	}
}

// TestPickLatestStableWithHyphenTag verifies that a stable-channel request
// skips releases whose tag contains a hyphen suffix (even if prerelease=false).
func TestPickLatestStableWithHyphenTag(t *testing.T) {
	t.Parallel()

	releases := []githubRelease{
		// Tag has hyphen suffix but prerelease flag is false (inconsistent metadata).
		{TagName: "v1.0.0-custom", Prerelease: false, Draft: false},
		{TagName: "v0.9.6", Prerelease: false, Draft: false},
	}
	got := pickLatest(releases, ChannelStable)
	if got == nil {
		t.Fatal("expected v0.9.6, got nil")
	}
	if got.TagName != "v0.9.6" {
		t.Errorf("got %q, want v0.9.6", got.TagName)
	}
}

// TestPickLatestBackportOrder verifies that pickLatest selects the highest
// semver even when a backport is published after a newer major release.
// In a first-match strategy, v1.9.9 (index 0) would win; the correct answer
// is v2.0.0.
func TestPickLatestBackportOrder(t *testing.T) {
	t.Parallel()

	// Simulate GitHub's reverse-chronological list: v1.9.9 (backport) is
	// listed first because it was published most recently, but v2.0.0 has a
	// higher semver and should be returned.
	releases := []githubRelease{
		{TagName: "v1.9.9", Prerelease: false, Draft: false}, // backport, published last
		{TagName: "v2.0.0", Prerelease: false, Draft: false}, // original, published first
	}
	got := pickLatest(releases, ChannelStable)
	if got == nil {
		t.Fatal("expected v2.0.0, got nil")
	}
	if got.TagName != "v2.0.0" {
		t.Errorf("got %q, want v2.0.0 (highest semver)", got.TagName)
	}
}

// TestPickLatestBackportOrderPrerelease verifies the same semver-max behavior
// on the prerelease channel.
func TestPickLatestBackportOrderPrerelease(t *testing.T) {
	t.Parallel()

	releases := []githubRelease{
		{TagName: "v2.1.0-rc.1", Prerelease: true, Draft: false}, // newest by publish date
		{TagName: "v2.1.0", Prerelease: false, Draft: false},     // older publish date but higher semver (stable > prerelease)
		{TagName: "v1.9.9", Prerelease: false, Draft: false},
	}
	got := pickLatest(releases, ChannelPrerelease)
	if got == nil {
		t.Fatal("expected a result, got nil")
	}
	if got.TagName != "v2.1.0" {
		t.Errorf("got %q, want v2.1.0 (highest semver on prerelease channel)", got.TagName)
	}
}

// TestCheckAfterSetConfig verifies that Check reads the channel from DB,
// so setting prerelease config causes Check to use the prerelease channel.
func TestCheckAfterSetConfig(t *testing.T) {
	// Not parallel: buildTestService calls database.Migrate (goose global race).
	body, _ := json.Marshal([]githubRelease{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	svc := buildTestService(t)
	ctx := context.Background()

	if err := svc.SetConfig(ctx, Config{Channel: ChannelPrerelease, AutoCheck: false}); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	svc.SetHTTPClient(&http.Client{Transport: &rewriteHostTransport{base: srv.URL}})

	res, err := svc.Check(ctx)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Channel != ChannelPrerelease {
		t.Errorf("result channel = %q, want %q", res.Channel, ChannelPrerelease)
	}
}
