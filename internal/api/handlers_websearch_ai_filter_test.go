package api

// Tests for #2310: the per-user "Filter AI images" preference drops results
// hosted on blocklisted sites (Stillwater-side), defaults to filtering, and
// never changes what the provider is asked to do.
//
// The blocklist is the runtime-fetched aiblock store. Each test that needs a
// loaded list installs its own store fed from an httptest server (no real
// network) and restores the previous default in t.Cleanup. Tests that install
// a store touch a process-wide default, so they do not run in parallel.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/internal/provider/aiblock"
)

// installAIBlocklist installs an aiblock store as the process default. When
// loaded is true it is refreshed from an httptest server serving the aiblock
// test fixture; otherwise it stays in its initial not-loaded state.
func installAIBlocklist(t *testing.T, loaded bool) {
	t.Helper()
	installAIBlocklistOpts(t, loaded, false)
}

// installAIBlocklistOpts is installAIBlocklist with the store's Disabled flag.
func installAIBlocklistOpts(t *testing.T, loaded, disabled bool) {
	t.Helper()
	// Nothing in this package installs a default otherwise, so "restore the
	// previous default" is restoring none.
	t.Cleanup(func() { aiblock.SetDefault(nil) })

	body, err := os.ReadFile("../provider/aiblock/testdata/upstream_sample.txt")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	st := aiblock.NewStore(aiblock.Options{URL: srv.URL, Client: srv.Client(), Disabled: disabled})
	if loaded {
		if err := st.Refresh(context.Background()); err != nil {
			t.Fatalf("refreshing the test blocklist: %v", err)
		}
	}
	aiblock.SetDefault(st)
}

// countingWebProvider wraps the shared stub so a test can count searches.
type countingWebProvider struct {
	*stubWebImageProvider
	calls int
}

func (c *countingWebProvider) SearchImages(ctx context.Context, name string, it provider.ImageType) ([]provider.ImageResult, error) {
	c.calls++
	return c.stubWebImageProvider.SearchImages(ctx, name, it)
}

func aiFilterSearch(t *testing.T, r *Router, artistID, userID string, htmx bool) *httptest.ResponseRecorder {
	t.Helper()
	ctx := context.Background()
	if userID != "" {
		ctx = middleware.WithTestUserID(ctx, userID)
	}
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/artists/"+artistID+"/images/websearch?type=thumb", nil)
	req.SetPathValue("id", artistID)
	if htmx {
		req.Header.Set("HX-Request", "true")
	}
	w := httptest.NewRecorder()
	r.handleWebImageSearch(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", w.Code, w.Body.String())
	}
	return w
}

// aiFilterSearchValidated is the JSON path of aiFilterSearch routed through
// serveValidated, so the response (including the required ai_filter object)
// is checked against the OpenAPI spec.
func aiFilterSearchValidated(t *testing.T, r *Router, artistID, userID string) *httptest.ResponseRecorder {
	t.Helper()
	ctx := middleware.WithTestUserID(context.Background(), userID)
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/artists/"+artistID+"/images/websearch?type=thumb", nil)
	h := http.HandlerFunc(func(w http.ResponseWriter, rq *http.Request) {
		rq.SetPathValue("id", artistID)
		r.handleWebImageSearch(w, rq)
	})
	w := serveValidated(t, h, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", w.Code, w.Body.String())
	}
	return w
}

const (
	aiHostImage    = "https://images.nightcafe.studio/jobs/aGVsbG8/aGVsbG8.jpg"
	plainHostImage = "https://img.freepik.com/premium-vector/dragon-illustration_1234.jpg"
)

func aiFilterStub() *stubWebImageProvider {
	return &stubWebImageProvider{name: provider.NameDuckDuckGo, results: []provider.ImageResult{
		{URL: aiHostImage, Source: "duckduckgo"},
		{URL: plainHostImage, Source: "duckduckgo"},
	}}
}

func TestWebImageSearch_AIFilterDropsListedHosts(t *testing.T) {
	installAIBlocklist(t, true)
	r, svc := newImageHandlerTestServer(t)
	a := setUpWebSearchTest(t, r, svc, aiFilterStub())

	// Precondition: the installed list really blocks the AI host and not the
	// path-scoped freepik one, or the assertions below prove nothing.
	if !aiblock.Default().MatchURL(aiHostImage) || aiblock.Default().MatchURL(plainHostImage) {
		t.Fatal("fixture list does not separate the two test URLs")
	}

	// No stored preference: default is FILTER, for HTML and JSON callers.
	for _, htmx := range []bool{true, false} {
		body := aiFilterSearch(t, r, a.ID, "u-ai-filter", htmx).Body.String()
		if strings.Contains(body, "nightcafe.studio") {
			t.Errorf("htmx=%v, filter on: the nightcafe result must be dropped", htmx)
		}
		if !strings.Contains(body, "freepik.com/premium-vector") {
			t.Errorf("htmx=%v, filter on: the freepik premium-vector result must be kept", htmx)
		}
	}

	// Stored false: everything is kept.
	seedUserPref(t, r, "u-ai-filter", PrefFilterAIImages, "false")
	body := aiFilterSearch(t, r, a.ID, "u-ai-filter", false).Body.String()
	if !strings.Contains(body, "nightcafe.studio") || !strings.Contains(body, "freepik.com/premium-vector") {
		t.Error("filter off: both results must be kept")
	}

	// Another user and an anonymous caller still get the default (filtered).
	for _, uid := range []string{"someone-else", ""} {
		if strings.Contains(aiFilterSearch(t, r, a.ID, uid, false).Body.String(), "nightcafe.studio") {
			t.Errorf("user %q: default must filter", uid)
		}
	}
}

// Before any list has loaded the filter removes nothing, even with the
// preference on: results are returned unfiltered rather than dropped.
func TestWebImageSearch_AIFilterNotLoadedKeepsEverything(t *testing.T) {
	installAIBlocklist(t, false)
	r, svc := newImageHandlerTestServer(t)
	a := setUpWebSearchTest(t, r, svc, aiFilterStub())
	body := aiFilterSearch(t, r, a.ID, "u-not-loaded", false).Body.String()
	if !strings.Contains(body, "nightcafe.studio") || !strings.Contains(body, "freepik.com/premium-vector") {
		t.Error("list not loaded: both results must be returned unfiltered")
	}
}

// The filter must never change what a provider is asked to do: exactly one
// provider search per GET.
func TestWebImageSearch_AIFilterOneSearchPerRequest(t *testing.T) {
	installAIBlocklist(t, true)
	r, svc := newImageHandlerTestServer(t)
	stub := &countingWebProvider{stubWebImageProvider: aiFilterStub()}
	r.webSearchRegistry.Register(stub)
	if err := r.providerSettings.SetWebSearchEnabled(context.Background(), stub.name, true); err != nil {
		t.Fatal(err)
	}
	a := &artist.Artist{Name: "AI Filter Count Artist", SortName: "AI Filter Count Artist", Path: t.TempDir()}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	aiFilterSearch(t, r, a.ID, "u1", true)
	if stub.calls != 1 {
		t.Errorf("provider searches = %d, want 1", stub.calls)
	}
}

func TestPreferences_FilterAIImagesDefaultsTrue(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)
	ctx := middleware.WithTestUserID(context.Background(), "fresh-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/preferences/"+PrefFilterAIImages, nil)
	req.SetPathValue("key", PrefFilterAIImages)
	w := httptest.NewRecorder()
	r.handleGetPreference(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"true"`) {
		t.Errorf("GET %s for a user with nothing stored = %d %s, want value \"true\"", PrefFilterAIImages, w.Code, w.Body.String())
	}
}

// API-first (#2310 review F6): JSON callers get the filter outcome, so they
// can tell "filtered" from "no list loaded" from "download turned off".
func TestWebImageSearch_AIFilterJSONOutcome(t *testing.T) {
	type outcome struct {
		Applied    bool `json:"applied"`
		ListLoaded bool `json:"list_loaded"`
		Disabled   bool `json:"disabled"`
		Removed    int  `json:"removed"`
	}
	for _, tc := range []struct {
		name             string
		loaded, disabled bool
		prefOff          bool
		want             outcome
	}{
		{"loaded, on", true, false, false, outcome{Applied: true, ListLoaded: true, Removed: 1}},
		{"loaded, off", true, false, true, outcome{Applied: false, ListLoaded: true}},
		{"not loaded, on", false, false, false, outcome{Applied: true}},
		{"disabled, on", false, true, false, outcome{Applied: true, Disabled: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installAIBlocklistOpts(t, tc.loaded, tc.disabled)
			r, svc := newImageHandlerTestServer(t)
			a := setUpWebSearchTest(t, r, svc, aiFilterStub())
			if tc.prefOff {
				seedUserPref(t, r, "u-json", PrefFilterAIImages, "false")
			}
			var resp struct {
				AIFilter *outcome `json:"ai_filter"`
			}
			if err := json.Unmarshal(aiFilterSearchValidated(t, r, a.ID, "u-json").Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if resp.AIFilter == nil {
				t.Fatal("JSON response has no ai_filter object")
			}
			if *resp.AIFilter != tc.want {
				t.Errorf("ai_filter = %+v, want %+v", *resp.AIFilter, tc.want)
			}
		})
	}
}

// The results panel renders the switch (checked = filtering) only when
// DuckDuckGo is an enabled web search provider.
func TestWebImageSearch_AIFilterToggleRendering(t *testing.T) {
	installAIBlocklist(t, true)
	r, svc := newImageHandlerTestServer(t)
	a := setUpWebSearchTest(t, r, svc, aiFilterStub())

	body := aiFilterSearch(t, r, a.ID, "u-toggle", true).Body.String()
	if !strings.Contains(body, `id="sw-ai-filter-toggle"`) || !strings.Contains(body, `aria-checked="true"`) {
		t.Error("DuckDuckGo enabled, default pref: the switch must render checked")
	}
	seedUserPref(t, r, "u-toggle", PrefFilterAIImages, "false")
	if body := aiFilterSearch(t, r, a.ID, "u-toggle", true).Body.String(); !strings.Contains(body, `aria-checked="false"`) {
		t.Error("pref false: the switch must render unchecked")
	}

	if err := r.providerSettings.SetWebSearchEnabled(context.Background(), provider.NameDuckDuckGo, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(aiFilterSearch(t, r, a.ID, "u-toggle", true).Body.String(), "sw-ai-filter-toggle") {
		t.Error("the switch must not render when DuckDuckGo is not an enabled web search provider")
	}
}

// Filter on but no list loaded: the panel says the results are unfiltered
// instead of implying it filtered them. The notice is absent once a list has
// loaded, and absent when the filter is off (nothing was promised).
func TestWebImageSearch_AIFilterNotLoadedNotice(t *testing.T) {
	// Match the rendered element (attribute plus its role), not the bare
	// attribute name, which the switch's inline script also mentions in a
	// selector.
	const notice, offNotice = `data-sw-ai-filter-not-loaded role="status"`, `data-sw-ai-filter-disabled role="status"`
	for _, tc := range []struct {
		name          string
		loaded        bool
		disabled      bool
		prefOff       bool
		wantNotice    bool
		wantOffNotice bool
	}{
		{"filter on, list not loaded", false, false, false, true, false},
		{"filter on, list loaded", true, false, false, false, false},
		{"filter off, list not loaded", false, false, true, false, false},
		{"filter on, download turned off", false, true, false, false, true},
		{"filter off, download turned off", false, true, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installAIBlocklistOpts(t, tc.loaded, tc.disabled)
			r, svc := newImageHandlerTestServer(t)
			a := setUpWebSearchTest(t, r, svc, aiFilterStub())
			if tc.prefOff {
				seedUserPref(t, r, "u-notice", PrefFilterAIImages, "false")
			}
			body := aiFilterSearch(t, r, a.ID, "u-notice", true).Body.String()
			if got := strings.Contains(body, notice); got != tc.wantNotice {
				t.Errorf("not-loaded notice present = %v, want %v", got, tc.wantNotice)
			}
			if got := strings.Contains(body, offNotice); got != tc.wantOffNotice {
				t.Errorf("turned-off notice present = %v, want %v", got, tc.wantOffNotice)
			}
			if (tc.wantNotice || tc.wantOffNotice) && !strings.Contains(body, "nightcafe.studio") {
				t.Error("not loaded: the results must be shown unfiltered")
			}
		})
	}
}

// filteringWebProvider is a stub FilteringWebImageProvider (#3309). It records
// which entry point the handler used and applies the handler-supplied filter
// to its own batch, as the DuckDuckGo adapter does per page.
type filteringWebProvider struct {
	*stubWebImageProvider
	plainCalls, filteredCalls int
	batch                     []provider.ImageResult
}

func (f *filteringWebProvider) SearchImages(ctx context.Context, name string, it provider.ImageType) ([]provider.ImageResult, error) {
	f.plainCalls++
	return f.batch, nil
}

func (f *filteringWebProvider) SearchImagesFiltered(_ context.Context, _ string, _ provider.ImageType, keep provider.ImageFilter) ([]provider.ImageResult, int, error) {
	f.filteredCalls++
	kept, removed := keep(f.batch)
	return kept, removed, nil
}

func TestWebImageSearch_FilteringProviderCountsRemovalsOnce(t *testing.T) {
	installAIBlocklist(t, true)
	batch := []provider.ImageResult{
		{URL: aiHostImage, Source: "duckduckgo"},
		{URL: plainHostImage, Source: "duckduckgo"},
		{URL: aiHostImage + "?2", Source: "duckduckgo"},
	}
	// PRECONDITION: two of the three really are blocked.
	if !aiblock.Default().MatchURL(batch[0].URL) || !aiblock.Default().MatchURL(batch[2].URL) {
		t.Fatal("fixture list does not block the test URLs")
	}
	removed := func(w *httptest.ResponseRecorder) int {
		var resp struct {
			Images   []provider.ImageResult `json:"images"`
			AIFilter struct {
				Removed int `json:"removed"`
			} `json:"ai_filter"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if len(resp.Images) != 1 {
			t.Errorf("images = %d, want 1 survivor", len(resp.Images))
		}
		return resp.AIFilter.Removed
	}

	r, svc := newImageHandlerTestServer(t)
	stub := &filteringWebProvider{stubWebImageProvider: aiFilterStub(), batch: batch}
	r.webSearchRegistry.Register(stub)
	if err := r.providerSettings.SetWebSearchEnabled(context.Background(), stub.name, true); err != nil {
		t.Fatal(err)
	}
	a := &artist.Artist{Name: "Filtering Provider Artist", SortName: "Filtering Provider Artist", Path: t.TempDir()}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatal(err)
	}

	// Filter on: the provider is asked to filter, removals counted exactly once
	// (the post-loop pass over already-filtered results removes nothing more).
	if got := removed(aiFilterSearch(t, r, a.ID, "u-fp", false)); got != 2 {
		t.Errorf("ai_filter.removed = %d, want 2", got)
	}
	if stub.filteredCalls != 1 || stub.plainCalls != 0 {
		t.Errorf("filtered/plain calls = %d/%d, want 1/0", stub.filteredCalls, stub.plainCalls)
	}

	// Filter off: the plain entry point, nothing removed.
	seedUserPref(t, r, "u-fp", PrefFilterAIImages, "false")
	w := aiFilterSearch(t, r, a.ID, "u-fp", false)
	if stub.filteredCalls != 1 || stub.plainCalls != 1 {
		t.Errorf("filter off: filtered/plain calls = %d/%d, want 1/1", stub.filteredCalls, stub.plainCalls)
	}
	var resp struct {
		Images   []provider.ImageResult `json:"images"`
		AIFilter struct {
			Removed int `json:"removed"`
		} `json:"ai_filter"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Images) != 3 || resp.AIFilter.Removed != 0 {
		t.Errorf("filter off: images/removed = %d/%d, want 3/0", len(resp.Images), resp.AIFilter.Removed)
	}
}
