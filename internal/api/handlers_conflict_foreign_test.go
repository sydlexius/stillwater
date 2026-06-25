package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/conflict"
	"github.com/sydlexius/stillwater/internal/foreign"
)

// TestHandleGetConflictBanner_NoForeignFilesStateWhenFilesPresent is the
// real-handler proof for the M55 TRANQUILITY requirement (#1773): with foreign
// files PRESENT (count > 0) and NO connection conflict, the conflict banner
// must NOT render any "foreign files detected" state. Foreign-file detection
// surfaces only via the sidebar "Unmatched Files" pill, so the banner falls
// back to its clean (all-clear) state and the former slate/blue state E never
// appears. The companion assertion confirms the same seeded count still drives
// the sidebar pill, so removing the banner state did not also kill the pill.
func TestHandleGetConflictBanner_NoForeignFilesStateWhenFilesPresent(t *testing.T) {
	t.Parallel()
	r, db := newTestRouterWithForeign(t)
	// A detector with no connections yields a clean (no-conflict) ledger, so
	// the only thing that could promote the banner is the (now removed)
	// foreign-files state.
	r.conflictDetector = conflict.NewForTest(&fakeRepo{conns: nil}, testDiscardLogger())

	mustExec(t, db, `INSERT INTO artists (id, name, path) VALUES ('a1','x','/x')`)
	for _, fn := range []string{"fanart.jpg", "backdrop.jpg", "poster.jpg"} {
		if err := r.foreignRepo.Upsert(context.Background(), foreign.Entry{
			ArtistID: "a1", FilePath: "/x/" + fn, FileName: fn,
		}); err != nil {
			t.Fatalf("seed %s: %v", fn, err)
		}
	}
	// Precondition: the pill's data source counts all three seeded files.
	if got := r.foreignSummaryForBanner(context.Background()); got != 3 {
		t.Fatalf("precondition: foreign count = %d, want 3", got)
	}

	// Exercise the REAL banner handler.
	req := withI18nCtx(t, httptest.NewRequest(http.MethodGet, "/api/v1/config/conflict-banner", nil))
	w := httptest.NewRecorder()
	r.handleGetConflictBanner(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()

	// None of the removed state-E markers may appear.
	for _, marker := range []string{
		"Foreign image files detected",
		"sw-conflict-foreign",
		`data-conflict-state="foreign_files"`,
		"foreign-files page",
		"allowlist all",
	} {
		if strings.Contains(body, marker) {
			t.Errorf("conflict banner must not contain foreign-files content, but found %q\nbody:\n%s", marker, body)
		}
	}
	// The banner state span must report "clean", proving foreign files did not
	// promote any banner state.
	if !strings.Contains(body, `data-state="clean"`) {
		t.Errorf("banner state should be clean with foreign files present and no conflict; body:\n%s", body)
	}

	// Companion: the sidebar pill (next channel) still renders the count, so
	// foreign-file presence remains discoverable -- just not via the banner.
	// Apply the same three-step context chain used by sibling tests in
	// handlers_foreign_files_test.go: i18n first, then user ID, then role.
	pillReq := withI18nCtx(t, httptest.NewRequest(http.MethodGet, "/api/v1/foreign-files/count?ch=next", nil))
	pillCtx := middleware.WithTestUserID(pillReq.Context(), "admin-1")
	pillCtx = middleware.WithTestRole(pillCtx, "administrator")
	pillReq = pillReq.WithContext(pillCtx)
	pillRec := httptest.NewRecorder()
	r.handleForeignFilesCount(pillRec, pillReq)
	if pillRec.Code != http.StatusOK {
		t.Fatalf("pill status = %d, want 200", pillRec.Code)
	}
	// data-count carries the same value so swForeignPillSwap (sidebar.js) can
	// detect a count increase across HTMX swaps and fire the one-shot pulse.
	pillBody := pillRec.Body.String()
	if !strings.Contains(pillBody, `class="sw-sidebar-count-pill" data-count="3"`) ||
		!strings.Contains(pillBody, `>3</span>`) {
		t.Errorf("sidebar pill should show count 3 with data-count; got %q", pillBody)
	}
	// data-aria is the localized, count-bearing accessible name the JS folds onto
	// the nav link so screen-reader users hear the count (not a count-less label).
	if !strings.Contains(pillBody, `data-aria="3 unrecognized files`) {
		t.Errorf("sidebar pill data-aria should carry the count; got %q", pillBody)
	}
	// title supplies the calm hover tooltip.
	if !strings.Contains(pillBody, `title="Unrecognized files`) {
		t.Errorf("sidebar pill should carry the calm title tooltip; got %q", pillBody)
	}
}
