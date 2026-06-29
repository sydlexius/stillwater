package api

import (
	"net/http"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/web/components"
	"github.com/sydlexius/stillwater/web/templates"
	"github.com/sydlexius/stillwater/web/templates/next"
)

// handleNextSettingsPage serves the next/ channel settings page (M55 #1339): a
// Firefox about:preferences-style single-scroll screen with a sticky rail of
// collapsible groups, a keyword filter, and scroll-spy anchors. The section
// CONTENT is the same shared templates.Section* / Settings*Tab funcs the stable
// page renders; only the chrome (rail/filter/scroll-spy) is next-specific.
//
// In stable mode (SW_UX=stable) the UX middleware 404s any /next/* request
// before this handler runs (decision 12). The checkNextChannel guard below is
// therefore only reachable in next/dual mode; it 404s an explicit
// X-Stillwater-UX: stable opt-out rather than serving stable content.
//
// Auth mirrors the stable handleSettingsPage exactly: the settings screen is
// administrator-only. wrapOptionalAuth means an unauthenticated visitor reaches
// here, so render the login page for them; operators get a 403.
func (r *Router) handleNextSettingsPage(w http.ResponseWriter, req *http.Request) {
	if !checkNextChannel(w, req) {
		return
	}

	userID := middleware.UserIDFromContext(req.Context())
	if userID == "" {
		r.renderLoginPage(w, req)
		return
	}
	if middleware.RoleFromContext(req.Context()) != "administrator" {
		http.Error(w, "Forbidden: administrator role required", http.StatusForbidden)
		return
	}

	// loadUsers=true: the next/ chrome renders the Users section eagerly on the
	// single-scroll page, so its data must always be present (unlike the stable
	// tab, which loads it only when the Users tab is active). tab is irrelevant
	// to the next/ chrome (it scroll-spies), so pass the General default.
	data, ok := r.buildSettingsData(req, userID, templates.TabGeneral, true)
	if !ok {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// #1339 A1: the next/ page adds an <h2> group-divider tier (Essentials / Data
	// / Integrations / System) between the page <h1> and the section cards, so the
	// shared section CARD titles must render one level deeper (<h3>, sub-heads
	// <h4>) to keep a non-skipping heading outline. Thread that base level through
	// the render context; the stable handler leaves it unset (default <h2>), so
	// the shared content is byte-identical there (golden-locked).
	req = req.WithContext(components.WithHeadingLevel(req.Context(), 3))
	renderTempl(w, req, next.SettingsPageNext(r.assetsFor(req), data))
}
