package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/artist"
)

// TestHandleNextActivityPage_RendersNextWhenChannelNext verifies that on the
// "next" channel GET /next/activity returns 200 and renders the next-scoped
// shell (sw-next-activity) wrapping the shared ActivityBody chrome, with the
// preserved hook ids intact (M55 #1772).
func TestHandleNextActivityPage_RendersNextWhenChannelNext(t *testing.T) {
	t.Parallel()
	r := newTestRouterFull(t)

	h := middleware.UX("next", "")(http.HandlerFunc(r.handleNextActivityPage))
	req := httptest.NewRequestWithContext(adminContext(), http.MethodGet, "/next/activity", nil)
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "sw-next-activity") {
		t.Errorf("next channel should render ActivityPageNext (sw-next-activity scope absent)")
	}
	// Hook ids that always render regardless of feed contents (they live in the
	// shared ActivityBody, not the row list). Their presence proves the next/
	// page reuses the same chrome byte-for-byte rather than forking it.
	for _, id := range []string{
		`id="activity-filter-trigger"`,
		`id="activity-content"`,
		`id="activity-date-apply"`,
		`id="activity-filters-flyout"`,
	} {
		if !strings.Contains(body, id) {
			t.Errorf("preserved hook id missing from next/ activity page: %s", id)
		}
	}
}

// TestHandleNextActivityPage_StableMode404 verifies that GET /next/activity
// returns 404 in stable mode. The UX middleware blocks /next/* paths before the
// handler runs when the lane is disabled (decision 12 in
// architecture-decisions.md).
func TestHandleNextActivityPage_StableMode404(t *testing.T) {
	t.Parallel()
	r := newTestRouterFull(t)

	h := middleware.UX("stable", "")(http.HandlerFunc(r.handleNextActivityPage))
	req := httptest.NewRequestWithContext(adminContext(), http.MethodGet, "/next/activity", nil)
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("stable mode: status = %d, want 404 (/next/* must 404 when lane is disabled)", w.Code)
	}
}

// TestHandleNextActivityPage_OptOutHeader404 verifies the handler-level
// decision-12 guard: when the lane IS enabled (next/dual mode) but the
// per-request X-Stillwater-UX: stable header opts back to the stable channel,
// checkNextChannel returns 404 rather than serving stable content.
func TestHandleNextActivityPage_OptOutHeader404(t *testing.T) {
	t.Parallel()
	r := newTestRouterFull(t)

	ctx := middleware.WithTestUXChannel(adminContext(), middleware.UXStable)
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/next/activity", nil)
	w := httptest.NewRecorder()
	r.handleNextActivityPage(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("opt-out header: status = %d, want 404 (decision 12)", w.Code)
	}
}

// TestHandleNextActivityPage_UnauthRendersLoginPage asserts that an
// unauthenticated GET /next/activity returns HTTP 200 with the login page
// rather than a 401 JSON error. The route uses wrapOptionalAuth so the
// in-handler empty-userID guard renders the login page for cookieless visitors.
func TestHandleNextActivityPage_UnauthRendersLoginPage(t *testing.T) {
	t.Parallel()
	r := newTestRouterFull(t)

	h := middleware.UX("next", "")(http.HandlerFunc(r.handleNextActivityPage))
	req := withI18nCtx(t, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/next/activity", nil))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unauthenticated request should get login page (200), got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "sw-next-activity") {
		t.Error("unauthenticated visitor must not see the activity surface")
	}
	if !strings.Contains(body, "/api/v1/auth/login") {
		t.Error("response should contain the login form action (/api/v1/auth/login)")
	}
}

// TestHandleNextActivityPage_DBError exercises the 500 branch: a closed DB
// makes historyService.ListGlobal fail inside buildActivityPageData, so the
// next/ handler must return 500 (and not attempt to render).
func TestHandleNextActivityPage_DBError(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithHistory(t)
	if err := r.db.Close(); err != nil {
		t.Fatalf("closing db: %v", err)
	}

	ctx := middleware.WithTestUXChannel(adminContext(), middleware.UXNext)
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/next/activity", nil)
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	r.handleNextActivityPage(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("closed-DB should yield 500; got %d", w.Code)
	}
}

// TestHandleActivityPage_DBError exercises the same 500 branch on the stable
// page handler, covering its !ok return path after buildActivityPageData fails.
func TestHandleActivityPage_DBError(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithHistory(t)
	if err := r.db.Close(); err != nil {
		t.Fatalf("closing db: %v", err)
	}

	req := httptest.NewRequestWithContext(adminContext(), http.MethodGet, "/activity", nil)
	w := httptest.NewRecorder()
	r.handleActivityPage(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("closed-DB should yield 500; got %d", w.Code)
	}
}

// TestHandleRevertHistory_NextActivity locks the directive-D revert seam: a
// revert triggered from /next/activity must render the ACTIVITY revert fragment
// (shared with stable /activity), not the artist-tab fragment. "/next/activity"
// contains the "/activity" substring the seam keys on, so the shared fragment is
// correct for both channels and no channel-specific branch is needed. This test
// guards against a regression that reorders or narrows that Contains check and
// accidentally routes next/ reverts to the artist-tab fragment.
func TestHandleRevertHistory_NextActivity(t *testing.T) {
	t.Parallel()
	r, artistSvc, historySvc := testRouterWithHistory(t)
	artistSvc.SetHistoryService(historySvc)

	a := addTestArtist(t, artistSvc, "Next Activity Revert Artist")
	ctx := artist.ContextWithSource(context.Background(), "manual")
	if _, err := artistSvc.UpdateField(ctx, a.ID, "biography", "next bio"); err != nil {
		t.Fatalf("UpdateField: %v", err)
	}
	changes, _, err := historySvc.List(context.Background(), a.ID, 1, 0)
	if err != nil || len(changes) == 0 {
		t.Fatalf("List: err=%v len=%d", err, len(changes))
	}
	changeID := changes[0].ID

	req := httptest.NewRequest(http.MethodPost, "/api/v1/history/"+changeID+"/revert", nil)
	req.SetPathValue("id", changeID)
	req.Header.Set("HX-Request", "true")
	req.Header.Set("HX-Current-URL", "http://localhost:1973/next/activity")
	w := httptest.NewRecorder()

	r.handleRevertHistory(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	body := w.Body.String()
	// Activity fragment markers (ActivityRevertFragment): present.
	if !strings.Contains(body, "activity-entries") {
		t.Errorf("/next/activity revert must render the activity fragment (activity-entries absent); body: %s", body)
	}
	// Artist-tab fragment markers (HistoryRevertFragment): must NOT appear.
	if strings.Contains(body, "history-change-list") {
		t.Errorf("/next/activity revert must NOT render the artist-tab fragment (history-change-list present)")
	}
}
