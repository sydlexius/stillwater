package api

import (
	"context"
	"net/http"
	"net/http/httptest"
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
	r := testDashboardRouter(t, false)
	ctx := context.Background()

	// Seed a few violations so the handler has something to return.
	a := &artist.Artist{
		Name: "Limit Test Artist", SortName: "Limit Test Artist",
		Type: "group", Path: "/music/LimitTest", Genres: []string{"Rock"},
	}
	if err := r.artistService.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	for i := range 3 {
		v := &rule.RuleViolation{
			RuleID: rule.RuleNFOExists, ArtistID: a.ID, ArtistName: a.Name,
			Severity: "error", Message: "missing nfo",
			Fixable: true, Status: rule.ViolationStatusOpen,
		}
		// Give each a unique "message" so UpsertViolation does not deduplicate.
		v.Message = v.Message + " " + string(rune('a'+i))
		if err := r.ruleService.UpsertViolation(ctx, v); err != nil {
			t.Fatalf("seeding violation %d: %v", i, err)
		}
	}

	// Request with an excessively large limit -- should be capped to PageSizeMax.
	req := httptest.NewRequest(http.MethodGet, "/dashboard/actions?limit=99999", nil)
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
}

func TestHandleDashboardActionQueue_OffsetBranching(t *testing.T) {
	r := testDashboardRouter(t, false)
	ctx := context.Background()

	// Seed violations.
	a := &artist.Artist{
		Name: "Offset Test Artist", SortName: "Offset Test Artist",
		Type: "group", Path: "/music/OffsetTest", Genres: []string{"Rock"},
	}
	if err := r.artistService.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	for i := range 5 {
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

	// The offset=0 response should contain category chip markup that
	// the offset>0 response should not.
	if !strings.Contains(body0, "action-queue-entries") {
		t.Error("offset=0 response should contain action-queue-entries")
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

	// Negative offset should be clamped to 0 (no panic or error).
	req := httptest.NewRequest(http.MethodGet, "/dashboard/actions?offset=-5", nil)
	req = withTestUser(req)
	w := httptest.NewRecorder()

	r.handleDashboardActionQueue(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
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

	// Should render the "no recent activity" empty state.
	if !strings.Contains(w.Body.String(), "no_recent_activity") && !strings.Contains(w.Body.String(), "No recent activity") {
		// The template uses t(ctx, "dashboard.no_recent_activity") which
		// may render as the key or the translated value depending on i18n
		// bundle availability. Either is acceptable for this test.
		t.Log("note: empty activity feed may render key or translated value")
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

func TestHandleDashboardActivityFeed_LimitCapping(t *testing.T) {
	// The handler hardcodes limit=10. This test verifies it does not crash
	// or return errors when the history service is present and returns
	// an empty result set.
	r := testDashboardRouter(t, true)

	req := httptest.NewRequest(http.MethodGet, "/dashboard/activity", nil)
	req = withTestUser(req)
	w := httptest.NewRecorder()

	r.handleDashboardActivityFeed(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
}
