package api

import (
	"net/http"

	"github.com/sydlexius/stillwater/internal/api/middleware"
)

// handlePostOnboardingReset clears the onboarding completion and step flags
// so the user is redirected to /setup/wizard on the next visit to /.
// Admin-only (administrator role). User-facing settings (libraries, providers,
// connections, profile) are preserved; only the wizard-progress flags are reset.
//
// The two UPSERTs are issued as individual statements rather than in a
// transaction: each is idempotent and the failure window (one succeeds, one
// does not) is operationally harmless -- the user can retry from Settings.
//
// POST /api/v1/onboarding/reset
func (r *Router) handlePostOnboardingReset(w http.ResponseWriter, req *http.Request) {
	userID := middleware.UserIDFromContext(req.Context())
	if userID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	if _, err := r.db.ExecContext(req.Context(),
		`INSERT INTO settings (key, value) VALUES ('onboarding.completed', '')
		 ON CONFLICT(key) DO UPDATE SET value=''`); err != nil {
		r.logger.Error("onboarding reset: clearing completed flag", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if _, err := r.db.ExecContext(req.Context(),
		`INSERT INTO settings (key, value) VALUES ('onboarding.step', '0')
		 ON CONFLICT(key) DO UPDATE SET value='0'`); err != nil {
		r.logger.Error("onboarding reset: resetting step to 0", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
