package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/database"
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

// TestHandleDashboardActionQueue_StatsError verifies that when GetHealthStats
// returns an error the handler still returns 200 OK and renders the
// stats-unavailable placeholder ("---") instead of a percentage like "0.0%".
//
// To isolate the failure to just the health-stats query, a second in-memory
// database is opened exclusively for the artist service and then closed before
// the request is issued. The rule service retains its own open connection so
// the violation-list queries succeed normally.
func TestHandleDashboardActionQueue_StatsError(t *testing.T) {
	r := testDashboardRouter(t, false)

	// Open a separate in-memory database for the artist service so we can
	// close it independently of the rule service database.
	artistDB, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("opening artist test db: %v", err)
	}
	if err := database.Migrate(artistDB); err != nil {
		_ = artistDB.Close()
		t.Fatalf("migrating artist test db: %v", err)
	}

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

func TestUrlValuesFromFilters(t *testing.T) {
	tests := []struct {
		name  string
		input dashboardFilterParams
		want  map[string]string // expected single-value per key; absent = key must not be set
	}{
		{
			name:  "empty filters produce empty values",
			input: dashboardFilterParams{},
			want:  map[string]string{},
		},
		{
			name: "all fields populated map to their keys",
			input: dashboardFilterParams{
				Search:    "indie",
				Severity:  "error",
				Category:  "image",
				LibraryID: "lib-1",
				RuleID:    "rule-1",
				Fixable:   "yes",
			},
			want: map[string]string{
				"search":   "indie",
				"severity": "error",
				"category": "image",
				"library":  "lib-1",
				"rule":     "rule-1",
				"fixable":  "yes",
			},
		},
		{
			name: "empty fields are omitted so the URL stays clean",
			input: dashboardFilterParams{
				Severity: "warning",
				Fixable:  "no",
			},
			want: map[string]string{
				"severity": "warning",
				"fixable":  "no",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := urlValuesFromFilters(tc.input)
			if len(got) != len(tc.want) {
				t.Errorf("result size = %d, want %d (got=%v)", len(got), len(tc.want), got)
			}
			for k, v := range tc.want {
				if got.Get(k) != v {
					t.Errorf("key %q = %q, want %q", k, got.Get(k), v)
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

func TestDashboardPushURLFromFilters(t *testing.T) {
	tests := []struct {
		name     string
		basePath string
		input    dashboardFilterParams
		want     string
	}{
		{
			name:     "no filters at root base path",
			basePath: "",
			input:    dashboardFilterParams{},
			want:     "/",
		},
		{
			name:     "no filters under sub-path base",
			basePath: "/stillwater",
			input:    dashboardFilterParams{},
			want:     "/stillwater/",
		},
		{
			name:     "single filter at root",
			basePath: "",
			input:    dashboardFilterParams{Severity: "warning"},
			want:     "/?severity=warning",
		},
		{
			name:     "single filter under sub-path preserves base",
			basePath: "/stillwater",
			input:    dashboardFilterParams{Category: "image"},
			want:     "/stillwater/?category=image",
		},
		{
			name:     "multi-filter encoding is deterministic",
			basePath: "",
			input: dashboardFilterParams{
				Search:   "bad artist",
				Severity: "error",
			},
			want: "/?search=bad+artist&severity=error",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := dashboardPushURLFromFilters(tc.basePath, tc.input)
			if got != tc.want {
				t.Errorf("dashboardPushURLFromFilters(%q, %+v) = %q, want %q",
					tc.basePath, tc.input, got, tc.want)
			}
		})
	}
}
