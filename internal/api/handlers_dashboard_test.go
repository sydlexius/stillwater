package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/rule"
)

// testDashboardRouter creates a minimal Router suitable for dashboard handler
// tests. It includes an artist service, rule service, and optionally a
// history service. The caller can set historyService to nil to test the
// nil-guard path.
func testDashboardRouter(t *testing.T, includeHistory bool) *Router {
	t.Helper()

	stub := &stubPipeline{}
	r, _ := testRouterWithStubPipeline(t, stub)

	if includeHistory {
		r.historyService = artist.NewHistoryService(r.db)
	} else {
		r.historyService = nil
	}

	return r
}

// withTestUser injects a test user ID into the request context so the
// dashboard handlers pass the auth check.
func withTestUser(req *http.Request) *http.Request {
	ctx := middleware.WithTestUserID(req.Context(), "test-user")
	return req.WithContext(ctx)
}

// --- handleDashboardActionQueue tests (#876) ---

func TestHandleDashboardActionQueue_LimitCapping(t *testing.T) {
	t.Parallel()
	r := testDashboardRouter(t, false)
	ctx := context.Background()

	// Seed more violations than PageSizeMin (10) so the limit parameter
	// actually constrains the result set. getUserPageSize clamps the limit
	// to [PageSizeMin, PageSizeMax], so we seed 15 and request limit=10.
	// UpsertViolation deduplicates on (rule_id, artist_id), so we create
	// separate artists to guarantee unique violations.
	const seedCount = 15
	for i := range seedCount {
		a := &artist.Artist{
			Name:     "Limit Test Artist " + string(rune('A'+i)),
			SortName: "Limit Test Artist " + string(rune('A'+i)),
			Type:     "group",
			Path:     "/music/LimitTest" + string(rune('A'+i)),
			Genres:   []string{"Rock"},
		}
		if err := r.artistService.Create(ctx, a); err != nil {
			t.Fatalf("creating artist %d: %v", i, err)
		}
		v := &rule.RuleViolation{
			RuleID: rule.RuleNFOExists, ArtistID: a.ID, ArtistName: a.Name,
			Severity: "error", Message: "missing nfo",
			Fixable: true, Status: rule.ViolationStatusOpen,
		}
		if err := r.ruleService.UpsertViolation(ctx, v); err != nil {
			t.Fatalf("seeding violation %d: %v", i, err)
		}
	}

	// Request with limit=10 (PageSizeMin) -- should return at most 10 action
	// cards despite 15 being available.
	req := httptest.NewRequest(http.MethodGet, "/dashboard/actions?limit=10", nil)
	req = withTestUser(req)
	w := httptest.NewRecorder()

	r.handleDashboardActionQueue(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	// Verify we got HTML back (the handler renders templ).
	ct := w.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}

	// Count the number of action cards rendered. Each card div has a unique
	// id like id="action-card-<uuid>". Count those to avoid double-counting
	// the class attribute.
	body := w.Body.String()
	cardCount := strings.Count(body, "id=\"action-card-")
	if cardCount != 10 {
		t.Errorf("expected 10 action cards with limit=10 and 15 seeded, got %d", cardCount)
	}

	// Should render a load-more button since total (15) > returned (10).
	if !strings.Contains(body, "action-queue-load-more") {
		t.Error("expected load-more button when total exceeds limit")
	}
}

func TestHandleDashboardActionQueue_OffsetBranching(t *testing.T) {
	t.Parallel()
	r := testDashboardRouter(t, false)
	ctx := context.Background()

	// Seed violations with unique artists so UpsertViolation (which
	// deduplicates on rule_id + artist_id) creates distinct rows.
	for i := range 5 {
		a := &artist.Artist{
			Name:     "Offset Test Artist " + string(rune('A'+i)),
			SortName: "Offset Test Artist " + string(rune('A'+i)),
			Type:     "group",
			Path:     "/music/OffsetTest" + string(rune('A'+i)),
			Genres:   []string{"Rock"},
		}
		if err := r.artistService.Create(ctx, a); err != nil {
			t.Fatalf("creating artist %d: %v", i, err)
		}
		v := &rule.RuleViolation{
			RuleID: rule.RuleThumbExists, ArtistID: a.ID, ArtistName: a.Name,
			Severity: "warning", Message: "missing thumb " + string(rune('a'+i)),
			Fixable: true, Status: rule.ViolationStatusOpen,
		}
		if err := r.ruleService.UpsertViolation(ctx, v); err != nil {
			t.Fatalf("seeding violation %d: %v", i, err)
		}
	}

	// offset=0 should render the full fragment (header + chips + cards).
	req0 := httptest.NewRequest(http.MethodGet, "/dashboard/actions?limit=2&offset=0", nil)
	req0 = withTestUser(req0)
	w0 := httptest.NewRecorder()
	r.handleDashboardActionQueue(w0, req0)

	if w0.Code != http.StatusOK {
		t.Fatalf("offset=0: status = %d, want %d", w0.Code, http.StatusOK)
	}
	body0 := w0.Body.String()

	// offset > 0 should render the "more rows" fragment (no header/chips).
	req1 := httptest.NewRequest(http.MethodGet, "/dashboard/actions?limit=2&offset=2", nil)
	req1 = withTestUser(req1)
	w1 := httptest.NewRecorder()
	r.handleDashboardActionQueue(w1, req1)

	if w1.Code != http.StatusOK {
		t.Fatalf("offset=2: status = %d, want %d", w1.Code, http.StatusOK)
	}
	body1 := w1.Body.String()

	// The offset=0 response should contain select-all toggle markup that
	// only the full DashboardActionQueue fragment includes.
	if !strings.Contains(body0, "select-all-toggle") {
		t.Error("offset=0 response should contain select-all-toggle (full fragment)")
	}
	// The offset>0 response should use OOB swap for appending rows.
	if !strings.Contains(body1, "hx-swap-oob") {
		t.Error("offset>0 response should contain hx-swap-oob for load-more appending")
	}
}

func TestHandleDashboardActionQueue_Unauthorized(t *testing.T) {
	t.Parallel()
	r := testDashboardRouter(t, false)

	// No user in context -- should return 401.
	req := httptest.NewRequest(http.MethodGet, "/dashboard/actions", nil)
	w := httptest.NewRecorder()

	r.handleDashboardActionQueue(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

func TestHandleDashboardActionQueue_NegativeOffset(t *testing.T) {
	t.Parallel()
	r := testDashboardRouter(t, false)
	ctx := context.Background()

	// Seed one violation so the response is non-trivial and the select-all
	// toggle is rendered (which only appears on the full offset=0 fragment).
	a := &artist.Artist{
		Name:     "Negative Offset Artist",
		SortName: "Negative Offset Artist",
		Type:     "group",
		Path:     "/music/NegativeOffset",
		Genres:   []string{"Rock"},
	}
	if err := r.artistService.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	v := &rule.RuleViolation{
		RuleID: rule.RuleNFOExists, ArtistID: a.ID, ArtistName: a.Name,
		Severity: "error", Message: "missing nfo",
		Fixable: true, Status: rule.ViolationStatusOpen,
	}
	if err := r.ruleService.UpsertViolation(ctx, v); err != nil {
		t.Fatalf("seeding violation: %v", err)
	}

	// A negative offset should be clamped to 0 and return the full fragment
	// (header + chips + cards), identical to requesting offset=0 explicitly.
	req := httptest.NewRequest(http.MethodGet, "/dashboard/actions?offset=-1", nil)
	req = withTestUser(req)
	w := httptest.NewRecorder()

	r.handleDashboardActionQueue(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	// The full fragment (offset=0 after clamping) includes the select-all
	// toggle, which the "more rows" fragment does not.
	body := w.Body.String()
	if !strings.Contains(body, "select-all-toggle") {
		t.Error("negative offset should clamp to 0 and render full fragment with select-all-toggle")
	}
}

// --- handleDashboardActivityFeed tests (#876) ---

func TestHandleDashboardActivityFeed_WithHistoryService(t *testing.T) {
	t.Parallel()
	r := testDashboardRouter(t, true)

	req := httptest.NewRequest(http.MethodGet, "/dashboard/activity", nil)
	req = withTestUser(req)
	w := httptest.NewRecorder()

	r.handleDashboardActivityFeed(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	ct := w.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}

	// Should render the "no recent activity" empty state. The template uses
	// t(ctx, "dashboard.no_recent_activity") which may render as the key or
	// the translated value depending on i18n bundle availability.
	body := w.Body.String()
	if !strings.Contains(body, "no_recent_activity") && !strings.Contains(body, "No recent activity") {
		t.Fatalf("expected empty activity state text in response, got: %s", body)
	}
}

func TestHandleDashboardActivityFeed_NilHistoryService(t *testing.T) {
	t.Parallel()
	r := testDashboardRouter(t, false) // historyService = nil

	req := httptest.NewRequest(http.MethodGet, "/dashboard/activity", nil)
	req = withTestUser(req)
	w := httptest.NewRecorder()

	r.handleDashboardActivityFeed(w, req)

	// Should succeed with an empty changes list, not panic on nil service.
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
}

func TestHandleDashboardActivityFeed_Unauthorized(t *testing.T) {
	t.Parallel()
	r := testDashboardRouter(t, true)

	// No user in context.
	req := httptest.NewRequest(http.MethodGet, "/dashboard/activity", nil)
	w := httptest.NewRecorder()

	r.handleDashboardActivityFeed(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

func TestHandleDashboardActivityFeed_EmptyResultSet(t *testing.T) {
	t.Parallel()
	// Verifies the handler renders successfully when the history service is
	// present but returns an empty result set (no metadata changes recorded).
	r := testDashboardRouter(t, true)

	req := httptest.NewRequest(http.MethodGet, "/dashboard/activity", nil)
	req = withTestUser(req)
	w := httptest.NewRecorder()

	r.handleDashboardActivityFeed(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	// With no history entries, the handler should render the empty state.
	body := w.Body.String()
	if !strings.Contains(body, "no_recent_activity") && !strings.Contains(body, "No recent activity") {
		t.Errorf("expected empty activity state in response, got: %s", body)
	}
}

// TestHandleDashboardActivityFeed_NextChannelRendersRailFragment verifies that
// when the resolved UI channel is "next", GET /dashboard/activity renders the
// next/ rail fragment (DashboardActivityFeedNext) with its empty-on-boot idle
// hint and run-rules affordance, not the stable "View all activity" feed. The
// next/ rail's live SSE rows must match this fragment's row shape (M55 #1334).
func TestHandleDashboardActivityFeed_NextChannelRendersRailFragment(t *testing.T) {
	t.Parallel()
	r := testDashboardRouter(t, true)

	// Wrap the handler in the UX middleware in "next" mode so the request
	// context carries UXNext, exactly as it would in production.
	h := middleware.UX("next", "")(http.HandlerFunc(r.handleDashboardActivityFeed))

	req := httptest.NewRequest(http.MethodGet, "/dashboard/activity", nil)
	req = withTestUser(req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	body := w.Body.String()
	// The next/ empty-on-boot state shows the localized idle hint and a
	// run-rules affordance, never the stable "View all activity" footer.
	if !strings.Contains(body, "activity_idle") && !strings.Contains(body, "Stillwater is idle") {
		t.Errorf("expected next/ idle hint in response, got: %s", body)
	}
	if strings.Contains(body, "view_all_activity") || strings.Contains(body, "View all activity") {
		t.Errorf("next/ rail must not render the stable view-all-activity footer, got: %s", body)
	}
	// The run-rules affordance reuses the dashboard panels controller.
	if !strings.Contains(body, "swDashboardPanels") {
		t.Errorf("expected next/ idle hint to expose a run-rules affordance, got: %s", body)
	}
}

// TestHandleDashboardActionQueue_NextChannel verifies the next-channel branch
// of handleDashboardActionQueue: when the resolved UI channel is "next" the
// handler (a) renders the slimmer next/ fragment (DashboardActionQueue in the
// next package, identified by the next-dash-count OOB span the stable fragment
// never emits) and (b) sets an HX-Push-Url that tracks the next/ screen root
// ("$basePath/next/"), not the stable app root, while round-tripping a tri-state
// exclude filter (?severity=-error -> push contains severity=-error). This is
// the channel branch the stable-channel tests do not exercise.
func TestHandleDashboardActionQueue_NextChannel(t *testing.T) {
	t.Parallel()
	r := testDashboardRouter(t, false)
	ctx := context.Background()

	// Seed one open violation so the queue renders a non-empty fragment.
	a := &artist.Artist{
		Name:     "Next Channel Artist",
		SortName: "Next Channel Artist",
		Type:     "group",
		Path:     "/music/NextChannel",
		Genres:   []string{"Rock"},
	}
	if err := r.artistService.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	v := &rule.RuleViolation{
		RuleID: rule.RuleThumbExists, ArtistID: a.ID, ArtistName: a.Name,
		Severity: "warning", Message: "missing thumb",
		Fixable: true, Status: rule.ViolationStatusOpen,
	}
	if err := r.ruleService.UpsertViolation(ctx, v); err != nil {
		t.Fatalf("seeding violation: %v", err)
	}

	// Wrap the handler in the UX middleware in "next" mode so the request
	// context carries UXNext exactly as it would in production.
	h := middleware.UX("next", "")(http.HandlerFunc(r.handleDashboardActionQueue))

	// Request an exclude tri-state filter (?severity=-error) over HTMX so the
	// push-URL branch fires.
	req := httptest.NewRequest(http.MethodGet, "/dashboard/actions?severity=-error", nil)
	req.Header.Set("HX-Request", "true")
	req = withTestUser(req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	// (a) The next/ fragment renders the next-dash-count OOB span; the stable
	// DashboardActionQueue fragment never emits this id.
	body := w.Body.String()
	if !strings.Contains(body, "next-dash-count") {
		t.Errorf("expected next/ fragment marker (next-dash-count) in body, got: %s", body)
	}

	// (b) The push URL must track the next/ screen root and round-trip the
	// exclude filter under the canonical tri-state contract.
	push := w.Header().Get("HX-Push-Url")
	if push == "" {
		t.Fatalf("expected HX-Push-Url to be set on HTMX next-channel request")
	}
	if !strings.HasPrefix(push, "/next/") {
		t.Errorf("HX-Push-Url = %q, want it to start with /next/", push)
	}
	pushURL, err := url.Parse(push)
	if err != nil {
		t.Fatalf("HX-Push-Url is not a valid URL: %q (%v)", push, err)
	}
	if got := pushURL.Query()["severity"]; !sameStringSet(got, []string{"-error"}) {
		t.Errorf("HX-Push-Url severity = %v, want [-error]; full=%q", got, push)
	}
}

// TestHandleDashboardActionQueue_StatsError verifies that when GetHealthStats
// returns an error the handler still returns 200 OK and renders the
// stats-unavailable placeholder ("---") instead of a percentage like "0.0%".
//
// To isolate the failure to just the health-stats query, a second in-memory
// database is opened exclusively for the artist service and then closed before
// the request is issued. The rule service retains its own open connection so
// the violation-list queries succeed normally.
func TestHandleDashboardActionQueue_StatsError(t *testing.T) {
	t.Parallel()
	r := testDashboardRouter(t, false)

	// Open a separate database for the artist service so we can close it
	// independently of the rule service database to simulate a stats error.
	artistDB := newTestDB(t)

	// Replace the router's artist service with one backed by the new database.
	r.artistService = artist.NewService(artistDB)

	// Closing the database forces any subsequent call to GetHealthStats to
	// return an error. The rule service still uses its own open connection.
	if err := artistDB.Close(); err != nil {
		t.Fatalf("closing artist test db: %v", err)
	}

	// No violations are seeded, so the handler takes the empty-state branch
	// and calls DashboardEmptyState with healthStatsError=true.
	req := httptest.NewRequest(http.MethodGet, "/dashboard/actions", nil)
	req = withTestUser(req)
	w := httptest.NewRecorder()

	r.handleDashboardActionQueue(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	body := w.Body.String()

	// The stats-unavailable placeholder must appear in the rendered output.
	if !strings.Contains(body, "---") {
		t.Errorf("expected stats-unavailable placeholder (---) in response, got: %s", body)
	}

	// A real health percentage must NOT appear; that would indicate the error
	// path was not taken and misleading data is being shown.
	if strings.Contains(body, "0.0%") {
		t.Errorf("response must not contain health percentage when stats are unavailable; got: %s", body)
	}
}

// TestUrlValuesFromFilters verifies that the filter struct round-trips into the
// flyout's URL contract: tri-state values carry a "+" (include) or "-"
// (exclude) prefix and the same key may repeat. Includes are always emitted
// with the "+" prefix (never bare) so the flyout's client-side hydration, which
// recognizes only the prefixed forms, restores each pill's state.
func TestUrlValuesFromFilters(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input dashboardFilterParams
		// want maps each key to the exact set of values expected under it
		// (order-independent). A key absent from want must not appear at all.
		want map[string][]string
	}{
		{
			name:  "empty filters produce empty values",
			input: dashboardFilterParams{},
			want:  map[string][]string{},
		},
		{
			name: "single-include fields emit the + prefix",
			input: dashboardFilterParams{
				Search:    "indie",
				Severity:  rule.IncludeOnly("error"),
				Category:  rule.IncludeOnly("image"),
				LibraryID: rule.IncludeOnly("lib-1"),
				RuleID:    rule.IncludeOnly("rule-1"),
				Fixable:   rule.IncludeOnly("yes"),
			},
			want: map[string][]string{
				"search":     {"indie"},
				"severity":   {"+error"},
				"category":   {"+image"},
				"library_id": {"+lib-1"},
				"rule":       {"+rule-1"},
				"fixable":    {"+yes"},
			},
		},
		{
			name: "include and exclude on the same dimension both emit",
			input: dashboardFilterParams{
				Severity: rule.TriFilter{Include: []string{"error"}, Exclude: []string{"info"}},
				Category: rule.TriFilter{Exclude: []string{"nfo", "image"}},
			},
			want: map[string][]string{
				"severity": {"+error", "-info"},
				"category": {"-nfo", "-image"},
			},
		},
		{
			name: "empty dimensions are omitted so the URL stays clean",
			input: dashboardFilterParams{
				Severity: rule.IncludeOnly("warning"),
				Fixable:  rule.IncludeOnly("no"),
			},
			want: map[string][]string{
				"severity": {"+warning"},
				"fixable":  {"+no"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := urlValuesFromFilters(tc.input)
			if len(got) != len(tc.want) {
				t.Errorf("result key count = %d, want %d (got=%v)", len(got), len(tc.want), got)
			}
			for k, wantVals := range tc.want {
				gotVals := got[k]
				if !sameStringSet(gotVals, wantVals) {
					t.Errorf("key %q = %v, want %v", k, gotVals, wantVals)
				}
			}
			// Keys not in `want` must not appear.
			for k := range got {
				if _, ok := tc.want[k]; !ok {
					t.Errorf("unexpected key %q in result", k)
				}
			}
		})
	}
}

// sameStringSet reports whether a and b contain the same values regardless of
// order. Used by tri-state URL assertions where query-param order is not
// significant.
func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	counts := make(map[string]int, len(a))
	for _, v := range a {
		counts[v]++
	}
	for _, v := range b {
		counts[v]--
		if counts[v] < 0 {
			return false
		}
	}
	return true
}

// TestParseDashboardFiltersLibraryAlias verifies the legacy `library` query
// param is accepted by parseDashboardFilters as an alias for `library_id` so
// old bookmarks keep working after the rename. Under the tri-state contract
// both keys merge into the same include/exclude set rather than one overriding
// the other.
func TestParseDashboardFiltersLibraryAlias(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		raw     string
		want    []string // expected include set (order-independent)
		wantExc []string // expected exclude set (order-independent)
	}{
		{name: "canonical key", raw: "library_id=lib-1", want: []string{"lib-1"}},
		{name: "legacy alias", raw: "library=lib-2", want: []string{"lib-2"}},
		{name: "both keys merge into one set", raw: "library=legacy&library_id=canonical", want: []string{"canonical", "legacy"}},
		// The "-" prefix on the legacy alias must route the value into the
		// Exclude set, not silently drop it: asserting only Include==nil here
		// would pass even if the exclude were lost, so pin Exclude too.
		{name: "exclude prefix on alias", raw: "library=-lib-x", want: nil, wantExc: []string{"lib-x"}},
		{name: "neither set", raw: "", want: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/dashboard/actions?"+tc.raw, nil)
			lib := parseDashboardFilters(req).LibraryID
			if !sameStringSet(lib.Include, tc.want) {
				t.Errorf("LibraryID include = %v, want %v (raw=%q)", lib.Include, tc.want, tc.raw)
			}
			if !sameStringSet(lib.Exclude, tc.wantExc) {
				t.Errorf("LibraryID exclude = %v, want %v (raw=%q)", lib.Exclude, tc.wantExc, tc.raw)
			}
		})
	}
}

// TestHandleDashboardActionQueue_HXPushURL_LibraryID verifies that an HTMX
// request carrying the canonical `library_id` param triggers an HX-Push-Url
// response header that also uses `library_id` (not the legacy `library`
// key). The address-bar URL should always reflect the post-rename canonical
// key so users bookmark the new form.
func TestHandleDashboardActionQueue_HXPushURL_LibraryID(t *testing.T) {
	t.Parallel()
	r := testDashboardRouter(t, false)

	req := httptest.NewRequest(http.MethodGet, "/dashboard/actions?library_id=lib-xyz&severity=error", nil)
	req.Header.Set("HX-Request", "true")
	req = withTestUser(req)
	w := httptest.NewRecorder()
	r.handleDashboardActionQueue(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	push := w.Header().Get("HX-Push-Url")
	if push == "" {
		t.Fatalf("expected HX-Push-Url to be set on HTMX request")
	}
	// The pushed URL carries the tri-state contract: a bare request value
	// parses as include and re-emits with the "+" prefix under the canonical
	// `library_id` key. Parse the query so we compare decoded values rather
	// than matching the URL-encoded "%2B" literal.
	pushURL, err := url.Parse(push)
	if err != nil {
		t.Fatalf("HX-Push-Url is not a valid URL: %q (%v)", push, err)
	}
	pushQ := pushURL.Query()
	if got := pushQ["library_id"]; !sameStringSet(got, []string{"+lib-xyz"}) {
		t.Errorf("HX-Push-Url library_id = %v, want [+lib-xyz]; full=%q", got, push)
	}
	if got := pushQ["severity"]; !sameStringSet(got, []string{"+error"}) {
		t.Errorf("HX-Push-Url severity = %v, want [+error]; full=%q", got, push)
	}
	if _, ok := pushQ["library"]; ok {
		t.Errorf("HX-Push-Url must not emit the legacy `library` key; got %q", push)
	}
}

// TestHandleDashboardActionQueue_HXPushURL_LegacyLibraryAlias confirms that
// even when the request arrives with the legacy `library` query param, the
// handler normalizes it to `library_id` in the address-bar URL it pushes
// back. Old bookmarks keep working, but the canonical key replaces them on
// the next interaction.
func TestHandleDashboardActionQueue_HXPushURL_LegacyLibraryAlias(t *testing.T) {
	t.Parallel()
	r := testDashboardRouter(t, false)

	req := httptest.NewRequest(http.MethodGet, "/dashboard/actions?library=lib-legacy", nil)
	req.Header.Set("HX-Request", "true")
	req = withTestUser(req)
	w := httptest.NewRecorder()
	r.handleDashboardActionQueue(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	push := w.Header().Get("HX-Push-Url")
	pushURL, err := url.Parse(push)
	if err != nil {
		t.Fatalf("HX-Push-Url is not a valid URL: %q (%v)", push, err)
	}
	pushQ := pushURL.Query()
	// The legacy `library` value parses as include and re-emits under the
	// canonical `library_id` key with the "+" include prefix.
	if got := pushQ["library_id"]; !sameStringSet(got, []string{"+lib-legacy"}) {
		t.Errorf("HX-Push-Url must rewrite `library` alias to `library_id`; library_id=%v, full=%q", got, push)
	}
	if _, ok := pushQ["library"]; ok {
		t.Errorf("HX-Push-Url must NOT echo the legacy `library` key; got %q", push)
	}
}

// TestParseDashboardFiltersTriState exercises the tri-state include/exclude
// parsing across all five filter families. It verifies the "+"/"-"/bare value
// contract: "+x" -> include, "-x" -> exclude, bare "x" -> include (back-compat),
// repeated keys accumulate, and a dimension can both include and exclude values
// at once.
func TestParseDashboardFiltersTriState(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		raw         string
		wantInclude map[string][]string // dimension -> expected include set
		wantExclude map[string][]string // dimension -> expected exclude set
	}{
		{
			name:        "bare value is treated as include (back-compat)",
			raw:         "severity=error",
			wantInclude: map[string][]string{"severity": {"error"}},
		},
		{
			name:        "explicit plus prefix includes",
			raw:         "severity=%2Berror",
			wantInclude: map[string][]string{"severity": {"error"}},
		},
		{
			name:        "minus prefix excludes",
			raw:         "severity=-warning",
			wantExclude: map[string][]string{"severity": {"warning"}},
		},
		{
			// Normalization whitelist rule: when a dimension has any include,
			// it is in whitelist mode and explicit excludes are dropped to
			// neutral (matching the SQL, which ignores excludes once an include
			// IN-list is present). So "+error" wins and "-warning" is stripped.
			name:        "include plus exclude on one dimension drops the exclude (whitelist)",
			raw:         "severity=%2Berror&severity=-warning",
			wantInclude: map[string][]string{"severity": {"error"}},
			wantExclude: map[string][]string{},
		},
		{
			name:        "multi-value include accumulates",
			raw:         "category=%2Bnfo&category=%2Bimage",
			wantInclude: map[string][]string{"category": {"nfo", "image"}},
		},
		{
			name: "all five families parse independently",
			raw:  "severity=%2Berror&category=-nfo&library_id=%2Blib-1&rule=-rule_x&fixable=%2Byes",
			wantInclude: map[string][]string{
				"severity":   {"error"},
				"library_id": {"lib-1"},
				"fixable":    {"yes"},
			},
			wantExclude: map[string][]string{
				"category": {"nfo"},
				"rule":     {"rule_x"},
			},
		},
		{
			name: "empty values are skipped",
			raw:  "severity=&severity=%2Berror",
			wantInclude: map[string][]string{
				"severity": {"error"},
			},
		},
		{
			// A bare "+" or "-" with no value after the prefix must be skipped,
			// not injected as an empty-string filter value (which would build a
			// degenerate IN ('') / NOT IN ('') clause). The surviving real value
			// is the only one that lands.
			name: "bare plus and minus with no value are skipped",
			raw:  "severity=%2B&severity=-&severity=%2Berror",
			wantInclude: map[string][]string{
				"severity": {"error"},
			},
			wantExclude: map[string][]string{},
		},
		{
			// Only a bare "+" / "-" -> the dimension stays neutral (no include,
			// no exclude), so it adds no SQL clause.
			name:        "only bare prefixes leave the dimension neutral",
			raw:         "category=%2B&category=-",
			wantInclude: map[string][]string{"category": nil},
			wantExclude: map[string][]string{"category": nil},
		},
	}

	dims := func(f dashboardFilterParams) map[string]rule.TriFilter {
		return map[string]rule.TriFilter{
			"severity":   f.Severity,
			"category":   f.Category,
			"library_id": f.LibraryID,
			"rule":       f.RuleID,
			"fixable":    f.Fixable,
		}
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/dashboard/actions?"+tc.raw, nil)
			got := dims(parseDashboardFilters(req))
			for dim, f := range got {
				wantInc := tc.wantInclude[dim] // nil when not expected
				wantExc := tc.wantExclude[dim]
				if !sameStringSet(f.Include, wantInc) {
					t.Errorf("%s include = %v, want %v (raw=%q)", dim, f.Include, wantInc, tc.raw)
				}
				if !sameStringSet(f.Exclude, wantExc) {
					t.Errorf("%s exclude = %v, want %v (raw=%q)", dim, f.Exclude, wantExc, tc.raw)
				}
			}
		})
	}
}
