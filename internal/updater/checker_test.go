package updater

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// fakeRelease builds a minimal GitHub release JSON for testing.
func fakeRelease(tag string, prerelease bool, body string) githubRelease {
	return githubRelease{
		TagName:     tag,
		Prerelease:  prerelease,
		Draft:       false,
		HTMLURL:     "https://github.com/example/repo/releases/tag/" + tag,
		PublishedAt: time.Now(),
		Body:        body,
		Assets: []githubAsset{
			{Name: "stillwater_linux_amd64", BrowserDownloadURL: "https://example.com/dl/" + tag + "_linux_amd64", Size: 1000},
			{Name: "stillwater_linux_amd64.sha256", BrowserDownloadURL: "https://example.com/dl/" + tag + "_linux_amd64.sha256", Size: 64},
		},
	}
}

func testCheckerWithServer(t *testing.T, releases []githubRelease, ch Channel) (*Checker, *httptest.Server) {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(releases); err != nil {
			t.Errorf("encoding releases: %v", err)
		}
	}))

	v, err := Parse("1.0.0")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	checker := NewChecker("example/repo", ch, v, srv.Client(), nil)
	// Override the internal URL by monkey-patching the repo field to point at
	// the test server. We need to replace the fetch method for testing.
	// Instead, we create a custom checker with a transport that redirects
	// api.github.com requests to the test server.
	checker.client = &http.Client{
		Transport: &redirectTransport{base: srv.URL, inner: srv.Client().Transport},
	}
	checker.repo = ""
	checker.client = &http.Client{
		Timeout:   5 * time.Second,
		Transport: &staticReleasesTransport{releases: releases},
	}

	return checker, srv
}

// staticReleasesTransport is an http.RoundTripper that returns a fixed list of
// GitHub release objects regardless of the request URL.
type staticReleasesTransport struct {
	releases []githubRelease
}

func (t *staticReleasesTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body, err := json.Marshal(t.releases)
	if err != nil {
		return nil, err
	}
	rec := httptest.NewRecorder()
	rec.WriteHeader(http.StatusOK)
	rec.Header().Set("Content-Type", "application/json")
	_, _ = rec.Body.Write(body)
	return rec.Result(), nil
}

// redirectTransport is unused; kept to avoid an unused import.
type redirectTransport struct {
	base  string
	inner http.RoundTripper
}

func (t *redirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.inner.RoundTrip(req)
}

func TestChecker_UpdateAvailable(t *testing.T) {
	releases := []githubRelease{
		fakeRelease("v1.1.0", false, "New features"),
		fakeRelease("v1.0.0", false, "Initial"),
	}
	checker, srv := testCheckerWithServer(t, releases, ChannelLatest)
	defer srv.Close()

	info, err := checker.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !info.Available {
		t.Error("expected update available")
	}
	if info.Latest != "1.1.0" {
		t.Errorf("Latest = %q, want %q", info.Latest, "1.1.0")
	}
	if info.Current != "1.0.0" {
		t.Errorf("Current = %q, want %q", info.Current, "1.0.0")
	}
}

func TestChecker_NoUpdate(t *testing.T) {
	releases := []githubRelease{
		fakeRelease("v1.0.0", false, ""),
	}
	checker, srv := testCheckerWithServer(t, releases, ChannelLatest)
	defer srv.Close()

	info, err := checker.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if info.Available {
		t.Error("expected no update available")
	}
}

func TestChecker_ChannelLatest_SkipsPrerelease(t *testing.T) {
	releases := []githubRelease{
		fakeRelease("v1.1.0-beta.1", true, ""),
		fakeRelease("v1.0.0", false, ""),
	}
	checker, srv := testCheckerWithServer(t, releases, ChannelLatest)
	defer srv.Close()

	info, err := checker.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	// Latest stable is 1.0.0 which is the current version, so no update.
	if info.Available {
		t.Errorf("expected no update for latest channel, got Latest=%q", info.Latest)
	}
}

func TestChecker_ChannelBeta_IncludesBeta(t *testing.T) {
	releases := []githubRelease{
		fakeRelease("v1.1.0-beta.1", true, ""),
		fakeRelease("v1.0.0", false, ""),
	}
	checker, srv := testCheckerWithServer(t, releases, ChannelBeta)
	defer srv.Close()

	info, err := checker.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !info.Available {
		t.Error("expected beta update to be available on beta channel")
	}
	if info.Latest != "1.1.0-beta.1" {
		t.Errorf("Latest = %q, want %q", info.Latest, "1.1.0-beta.1")
	}
}

func TestChecker_ChannelBeta_SkipsDevRelease(t *testing.T) {
	releases := []githubRelease{
		fakeRelease("v1.1.0-nightly.20260101", true, ""),
		fakeRelease("v1.0.0", false, ""),
	}
	checker, srv := testCheckerWithServer(t, releases, ChannelBeta)
	defer srv.Close()

	info, err := checker.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	// nightly is not beta or rc, so beta channel should skip it.
	if info.Available {
		t.Errorf("beta channel should skip nightly tag, got Latest=%q", info.Latest)
	}
}

func TestChecker_ChannelDev_IncludesAll(t *testing.T) {
	releases := []githubRelease{
		fakeRelease("v1.1.0-nightly.20260101", true, ""),
	}
	checker, srv := testCheckerWithServer(t, releases, ChannelDev)
	defer srv.Close()

	info, err := checker.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !info.Available {
		t.Error("expected nightly to be available on dev channel")
	}
}

func TestChecker_SkipsDraft(t *testing.T) {
	draft := fakeRelease("v1.1.0", false, "")
	draft.Draft = true
	releases := []githubRelease{draft, fakeRelease("v1.0.0", false, "")}
	checker, srv := testCheckerWithServer(t, releases, ChannelLatest)
	defer srv.Close()

	info, err := checker.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if info.Available {
		t.Error("expected draft release to be skipped")
	}
}

func TestChecker_CachedResult(t *testing.T) {
	releases := []githubRelease{fakeRelease("v1.1.0", false, "")}
	checker, srv := testCheckerWithServer(t, releases, ChannelLatest)
	defer srv.Close()

	cached, ts := checker.CachedResult()
	if cached != nil {
		t.Error("expected nil cached result before first check")
	}
	if !ts.IsZero() {
		t.Error("expected zero time before first check")
	}

	info, err := checker.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}

	cached, ts = checker.CachedResult()
	if cached == nil {
		t.Fatal("expected cached result after check")
	}
	if cached.Available != info.Available {
		t.Errorf("cached Available = %v, want %v", cached.Available, info.Available)
	}
	if ts.IsZero() {
		t.Error("expected non-zero checked_at time")
	}
}

func TestChecker_SetChannel(t *testing.T) {
	releases := []githubRelease{fakeRelease("v1.0.0", false, "")}
	checker, srv := testCheckerWithServer(t, releases, ChannelLatest)
	defer srv.Close()

	// Populate cache and verify it was set.
	if _, err := checker.Check(context.Background()); err != nil {
		t.Fatalf("Check: %v", err)
	}
	cached, ts1 := checker.CachedResult()
	if cached == nil || ts1.IsZero() {
		t.Fatal("expected cache to be populated after Check")
	}

	// Changing channel invalidates cache.
	checker.SetChannel(ChannelBeta)
	_, ts2 := checker.CachedResult()
	if !ts2.IsZero() {
		t.Error("expected cache to be cleared after SetChannel")
	}
	_ = ts1 // used in assertion above
}

func TestMapAssets(t *testing.T) {
	assets := []githubAsset{
		{Name: "stillwater_linux_amd64", BrowserDownloadURL: "https://example.com/dl", Size: 1000},
		{Name: "stillwater_linux_amd64.sha256", BrowserDownloadURL: "https://example.com/dl.sha256", Size: 64},
	}
	result := mapAssets(assets)
	if len(result) != 1 {
		t.Fatalf("len(mapAssets) = %d, want 1", len(result))
	}
	a := result[0]
	if a.Name != "stillwater_linux_amd64" {
		t.Errorf("Name = %q", a.Name)
	}
	if a.ChecksumURL != "https://example.com/dl.sha256" {
		t.Errorf("ChecksumURL = %q, want checksum URL", a.ChecksumURL)
	}
}
