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

// markOnboardingComplete writes the onboarding.completed=true setting so the
// next/ dashboard handler passes its onboarding guard and renders the page
// instead of redirecting to the setup wizard.
func markOnboardingComplete(t *testing.T, r *Router) {
	t.Helper()
	if _, err := r.db.ExecContext(context.Background(),
		`INSERT OR REPLACE INTO settings (key, value) VALUES ('onboarding.completed', 'true')`); err != nil {
		t.Fatalf("marking onboarding complete: %v", err)
	}
}

// seedFixableViolations creates fixCount fixable and manualCount non-fixable
// open violations, each on its own artist (UpsertViolation deduplicates on
// rule_id + artist_id, so distinct artists guarantee distinct rows). The total
// fixable/non-fixable counts are what CountActiveViolationsByFixable returns
// and what the Auto-fixable / Needs-you header bubbles render.
func seedFixableViolations(t *testing.T, r *Router, fixCount, manualCount int) {
	t.Helper()
	ctx := context.Background()
	seed := func(prefix string, n int, fixable bool, ruleID, severity string) {
		for i := range n {
			suffix := prefix + string(rune('A'+i))
			a := &artist.Artist{
				Name:     "Next " + suffix,
				SortName: "Next " + suffix,
				Type:     "group",
				Path:     "/music/Next" + suffix,
				Genres:   []string{"Rock"},
			}
			if err := r.artistService.Create(ctx, a); err != nil {
				t.Fatalf("creating artist %s: %v", suffix, err)
			}
			v := &rule.RuleViolation{
				RuleID: ruleID, ArtistID: a.ID, ArtistName: a.Name,
				Severity: severity, Message: "seeded " + suffix,
				Fixable: fixable, Status: rule.ViolationStatusOpen,
			}
			if err := r.ruleService.UpsertViolation(ctx, v); err != nil {
				t.Fatalf("seeding violation %s: %v", suffix, err)
			}
		}
	}
	seed("fix", fixCount, true, rule.RuleNFOExists, "error")
	seed("man", manualCount, false, rule.RuleThumbExists, "warning")
}

// nextDashboardHandler wraps handleNextDashboardPage in the UX middleware in
// "next" mode so a request to /next/ resolves to the UXNext channel exactly as
// production does (the /next lane is a path opt-in). Returns the composed
// handler ready for httptest.
func nextDashboardHandler(r *Router) http.Handler {
	return middleware.UX("next", "")(http.HandlerFunc(r.handleNextDashboardPage))
}

// TestHandleNextDashboardPage_HappyPath verifies that on the "next" channel
// with onboarding complete and seeded violations, GET /next/ returns 200, sets
// the next-channel response header, and renders the dashboard with the REAL
// Auto-fixable / Needs-you counts (not placeholders) plus the bubble links that
// scope the queue to fixable=yes / fixable=no.
func TestHandleNextDashboardPage_HappyPath(t *testing.T) {
	t.Parallel()
	r := testDashboardRouter(t, false)
	markOnboardingComplete(t, r)
	// 3 fixable, 2 non-fixable so the rendered counts are unambiguous and not
	// equal to each other (guards against a swapped fixable/needs-you wiring).
	seedFixableViolations(t, r, 3, 2)

	req := httptest.NewRequest(http.MethodGet, "/next/", nil)
	req = withTestUser(req)
	w := httptest.NewRecorder()
	nextDashboardHandler(r).ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	if got := w.Header().Get("X-Stillwater-UX"); got != "next" {
		t.Fatalf("X-Stillwater-UX = %q, want next", got)
	}
	body := w.Body.String()

	// The header bubbles link to the queue scoped by fixable state; these are
	// next/-specific markers that only DashboardPageNext renders.
	if !strings.Contains(body, "/next/?fixable=yes") {
		t.Errorf("expected Auto-fixable bubble link (/next/?fixable=yes) in body")
	}
	if !strings.Contains(body, "/next/?fixable=no") {
		t.Errorf("expected Needs-you bubble link (/next/?fixable=no) in body")
	}
	// The bubble labels (rendered as i18n keys or their translations).
	if !strings.Contains(body, "dashboard.bubble_auto_fixable") && !strings.Contains(body, "Auto-fixable") {
		t.Errorf("expected Auto-fixable bubble label in body")
	}
	if !strings.Contains(body, "dashboard.bubble_needs_you") && !strings.Contains(body, "Needs you") {
		t.Errorf("expected Needs-you bubble label in body")
	}
	// On the happy path the counts are real numbers, so the fixable-counts
	// placeholder must NOT appear in a bubble value slot. We assert the rendered
	// counts >span>3</span> and >span>2</span> are present by checking the
	// numeric text appears in the body.
	if !strings.Contains(body, ">3<") {
		t.Errorf("expected Auto-fixable count 3 in rendered body")
	}
	if !strings.Contains(body, ">2<") {
		t.Errorf("expected Needs-you count 2 in rendered body")
	}

	// Keyboard action-key adoption (#1790): the page wires the / f r action keys
	// declaratively for the shared helper (#1789) via data-sw-shortcut on the
	// search input, filter trigger, and run-rules button (replacing the prior
	// bespoke keydown listener). Assert all three are present.
	for _, marker := range []string{
		`data-sw-shortcut="/"`,
		`data-sw-shortcut="f"`,
		`data-sw-shortcut="r"`,
	} {
		if !strings.Contains(body, marker) {
			t.Errorf("expected page action-key attribute %q in body", marker)
		}
	}
	// The contextual u key is registered via swKeyboardShortcuts.onContext (a
	// conditional key); the bespoke single-key keydown listener must be gone.
	if !strings.Contains(body, "swKeyboardShortcuts.onContext") {
		t.Errorf("expected onContext registration for the u contextual key in body")
	}
	if strings.Contains(body, "__swNextDashKbd") {
		t.Errorf("bespoke keydown handler (__swNextDashKbd) should be removed after helper adoption")
	}
}

// TestHandleNextDashboardPage_FixableCountsError verifies the fixable-counts
// fallback: when the rule service's database is closed (so
// CountActiveViolationsByFixable errors) the handler still returns 200 and the
// template renders the "---" placeholder for the count bubbles rather than a
// misleading 0 or a 500. The artist service keeps its own open connection so
// health stats still succeed, isolating the failure to the counts query.
func TestHandleNextDashboardPage_FixableCountsError(t *testing.T) {
	t.Parallel()
	r := testDashboardRouter(t, false)
	markOnboardingComplete(t, r)

	// Replace the rule service with one backed by a database we then close,
	// forcing CountActiveViolationsByFixable to error. The artist service keeps
	// its original open connection so GetHealthStats still succeeds.
	ruleDB := newTestDB(t)
	r.ruleService = rule.NewService(ruleDB)
	if err := ruleDB.Close(); err != nil {
		t.Fatalf("closing rule test db: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/next/", nil)
	req = withTestUser(req)
	w := httptest.NewRecorder()
	nextDashboardHandler(r).ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	body := w.Body.String()
	// The bubble links still render (only the values fall back).
	if !strings.Contains(body, "/next/?fixable=yes") {
		t.Errorf("expected Auto-fixable bubble link even on counts error")
	}
	// Scope the placeholder assertion to the Auto-fixable AND Needs-you bubbles
	// specifically: a bare strings.Contains(body, "---") false-passes because
	// the Last-evaluated bubble always renders "---" as its initial value (the
	// JS fills it in client-side). Each bubble is an <a> element whose href is
	// unique, so we slice out that bubble's HTML (from its href to the closing
	// </a>) and assert "---" lives INSIDE the fixable-counts bubbles.
	if !widgetContainsPlaceholder(body, "/next/?fixable=yes") {
		t.Errorf("expected count-unavailable placeholder (---) inside the Auto-fixable bubble when counts error")
	}
	if !widgetContainsPlaceholder(body, "/next/?fixable=no") {
		t.Errorf("expected count-unavailable placeholder (---) inside the Needs-you bubble when counts error")
	}
}

// TestHandleNextDashboardPage_HealthStatsError verifies the health-stats
// fallback path: when the artist service's database is closed (so
// GetHealthStats errors) the handler still returns 200 and renders the page.
// The rule service keeps its open connection so the count bubbles still render
// real numbers; only the health bubble falls back to its placeholder.
func TestHandleNextDashboardPage_HealthStatsError(t *testing.T) {
	t.Parallel()
	r := testDashboardRouter(t, false)
	markOnboardingComplete(t, r)

	artistDB := newTestDB(t)
	r.artistService = artist.NewService(artistDB)
	if err := artistDB.Close(); err != nil {
		t.Fatalf("closing artist test db: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/next/", nil)
	req = withTestUser(req)
	w := httptest.NewRecorder()
	nextDashboardHandler(r).ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	body := w.Body.String()
	// Scope the placeholder to the HEALTH bubble specifically. A bare
	// strings.Contains(body, "---") false-passes here: the Last-evaluated bubble
	// always renders "---" initially, and on a health-stats error the Artists
	// bubble ALSO falls back to "---". The health bubble is the only one carrying
	// the unique "health-ring" SVG class, and it ends where the Artists bubble's
	// link begins, so we slice that window and assert "---" lives inside it.
	healthWidget := htmlBetween(body, "health-ring", "/next/artists")
	if healthWidget == "" {
		t.Fatalf("could not locate the health bubble (health-ring .. /next/artists) in body")
	}
	if !strings.Contains(healthWidget, "---") {
		t.Errorf("expected health-unavailable placeholder (---) inside the health bubble when health stats error")
	}
}

// widgetContainsPlaceholder reports whether the "---" unavailable placeholder
// appears inside the dashboard bubble whose <a> element has the given href.
// Each header bubble is an <a> with a unique href; this slices that anchor's
// HTML (from the href attribute to its closing </a>) so a placeholder rendered
// by an UNRELATED bubble (e.g. the Last-evaluated bubble's always-"---" initial
// value) cannot false-pass the assertion.
func widgetContainsPlaceholder(body, href string) bool {
	widget := htmlBetween(body, href, "</a>")
	return widget != "" && strings.Contains(widget, "---")
}

// htmlBetween returns the substring of body starting at the first occurrence of
// start and ending at the first occurrence of end that follows it (end
// exclusive). It returns "" when either marker is absent so callers can fail
// loudly rather than asserting against an empty window. It is a deliberately
// tiny, dependency-free way to scope a substring assertion to a single rendered
// widget instead of the whole page.
func htmlBetween(body, start, end string) string {
	i := strings.Index(body, start)
	if i < 0 {
		return ""
	}
	rest := body[i:]
	j := strings.Index(rest, end)
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// TestHandleNextDashboardPage_Unauthorized verifies that on the "next" channel
// with no authenticated user in context the handler renders the login page
// rather than the dashboard.
func TestHandleNextDashboardPage_Unauthorized(t *testing.T) {
	t.Parallel()
	r := testDashboardRouter(t, false)
	markOnboardingComplete(t, r)

	// No withTestUser: the auth check sees an empty user ID.
	req := httptest.NewRequest(http.MethodGet, "/next/", nil)
	w := httptest.NewRecorder()
	nextDashboardHandler(r).ServeHTTP(w, req)

	// renderLoginPage returns 200 with the login form, not the dashboard.
	body := w.Body.String()
	if strings.Contains(body, "/next/?fixable=yes") {
		t.Errorf("unauthenticated request must not render the dashboard bubbles")
	}
}

// TestHandleNextDashboardPage_OnboardingIncomplete verifies that when
// onboarding is not complete the handler redirects to the setup wizard instead
// of rendering the dashboard.
func TestHandleNextDashboardPage_OnboardingIncomplete(t *testing.T) {
	t.Parallel()
	r := testDashboardRouter(t, false)
	// Deliberately do NOT mark onboarding complete.

	req := httptest.NewRequest(http.MethodGet, "/next/", nil)
	req = withTestUser(req)
	w := httptest.NewRecorder()
	nextDashboardHandler(r).ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d (redirect to wizard)", w.Code, http.StatusSeeOther)
	}
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "/setup/wizard") {
		t.Errorf("Location = %q, want it to contain /setup/wizard", loc)
	}
}

// TestHandleNextDashboardPage_StableMode404 verifies that GET /next/ in stable
// mode returns 404 (the lane is off -- /next/* is not reachable when disabled).
func TestHandleNextDashboardPage_StableMode404(t *testing.T) {
	t.Parallel()
	r := testDashboardRouter(t, false)
	markOnboardingComplete(t, r)

	h := middleware.UX("stable", "")(http.HandlerFunc(r.handleNextDashboardPage))
	req := httptest.NewRequest(http.MethodGet, "/next/", nil)
	req = withTestUser(req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d (stable mode must 404 /next/ routes); body: %s", w.Code, http.StatusNotFound, w.Body.String())
	}
}

// TestHandleNextDashboardPage_OptOutHeader404 verifies the handler-level
// decision-12 guard: when the lane IS enabled (next/dual mode) but the per-request
// X-Stillwater-UX: stable header opts back to the stable channel, the handler
// returns 404. This is distinct from the middleware-level stable-mode 404
// (TestHandleNextDashboardPage_StableMode404) which fires before the handler runs.
// Here the channel is injected directly via WithTestUXChannel, simulating the
// header opt-out scenario.
func TestHandleNextDashboardPage_OptOutHeader404(t *testing.T) {
	t.Parallel()
	r := testDashboardRouter(t, false)
	markOnboardingComplete(t, r)

	ctx := middleware.WithTestUXChannel(context.Background(), middleware.UXStable)
	ctx = middleware.WithTestUserID(ctx, "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/next/", nil)
	w := httptest.NewRecorder()
	r.handleNextDashboardPage(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("opt-out header: status = %d, want 404 (decision 12)", w.Code)
	}
}

// TestHandleNextDashboardPage_InitialQueryForwarded verifies that recognized
// filter query params are forwarded into the initial HTMX load so a bookmarked
// /next/?severity=warning opens with that filter applied. The forwarded query
// is embedded in the rendered page (buildDashboardInitialQuery shared with
// handleIndex), so the encoded severity value appears in the body.
func TestHandleNextDashboardPage_InitialQueryForwarded(t *testing.T) {
	t.Parallel()
	r := testDashboardRouter(t, false)
	markOnboardingComplete(t, r)
	seedFixableViolations(t, r, 1, 1)

	req := httptest.NewRequest(http.MethodGet, "/next/?severity=warning", nil)
	req = withTestUser(req)
	w := httptest.NewRecorder()
	nextDashboardHandler(r).ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	body := w.Body.String()
	// Assert the CONCRETE forwarded query fragment buildDashboardInitialQuery
	// emits for the selected severity, not just the bare key "severity" (which
	// the static filter-flyout UI text already contains, so a key-only check
	// false-passes). The handler embeds the initial query both on the action
	// queue's hx-get URL and its data-queue-query carrier, so the encoded
	// "severity=warning" pair must appear verbatim in the rendered page.
	if !strings.Contains(body, "severity=warning") {
		t.Errorf("expected forwarded filter fragment %q in rendered initial query; body did not contain it", "severity=warning")
	}
}
