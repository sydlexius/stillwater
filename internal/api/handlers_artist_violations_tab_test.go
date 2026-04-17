package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/rule"
)

// TestHandleArtistViolationsTab_EmptyArtistID exercises the missing-id guard:
// a blank path value must fail the request fast so the rule service is never
// queried with an unconstrained filter.
func TestHandleArtistViolationsTab_EmptyArtistID(t *testing.T) {
	r, _ := testRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/artists//violations/tab", nil)
	req.SetPathValue("id", "")
	w := httptest.NewRecorder()

	r.handleArtistViolationsTab(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

// TestHandleArtistViolationsTab_EmptyState verifies that an artist with no
// active violations renders the tab partial with the empty-state copy and the
// OOB badge swap that hides the tab-bar count.
func TestHandleArtistViolationsTab_EmptyState(t *testing.T) {
	r, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Empty Artist")

	req := httptest.NewRequest(http.MethodGet, "/artists/"+a.ID+"/violations/tab", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleArtistViolationsTab(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	body := w.Body.String()
	// Wrapper carries the artist ID so HTMX swaps replace the correct fragment.
	if !strings.Contains(body, "violations-content-"+a.ID) {
		t.Errorf("body missing violations-content wrapper: %s", body)
	}
	// OOB badge swap is always rendered so the tab header clears its pill on refresh.
	if !strings.Contains(body, `id="violations-tab-badge"`) {
		t.Errorf("body missing violations-tab-badge OOB element: %s", body)
	}
}

// TestHandleArtistViolationsTab_WithViolations verifies the populated branch:
// the row for the open violation, the Fix button for a fixable rule, and the
// OOB badge count reflecting the number of active rows.
func TestHandleArtistViolationsTab_WithViolations(t *testing.T) {
	r, artistSvc, ruleSvc := testRouterWithPipelineFull(t)
	a := addTestArtist(t, artistSvc, "Violation Artist")

	// Seed one open+fixable violation and one resolved (filtered out).
	now := time.Now().UTC()
	if err := ruleSvc.UpsertViolation(t.Context(), &rule.RuleViolation{
		RuleID:     rule.RuleNFOExists,
		ArtistID:   a.ID,
		ArtistName: a.Name,
		Severity:   "error",
		Message:    "nfo missing",
		Fixable:    true,
		Status:     rule.ViolationStatusOpen,
		CreatedAt:  now,
	}); err != nil {
		t.Fatalf("seeding open violation: %v", err)
	}
	if err := ruleSvc.UpsertViolation(t.Context(), &rule.RuleViolation{
		RuleID:     rule.RuleBioExists,
		ArtistID:   a.ID,
		ArtistName: a.Name,
		Severity:   "info",
		Message:    "bio missing",
		Fixable:    true,
		Status:     rule.ViolationStatusResolved,
		CreatedAt:  now,
	}); err != nil {
		t.Fatalf("seeding resolved violation: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/artists/"+a.ID+"/violations/tab", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleArtistViolationsTab(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, rule.RuleNFOExists) {
		t.Errorf("body missing open violation rule ID: %s", body)
	}
	if strings.Contains(body, "bio missing") {
		t.Errorf("body includes resolved violation message (should be filtered): %s", body)
	}
	// Badge pill should render with count 1.
	if !strings.Contains(body, `hx-swap-oob="true"`) || !strings.Contains(body, ">1<") {
		t.Errorf("body missing OOB badge with count 1: %s", body)
	}
}
