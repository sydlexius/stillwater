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
// The two UPSERTs run inside a single transaction so the wizard-redirect flag
// and the wizard step always move together. Without the transaction, if the
// first statement succeeded but the second failed the user would be sent back
// into the wizard but land mid-flow on the previously-stored step (e.g. step
// 7 / Discovery) instead of step 1.
//
// POST /api/v1/onboarding/reset
func (r *Router) handlePostOnboardingReset(w http.ResponseWriter, req *http.Request) {
	// Defensive: NewRouter accepts a nil DB (see internal/api/router.go
	// where foreignRepo wiring is guarded by a deps.DB != nil check), so a
	// partially-wired router could otherwise panic here. Treat the missing
	// dependency as a 500 and surface the standard JSON error envelope.
	if r.db == nil {
		r.logger.Error("onboarding reset: database unavailable")
		writeError(w, req, http.StatusInternalServerError, "internal error")
		return
	}
	userID := middleware.UserIDFromContext(req.Context())
	if userID == "" {
		writeError(w, req, http.StatusUnauthorized, "unauthorized")
		return
	}

	tx, err := r.db.BeginTx(req.Context(), nil)
	if err != nil {
		r.logger.Error("onboarding reset: starting transaction", "error", err)
		writeError(w, req, http.StatusInternalServerError, "internal error")
		return
	}
	// Rollback is a no-op once Commit() succeeds; safe to defer unconditionally
	// so the early-return error paths below release the transaction handle.
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(req.Context(),
		`INSERT INTO settings (key, value) VALUES ('onboarding.completed', '')
		 ON CONFLICT(key) DO UPDATE SET value=''`); err != nil {
		r.logger.Error("onboarding reset: clearing completed flag", "error", err)
		writeError(w, req, http.StatusInternalServerError, "internal error")
		return
	}
	if _, err := tx.ExecContext(req.Context(),
		`INSERT INTO settings (key, value) VALUES ('onboarding.step', '0')
		 ON CONFLICT(key) DO UPDATE SET value='0'`); err != nil {
		r.logger.Error("onboarding reset: resetting step to 0", "error", err)
		writeError(w, req, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(); err != nil {
		r.logger.Error("onboarding reset: committing transaction", "error", err)
		writeError(w, req, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
