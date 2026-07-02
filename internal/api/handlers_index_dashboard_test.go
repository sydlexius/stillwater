package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/library"
	"github.com/sydlexius/stillwater/internal/rule"
)

// These tests cover handleIndex rendering the canonical dashboard page
// (templates.IndexPage, promoted from the former next/ channel in M55 #1757
// PR-2). They were retargeted from the deleted handleNextDashboardPage tests:
// the same guard paths (auth, onboarding) and the same header-bubble
// fallbacks now apply to GET /.

// seedIndexAdmin creates a local admin user so handleIndex's HasUsers gate
// passes and the handler proceeds to the auth/onboarding checks instead of
// rendering the first-run setup page.
func seedIndexAdmin(t *testing.T, r *Router) {
	t.Helper()
	if _, err := r.authService.CreateLocalUser(context.Background(),
		"indexadmin", "password123", "Index Admin", "administrator", ""); err != nil {
		t.Fatalf("seeding admin user: %v", err)
	}
}

// markOnboardingComplete writes the onboarding.completed=true setting so
// handleIndex passes its onboarding guard and renders the dashboard instead
// of redirecting to the setup wizard.
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

// indexDashboardRouter builds the dashboard test router with the HasUsers gate
// satisfied, ready for handleIndex requests.
func indexDashboardRouter(t *testing.T) *Router {
	t.Helper()
	r := testDashboardRouter(t, false)
	seedIndexAdmin(t, r)
	return r
}

// TestHandleIndex_HappyPath verifies that with onboarding complete and seeded
// violations, GET / returns 200 and renders the promoted dashboard with the
// REAL Auto-fixable / Needs-you counts (not placeholders) plus the bubble
// links that scope the queue to fixable=yes / fixable=no on the CANONICAL
// root path (no /next/ URLs remain in the promoted page, #1894).
func TestHandleIndex_HappyPath(t *testing.T) {
	t.Parallel()
	r := indexDashboardRouter(t)
	markOnboardingComplete(t, r)
	// 3 fixable, 2 non-fixable so the rendered counts are unambiguous and not
	// equal to each other (guards against a swapped fixable/needs-you wiring).
	seedFixableViolations(t, r, 3, 2)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = withTestUser(req)
	w := httptest.NewRecorder()
	r.handleIndex(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	body := w.Body.String()

	// The header bubbles link to the queue scoped by fixable state, on the
	// canonical root (the promoted page must carry NO /next/ hrefs).
	if !strings.Contains(body, `/?fixable=yes`) {
		t.Errorf("expected Auto-fixable bubble link (/?fixable=yes) in body")
	}
	if !strings.Contains(body, `/?fixable=no`) {
		t.Errorf("expected Needs-you bubble link (/?fixable=no) in body")
	}
	// #1894 fold: the promoted DASHBOARD templates carry no /next/ hrefs (the
	// artist links, bubble links, and JS URL builders were all retargeted to
	// canonical paths). The page shell (sidebar/layout, promoted in PR-1) still
	// links /next/ for screens that have not yet cut over (foreign-files, logs,
	// preferences-drawer), so a full-body assertion would false-fail; the
	// dashboard-owned assertions above (canonical /?fixable links) plus the
	// artist-link check below cover the dashboard's own surface.
	if strings.Contains(body, "/next/artists") {
		t.Errorf("promoted dashboard must not link artist rows to /next/artists (#1894)")
	}
	// The bubble labels (rendered as i18n keys or their translations).
	if !strings.Contains(body, "dashboard.bubble_auto_fixable") && !strings.Contains(body, "Auto-fixable") {
		t.Errorf("expected Auto-fixable bubble label in body")
	}
	if !strings.Contains(body, "dashboard.bubble_needs_you") && !strings.Contains(body, "Needs you") {
		t.Errorf("expected Needs-you bubble label in body")
	}
	// On the happy path the counts are real numbers rendered inside each
	// bubble's <a> element. Scope each count assertion to ITS bubble by slicing
	// the anchor's HTML (from its unique href to the closing </a>), so a swapped
	// fixable/needs-you wiring (2 in the Auto-fixable bubble, 3 in Needs-you)
	// FAILS: a whole-body Contains(">3<") && Contains(">2<") check would still
	// pass on a swap because both numbers appear somewhere in the page.
	fixableBubble := htmlBetween(body, `/?fixable=yes`, "</a>")
	if fixableBubble == "" {
		t.Fatalf("could not locate the Auto-fixable bubble (/?fixable=yes .. </a>) in body")
	}
	if !strings.Contains(fixableBubble, ">3<") {
		t.Errorf("expected Auto-fixable count 3 inside the /?fixable=yes bubble; got: %s", fixableBubble)
	}
	needsYouBubble := htmlBetween(body, `/?fixable=no`, "</a>")
	if needsYouBubble == "" {
		t.Fatalf("could not locate the Needs-you bubble (/?fixable=no .. </a>) in body")
	}
	if !strings.Contains(needsYouBubble, ">2<") {
		t.Errorf("expected Needs-you count 2 inside the /?fixable=no bubble; got: %s", needsYouBubble)
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

// TestHandleIndex_FixableCountsError verifies the fixable-counts fallback:
// when the rule service's database is closed (so CountActiveViolationsByFixable
// errors) the handler still returns 200 and the template renders the "---"
// placeholder for the count bubbles rather than a misleading 0 or a 500. The
// artist service keeps its own open connection so health stats still succeed,
// isolating the failure to the counts query.
func TestHandleIndex_FixableCountsError(t *testing.T) {
	t.Parallel()
	r := indexDashboardRouter(t)
	markOnboardingComplete(t, r)

	// Replace the rule service with one backed by a database we then close,
	// forcing CountActiveViolationsByFixable to error. The artist service keeps
	// its original open connection so GetHealthStats still succeeds.
	ruleDB := newTestDB(t)
	r.ruleService = rule.NewService(ruleDB)
	if err := ruleDB.Close(); err != nil {
		t.Fatalf("closing rule test db: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = withTestUser(req)
	w := httptest.NewRecorder()
	r.handleIndex(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	body := w.Body.String()
	// The bubble links still render (only the values fall back).
	if !strings.Contains(body, `/?fixable=yes`) {
		t.Errorf("expected Auto-fixable bubble link even on counts error")
	}
	// Scope the placeholder assertion to the Auto-fixable AND Needs-you bubbles
	// specifically: a bare strings.Contains(body, "---") false-passes because
	// the Last-evaluated bubble always renders "---" as its initial value (the
	// JS fills it in client-side). Each bubble is an <a> element whose href is
	// unique, so we slice out that bubble's HTML (from its href to the closing
	// </a>) and assert "---" lives INSIDE the fixable-counts bubbles.
	if !widgetContainsPlaceholder(body, "/?fixable=yes") {
		t.Errorf("expected count-unavailable placeholder (---) inside the Auto-fixable bubble when counts error")
	}
	if !widgetContainsPlaceholder(body, "/?fixable=no") {
		t.Errorf("expected count-unavailable placeholder (---) inside the Needs-you bubble when counts error")
	}
}

// TestHandleIndex_HealthStatsError verifies the health-stats fallback path:
// when the artist service's database is closed (so GetHealthStats errors) the
// handler still returns 200 and renders the page. The rule service keeps its
// open connection so the count bubbles still render real numbers; only the
// health bubble falls back to its placeholder.
func TestHandleIndex_HealthStatsError(t *testing.T) {
	t.Parallel()
	r := indexDashboardRouter(t)
	markOnboardingComplete(t, r)

	artistDB := newTestDB(t)
	r.artistService = artist.NewService(artistDB)
	if err := artistDB.Close(); err != nil {
		t.Fatalf("closing artist test db: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = withTestUser(req)
	w := httptest.NewRecorder()
	r.handleIndex(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	body := w.Body.String()
	// Scope the placeholder to the HEALTH bubble specifically. A bare
	// strings.Contains(body, "---") false-passes here: the Last-evaluated bubble
	// always renders "---" initially, and on a health-stats error the Artists
	// bubble ALSO falls back to "---". The health bubble is the only one carrying
	// the unique "health-ring" SVG class, and it ends where the Artists bubble's
	// link begins ("/artists"), so we slice that window and assert "---" lives
	// inside it.
	healthWidget := htmlBetween(body, "health-ring", "/artists")
	if healthWidget == "" {
		t.Fatalf("could not locate the health bubble (health-ring .. /artists) in body")
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

// TestHandleIndex_Unauthorized verifies that with no authenticated user in
// context (but users existing, so the setup page is not shown) the handler
// renders the login page rather than the dashboard.
func TestHandleIndex_Unauthorized(t *testing.T) {
	t.Parallel()
	r := indexDashboardRouter(t)
	markOnboardingComplete(t, r)

	// No withTestUser: the auth check sees an empty user ID.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	r.handleIndex(w, req)

	// renderLoginPage returns 200 with the login form, not the dashboard.
	body := w.Body.String()
	if strings.Contains(body, `/?fixable=yes`) {
		t.Errorf("unauthenticated request must not render the dashboard bubbles")
	}
}

// TestHandleIndex_OnboardingIncomplete verifies that when onboarding is not
// complete the handler redirects to the setup wizard instead of rendering the
// dashboard.
func TestHandleIndex_OnboardingIncomplete(t *testing.T) {
	t.Parallel()
	r := indexDashboardRouter(t)
	// Deliberately do NOT mark onboarding complete.

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = withTestUser(req)
	w := httptest.NewRecorder()
	r.handleIndex(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d (redirect to wizard)", w.Code, http.StatusSeeOther)
	}
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "/setup/wizard") {
		t.Errorf("Location = %q, want it to contain /setup/wizard", loc)
	}
}

// TestBuildDashboardFlyoutData_NilRuleServiceStillPopulatesLibraries verifies
// that when a router is configured with a libraryService but NO ruleService,
// the flyout builder's rule-service-nil early-return branch still fetches and
// populates the Libraries field. Before the fix that branch returned an
// ActionQueueData without Libraries, so the flyout's Library filter dimension
// rendered empty even though the library data was available. The assertion is
// specific: it fails against the pre-fix code (empty Libraries) and against a
// swapped/misnamed field, because it checks both the count and the concrete
// library name/ID round-tripped from the seeded row.
func TestBuildDashboardFlyoutData_NilRuleServiceStillPopulatesLibraries(t *testing.T) {
	t.Parallel()
	r := testDashboardRouter(t, false)

	// Give the router a real library service and seed exactly one library, then
	// remove the rule service so buildDashboardFlyoutData takes the early-return
	// branch under test.
	libSvc := library.NewService(r.db)
	r.libraryService = libSvc
	lib := &library.Library{Name: "Flyout Lib", Path: t.TempDir(), Type: "regular"}
	if err := libSvc.Create(context.Background(), lib); err != nil {
		t.Fatalf("creating library: %v", err)
	}
	r.ruleService = nil

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = withTestUser(req)

	data := r.buildDashboardFlyoutData(req)

	if len(data.Libraries) != 1 {
		t.Fatalf("Libraries length = %d, want 1 (nil-ruleService branch must still fetch libraries); Libraries=%+v", len(data.Libraries), data.Libraries)
	}
	if got := data.Libraries[0].Name; got != "Flyout Lib" {
		t.Errorf("Libraries[0].Name = %q, want %q", got, "Flyout Lib")
	}
	if got := data.Libraries[0].ID; got != lib.ID {
		t.Errorf("Libraries[0].ID = %q, want %q (seeded library id)", got, lib.ID)
	}
}

// TestHandleIndex_InitialQueryForwarded verifies that recognized filter query
// params are forwarded into the initial HTMX load so a bookmarked
// /?severity=warning opens with that filter applied. The forwarded query is
// embedded in the rendered page (buildDashboardInitialQuery), so the encoded
// severity value appears in the body.
func TestHandleIndex_InitialQueryForwarded(t *testing.T) {
	t.Parallel()
	r := indexDashboardRouter(t)
	markOnboardingComplete(t, r)
	seedFixableViolations(t, r, 1, 1)

	req := httptest.NewRequest(http.MethodGet, "/?severity=warning", nil)
	req = withTestUser(req)
	w := httptest.NewRecorder()
	r.handleIndex(w, req)

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
