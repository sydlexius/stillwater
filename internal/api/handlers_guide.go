package api

import (
	"net/http"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	templates "github.com/sydlexius/stillwater/web/templates"
)

// handleGuidePage renders the in-app user guide.
// GET /guide
func (r *Router) handleGuidePage(w http.ResponseWriter, req *http.Request) {
	userID := middleware.UserIDFromContext(req.Context())
	if userID == "" {
		renderTempl(w, req, templates.LoginPage(r.assets()))
		return
	}
	renderTempl(w, req, templates.GuidePage(r.assets()))
}
