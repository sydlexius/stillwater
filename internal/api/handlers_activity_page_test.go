package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
)

// TestHandleActivityPage_RendersPromotedShell verifies that GET /activity
// returns 200 and renders the promoted activity screen (sw-next-activity scope)
// wrapping the shared ActivityBody chrome, with the preserved hook ids intact
// (M55 #1772; promoted to the canonical /activity in #1757 PR-5).
func TestHandleActivityPage_RendersPromotedShell(t *testing.T) {
	t.Parallel()
	r := newTestRouterFull(t)

	req := httptest.NewRequestWithContext(adminContext(), http.MethodGet, "/activity", nil)
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	r.handleActivityPage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "sw-next-activity") {
		t.Errorf("promoted activity page should render the sw-next-activity scope")
	}
	// Hook ids that always render regardless of feed contents (they live in the
	// shared ActivityBody, not the row list). Their presence proves the promoted
	// page reuses the same chrome byte-for-byte rather than forking it.
	for _, id := range []string{
		`id="activity-filter-trigger"`,
		`id="activity-content"`,
		`id="activity-date-apply"`,
		`id="activity-filters-flyout"`,
	} {
		if !strings.Contains(body, id) {
			t.Errorf("preserved hook id missing from activity page: %s", id)
		}
	}

	// #1757 PR-5 fix-round: the promoted page must set SW_IS_NEXT_PAGE so
	// keyboard.js's isNextPage() registers the Global cheat-sheet/g-leader
	// shortcuts here, and it must render before the keyboard.js script tag.
	flagIdx := strings.Index(body, "window.SW_IS_NEXT_PAGE = true;")
	kbdIdx := strings.Index(body, "/static/js/keyboard.js")
	if flagIdx == -1 || kbdIdx == -1 || flagIdx >= kbdIdx {
		t.Errorf("SW_IS_NEXT_PAGE flag (idx %d) must appear before the keyboard.js script tag (idx %d)", flagIdx, kbdIdx)
	}
}

// TestHandleActivityPage_UnauthRendersLoginPage asserts that an unauthenticated
// GET /activity returns HTTP 200 with the login page rather than a 401 JSON
// error. The route uses wrapOptionalAuth so the in-handler empty-userID guard
// renders the login page for cookieless visitors.
func TestHandleActivityPage_UnauthRendersLoginPage(t *testing.T) {
	t.Parallel()
	r := newTestRouterFull(t)

	req := withI18nCtx(t, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/activity", nil))
	w := httptest.NewRecorder()
	r.handleActivityPage(w, req)

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

// TestHandleActivityPage_DBError exercises the 500 branch: a closed DB makes
// historyService.ListGlobal fail inside buildActivityPageData, covering the
// handler's !ok return path.
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

// TestHandleRevertHistory_ActivityURL locks the directive-D revert seam: a
// revert triggered from the activity feed must render the ACTIVITY revert
// fragment (shared with /activity), not the artist-tab fragment. The HX-Current-
// URL's "/activity" substring is what the seam keys on, so the shared fragment
// is correct and no channel-specific branch is needed. This test guards against
// a regression that reorders or narrows that Contains check and accidentally
// routes activity reverts to the artist-tab fragment.
func TestHandleRevertHistory_ActivityURL(t *testing.T) {
	t.Parallel()
	r, artistSvc, historySvc := testRouterWithHistory(t)
	artistSvc.SetHistoryService(historySvc)

	a := addTestArtist(t, artistSvc, "Activity Revert Artist")
	ctx := artist.ContextWithSource(context.Background(), "manual")
	if _, err := artistSvc.UpdateField(ctx, a.ID, "biography", "activity bio"); err != nil {
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
	req.Header.Set("HX-Current-URL", "http://localhost:1973/activity")
	w := httptest.NewRecorder()

	r.handleRevertHistory(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	body := w.Body.String()
	// Activity fragment markers (ActivityRevertFragment): present.
	if !strings.Contains(body, "activity-entries") {
		t.Errorf("activity revert must render the activity fragment (activity-entries absent); body: %s", body)
	}
	// Artist-tab fragment markers (HistoryRevertFragment): must NOT appear.
	if strings.Contains(body, "history-change-list") {
		t.Errorf("activity revert must NOT render the artist-tab fragment (history-change-list present)")
	}
}
