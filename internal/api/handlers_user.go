package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/auth"
)

// handleCreateInvite generates a new single-use invite link.
// POST /api/v1/users/invites (admin only)
func (r *Router) handleCreateInvite(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Role      string `json:"role"`
		ExpiresIn string `json:"expires_in"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid request body."})
		return
	}

	if body.Role != "administrator" && body.Role != "operator" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Role must be administrator or operator."})
		return
	}

	dur, err := parseDuration(body.ExpiresIn)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid expires_in value. Use a duration like 24h, 7d, or 30d."})
		return
	}

	callerID := middleware.UserIDFromContext(req.Context())
	invite, err := r.authService.CreateInvite(req.Context(), body.Role, callerID, dur)
	if err != nil {
		r.logger.Error("failed to create invite", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "An internal error occurred. Please try again."})
		return
	}

	writeJSON(w, http.StatusCreated, invite)
}

// handleListInvites returns all pending (unredeemed, non-expired) invites.
// GET /api/v1/users/invites (admin only)
func (r *Router) handleListInvites(w http.ResponseWriter, req *http.Request) {
	invites, err := r.authService.ListPendingInvites(req.Context())
	if err != nil {
		r.logger.Error("failed to list invites", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "An internal error occurred. Please try again."})
		return
	}

	// Return an empty array rather than null when there are no invites.
	if invites == nil {
		invites = []auth.Invite{}
	}

	writeJSON(w, http.StatusOK, invites)
}

// handleRevokeInvite deletes an unredeemed invite by its ID.
// DELETE /api/v1/users/invites/{id} (admin only)
func (r *Router) handleRevokeInvite(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invite ID is required."})
		return
	}

	err := r.authService.RevokeInvite(req.Context(), id)
	if err != nil {
		if errors.Is(err, auth.ErrInviteNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "Invite not found or already redeemed."})
			return
		}
		r.logger.Error("failed to revoke invite", "invite_id", id, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "An internal error occurred. Please try again."})
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleRegister creates a new user account from a valid invite code and
// immediately creates a session so the user is logged in.
// POST /api/v1/users/register (public)
func (r *Router) handleRegister(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Code        string `json:"code"`
		Username    string `json:"username"`
		Password    string `json:"password"` //nolint:gosec // G117: not a hardcoded secret, this is a request field
		DisplayName string `json:"display_name"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid request body."})
		return
	}

	if body.Code == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invite code is required."})
		return
	}
	if body.Username == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Username is required."})
		return
	}
	if len(body.Password) < 8 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Password must be at least 8 characters."})
		return
	}
	if body.DisplayName == "" {
		body.DisplayName = body.Username
	}

	// Validate the invite code before creating the user.
	invite, err := r.authService.GetInviteByCode(req.Context(), body.Code)
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrInviteNotFound):
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid invite code."})
		case errors.Is(err, auth.ErrInviteRedeemed):
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "This invite has already been used."})
		case errors.Is(err, auth.ErrInviteExpired):
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "This invite has expired."})
		default:
			r.logger.Error("failed to validate invite", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "An internal error occurred. Please try again."})
		}
		return
	}

	// Create the local user account.
	user, err := r.authService.CreateLocalUser(req.Context(), body.Username, body.Password, body.DisplayName, invite.Role, invite.CreatedBy)
	if err != nil {
		r.logger.Error("failed to create user from invite", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "An internal error occurred. Please try again."})
		return
	}

	// Mark the invite as redeemed.
	if _, err := r.authService.RedeemInvite(req.Context(), body.Code, user.ID); err != nil {
		// Non-fatal: the user is already created. Log but do not block login.
		r.logger.Error("failed to redeem invite after user creation", "invite_code", body.Code, "user_id", user.ID, "error", err)
	}

	// Auto-login: create a session for the new user.
	token, err := r.authService.Login(req.Context(), body.Username, body.Password)
	if err != nil {
		// User was created successfully; failure to auto-login is non-fatal.
		// The client can redirect to the login page.
		r.logger.Error("failed to auto-login after registration", "username", body.Username, "error", err)
		writeJSON(w, http.StatusCreated, user)
		return
	}

	r.setSessionCookie(w, req, token)
	writeJSON(w, http.StatusCreated, user)
}

// handleListUsers returns all user accounts.
// GET /api/v1/users (admin only)
func (r *Router) handleListUsers(w http.ResponseWriter, req *http.Request) {
	users, err := r.authService.ListUsers(req.Context())
	if err != nil {
		r.logger.Error("failed to list users", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "An internal error occurred. Please try again."})
		return
	}

	// Return an empty array rather than null when there are no users.
	if users == nil {
		users = []auth.User{}
	}

	writeJSON(w, http.StatusOK, users)
}

// handleGetUser returns a single user by ID.
// Admins may fetch any user; non-admins may only fetch their own record.
// GET /api/v1/users/{id} (admin or self)
func (r *Router) handleGetUser(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "User ID is required."})
		return
	}

	callerID := middleware.UserIDFromContext(req.Context())
	role := middleware.RoleFromContext(req.Context())

	// Non-admins may only view their own record.
	if role != "administrator" && id != callerID {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "Forbidden."})
		return
	}

	user, err := r.authService.GetUserByID(req.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "User not found."})
			return
		}
		r.logger.Error("failed to get user", "user_id", id, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "An internal error occurred. Please try again."})
		return
	}

	writeJSON(w, http.StatusOK, user)
}

// handleUpdateUser changes a user's role.
// PATCH /api/v1/users/{id} (admin only)
func (r *Router) handleUpdateUser(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "User ID is required."})
		return
	}

	var body struct {
		Role string `json:"role"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid request body."})
		return
	}

	if body.Role != "administrator" && body.Role != "operator" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Role must be administrator or operator."})
		return
	}

	if err := r.authService.UpdateUserRole(req.Context(), id, body.Role); err != nil {
		switch {
		case errors.Is(err, auth.ErrLastAdmin):
			writeJSON(w, http.StatusConflict, map[string]string{"error": "Cannot downgrade the last active administrator."})
		case errors.Is(err, sql.ErrNoRows):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "User not found."})
		default:
			r.logger.Error("failed to update user role", "user_id", id, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "An internal error occurred. Please try again."})
		}
		return
	}

	user, err := r.authService.GetUserByID(req.Context(), id)
	if err != nil {
		r.logger.Error("failed to fetch user after role update", "user_id", id, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "An internal error occurred. Please try again."})
		return
	}

	writeJSON(w, http.StatusOK, user)
}

// handleDeactivateUser deactivates a user account and invalidates all their sessions.
// DELETE /api/v1/users/{id} (admin only)
func (r *Router) handleDeactivateUser(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "User ID is required."})
		return
	}

	if err := r.authService.DeactivateUser(req.Context(), id); err != nil {
		switch {
		case errors.Is(err, auth.ErrLastAdmin):
			writeJSON(w, http.StatusConflict, map[string]string{"error": "Cannot deactivate the last active administrator."})
		default:
			r.logger.Error("failed to deactivate user", "user_id", id, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "An internal error occurred. Please try again."})
		}
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// parseDuration parses a duration string. It supports standard Go duration
// syntax (e.g. "24h") as well as a shorthand suffix "d" for whole days
// (e.g. "7d" equals 168h).
func parseDuration(s string) (time.Duration, error) {
	if strings.HasSuffix(s, "d") {
		days, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil || days < 1 {
			return 0, fmt.Errorf("invalid duration: %s", s)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	if d <= 0 {
		return 0, fmt.Errorf("invalid duration: %s", s)
	}
	return d, nil
}
