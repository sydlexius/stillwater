package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/settingsio"
)

// handleSetupRestore accepts a settings-export envelope + passphrase during
// the pre-admin OOBE flow, restores the entire envelope (settings,
// connections, platform profiles, webhooks, providers, rules, scraper,
// user prefs, libraries, api tokens, AND users by UUID id) into a fresh
// install, and atomically marks onboarding.completed = "true" on success.
//
// The endpoint is gated on `HasUsers == false` AND
// `onboarding.completed != "true"`. The HasUsers check is the pre-admin
// gate: no admin exists yet at OOBE, so the restored envelope's users
// become the only credentials on the instance and the client is forced
// to / where the login page renders. Once OOBE finishes, the secondary
// onboarding flag gate locks out post-install privilege-escalation
// attempts.
//
// CSRF: the endpoint is registered in the router's pre-auth public block
// and added to csrfExempt -- it shares the same entry-point treatment as
// /auth/login and /auth/setup. The setup.templ page that renders the UI
// is also unauthenticated, so there is no session-bound CSRF token to
// validate.
//
// Rate-limiting is wired at the router (loginRL) so brute-forcing the
// passphrase against this endpoint is throttled the same way as login.
//
// On success the response carries an HX-Redirect to the root path so the
// user signs in with the restored credentials.
//
// POST /api/v1/setup/restore (multipart form: file, passphrase)
func (r *Router) handleSetupRestore(w http.ResponseWriter, req *http.Request) {
	// Serialize the entire HasUsers -> onboarding probe -> Import ->
	// onboarding flip sequence. The handler is unauthenticated and
	// idempotent only after the first success; without this lock two
	// coincident POSTs could both pass the HasUsers gate before either
	// inserts a user row, racing into a half-restored DB. Held for the
	// duration of one request, which is bounded by loginRL.
	r.setupRestoreMu.Lock()
	defer r.setupRestoreMu.Unlock()

	if status, msg, ok := r.checkRestoreGates(req); !ok {
		r.writeRestoreErr(w, req, status, msg)
		return
	}

	envelope, passphrase, status, msg, ok := parseRestoreInput(req)
	if !ok {
		r.writeRestoreErr(w, req, status, msg)
		return
	}

	// Apply the envelope. We do NOT enable admin-fallback for tokens here:
	// the envelope's Users block is what owns every downstream reference,
	// and the OOBE flow always has the source admin available to reassign
	// to via the normal username/id path.
	result, err := r.settingsIOService.Import(req.Context(), envelope, passphrase)
	if err != nil {
		status, clientMsg := classifyRestoreError(err)
		// Log without including the passphrase or the decrypted payload.
		r.logger.Error("setup restore failed", "error", err)
		r.writeRestoreErr(w, req, status, clientMsg)
		return
	}

	// Mark onboarding complete so the user is bounced to /login on next
	// request. We do this OUTSIDE the import transaction (the import is
	// itself a series of per-section upserts that already committed), so a
	// successful restore followed by a failed settings-write yields a
	// fully-restored DB without the completion flag -- on reload the user
	// would land back on the OOBE page and could re-run the restore
	// idempotently (every section's import is upsert-by-natural-key). That
	// is a deliberate "fail soft" because the alternative -- a half-applied
	// restore -- is worse.
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := r.db.ExecContext(req.Context(), `
		INSERT INTO settings (key, value, updated_at)
		VALUES ('onboarding.completed', 'true', ?)
		ON CONFLICT(key) DO UPDATE SET value = 'true', updated_at = excluded.updated_at
	`, now); err != nil {
		r.logger.Error("restore: marking onboarding.completed", "error", err)
		r.writeRestoreErr(w, req, http.StatusInternalServerError, "Restore applied but completing setup failed. Reload the page.")
		return
	}

	// Force the client back to the root path so the login page can render
	// against the freshly-restored users table. The HasUsers gate above
	// guarantees no session existed before this handler ran, so there is
	// no session to invalidate.
	r.logger.Info("setup restore complete",
		"users_imported", result.UsersImported,
		"connections", result.Connections,
		"libraries", result.Libraries,
		"api_tokens", result.APITokens,
	)

	loginPath := r.basePath + "/"
	if req.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", loginPath)
		w.Header().Set("Content-Type", "text/html")
		// Best-effort write; HTMX uses HX-Redirect to drive the browser, so
		// the body is informational.
		fmt.Fprintf(w, `<div class="text-sm text-green-600 dark:text-green-400">Restore complete. Redirecting to sign in.</div>`) //nolint:errcheck // Best-effort write to HTTP response; client disconnect mid-write is not actionable
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":         "restored",
		"redirect":       loginPath,
		"users_imported": result.UsersImported,
		"connections":    result.Connections,
		"libraries":      result.Libraries,
		"api_tokens":     result.APITokens,
	})
}

// writeRestoreErr writes a restore-flow error response. An explicit
// Accept: application/json wins over HX-Request so API callers keep
// their status-code contract even when the OOBE UI's HX-Request marker
// is also present.
func (r *Router) writeRestoreErr(w http.ResponseWriter, req *http.Request, status int, msg string) {
	if !acceptsJSON(req) && req.Header.Get("HX-Request") == "true" {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<div class="text-sm text-red-600 dark:text-red-400">%s</div>`, html.EscapeString(msg)) //nolint:errcheck // Best-effort write to HTTP response; client disconnect mid-write is not actionable
		return
	}
	writeJSON(w, status, map[string]string{"error": msg})
}

// acceptsJSON reports whether the request's Accept header explicitly
// requests application/json. Empty Accept returns false.
func acceptsJSON(req *http.Request) bool {
	accept := req.Header.Get("Accept")
	if accept == "" {
		return false
	}
	for _, part := range strings.Split(accept, ",") {
		if strings.TrimSpace(strings.SplitN(part, ";", 2)[0]) == "application/json" {
			return true
		}
	}
	return false
}

// checkRestoreGates runs the OOBE preconditions: required services
// present, no admin yet (HasUsers), and onboarding.completed unset.
// Returns (status, msg, true) on pass, or (status, msg, false) on the
// first failed gate so the handler can write the matching response.
func (r *Router) checkRestoreGates(req *http.Request) (int, string, bool) {
	if r.settingsIOService == nil {
		r.logger.Error("restore: settings-io service not configured")
		return http.StatusServiceUnavailable, "Restore is not available on this server.", false
	}
	if r.authService == nil {
		r.logger.Error("restore: auth service not configured")
		return http.StatusServiceUnavailable, "Restore is not available on this server.", false
	}
	hasUsers, err := r.authService.HasUsers(req.Context())
	if err != nil {
		r.logger.Error("restore: checking user count", "error", err)
		return http.StatusInternalServerError, "An internal error occurred. Please try again.", false
	}
	if hasUsers {
		return http.StatusForbidden, "Restore is only available before an admin account is created.", false
	}
	var completed string
	err = r.db.QueryRowContext(req.Context(),
		`SELECT value FROM settings WHERE key = 'onboarding.completed'`).Scan(&completed)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		r.logger.Error("restore: checking onboarding status", "error", err)
		return http.StatusInternalServerError, "An internal error occurred. Please try again.", false
	}
	if completed == "true" {
		return http.StatusForbidden, "Restore is only available during initial setup.", false
	}
	return 0, "", true
}

// parseRestoreInput pulls the envelope and passphrase out of the
// multipart form, enforcing the size cap and scrubbing the passphrase
// from the parsed form maps. Returns (envelope, passphrase, 0, "",
// true) on success or (nil, "", status, msg, false) on the first
// failure so the handler can write the matching response.
func parseRestoreInput(req *http.Request) (*settingsio.Envelope, string, int, string, bool) {
	if err := req.ParseMultipartForm(maxImportSize); err != nil {
		return nil, "", http.StatusBadRequest, "Upload too large or malformed.", false
	}
	passphrase := req.FormValue("passphrase")
	// Clear the passphrase from the parsed form maps so any downstream
	// middleware that dumps req.Form (panic-recovery handlers, request
	// loggers, debug endpoints) cannot accidentally surface it. The
	// local passphrase variable still holds the value for the Import
	// call.
	if req.MultipartForm != nil {
		delete(req.MultipartForm.Value, "passphrase")
	}
	if req.PostForm != nil {
		req.PostForm.Del("passphrase")
	}
	if req.Form != nil {
		req.Form.Del("passphrase")
	}
	if passphrase == "" {
		return nil, "", http.StatusBadRequest, "Passphrase is required.", false
	}
	file, _, err := req.FormFile("file")
	if err != nil {
		return nil, "", http.StatusBadRequest, "Backup file is required.", false
	}
	defer file.Close() //nolint:errcheck // Close error not actionable on cleanup
	data, err := io.ReadAll(io.LimitReader(file, maxImportSize+1))
	if err != nil {
		return nil, "", http.StatusBadRequest, "Reading uploaded file failed.", false
	}
	if len(data) > maxImportSize {
		return nil, "", http.StatusRequestEntityTooLarge, "Backup file exceeds 10MB limit.", false
	}
	var envelope settingsio.Envelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, "", http.StatusBadRequest, "Backup file is not a valid Stillwater export.", false
	}
	return &envelope, passphrase, 0, "", true
}

// classifyRestoreError maps a settingsio.Import error to a user-facing
// status + message. Never echoes the raw error string -- in particular
// the passphrase must not land in any user-facing message.
func classifyRestoreError(err error) (int, string) {
	switch {
	case errors.Is(err, settingsio.ErrWrongPassphrase):
		return http.StatusBadRequest, "Restore failed: incorrect passphrase or corrupted backup file."
	case errors.Is(err, settingsio.ErrUnsupportedVersion):
		return http.StatusBadRequest, "Restore failed: this backup file uses an unsupported format version."
	case errors.Is(err, settingsio.ErrUserIDCollision):
		return http.StatusConflict, "Restore failed: a user account on this server collides with the backup. Reset the server and try again."
	default:
		return http.StatusInternalServerError, "Restore failed: see server logs for details."
	}
}
