package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/auth"
	"github.com/sydlexius/stillwater/internal/connection"
)

func TestHandleArtistsBadge_ZeroCount(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/badge", nil)
	w := httptest.NewRecorder()
	r.handleArtistsBadge(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if body := w.Body.String(); body != "" {
		t.Errorf("body = %q, want empty for zero count", body)
	}
}

func TestHandleArtistsBadge_NonZeroCount(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	if err := artistSvc.Create(context.Background(), &artist.Artist{Name: "Badge Artist"}); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/badge", nil)
	w := httptest.NewRecorder()
	r.handleArtistsBadge(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if body := w.Body.String(); !strings.Contains(body, "1") {
		t.Errorf("body = %q, want span containing count", body)
	}
}

func TestHandleArtistsBadge_ServiceError(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)
	// Close the DB to force a service error. testRouter's t.Cleanup will
	// attempt a second close; that error is intentionally ignored there.
	if err := r.db.Close(); err != nil {
		t.Fatalf("closing db: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/badge", nil)
	w := httptest.NewRecorder()
	r.handleArtistsBadge(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
}

func TestArtistsPageSortParams(t *testing.T) {
	t.Parallel()
	r, _, artistSvc := testRouterWithLibrary(t)

	a1 := &artist.Artist{Name: "Zydeco Band"}
	if err := artistSvc.Create(context.Background(), a1); err != nil {
		t.Fatalf("creating artist a1: %v", err)
	}
	a2 := &artist.Artist{Name: "Alpha Artist"}
	if err := artistSvc.Create(context.Background(), a2); err != nil {
		t.Fatalf("creating artist a2: %v", err)
	}

	cases := []struct {
		sort  string
		order string
	}{
		{"name", "asc"},
		{"name", "desc"},
		{"health_score", "desc"},
		{"updated_at", "asc"},
	}

	for _, tc := range cases {
		t.Run(tc.sort+"_"+tc.order, func(t *testing.T) {
			ctx := middleware.WithTestUserID(context.Background(), "test-user")
			req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/artists?sort="+tc.sort+"&order="+tc.order, nil)
			w := httptest.NewRecorder()
			r.handleArtistsPage(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("status %d, want 200", w.Code)
			}
			body := w.Body.String()
			// Check that sort and order values are reflected in the specific hidden inputs.
			wantSort := fmt.Sprintf(`id="artist-sort-input" value=%q`, tc.sort)
			wantOrder := fmt.Sprintf(`id="artist-order-input" value=%q`, tc.order)
			if !strings.Contains(body, wantSort) {
				t.Errorf("response missing sort input: want %s", wantSort)
			}
			if !strings.Contains(body, wantOrder) {
				t.Errorf("response missing order input: want %s", wantOrder)
			}
		})
	}
}

// TestArtistsPage_BulkSelectionSurvivesSort verifies the markup contract that
// the client-side bulk-selection store relies on across sort changes. Issue
// #1081: F1 (gallery) was reported to wipe selection on sort, F2 (table) was
// reported to keep positional row state instead of artist-id state. The
// client store keys selection by stable artist ID and re-derives the
// `cb.checked` DOM state on every htmx swap, which only works if the server
// (a) always emits a unique `data-artist-id` per row, (b) never bakes a
// `checked` attribute into the freshly-rendered checkbox HTML, and (c)
// excludes sort/order/page/page_size/view from the client-side
// `currentScopeToken` so the sort-triggered swap does not look like a scope
// change to the selection controller. This test pins (a)-(c) so a future
// edit cannot regress the behavior described in #1081.
func TestArtistsPage_BulkSelectionSurvivesSort(t *testing.T) {
	t.Parallel()
	r, _, artistSvc := testRouterWithLibrary(t)

	// Two artists are enough to assert per-row uniqueness while keeping the
	// rendered body small.
	for _, name := range []string{"Zydeco Band", "Alpha Artist"} {
		if err := artistSvc.Create(context.Background(), &artist.Artist{Name: name}); err != nil {
			t.Fatalf("creating artist %q: %v", name, err)
		}
	}
	listed, _, err := artistSvc.List(context.Background(), artist.ListParams{Page: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("listing artists: %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("len(listed) = %d, want 2", len(listed))
	}

	// Render both views (table + grid) and assert the same contract for each.
	for _, view := range []string{"table", "grid"} {
		t.Run("view="+view, func(t *testing.T) {
			ctx := middleware.WithTestUserID(context.Background(), "test-user")
			req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/artists?view="+view+"&sort=name&order=asc", nil)
			w := httptest.NewRecorder()
			r.handleArtistsPage(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("status %d, want 200", w.Code)
			}
			body := w.Body.String()

			// (a) Each artist row carries its own `data-artist-id` so the
			// client store can key by stable ID rather than row position.
			for _, a := range listed {
				wantAttr := fmt.Sprintf(`data-artist-id=%q`, a.ID)
				if !strings.Contains(body, wantAttr) {
					t.Errorf("rendered body missing %s for artist %q", wantAttr, a.Name)
				}
			}

			// (b) No baked-in `checked` attribute on bulk-select inputs.
			// updateBar() re-derives state after every swap; if the server
			// pre-checks any box, sort swaps would race against the
			// re-derivation and produce stale (positional) state.
			for _, a := range listed {
				marker := fmt.Sprintf(`data-artist-id=%q`, a.ID)
				idx := strings.Index(body, marker)
				if idx < 0 {
					continue
				}
				// Look back to the input tag opening for this row's
				// checkbox: the marker appears inside the <input ...>
				// tag that opens a few characters earlier.
				inputStart := strings.LastIndex(body[:idx], "<input")
				if inputStart < 0 {
					t.Errorf("could not locate <input opener for artist %q", a.Name)
					continue
				}
				inputEnd := strings.Index(body[inputStart:], ">")
				if inputEnd < 0 {
					t.Errorf("malformed <input tag for artist %q", a.Name)
					continue
				}
				inputTag := body[inputStart : inputStart+inputEnd+1]
				if strings.Contains(inputTag, " checked") || strings.Contains(inputTag, "checked=") {
					t.Errorf("bulk-select input has baked-in checked: %s", inputTag)
				}
			}

			// (c) The client-side scope-token routine excludes the params
			// that the issue called out as scope-irrelevant: sort, order,
			// page, page_size, view. Pin the inclusion list to catch any
			// drift that would re-introduce a wipe-on-sort regression.
			// This is an exact-string match against the emitted JS; if the
			// templ-side condition is reformatted or the variable is
			// renamed, update this marker rather than converting to AST
			// parsing (overkill for a contract test).
			scopeMarker := `if (k === 'search' || k === 'library_id' || k === 'filter' || k.indexOf('filter_') === 0)`
			if !strings.Contains(body, scopeMarker) {
				t.Errorf("rendered body missing scope-token allowlist: want %q", scopeMarker)
			}
			for _, irrelevant := range []string{"'sort'", "'order'", "'page'", "'page_size'", "'view'"} {
				// The allowlist must NOT mention these as scope keys.
				// Match within the allowlist line only to avoid catching
				// hidden inputs and unrelated JS that legitimately
				// reference these names.
				lineIdx := strings.Index(body, scopeMarker)
				if lineIdx < 0 {
					continue
				}
				line := body[lineIdx : lineIdx+len(scopeMarker)]
				if strings.Contains(line, irrelevant) {
					t.Errorf("scope-token allowlist must not include %s", irrelevant)
				}
			}
		})
	}
}

// TestArtistsPage_IDsFilter pins the "Show selected" affordance contract
// from issue #1227. When the URL carries `?ids=a,b,c` the artist list must
// be restricted to exactly those IDs across both views (table + grid), and
// must remain disjoint from the unrelated artist a row hidden by the filter
// would otherwise expose. Without this round-trip the cross-page selection
// still appears "lost" because the user has no way to focus on it.
func TestArtistsPage_IDsFilter(t *testing.T) {
	t.Parallel()
	r, _, artistSvc := testRouterWithLibrary(t)

	// Three artists give the test something to filter against. We pick
	// names that span the alphabet so a sort=name swap is meaningful even
	// though we are not exercising sort here -- the asserted behavior is
	// "ids restricts results regardless of sort/page/view".
	a1 := &artist.Artist{Name: "Alpha Artist"}
	a2 := &artist.Artist{Name: "Mid Artist"}
	a3 := &artist.Artist{Name: "Zulu Artist"}
	for _, a := range []*artist.Artist{a1, a2, a3} {
		if err := artistSvc.Create(context.Background(), a); err != nil {
			t.Fatalf("creating %q: %v", a.Name, err)
		}
	}

	for _, view := range []string{"table", "grid"} {
		t.Run("view="+view, func(t *testing.T) {
			ctx := testI18nCtx(t, middleware.WithTestUserID(context.Background(), "test-user"))
			// Restrict the list to a1 + a3 only; a2 must be excluded.
			url := fmt.Sprintf("/artists?view=%s&ids=%s,%s", view, a1.ID, a3.ID)
			req := httptest.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			w := httptest.NewRecorder()
			r.handleArtistsPage(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("status %d, want 200; body: %s", w.Code, w.Body.String())
			}
			body := w.Body.String()

			// a1 and a3 must be present (their data-artist-id attribute
			// is the unambiguous markup signal -- search uses HTML
			// escaping that could otherwise tangle with the chip text).
			for _, a := range []*artist.Artist{a1, a3} {
				marker := fmt.Sprintf(`data-artist-id=%q`, a.ID)
				if !strings.Contains(body, marker) {
					t.Errorf("response missing %s for %q", marker, a.Name)
				}
			}
			// a2 (Mid Artist) was excluded by the ids filter and must
			// not appear in the rendered rows.
			if strings.Contains(body, fmt.Sprintf(`data-artist-id=%q`, a2.ID)) {
				t.Errorf("response includes %q (id=%s) which was excluded by the ids filter", a2.Name, a2.ID)
			}

			// The "Showing N selected" chip surfaces server-side when
			// ids is in effect, so the user can see at a glance that
			// they are looking at the selection rather than the full
			// list. We pin the count to keep the chip in sync with the
			// filter (a stale count would defeat #1227's UX intent).
			if !strings.Contains(body, "Showing 2 selected artists") {
				t.Errorf("response missing 'Showing 2 selected artists' chip; body excerpt: %.500s", body)
			}
		})
	}
}

// TestArtistsPage_IDsFilter_Empty pins that an empty/whitespace-only `ids`
// param degrades to the unfiltered listing rather than returning zero rows.
// The handler treats the absent-or-empty case the same so a browser that
// strips the param on its own (older htmx, manual URL edits) does not show
// an empty list with an alarming "Showing 0 selected" chip.
func TestArtistsPage_IDsFilter_Empty(t *testing.T) {
	t.Parallel()
	r, _, artistSvc := testRouterWithLibrary(t)
	if err := artistSvc.Create(context.Background(), &artist.Artist{Name: "Solo Artist"}); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	ctx := testI18nCtx(t, middleware.WithTestUserID(context.Background(), "test-user"))
	cases := []struct {
		name string
		// raw is the *unencoded* form for human readability; we URL-encode
		// before issuing the request so spaces and trailing commas do not
		// break httptest.NewRequest's URL parser.
		raw string
	}{
		{"absent", ""},
		{"trailing-comma", ","},
		{"whitespace-only", " , , "},
		// "malformed" tokens fail the canonical idPattern check and must
		// be dropped server-side so they never round-trip into the SQL
		// IN-clause or the "Showing N selected" chip.
		{"malformed", "@@,!!"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// "absent" must exercise a genuinely missing ids parameter -- not
			// ids= with an empty value -- so the handler hits the raw == ""
			// short-circuit in parseIDsParam rather than the post-split
			// no-tokens branch. Without this distinction the subtest name
			// over-promises coverage.
			url := "/artists?view=table"
			if tc.name != "absent" {
				url += "&ids=" + neturl.QueryEscape(tc.raw)
			}
			req := httptest.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			w := httptest.NewRecorder()
			r.handleArtistsPage(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("status %d, want 200; body: %s", w.Code, w.Body.String())
			}
			body := w.Body.String()
			if !strings.Contains(body, "Solo Artist") {
				t.Errorf("empty-ids should not filter rows out; body excerpt: %.500s", body)
			}
			if strings.Contains(body, "Showing ") && strings.Contains(body, "selected artist") {
				t.Errorf("empty-ids should not render a Showing-N-selected chip; body excerpt: %.500s", body)
			}
		})
	}
}

// TestArtistDetailPage_TabDebugFallback verifies that tab=debug falls back to
// overview when the setting is disabled, when no connections exist, or when
// only non-debug-capable (Lidarr) connections exist.
func TestArtistDetailPage_TabDebugFallback(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Debug Tab Artist")

	// Helper to enable the show_platform_debug per-user preference for "test-user"
	// (the user ID injected by doRequest via middleware.WithTestUserID).
	enableDebug := func() {
		_, err := r.db.ExecContext(context.Background(),
			`INSERT OR REPLACE INTO user_preferences (user_id, key, value, updated_at)
			 VALUES ('test-user', 'show_platform_debug', 'true', datetime('now'))`)
		if err != nil {
			t.Fatalf("setting show_platform_debug preference: %v", err)
		}
	}

	// Helper to add a connection and link it to the artist.
	addConn := func(id, connType string) {
		c := &connection.Connection{
			ID:      id,
			Name:    id,
			Type:    connType,
			URL:     "http://localhost:8096",
			APIKey:  "test-key",
			Enabled: true,
			Status:  "ok",
		}
		if err := r.connectionService.Create(context.Background(), c); err != nil {
			t.Fatalf("creating connection %s: %v", id, err)
		}
		if err := artistSvc.SetPlatformID(context.Background(), a.ID, id, "platform-"+id); err != nil {
			t.Fatalf("setting platform ID for %s: %v", id, err)
		}
	}

	// Helper to make a request with tab=debug and return the response body.
	doRequest := func() string {
		ctx := middleware.WithTestUserID(context.Background(), "test-user")
		req := httptest.NewRequestWithContext(ctx, http.MethodGet,
			"/artists/"+a.ID+"?tab=debug", nil)
		req.SetPathValue("id", a.ID)
		w := httptest.NewRecorder()
		r.handleArtistDetailPage(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		return w.Body.String()
	}

	// Case 1: setting disabled, no connections -- debug panel should be hidden.
	t.Run("setting_disabled", func(t *testing.T) {
		body := doRequest()
		if strings.Contains(body, `id="tab-debug"`) {
			t.Error("debug tab should not appear when setting is disabled")
		}
	})

	// Case 2: setting enabled but only Lidarr connection -- no debug-capable connection.
	t.Run("lidarr_only", func(t *testing.T) {
		enableDebug()
		addConn("conn-lidarr", connection.TypeLidarr)
		body := doRequest()
		if strings.Contains(body, `id="tab-debug"`) {
			t.Error("debug tab should not appear with only Lidarr connections")
		}
	})

	// Case 3: setting enabled with Emby connection -- debug tab should appear.
	t.Run("emby_connection", func(t *testing.T) {
		addConn("conn-emby", connection.TypeEmby)
		body := doRequest()
		if !strings.Contains(body, `id="tab-debug"`) {
			t.Error("debug tab should appear when setting is enabled and Emby connection exists")
		}
		// The debug panel should be present since tab=debug is active with a valid connection.
		if !strings.Contains(body, `data-tab-panel="debug"`) {
			t.Error("debug panel should be rendered when tab=debug is active with Emby connection")
		}
	})
}

// TestParseFlyoutFilters_NewKeys verifies that every key added in issue #1125
// is recognized by parseFlyoutFilters and that +y maps to "include" and -y
// maps to "exclude". A key that is not in the allowlist would silently be
// dropped from the returned map, making the filter a no-op at the DB layer.
func TestParseFlyoutFilters_NewKeys(t *testing.T) {
	t.Parallel()
	newKeys := []string{
		"has_biography", "has_years_active", "has_formed", "has_disbanded",
		"has_born", "has_died", "has_gender", "has_type", "has_country",
		"has_genres", "has_styles", "has_moods", "has_members", "has_discography",
		"has_thumb", "has_fanart", "has_logo", "has_banner",
		"in_emby", "in_jellyfin", "has_lidarr",
		"has_violations",
	}
	for _, key := range newKeys {
		key := key
		t.Run(key+"_include", func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodGet,
				"/artists?filter_"+key+"=%2By", nil)
			got := parseFlyoutFilters(req)
			if got[key] != "include" {
				t.Errorf("parseFlyoutFilters(%q=+y): got %q, want %q", key, got[key], "include")
			}
		})
		t.Run(key+"_exclude", func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodGet,
				"/artists?filter_"+key+"=-y", nil)
			got := parseFlyoutFilters(req)
			if got[key] != "exclude" {
				t.Errorf("parseFlyoutFilters(%q=-y): got %q, want %q", key, got[key], "exclude")
			}
		})
	}
}

// TestParseFlyoutFilters_WhitelistNormalizesExcludes verifies the issue #1217
// whitelist normalization: once any library is set to Include, every library
// Exclude is dropped, so the render layer (pills, active-filter chips, the
// active count) and the SQL layer agree on one effective filter set. Non-library
// excludes and the library includes themselves are left untouched.
func TestParseFlyoutFilters_WhitelistNormalizesExcludes(t *testing.T) {
	t.Parallel()

	t.Run("library exclude dropped when a library include is present", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequest(http.MethodGet,
			"/artists?filter_library_aaa=%2By&filter_library_bbb=-y", nil)
		got := parseFlyoutFilters(req)
		if got["library_aaa"] != "include" {
			t.Errorf("library_aaa: got %q, want %q", got["library_aaa"], "include")
		}
		if _, ok := got["library_bbb"]; ok {
			t.Errorf("library_bbb: got %q, want it dropped in whitelist mode", got["library_bbb"])
		}
	})

	t.Run("library excludes kept when no library include is present", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequest(http.MethodGet,
			"/artists?filter_library_aaa=-y&filter_library_bbb=-y", nil)
		got := parseFlyoutFilters(req)
		if got["library_aaa"] != "exclude" || got["library_bbb"] != "exclude" {
			t.Errorf("exclude-only mode: got %v, want both library_aaa and library_bbb exclude", got)
		}
	})

	t.Run("non-library excludes survive whitelist normalization", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequest(http.MethodGet,
			"/artists?filter_library_aaa=%2By&filter_has_thumb=-y", nil)
		got := parseFlyoutFilters(req)
		if got["library_aaa"] != "include" {
			t.Errorf("library_aaa: got %q, want %q", got["library_aaa"], "include")
		}
		if got["has_thumb"] != "exclude" {
			t.Errorf("has_thumb: got %q, want %q (a non-library exclude must survive)", got["has_thumb"], "exclude")
		}
	})
}

// TestHandleArtistsPage_UnauthRendersLoginPage asserts that an unauthenticated
// GET /artists returns HTTP 200 with the login page rather than artist data.
// buildArtistListData calls requireAuth as its first action, so a visitor with
// no session is shown the login page in-page (HTTP 200) rather than a 401 JSON
// error. This covers the false-branch lines added in #2018.
func TestHandleArtistsPage_UnauthRendersLoginPage(t *testing.T) {
	t.Parallel()
	r := newTestRouterFull(t)

	req := withI18nCtx(t, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/artists", nil))
	w := httptest.NewRecorder()
	r.handleArtistsPage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unauthenticated request should get login page (200), got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "artist-list") {
		t.Error("unauthenticated visitor must not see the artist list")
	}
	if !strings.Contains(body, "/api/v1/auth/login") {
		t.Error("login page must have the login form action (/api/v1/auth/login)")
	}
	if !strings.Contains(body, `name="username"`) {
		t.Error("login page must include a username input field (name=username)")
	}
	if !strings.Contains(body, `type="password"`) {
		t.Error("login page must include a password input field (type=password)")
	}
}

// TestHandleArtistDetailPage_UnauthRendersLoginPage asserts that an
// unauthenticated GET /artists/{id} returns HTTP 200 with the login page.
// buildArtistDetailData calls requireAuth before fetching the artist record,
// so unauthenticated visitors never reach the detail data. This covers the
// false-branch lines added in #2018.
func TestHandleArtistDetailPage_UnauthRendersLoginPage(t *testing.T) {
	t.Parallel()
	r := newTestRouterFull(t)

	req := withI18nCtx(t, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/artists/any-id", nil))
	req.SetPathValue("id", "any-id")
	w := httptest.NewRecorder()
	r.handleArtistDetailPage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unauthenticated request should get login page (200), got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "artist-detail") {
		t.Error("unauthenticated visitor must not see the artist detail page")
	}
	if !strings.Contains(body, "/api/v1/auth/login") {
		t.Error("login page must have the login form action (/api/v1/auth/login)")
	}
	if !strings.Contains(body, `name="username"`) {
		t.Error("login page must include a username input field (name=username)")
	}
	if !strings.Contains(body, `type="password"`) {
		t.Error("login page must include a password input field (type=password)")
	}
}

// TestHandleArtistImagesPage_UnauthRendersLoginPage asserts that an
// unauthenticated GET /artists/{id}/images returns HTTP 200 with the login
// page. handleArtistImagesPage calls requireAuth as its first action, so
// unauthenticated visitors never reach the image data. This covers the
// false-branch lines added in #2018.
func TestHandleArtistImagesPage_UnauthRendersLoginPage(t *testing.T) {
	t.Parallel()
	r := newTestRouterFull(t)

	req := withI18nCtx(t, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/artists/any-id/images", nil))
	req.SetPathValue("id", "any-id")
	w := httptest.NewRecorder()
	r.handleArtistImagesPage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unauthenticated request should get login page (200), got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "artist-images") {
		t.Error("unauthenticated visitor must not see the artist images page")
	}
	if !strings.Contains(body, "/api/v1/auth/login") {
		t.Error("login page must have the login form action (/api/v1/auth/login)")
	}
	if !strings.Contains(body, `name="username"`) {
		t.Error("login page must include a username input field (name=username)")
	}
	if !strings.Contains(body, `type="password"`) {
		t.Error("login page must include a password input field (type=password)")
	}
}

// TestHandleArtistsPage_AuthRendersArtistList asserts that an authenticated
// GET /artists returns HTTP 200 with the real artist list page (not the login
// page). buildArtistListData calls requireAuth as its first action; with a
// valid user ID in context, the list page renders normally.
func TestHandleArtistsPage_AuthRendersArtistList(t *testing.T) {
	t.Parallel()
	r := newTestRouterFull(t)

	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/artists", nil)
	w := httptest.NewRecorder()
	r.handleArtistsPage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("authenticated request should get artist list (200), got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "/api/v1/auth/login") {
		t.Error("authenticated user must not see the login page")
	}
	if !strings.Contains(body, "artist-sort-input") {
		t.Error("artist list page must include the sort input (artist-sort-input)")
	}
}

// TestHandleArtistDetailPage_AuthRendersArtistDetail asserts that an
// authenticated GET /artists/{id} returns HTTP 200 with the real artist detail
// page. buildArtistDetailData calls requireAuth before fetching the artist; with
// a valid user ID in context, the detail page renders for the requested artist.
func TestHandleArtistDetailPage_AuthRendersArtistDetail(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Auth Detail Artist")

	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/artists/"+a.ID, nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()
	r.handleArtistDetailPage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("authenticated request should get artist detail (200), got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "/api/v1/auth/login") {
		t.Error("authenticated user must not see the login page")
	}
	if !strings.Contains(body, "data-artist-id") {
		t.Error("artist detail page must include the data-artist-id attribute")
	}
}

// TestHandleArtistImagesPage_AuthRendersArtistImages asserts that an
// authenticated GET /artists/{id}/images returns HTTP 200 with the real image
// management page. handleArtistImagesPage calls requireAuth as its first
// action; with a valid user ID in context, the image management page renders.
func TestHandleArtistImagesPage_AuthRendersArtistImages(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Auth Images Artist")

	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/artists/"+a.ID+"/images", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()
	r.handleArtistImagesPage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("authenticated request should get artist images (200), got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "/api/v1/auth/login") {
		t.Error("authenticated user must not see the login page")
	}
	if !strings.Contains(body, a.Name) {
		t.Errorf("artist images page must include the artist name (%q)", a.Name)
	}
}

// The tests below were consolidated from handlers_next_artists_test.go when
// the artists list promoted-by-move into the canonical handler (#1757 PR-3a).
// The channel-gating cases (StableMode404 / OptOutHeader404 / stable-fallback)
// are moot with the /next/artists route removed; the fragment-dispatch,
// base-URL, saved-views, and sort-validation coverage carries over against
// handleArtistsPage.

// TestHandleArtistsPage_WiresCanonicalBaseURL verifies buildArtistListData
// stamps the canonical /artists BaseURL unconditionally since the promotion,
// so the rendered toolbar/pagination hx-get targets /artists and never the
// removed /next/artists route.
func TestHandleArtistsPage_WiresCanonicalBaseURL(t *testing.T) {
	t.Parallel()
	r, _, artistSvc := testRouterWithLibrary(t)
	if err := artistSvc.Create(context.Background(), &artist.Artist{Name: "Alpha Artist"}); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/artists", nil)
	w := httptest.NewRecorder()
	r.handleArtistsPage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `hx-get="/artists"`) {
		t.Errorf("promoted page must wire the canonical /artists BaseURL (hx-get=\"/artists\" absent)")
	}
	if strings.Contains(body, `hx-get="/next/artists"`) {
		t.Errorf("promoted page must not target the removed /next/artists endpoint")
	}
}

// TestHandleArtistsPage_HTMXFragmentDispatch verifies that an HTMX request
// (HX-Request: true) renders only the table or grid FRAGMENT, not the full
// page shell, and that ?view= selects the right fragment. The table fragment
// carries the consolidated Sources/Coverage columns; the grid fragment carries
// the card grid layout. Neither must emit the full-page <html>/sidebar chrome.
func TestHandleArtistsPage_HTMXFragmentDispatch(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		view        string
		wantMarkers []string
		denyMarkers []string
	}{
		{
			name:        "table",
			view:        "table",
			wantMarkers: []string{`data-col="sources"`, `data-col="coverage"`, `id="artists-table"`},
			denyMarkers: []string{"grid-cols"},
		},
		{
			name:        "grid",
			view:        "grid",
			wantMarkers: []string{"grid-cols"},
			denyMarkers: []string{`id="artists-table"`},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, _, artistSvc := testRouterWithLibrary(t)
			if err := artistSvc.Create(context.Background(), &artist.Artist{Name: "Alpha Artist"}); err != nil {
				t.Fatalf("creating artist: %v", err)
			}

			ctx := middleware.WithTestUserID(context.Background(), "test-user")
			req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/artists?view="+tc.view, nil)
			req.Header.Set("HX-Request", "true")
			w := httptest.NewRecorder()
			r.handleArtistsPage(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", w.Code)
			}
			body := w.Body.String()

			// A fragment must NOT carry the full-page shell: no <html> document
			// element and no page scoping chrome.
			for _, deny := range []string{"<html", "sw-next-artists"} {
				if strings.Contains(body, deny) {
					t.Errorf("%s fragment must not render full-page chrome (found %q)", tc.name, deny)
				}
			}
			// The fragment is the #artist-content swap target, not the whole page.
			if !strings.Contains(body, `id="artist-content"`) {
				t.Errorf("%s fragment must render the #artist-content swap target", tc.name)
			}
			for _, want := range tc.wantMarkers {
				if !strings.Contains(body, want) {
					t.Errorf("%s fragment missing marker %q", tc.name, want)
				}
			}
			for _, deny := range tc.denyMarkers {
				if strings.Contains(body, deny) {
					t.Errorf("%s fragment must not contain %q (wrong view selected)", tc.name, deny)
				}
			}
		})
	}
}

// TestBuildArtistListData_LoadsSavedViews verifies that a stored saved_views
// preference renders the saved-views chips row (M55 #1777). The load was
// next-channel-gated before #1757 PR-3a; it is always-on since the promotion,
// so no UX middleware or channel context is involved.
func TestBuildArtistListData_LoadsSavedViews(t *testing.T) {
	t.Parallel()
	r, _, artistSvc := testRouterWithLibrary(t)
	if err := artistSvc.Create(context.Background(), &artist.Artist{Name: "Test Artist"}); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// Create a real user to satisfy the FK constraint on user_preferences.
	authSvc := auth.NewService(r.db)
	if _, err := authSvc.Setup(context.Background(), "viewuser", "testpassword"); err != nil {
		t.Fatalf("creating user: %v", err)
	}
	var userID string
	if err := r.db.QueryRowContext(context.Background(),
		`SELECT id FROM users WHERE username = 'viewuser'`).Scan(&userID); err != nil {
		t.Fatalf("looking up user id: %v", err)
	}

	// Seed a valid saved_views preference row directly so the handler's read
	// path has data to load.
	savedViewsJSON := `[{"name":"My Saved View","params":"filter=complete","created_at":"2024-01-01T00:00:00Z"}]`
	if _, err := r.db.ExecContext(context.Background(),
		`INSERT INTO user_preferences (user_id, key, value, updated_at) VALUES (?, ?, ?, datetime('now'))
		 ON CONFLICT(user_id, key) DO UPDATE SET value = excluded.value`,
		userID, PrefSavedViews, savedViewsJSON); err != nil {
		t.Fatalf("seeding saved_views preference: %v", err)
	}

	ctx := middleware.WithTestUserID(context.Background(), userID)
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/artists", nil)
	w := httptest.NewRecorder()
	r.handleArtistsPage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	// The template renders #saved-views-row unconditionally (hidden when
	// empty); the seeded view's chip must be present.
	if !strings.Contains(body, `saved-views-row`) {
		t.Errorf("saved-views-row not rendered")
	}
	if !strings.Contains(body, "My Saved View") {
		t.Errorf("saved view name %q not found in rendered output", "My Saved View")
	}
}

// TestBuildArtistListData_NoSavedViewsRendersNoChips is the negative sibling
// of TestBuildArtistListData_LoadsSavedViews: with no saved_views preference
// row for the user, the page must render ZERO saved-view chips and the
// #saved-views-row container must carry the "hidden" class. A regression that
// always injects a spurious chip row (or unhides the empty row) fails here.
func TestBuildArtistListData_NoSavedViewsRendersNoChips(t *testing.T) {
	t.Parallel()
	r, _, artistSvc := testRouterWithLibrary(t)
	if err := artistSvc.Create(context.Background(), &artist.Artist{Name: "Test Artist"}); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// Create a real user (as in the positive test) but deliberately do NOT
	// seed a saved_views preference row: this exercises the absent/empty
	// preference path of the always-on saved-views load.
	authSvc := auth.NewService(r.db)
	if _, err := authSvc.Setup(context.Background(), "noviewuser", "testpassword"); err != nil {
		t.Fatalf("creating user: %v", err)
	}
	var userID string
	if err := r.db.QueryRowContext(context.Background(),
		`SELECT id FROM users WHERE username = 'noviewuser'`).Scan(&userID); err != nil {
		t.Fatalf("looking up user id: %v", err)
	}

	ctx := middleware.WithTestUserID(context.Background(), userID)
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/artists", nil)
	w := httptest.NewRecorder()
	r.handleArtistsPage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()

	// The chip buttons are the ONLY places these JS calls render (the
	// save-view modal button uses swSavedViews.openSaveViewModal, a different
	// call); the positive test asserts their presence via the chip name.
	if strings.Contains(body, "swSavedViews.applySavedView") {
		t.Errorf("saved-view apply chip rendered despite no saved views")
	}
	if strings.Contains(body, "swSavedViews.deleteSavedView") {
		t.Errorf("saved-view delete button rendered despite no saved views")
	}

	// The row div itself renders unconditionally (JS updates it in place) but
	// must be hidden when there are no saved views: assert the "hidden" class
	// appears within the row's opening tag.
	idx := strings.Index(body, `id="saved-views-row"`)
	if idx < 0 {
		t.Fatalf("saved-views-row not rendered")
	}
	openTag := body[idx:]
	if end := strings.Index(openTag, ">"); end >= 0 {
		openTag = openTag[:end]
	}
	if !strings.Contains(openTag, "hidden") {
		t.Errorf("saved-views-row is not hidden with no saved views; opening tag: %s", openTag)
	}
}

func TestHandleArtistsPage_InvalidSortReturnsBadRequest(t *testing.T) {
	t.Parallel()

	r, _, _ := testRouterWithLibrary(t)

	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/artists?sort=invalid_sort_key", nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	r.handleArtistsPage(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}
