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
	"strings"
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

	// Round-trip nightly: both SetConfig validation and GetConfig parsing
	// must accept the nightly channel end-to-end. This is the integration
	// point that ties the settings KV serialization to the Channel enum.
	if err := svc.SetConfig(ctx, Config{Channel: ChannelNightly, AutoCheck: false}); err != nil {
		t.Fatalf("SetConfig (nightly): %v", err)
	}
	cfg, err = svc.GetConfig(ctx)
	if err != nil {
		t.Fatalf("GetConfig after nightly set: %v", err)
	}
	if cfg.Channel != ChannelNightly {
		t.Errorf("channel = %q, want %q", cfg.Channel, ChannelNightly)
	}
	if cfg.AutoCheck {
		t.Error("auto_check should be false after nightly set")
	}
}

// TestSetConfigInvalidChannel verifies that an invalid channel is rejected.
func TestSetConfigInvalidChannel(t *testing.T) {
	svc := buildTestService(t)
	err := svc.SetConfig(context.Background(), Config{Channel: "bogus"})
	if err == nil {
		t.Fatal("expected error for invalid channel")
	}
}

// TestSetConfigClearsCacheOnChannelChange verifies that switching the
// updater channel invalidates the in-memory cached release fields. Without
// this, the sidebar "update available" pill (and the Settings > Updates
// latest row) would advertise a release from the previous channel until
// the next Check, breaking the "channel selector = source of truth" UX.
// Seeding the cache directly sidesteps the HTTP test harness because the
// invariant under test is a pure bookkeeping step in SetConfig, not a
// network-dependent behavior.
func TestSetConfigClearsCacheOnChannelChange(t *testing.T) {
	svc := buildTestService(t)
	ctx := context.Background()
	wantChecked := "2026-01-01T00:00:00Z"

	// Default channel is stable. Seed the in-memory cache as if a prior
	// Check found an update on stable. LastChecked is seeded separately
	// so we can prove it is preserved across both branches below: the
	// Updates tab keeps displaying "Last checked <time>" even when
	// latest/releaseURL are cleared by a channel switch.
	svc.mu.Lock()
	svc.updateAvailable = true
	svc.latestVersion = "v999.0.0"
	svc.releaseURL = "https://example.com/v999"
	svc.lastChecked = time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	svc.mu.Unlock()

	// Saving the same channel (no change) must NOT clear the cache: users
	// toggling auto_check alone should not see the sidebar pill flash,
	// and LastChecked must remain available for the Updates tab.
	if err := svc.SetConfig(ctx, Config{Channel: ChannelStable, AutoCheck: true}); err != nil {
		t.Fatalf("SetConfig (same channel): %v", err)
	}
	if st := svc.Status(); !st.UpdateAvailable || st.ReleaseURL == "" || st.LastChecked != wantChecked {
		t.Errorf("same-channel save cleared cache: status=%+v", st)
	}

	// Switching to a different channel MUST invalidate the cache so the
	// next /status response reflects the fresh (not yet checked) state.
	// LastChecked must survive: the Updates tab still wants to show when
	// we last looked, even though what we found no longer applies.
	if err := svc.SetConfig(ctx, Config{Channel: ChannelPrerelease, AutoCheck: false}); err != nil {
		t.Fatalf("SetConfig (channel change): %v", err)
	}
	st := svc.Status()
	if st.UpdateAvailable {
		t.Errorf("after channel change, UpdateAvailable = true, want false")
	}
	if st.ReleaseURL != "" {
		t.Errorf("after channel change, ReleaseURL = %q, want empty", st.ReleaseURL)
	}
	if st.Latest != "" {
		t.Errorf("after channel change, Latest = %q, want empty", st.Latest)
	}
	if st.LastChecked != wantChecked {
		t.Errorf("after channel change, LastChecked = %q, want %q", st.LastChecked, wantChecked)
	}
}

// TestSetConfigClearsCacheOnChannelChangeToNightly verifies the cache
// invalidation also fires when the target channel is nightly. Without
// this coverage the nightly channel could inherit stale stable- or
// prerelease-channel release metadata until the next Check and re-surface
// the cross-channel sidebar pill this guard is meant to suppress.
func TestSetConfigClearsCacheOnChannelChangeToNightly(t *testing.T) {
	svc := buildTestService(t)
	ctx := context.Background()

	// Start on prerelease with a seeded cache, then switch to nightly.
	if err := svc.SetConfig(ctx, Config{Channel: ChannelPrerelease}); err != nil {
		t.Fatalf("SetConfig (prerelease): %v", err)
	}
	svc.mu.Lock()
	svc.updateAvailable = true
	svc.latestVersion = "v0.9.6-rc.1"
	svc.releaseURL = "https://example.com/rc"
	svc.mu.Unlock()

	if err := svc.SetConfig(ctx, Config{Channel: ChannelNightly}); err != nil {
		t.Fatalf("SetConfig (nightly): %v", err)
	}
	st := svc.Status()
	if st.UpdateAvailable || st.Latest != "" || st.ReleaseURL != "" {
		t.Errorf("switch to nightly did not clear cache: status=%+v", st)
	}
}

// TestSetConfigBumpsConfigGenOnChannelChange verifies the generation
// token is incremented exactly when the channel changes. The token
// is the mechanism that lets Check() discard its cache write when a
// channel switch happens mid-flight: if this bump is missing, an
// in-flight Check can resurrect old-channel release data after
// SetConfig clears the cache, reintroducing the stale cross-channel
// badge this PR is trying to eliminate.
func TestSetConfigBumpsConfigGenOnChannelChange(t *testing.T) {
	svc := buildTestService(t)
	ctx := context.Background()

	svc.mu.RLock()
	genBefore := svc.configGen
	svc.mu.RUnlock()

	// Same-channel save (stable -> stable) must NOT bump the gen.
	// Users toggling auto_check alone have not invalidated the cache,
	// so a concurrent Check's write should still land.
	if err := svc.SetConfig(ctx, Config{Channel: ChannelStable, AutoCheck: true}); err != nil {
		t.Fatalf("SetConfig (same channel): %v", err)
	}
	svc.mu.RLock()
	genAfterSame := svc.configGen
	svc.mu.RUnlock()
	if genAfterSame != genBefore {
		t.Errorf("same-channel save bumped configGen: before=%d after=%d", genBefore, genAfterSame)
	}

	// Channel switch (stable -> prerelease) MUST bump the gen so any
	// in-flight Check started on stable has its write discarded.
	if err := svc.SetConfig(ctx, Config{Channel: ChannelPrerelease, AutoCheck: false}); err != nil {
		t.Fatalf("SetConfig (channel change): %v", err)
	}
	svc.mu.RLock()
	genAfterSwitch := svc.configGen
	svc.mu.RUnlock()
	if genAfterSwitch != genAfterSame+1 {
		t.Errorf("channel switch: configGen = %d, want %d", genAfterSwitch, genAfterSame+1)
	}
}

// TestStoreCheckResultDoesNotAdvanceLastCheckedOnReject verifies that a
// discarded Check() leaves lastChecked untouched, not advanced. If
// setState(StateChecking) still wrote lastChecked at check-start (the
// behavior removed alongside the configGen guard), a Check() racing a
// channel switch would leave /status reporting "checked just now, no
// update" -- indistinguishable from a real successful empty check. The
// guarantee we want: lastChecked reflects the moment we last cached a
// result, so operators can tell "check was discarded, we have no info
// on the new channel yet" (lastChecked unchanged) from "we actually
// looked and found nothing" (lastChecked advanced).
func TestStoreCheckResultDoesNotAdvanceLastCheckedOnReject(t *testing.T) {
	svc := buildTestService(t)

	// Seed lastChecked to a known point-in-time so we can detect any
	// unintended advancement by the discard path.
	seeded := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	svc.mu.Lock()
	svc.lastChecked = seeded
	gen := svc.configGen
	svc.configGen++
	svc.mu.Unlock()

	// storeCheckResult with a stale gen must reject and leave lastChecked
	// unchanged. The later-time candidate would obviously be "newer" if
	// the rejection path wrote through -- the assertion would catch that.
	later := seeded.Add(10 * time.Minute)
	if svc.storeCheckResult(gen, later, true, "v2.0.0", "https://example.com/v2") {
		t.Fatal("storeCheckResult with stale gen returned true, want false")
	}
	svc.mu.RLock()
	got := svc.lastChecked
	svc.mu.RUnlock()
	if !got.Equal(seeded) {
		t.Errorf("rejected storeCheckResult advanced lastChecked: got %v, want %v", got, seeded)
	}
}

// TestStoreCheckResultGenGuard covers the core invariant that protects
// the cache from stale in-flight Check() writes. A Check that captured
// gen=N before a channel switch must not be able to write back after
// SetConfig bumps gen to N+1; storeCheckResult is the single point
// where that guard is enforced.
func TestStoreCheckResultGenGuard(t *testing.T) {
	svc := buildTestService(t)
	when := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)

	// Matching gen -- write must apply. This is the happy path: no
	// concurrent channel switch, Check's captured gen still matches,
	// cache reflects what Check found.
	svc.mu.RLock()
	gen := svc.configGen
	svc.mu.RUnlock()
	if !svc.storeCheckResult(gen, when, true, "v2.0.0", "https://example.com/v2") {
		t.Fatal("storeCheckResult with matching gen returned false, want true")
	}
	if st := svc.Status(); !st.UpdateAvailable || st.Latest != "v2.0.0" || st.ReleaseURL != "https://example.com/v2" {
		t.Errorf("after matching-gen write, status=%+v, want update=true latest=v2.0.0", st)
	}

	// Simulate a channel switch bumping the gen. The values we wrote
	// above are now considered stale by the guard.
	svc.mu.Lock()
	svc.configGen++
	svc.mu.Unlock()

	// Stale gen -- write must be rejected and cache must be untouched.
	if svc.storeCheckResult(gen, when.Add(time.Minute), false, "v3.0.0-stale", "https://example.com/stale") {
		t.Fatal("storeCheckResult with stale gen returned true, want false")
	}
	st := svc.Status()
	if !st.UpdateAvailable || st.Latest != "v2.0.0" || st.ReleaseURL != "https://example.com/v2" {
		t.Errorf("stale-gen write mutated cache: status=%+v, want the matching-gen values preserved", st)
	}
}

// TestDecideChannelChanged covers the pure decision used by SetConfig to
// decide whether the cached release fields should be invalidated. The
// error-read branch exists as a fail-safe: if we can't confirm the
// previous channel, we must assume it changed, otherwise a real switch
// with an unreadable previous state would leave stale release metadata
// pointing at the wrong channel's release.
func TestDecideChannelChanged(t *testing.T) {
	tests := []struct {
		name    string
		prev    Config
		prevErr error
		cfg     Config
		want    bool
	}{
		{
			name: "same channel and no error returns false",
			prev: Config{Channel: ChannelStable},
			cfg:  Config{Channel: ChannelStable},
			want: false,
		},
		{
			name: "different channel and no error returns true",
			prev: Config{Channel: ChannelStable},
			cfg:  Config{Channel: ChannelPrerelease},
			want: true,
		},
		{
			name:    "read error returns true (fail-safe)",
			prev:    Config{},
			prevErr: errors.New("read failed"),
			cfg:     Config{Channel: ChannelStable},
			want:    true,
		},
		{
			name:    "read error returns true even when channels match",
			prev:    Config{Channel: ChannelStable},
			prevErr: errors.New("read failed"),
			cfg:     Config{Channel: ChannelStable},
			want:    true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := decideChannelChanged(tc.prev, tc.prevErr, tc.cfg); got != tc.want {
				t.Errorf("decideChannelChanged(%+v, %v, %+v) = %v, want %v",
					tc.prev, tc.prevErr, tc.cfg, got, tc.want)
			}
		})
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

// TestPickLatestNightly verifies that the nightly channel picks the
// lexicographically largest "nightly-YYYYMMDD" tag and ignores semver
// releases even when those are newer by publish order.
func TestPickLatestNightly(t *testing.T) {
	releases := []githubRelease{
		{TagName: "v0.9.5", Prerelease: false},
		{TagName: "v0.9.6-rc.1", Prerelease: true},
		{TagName: "nightly-20260420", Prerelease: true},
		{TagName: "nightly-20260422", Prerelease: true},
	}

	got := pickLatest(releases, ChannelNightly)
	if got == nil {
		t.Fatal("expected a nightly release")
	}
	if got.TagName != "nightly-20260422" {
		t.Errorf("nightly latest = %q, want nightly-20260422", got.TagName)
	}
}

// TestPickLatestNightlyIgnoredByStable verifies that nightly tags are
// invisible to the stable and prerelease channels, which must remain
// semver-only to keep binary-asset naming deterministic.
func TestPickLatestNightlyIgnoredByStable(t *testing.T) {
	releases := []githubRelease{
		{TagName: "nightly-20260422", Prerelease: true},
		{TagName: "v0.9.6", Prerelease: false},
	}

	got := pickLatest(releases, ChannelStable)
	if got == nil {
		t.Fatal("expected a stable release")
	}
	if got.TagName != "v0.9.6" {
		t.Errorf("stable latest = %q, want v0.9.6", got.TagName)
	}

	got = pickLatest(releases, ChannelPrerelease)
	if got == nil {
		t.Fatal("expected a prerelease match")
	}
	if got.TagName != "v0.9.6" {
		t.Errorf("prerelease latest = %q, want v0.9.6 (nightly must be excluded)", got.TagName)
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

// TestNewerThan verifies version comparison, including cross-channel
// cases where either side may be a nightly tag. Nightly vs semver is
// treated as "any difference is newer" so a channel switch always
// surfaces an actionable update in the UI.
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

		// Nightly vs nightly: lex compare on the "nightly-YYYYMMDD" tag.
		{"nightly-20260422", "nightly-20260420", true},
		{"nightly-20260420", "nightly-20260422", false},
		{"nightly-20260422", "nightly-20260422", false},

		// Cross-kind: any difference is considered an update.
		{"nightly-20260422", "v0.9.5", true},
		{"v1.0.0", "nightly-20260422", true},
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

// TestStatusCarriesCheckResultFields verifies that after a successful Check
// that finds an update, Status() returns update_available=true and the
// release_url from the GitHub response. This is the contract the sidebar
// badge depends on: the sidebar calls GET /updates/status (never /check,
// because /check hits GitHub) and expects the last-known check result to
// have been cached on the service.
func TestStatusCarriesCheckResultFields(t *testing.T) {
	svc := buildTestService(t)
	ctx := context.Background()

	const wantURL = "https://github.com/test/repo/releases/v999.0.0"
	releases := []githubRelease{
		{
			TagName:     "v999.0.0",
			Prerelease:  false,
			Draft:       false,
			HTMLURL:     wantURL,
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

	svc.httpClient = &http.Client{
		Transport: &rewriteHostTransport{base: srv.URL},
	}

	// Pre-condition: before any check, Status() must not advertise an update.
	if st := svc.Status(); st.UpdateAvailable || st.ReleaseURL != "" {
		t.Fatalf("initial Status = %+v, want update_available=false and empty release_url", st)
	}

	if _, err := svc.Check(ctx); err != nil {
		t.Fatalf("Check: %v", err)
	}

	st := svc.Status()
	if !st.UpdateAvailable {
		t.Errorf("Status.UpdateAvailable = false, want true after Check found an update")
	}
	if st.ReleaseURL != wantURL {
		t.Errorf("Status.ReleaseURL = %q, want %q", st.ReleaseURL, wantURL)
	}
	if st.Latest != "v999.0.0" {
		t.Errorf("Status.Latest = %q, want v999.0.0", st.Latest)
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
	db, err := database.Open(filepath.Join(dir, "d.db"))
	if err != nil {
		t.Fatalf("opening db: %v", err)
	}
	if err := database.Migrate(db); err != nil {
		t.Fatalf("migrating db: %v", err)
	}
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

	// Hold the release fetch open so the winning goroutine's runApply
	// stays in-flight (keeping applyRunning=1) until after both
	// goroutines have raced through CompareAndSwap. Without the block,
	// runApply can finish its fast-path (GetConfig -> empty release
	// list -> defer Store(0)) before the second goroutine's Apply
	// runs on a slow CI runner, leaving both CAS calls to succeed and
	// the test to report "got 2/0".
	releases := []map[string]interface{}{}
	body, _ := json.Marshal(releases)
	httpBlock := make(chan struct{})
	var released sync.Once
	unblock := func() { released.Do(func() { close(httpBlock) }) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-httpBlock
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	defer unblock()
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

	// Release runApply so it can complete and drain its goroutine cleanly.
	unblock()
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
		if !strings.Contains(s, sub) {
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
