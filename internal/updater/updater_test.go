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
// cases where either side may be a nightly tag. The cross-kind rule is
// asymmetric: advertising an update pill only makes sense when moving
// forward (stable -> nightly opt-in), not when the selected channel's
// latest is older than the nightly build the user is already running.
func TestNewerThan(t *testing.T) {
	svc := buildTestService(t)
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

		// Cross-kind: advertising only makes sense when moving FROM semver
		// TO nightly (opt-in). The reverse direction (running nightly,
		// picked stable/prerelease) must not produce a pill because the
		// semver release is almost certainly older than the nightly.
		{"nightly-20260422", "v0.9.5", true},       // stable -> nightly opt-in
		{"v1.0.0", "nightly-20260422", false},      // nightly -> stable (no pill)
		{"v0.9.6-rc.2", "nightly-20260422", false}, // nightly -> prerelease (no pill)
	}

	for _, tc := range cases {
		got := svc.newerThan(tc.candidate, tc.current)
		if got != tc.want {
			t.Errorf("newerThan(%q, %q) = %v, want %v", tc.candidate, tc.current, got, tc.want)
		}
	}
}

// TestNewerThanLogsParseFailure pins that when both inputs are non-nightly
// and either fails parseSemver, the service logs a Warn. The function itself
// returns false (conservative "not newer"), but the operator-visible log is
// the forensic breadcrumb that lets debugging "updater says up-to-date but a
// new release exists" actually start somewhere.
func TestNewerThanLogsParseFailure(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	svc := buildTestService(t)
	svc.logger = logger

	got := svc.newerThan("not-a-version", "also-not-a-version")
	if got {
		t.Errorf("newerThan on malformed pair = true, want false")
	}
	out := buf.String()
	if !strings.Contains(out, "semver parse failed") {
		t.Errorf("expected Warn log mentioning \"semver parse failed\", got:\n%s", out)
	}
	if !strings.Contains(out, "not-a-version") {
		t.Errorf("expected log to include the candidate string, got:\n%s", out)
	}
}

// TestCheckWithMockGitHub_Nightly exercises Check end-to-end on the nightly
// channel. Each link (pickLatest, newerThan, GetConfig, storeCheckResult) is
// covered in isolation elsewhere; this test wires the full chain so a
// refactor that breaks only the nightly path fails here rather than after a
// release that should have surfaced never does.
func TestCheckWithMockGitHub_Nightly(t *testing.T) {
	svc := buildTestService(t)
	ctx := context.Background()

	if err := svc.SetConfig(ctx, Config{Channel: ChannelNightly}); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}

	releases := []githubRelease{
		{TagName: "v0.9.5", HTMLURL: "https://example.com/v0.9.5"},
		{TagName: "v0.9.6-rc.1", Prerelease: true, HTMLURL: "https://example.com/rc"},
		{TagName: "nightly-20260420", Prerelease: true, HTMLURL: "https://example.com/n20"},
		{TagName: "nightly-20260422", Prerelease: true, HTMLURL: "https://example.com/n22"},
	}
	body, err := json.Marshal(releases)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	svc.httpClient = &http.Client{Transport: &rewriteHostTransport{base: srv.URL}}

	result, err := svc.Check(ctx)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.Latest != "nightly-20260422" {
		t.Errorf("Latest = %q, want nightly-20260422", result.Latest)
	}
	if result.Channel != ChannelNightly {
		t.Errorf("Channel = %q, want %q", result.Channel, ChannelNightly)
	}
	// version.Version at test time defaults to "0.9.6-rc.2" (a valid semver).
	// newerThan: candidate is nightly, current is semver -> cross-kind opt-in
	// branch returns true. Locking this here prevents a future refactor from
	// silently suppressing the stable->nightly pill.
	if !result.UpdateAvailable {
		t.Error("UpdateAvailable = false, want true (stable-semver current + nightly candidate)")
	}
}

// TestCheckLogsWhenAllReleasesFiltered pins the pickLatest no-match
// diagnostic: when GitHub returns releases but none pass the channel filter,
// Check logs a Warn with the fetched count and a sample of rejected tag
// names. Without this, "releases exist but updater reports up-to-date" is
// indistinguishable in operator logs from "channel is genuinely empty".
func TestCheckLogsWhenAllReleasesFiltered(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	svc := buildTestService(t)
	svc.logger = logger
	ctx := context.Background()

	if err := svc.SetConfig(ctx, Config{Channel: ChannelNightly}); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}

	// All non-nightly tags on the nightly channel: pickLatest returns nil.
	releases := []githubRelease{
		{TagName: "v0.9.5", HTMLURL: "https://example.com/a"},
		{TagName: "v0.9.6-rc.1", Prerelease: true, HTMLURL: "https://example.com/b"},
	}
	body, err := json.Marshal(releases)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	svc.httpClient = &http.Client{Transport: &rewriteHostTransport{base: srv.URL}}

	result, err := svc.Check(ctx)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.Latest != "" {
		t.Errorf("Latest = %q, want empty string (no match)", result.Latest)
	}
	out := buf.String()
	if !strings.Contains(out, "no release matched channel filter") {
		t.Errorf("expected Warn about unmatched channel filter, got:\n%s", out)
	}
	if !strings.Contains(out, "v0.9.5") {
		t.Errorf("expected log to include fetched tag sample, got:\n%s", out)
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
	// Pre-condition for the restart-required flag: a fresh Service must NOT
	// advertise restart_required, otherwise the UI would render the
	// "restart to finish" banner on first load before any Apply has run.
	if st.RestartRequired {
		t.Error("initial RestartRequired = true, want false")
	}
	if st.PendingVersion != "" {
		t.Errorf("initial PendingVersion = %q, want empty", st.PendingVersion)
	}
}

// TestMarkRestartRequiredForTestExposesInternalTransition verifies that the
// exported helper used by cross-package tests in internal/api delegates to
// the same internal markRestartRequired path. Without this, the helper
// could drift to a no-op (or set only one of the two fields) and the api
// package's TestHandleGetUpdateStatus_RestartRequiredSurfaced would
// silently regress to passing for the wrong reason.
func TestMarkRestartRequiredForTestExposesInternalTransition(t *testing.T) {
	svc := buildTestService(t)

	svc.MarkRestartRequiredForTest("v1.0.0")

	st := svc.Status()
	if !st.RestartRequired || st.PendingVersion != "v1.0.0" {
		t.Errorf("MarkRestartRequiredForTest did not delegate properly; status = %+v", st)
	}
}

// TestMarkRestartRequiredSetsStickyFlag verifies that markRestartRequired
// sets both fields atomically and that they survive subsequent Status reads.
// This is the load-bearing post-Apply UI signal for issue #1169: without
// it the Updates tab cannot distinguish "Apply succeeded, please restart"
// from "Apply did nothing" (both previously left state=idle with no flag).
func TestMarkRestartRequiredSetsStickyFlag(t *testing.T) {
	svc := buildTestService(t)

	svc.markRestartRequired("v1.2.3")

	st := svc.Status()
	if !st.RestartRequired {
		t.Fatal("RestartRequired = false, want true after markRestartRequired")
	}
	if st.PendingVersion != "v1.2.3" {
		t.Errorf("PendingVersion = %q, want v1.2.3", st.PendingVersion)
	}

	// Stickiness: a follow-up Check (or any state mutation that does not
	// itself touch the flag) must NOT clear restartRequired. The UI banner
	// has to survive a tab re-open or a sidebar pill refresh, both of
	// which call /status again. Simulate a state transition to verify
	// nothing in setState wipes the flag.
	svc.setState(StateChecking, 0, "")
	svc.setState(StateIdle, 0, "")
	st = svc.Status()
	if !st.RestartRequired {
		t.Error("RestartRequired = false after setState cycle, want true (must be sticky)")
	}
	if st.PendingVersion != "v1.2.3" {
		t.Errorf("PendingVersion = %q after setState cycle, want v1.2.3 (must be sticky)", st.PendingVersion)
	}
}

// TestRestartRequiredNotSetOnRunApplyEarlyExit covers the early-return
// branches of runApply (no update, fetch failure, checksum failure, etc.):
// none of them should mark restart-required, because the binary on disk
// was NOT replaced. This is the negative complement to the
// markRestartRequired sticky-flag test: if a refactor accidentally moved
// markRestartRequired earlier in runApply, this test catches it by
// failing on the no-update branch where no swap actually happened.
func TestRestartRequiredNotSetOnRunApplyEarlyExit(t *testing.T) {
	svc := buildTestService(t)
	ctx := context.Background()

	// Empty release list -> pickLatest returns nil -> runApply exits
	// through the "no update needed" branch without calling
	// markRestartRequired. atomicReplaceFile is never reached.
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
	waitForIdle(t, svc)

	st := svc.Status()
	if st.RestartRequired {
		t.Error("RestartRequired = true after no-update Apply; expected false (no swap happened)")
	}
	if st.PendingVersion != "" {
		t.Errorf("PendingVersion = %q after no-update Apply, want empty", st.PendingVersion)
	}
}

// TestRunApplySuccessSetsRestartRequired drives runApply through its full
// success path -- download, checksum verification, extract, atomic replace,
// markRestartRequired, idle 100% -- so the post-Apply UI surface is locked
// against regressions and the Wave-3 patch-coverage gap on this path is
// closed.
//
// The test stubs `executablePath` (a package var indirecting os.Executable)
// to point at a temp file under t.TempDir() so atomicReplaceFile rewrites
// the temp file rather than the test runner binary. Every other step
// (release fetch, asset download, checksum file, sha256 verify, tar.gz
// extract) runs the production code path against an httptest server.
func TestRunApplySuccessSetsRestartRequired(t *testing.T) {
	svc := buildTestService(t)
	ctx := context.Background()

	// Swap the executable-path resolver to a real temp file we own.
	// atomicReplaceFile statics the target's mode, so the file must
	// exist before runApply reaches it.
	tmpBin := filepath.Join(t.TempDir(), "stillwater")
	if err := os.WriteFile(tmpBin, []byte("old binary content"), 0o755); err != nil {
		t.Fatalf("seeding tmp binary: %v", err)
	}
	prevExec := executablePath
	executablePath = func() (string, error) { return tmpBin, nil }
	t.Cleanup(func() { executablePath = prevExec })

	// Build the tarball whose extraction will produce the new binary
	// content. Asset name must match binaryAssetName(tagName) so runApply
	// picks it up from the release manifest.
	const tagName = "v999.0.0"
	const newBinaryContent = "new binary content"
	var tarBuf bytes.Buffer
	gw := gzip.NewWriter(&tarBuf)
	tw := tar.NewWriter(gw)
	hdr := &tar.Header{Name: "stillwater", Mode: 0o755, Size: int64(len(newBinaryContent))}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	if _, err := tw.Write([]byte(newBinaryContent)); err != nil {
		t.Fatalf("tar write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	tarBytes := tarBuf.Bytes()
	binAsset := binaryAssetName(tagName)
	checksumAsset := checksumAssetName(tagName)
	checksumLine := sha256Hex(tarBytes) + "  " + binAsset + "\n"

	// Mock GitHub: the releases endpoint returns one release whose Assets
	// point back at this same server's /asset/<name> paths. The asset
	// endpoint serves the tarball or checksum file depending on the path.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/asset/"+binAsset):
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = w.Write(tarBytes)
		case strings.HasSuffix(r.URL.Path, "/asset/"+checksumAsset):
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte(checksumLine))
		default:
			// Releases manifest for any path that isn't an asset URL.
			rels := []githubRelease{{
				TagName:     tagName,
				Prerelease:  false,
				Draft:       false,
				HTMLURL:     "https://example.invalid/releases/" + tagName,
				PublishedAt: "2026-01-01T00:00:00Z",
				Assets: []githubAsset{
					// downloadBytes requires https://; rewriteHostTransport
					// rewrites scheme+host at request time so the actual
					// fetch lands on the http httptest server.
					{Name: binAsset, BrowserDownloadURL: "https://api.github.com/asset/" + binAsset},
					{Name: checksumAsset, BrowserDownloadURL: "https://api.github.com/asset/" + checksumAsset},
				},
			}}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(rels)
		}
	}))
	defer srv.Close()
	svc.SetHTTPClient(&http.Client{Transport: &rewriteHostTransport{base: srv.URL}})

	if err := svc.Apply(ctx); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// Apply() spawns runApply in a goroutine. Polling for an intermediate
	// state (StateChecking) is racy on fast machines: the success path
	// goes idle -> checking -> downloading -> applying -> idle, and the
	// whole sequence can complete inside a single 2ms poll gap, which
	// makes a transition-watcher that compares against StateIdle never
	// observe a non-idle reading. Poll the monotonic post-condition
	// instead: RestartRequired flips false -> true exactly once and never
	// flips back, so a sufficiently long deadline cannot miss it.
	waitForRestartRequired(t, svc)
	// runApply calls markRestartRequired before its final
	// setState(StateIdle, 100), so the State / Progress assertions below
	// would otherwise race the goroutine's last setState. waitForIdle
	// observes applyRunning return to 0 plus the terminal State, which
	// guarantees runApply has fully exited.
	waitForIdle(t, svc)

	st := svc.Status()
	if st.State != StateIdle {
		t.Fatalf("state = %q, want idle (success path); status = %+v", st.State, st)
	}
	if !st.RestartRequired {
		t.Errorf("RestartRequired = false after successful Apply; want true")
	}
	if st.PendingVersion != tagName {
		t.Errorf("PendingVersion = %q, want %q", st.PendingVersion, tagName)
	}
	if st.Progress != 100 {
		t.Errorf("Progress = %d, want 100", st.Progress)
	}

	// The temp binary file must have been overwritten with the new content,
	// proving atomicReplaceFile actually ran end-to-end (not just the
	// markRestartRequired bookkeeping).
	got, err := os.ReadFile(tmpBin)
	if err != nil {
		t.Fatalf("reading tmp binary after Apply: %v", err)
	}
	if string(got) != newBinaryContent {
		t.Errorf("tmp binary content = %q, want %q", string(got), newBinaryContent)
	}
}

// waitForRestartRequired polls Status().RestartRequired until it flips to
// true. This is race-free on fast machines: markRestartRequired sets the
// field exactly once and runApply never resets it, so a poll loop cannot
// miss the transition the way an intermediate-State watcher can miss
// State values when the whole runApply pipeline completes inside a
// single sleep gap.
func waitForRestartRequired(t *testing.T, svc *Service) {
	t.Helper()
	deadline := time.Now().Add(waitForIdleTimeout)
	for time.Now().Before(deadline) {
		if svc.Status().RestartRequired {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("service did not set RestartRequired within %s; final status = %+v",
		waitForIdleTimeout, svc.Status())
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

// TestApplyRestartRequired verifies that Apply short-circuits with
// ErrRestartRequired once a prior apply has staged a binary, and that it
// does so without flipping applyRunning (so the caller can keep polling
// Status without seeing a phantom "running" state).
func TestApplyRestartRequired(t *testing.T) {
	svc := buildTestService(t)
	svc.MarkRestartRequiredForTest("v1.2.3")

	err := svc.Apply(context.Background())
	if !errors.Is(err, ErrRestartRequired) {
		t.Fatalf("expected ErrRestartRequired, got %v", err)
	}
	if got := svc.applyRunning.Load(); got != 0 {
		t.Errorf("applyRunning leaked to %d after refused Apply; expected 0", got)
	}
}

// TestApplyConcurrentRace verifies that exactly one of two concurrent Apply
// calls succeeds and the other returns ErrAlreadyRunning. Run with -race.
//
// Two synchronization mechanisms cooperate to make the test deterministic:
//
//  1. httpBlock keeps the winning goroutine's runApply pinned inside
//     fetchReleases, so its `defer Store(0)` cannot fire until the test
//     releases. This holds applyRunning=1 across both Apply calls and is
//     what prevents the historical "got 2/0" flake (#1162) where a fast
//     runApply finished its empty-release fast-path before the loser's CAS.
//
//  2. httpStarted is closed by the HTTP handler the first time it is hit
//     and is asserted at the end of the test. If httpBlock somehow fails to
//     engage (e.g. a future refactor reorders runApply so HTTP is no longer
//     reached on the empty-release path), httpStarted will be open and the
//     test fails loudly instead of passing for the wrong reason.
func TestApplyConcurrentRace(t *testing.T) {
	svc := buildTestService(t)
	ctx := context.Background()

	releases := []map[string]interface{}{}
	body, _ := json.Marshal(releases)
	httpBlock := make(chan struct{})
	httpStarted := make(chan struct{})
	var (
		released  sync.Once
		startedOK sync.Once
	)
	unblock := func() { released.Do(func() { close(httpBlock) }) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		startedOK.Do(func() { close(httpStarted) })
		<-httpBlock
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(unblock)
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

	// Sanity-check the test setup itself: confirm runApply actually hit
	// the HTTP block within a generous deadline. If it didn't, the
	// nils/blocked counts above were validated under unintended timing
	// rather than the documented httpBlock pin, and the test would be
	// flaky again the moment the runtime got faster or slower.
	select {
	case <-httpStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("httpBlock never engaged: runApply did not reach fetchReleases within 5s, " +
			"so the assertion above did not exercise the documented serialization path")
	}

	// Release runApply so it can complete and drain its goroutine cleanly.
	unblock()
	waitForIdle(t, svc)
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
	waitForIdle(t, svc)
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
	waitForIdle(t, svc)
}

// --- helpers ---

// waitForIdleTimeout is the deadline waitForIdle imposes on async runApply
// drains. Pulled out as a const because every existing caller used the same
// value; if a future test genuinely needs a different timeout, reintroduce
// the parameter rather than scaling this constant up across the suite.
const waitForIdleTimeout = 5 * time.Second

// waitForIdle polls Status() until the service reaches StateIdle or StateError,
// failing the test if neither is reached within waitForIdleTimeout. It is used
// by tests that exercise the async runApply goroutine.
//
// A naive "return as soon as State == Idle" would pass instantly: NewService
// initializes the service at StateIdle, and Apply() returns to the caller
// before the spawned runApply goroutine has had a chance to call setState.
// Apply()'s CompareAndSwap flips applyRunning 0 -> 1 pre-spawn, and runApply's
// `defer Store(0)` clears it on exit, so observing applyRunning back at 0
// after Apply() returned nil is itself proof that the goroutine ran to
// completion. We accept an idle/error State only once that flag has cleared,
// which also handles the fast no-update / old-release paths where runApply
// may finish before the first poll.
func waitForIdle(t *testing.T, svc *Service) {
	t.Helper()
	deadline := time.Now().Add(waitForIdleTimeout)
	for time.Now().Before(deadline) {
		st := svc.Status()
		if svc.applyRunning.Load() == 0 &&
			(st.State == StateIdle || st.State == StateError) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("service did not reach idle/error within %s; applyRunning=%d state=%q",
		waitForIdleTimeout, svc.applyRunning.Load(), svc.Status().State)
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

// TestGetConfigDefaultsForNewFields covers the defaults exposed by GetConfig
// when the new keys (#1117) are absent from the settings table. A fresh
// install must look "enabled, manual-check, 24h cadence" so existing users
// are not silently muted by a new field.
func TestGetConfigDefaultsForNewFields(t *testing.T) {
	svc := buildTestService(t)
	cfg, err := svc.GetConfig(context.Background())
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	if !cfg.Enabled {
		t.Error("default Enabled should be true")
	}
	if cfg.AutoUpdate {
		t.Error("default AutoUpdate should be false")
	}
	if cfg.CheckIntervalHours != DefaultCheckIntervalHours {
		t.Errorf("default CheckIntervalHours = %d, want %d",
			cfg.CheckIntervalHours, DefaultCheckIntervalHours)
	}
}

// TestSetConfigPersistsAllFields covers the round-trip of every new knob:
// Enabled, AutoUpdate, and CheckIntervalHours must survive a SetConfig +
// GetConfig pair so the UI can rely on values it just saved being read back.
func TestSetConfigPersistsAllFields(t *testing.T) {
	svc := buildTestService(t)
	ctx := context.Background()
	want := Config{
		Channel:            ChannelPrerelease,
		Enabled:            false,
		AutoCheck:          true,
		AutoUpdate:         true,
		CheckIntervalHours: 6,
	}
	if err := svc.SetConfig(ctx, want); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	got, err := svc.GetConfig(ctx)
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	if got.Channel != want.Channel {
		t.Errorf("Channel = %q, want %q", got.Channel, want.Channel)
	}
	if got.Enabled != want.Enabled {
		t.Errorf("Enabled = %v, want %v", got.Enabled, want.Enabled)
	}
	if got.AutoCheck != want.AutoCheck {
		t.Errorf("AutoCheck = %v, want %v", got.AutoCheck, want.AutoCheck)
	}
	if got.AutoUpdate != want.AutoUpdate {
		t.Errorf("AutoUpdate = %v, want %v", got.AutoUpdate, want.AutoUpdate)
	}
	if got.CheckIntervalHours != want.CheckIntervalHours {
		t.Errorf("CheckIntervalHours = %d, want %d",
			got.CheckIntervalHours, want.CheckIntervalHours)
	}
}

// TestSetConfigCoercesZeroInterval verifies that a zero CheckIntervalHours
// is silently coerced to the default rather than rejected. Older clients and
// the API handler test fixtures predate this field; coercion keeps them
// from breaking.
func TestSetConfigCoercesZeroInterval(t *testing.T) {
	svc := buildTestService(t)
	ctx := context.Background()
	if err := svc.SetConfig(ctx, Config{Channel: ChannelStable, CheckIntervalHours: 0}); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	got, err := svc.GetConfig(ctx)
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	if got.CheckIntervalHours != DefaultCheckIntervalHours {
		t.Errorf("CheckIntervalHours = %d, want default %d",
			got.CheckIntervalHours, DefaultCheckIntervalHours)
	}
}

// TestSetConfigRejectsNegativeInterval verifies the error path so an explicit
// garbage write (negative hours) is not silently coerced.
func TestSetConfigRejectsNegativeInterval(t *testing.T) {
	svc := buildTestService(t)
	err := svc.SetConfig(context.Background(), Config{Channel: ChannelStable, CheckIntervalHours: -1})
	if err == nil {
		t.Fatal("expected error for negative CheckIntervalHours")
	}
}

// TestStartSchedulerStopsOnContextCancel verifies the scheduler exits
// promptly when its context is canceled. Without a working stop path the
// goroutine would leak across process shutdowns.
func TestStartSchedulerStopsOnContextCancel(t *testing.T) {
	svc := buildTestService(t)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		svc.StartScheduler(ctx)
		close(done)
	}()

	// Give the scheduler a moment to enter the select{} loop, then cancel.
	time.Sleep(10 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// Good: returned after cancel.
	case <-time.After(2 * time.Second):
		t.Fatal("StartScheduler did not return after context cancel")
	}
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
