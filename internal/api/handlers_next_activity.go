package api

import (
	"net/http"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/web/templates/next"
)

// handleNextActivityPage serves the next/ channel global activity feed
// (M55 #1772). It is an Option-A chrome wrap: the page reuses the shared
// templates.ActivityBody surface byte-for-byte inside the next/ LayoutNext
// shell, and the content fragment is served by the existing stable
// /activity/content endpoint (no next/-specific content route), so every hook
// id and HTMX contract matches the stable /activity page.
//
// In stable mode (SW_UX=stable) the UX middleware 404s any /next/* request
// before this handler runs (decision 12 in architecture-decisions.md). The
// in-handler channel guard below is therefore only reachable in next/dual mode
// when the resolved channel is not "next" (an explicit X-Stillwater-UX: stable
// header opting a sub-request back to stable); in that edge case it returns 404
// rather than serving stable content, matching every other handleNext* handler.
func (r *Router) handleNextActivityPage(w http.ResponseWriter, req *http.Request) {
	if !checkNextChannel(w, req) {
		return
	}

	userID := middleware.UserIDFromContext(req.Context())
	if userID == "" {
		r.renderLoginPage(w, req)
		return
	}

	data, ok := r.buildActivityPageData(w, req, userID)
	if !ok {
		return
	}
	renderTempl(w, req, next.ActivityPageNext(r.assetsFor(req), data))
}
