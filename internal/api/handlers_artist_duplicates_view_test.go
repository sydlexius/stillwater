package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/artist"
)

// TestBuildArtistDuplicatesView_RecommendsSurvivor pins the contract between
// the detection layer and the page view-model: exactly one member per group
// carries Recommended=true, and the reason string mirrors what
// artist.ChooseSurvivor returns. The duplicates UI uses both fields to mark
// the recommended row's badge and tooltip; if this drifts, the user can no
// longer tell which artist the merge endpoint will pick by default.
func TestBuildArtistDuplicatesView_RecommendsSurvivor(t *testing.T) {
	groups := []artist.NearDuplicateGroup{
		{
			Key:    "the cure",
			Reason: "name_key",
			Members: []artist.NearDuplicateArtist{
				// Non-canonical basename; would lose precedence-a.
				{ID: "id-a", Name: "The Cure", Path: "/music/Cure"},
				// Canonical basename ("The Cure"); should win.
				{ID: "id-b", Name: "The Cure", Path: "/music/The Cure"},
			},
		},
	}

	view := buildArtistDuplicatesView(groups, "prefix")
	if len(view.Groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(view.Groups))
	}
	g := view.Groups[0]
	if len(g.Members) != 2 {
		t.Fatalf("expected 2 members, got %d", len(g.Members))
	}

	var recommended int
	for _, m := range g.Members {
		if m.Recommended {
			recommended++
			if m.ID != "id-b" {
				t.Errorf("recommended member id = %q, want %q", m.ID, "id-b")
			}
			if m.RecommendedReason != "canonical_basename" {
				t.Errorf("recommended reason = %q, want %q",
					m.RecommendedReason, "canonical_basename")
			}
		}
	}
	if recommended != 1 {
		t.Errorf("recommended count = %d, want 1", recommended)
	}
}

// TestBuildArtistDuplicatesView_EmptyGroups guards the no-duplicates path:
// the view must round-trip an empty slice without panicking on a missing
// recommended survivor (ChooseSurvivor returns "" for empty members).
func TestBuildArtistDuplicatesView_EmptyGroups(t *testing.T) {
	view := buildArtistDuplicatesView(nil, "")
	if view.Groups == nil {
		t.Errorf("Groups slice should be non-nil even when empty")
	}
	if len(view.Groups) != 0 {
		t.Errorf("expected 0 groups, got %d", len(view.Groups))
	}
}

// TestHandleDuplicates_Empty wires handleDuplicates and confirms it returns
// a 200 with an empty JSON array when no near-duplicate groups exist. The
// handler powers both /api/v1/artists/duplicates (deprecated alias) and
// /api/v1/reports/duplicates (canonical after the #1615 IA move) -- this
// single call covers both operationIds in the openapi coverage gate.
func TestHandleDuplicates_Empty(t *testing.T) {
	r, _, _ := mergeTestRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/reports/duplicates", nil)
	rec := httptest.NewRecorder()

	r.handleDuplicates(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var got []artist.DuplicateGroup
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal body: %v\nraw: %s", err, rec.Body.String())
	}
	if len(got) != 0 {
		t.Errorf("len(groups) = %d, want 0 (empty DB)", len(got))
	}
}

// TestHandleArtistDuplicatesPage_UnauthRendersLoginPage asserts that an
// unauthenticated GET /reports/duplicates returns HTTP 200 with the login page
// rather than a 401 JSON error. The route uses wrapOptionalAuth so
// requireForeignAdmin -> renderLoginPage runs for cookieless visitors.
func TestHandleArtistDuplicatesPage_UnauthRendersLoginPage(t *testing.T) {
	t.Parallel()
	r := newTestRouterFull(t)

	req := withI18nCtx(t, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/reports/duplicates", nil))
	w := httptest.NewRecorder()
	r.handleArtistDuplicatesPage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unauthenticated request should get login page (200), got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "artist-duplicates-table") {
		t.Error("unauthenticated visitor must not see the duplicates table")
	}
	if !strings.Contains(body, "/api/v1/auth/login") {
		t.Error("login page must have the login form action (/api/v1/auth/login)")
	}
	// Structural proof: both a username field and a password field must be
	// present - confirming this is the login form, not just any page that
	// happens to mention the auth endpoint.
	if !strings.Contains(body, `name="username"`) {
		t.Error("login page must include a username input field (name=username)")
	}
	if !strings.Contains(body, `type="password"`) {
		t.Error("login page must include a password input field (type=password)")
	}
}

// TestHandleArtistDuplicatesPage_AuthenticatedRendersPage is the
// authenticated-path regression test for handleArtistDuplicatesPage. An admin
// request must reach the real duplicates page, not the login render. This
// guards the wrapAuth change introduced in #1941: adding the auth gate must
// not break the authed path.
func TestHandleArtistDuplicatesPage_AuthenticatedRendersPage(t *testing.T) {
	t.Parallel()
	r := newTestRouterFull(t)

	ctx := middleware.WithTestUserID(context.Background(), "test-admin")
	ctx = middleware.WithTestRole(ctx, "administrator")
	req := withI18nCtx(t, httptest.NewRequestWithContext(ctx, http.MethodGet, "/reports/duplicates", nil))
	w := httptest.NewRecorder()
	r.handleArtistDuplicatesPage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("authenticated admin request should get 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// The real page must render the duplicates table container, not the
	// login form.
	if !strings.Contains(body, "artist-duplicates-table") {
		t.Error("authenticated admin must see the artist-duplicates-table in the response")
	}
	if strings.Contains(body, `type="password"`) {
		t.Error("authenticated admin must not see a login password field")
	}
}
