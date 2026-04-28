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
	"github.com/sydlexius/stillwater/internal/connection"
)

func TestHandleArtistsBadge_ZeroCount(t *testing.T) {
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
	r, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Debug Tab Artist")

	// Helper to enable the show_platform_debug setting.
	enableDebug := func() {
		_, err := r.db.ExecContext(context.Background(),
			`INSERT OR REPLACE INTO settings (key, value) VALUES ('show_platform_debug', 'true')`)
		if err != nil {
			t.Fatalf("setting show_platform_debug: %v", err)
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
