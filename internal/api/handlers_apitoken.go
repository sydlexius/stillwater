package api

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/auth"
)

// handleCreateAPIToken generates a new API token.
// POST /api/v1/auth/tokens
func (r *Router) handleCreateAPIToken(w http.ResponseWriter, req *http.Request) {
	userID := middleware.UserIDFromContext(req.Context())
	if userID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	var body struct {
		Name   string `json:"name"`
		Scopes string `json:"scopes"`
	}
	if !DecodeJSON(w, req, &body) {
		return
	}
	if body.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}

	// Validate and normalize scopes
	if body.Scopes == "" {
		body.Scopes = "read"
	}
	var normalizedScopes []string
	for _, s := range strings.Split(body.Scopes, ",") {
		trimmed := strings.TrimSpace(s)
		if trimmed == "" {
			continue
		}
		scope := auth.TokenScope(trimmed)
		if !auth.ValidScopes[scope] {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid scope: " + string(scope)})
			return
		}
		normalizedScopes = append(normalizedScopes, trimmed)
	}
	if len(normalizedScopes) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no valid scopes provided"})
		return
	}
	body.Scopes = strings.Join(normalizedScopes, ",")

	plaintext, id, err := r.authService.CreateAPIToken(req.Context(), userID, body.Name, body.Scopes)
	if err != nil {
		r.logger.Error("creating api token", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create token"})
		return
	}

	// Best-effort audit log entry for token creation.
	if logErr := r.authService.WriteAuditLog(req.Context(), "token_created", id, body.Name, userID, ""); logErr != nil {
		r.logger.Warn("failed to write audit log for token creation", "error", logErr)
	}

	writeJSON(w, http.StatusCreated, map[string]string{
		"id":    id,
		"token": plaintext,
		"name":  body.Name,
	})
}

// handleListAPITokens lists all tokens for the authenticated user.
// GET /api/v1/auth/tokens
// Pass ?include_archived=true to include archived tokens.
func (r *Router) handleListAPITokens(w http.ResponseWriter, req *http.Request) {
	userID := middleware.UserIDFromContext(req.Context())
	if userID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	var tokens []auth.APIToken
	var err error
	includeArchived, _ := strconv.ParseBool(req.URL.Query().Get("include_archived"))
	if includeArchived {
		tokens, err = r.authService.ListAPITokensAll(req.Context(), userID)
	} else {
		tokens, err = r.authService.ListAPITokens(req.Context(), userID)
	}
	if err != nil {
		r.logger.Error("listing api tokens", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to list tokens"})
		return
	}

	if tokens == nil {
		tokens = []auth.APIToken{}
	}

	writeJSON(w, http.StatusOK, tokens)
}

// handleRevokeAPIToken revokes a token.
// DELETE /api/v1/auth/tokens/{id}
func (r *Router) handleRevokeAPIToken(w http.ResponseWriter, req *http.Request) {
	userID := middleware.UserIDFromContext(req.Context())
	if userID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	tokenID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}

	// Look up the token name for audit logging before revoking.
	tok, err := r.authService.GetAPIToken(req.Context(), tokenID, userID)
	if err != nil {
		if errors.Is(err, auth.ErrTokenNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		} else {
			r.logger.Error("looking up api token for revoke", "token_id", tokenID, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to revoke token"})
		}
		return
	}

	if err := r.authService.RevokeAPIToken(req.Context(), tokenID, userID); err != nil {
		if errors.Is(err, auth.ErrTokenNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		} else {
			r.logger.Error("revoking api token", "token_id", tokenID, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to revoke token"})
		}
		return
	}

	if logErr := r.authService.WriteAuditLog(req.Context(), "token_revoked", tokenID, tok.Name, userID, ""); logErr != nil {
		r.logger.Warn("failed to write audit log for token revocation", "error", logErr)
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

// handleArchiveAPIToken archives a revoked token, hiding it from the default list.
// PATCH /api/v1/auth/tokens/{id}/archive
func (r *Router) handleArchiveAPIToken(w http.ResponseWriter, req *http.Request) {
	userID := middleware.UserIDFromContext(req.Context())
	if userID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	tokenID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}

	// Look up token name for audit logging.
	tok, err := r.authService.GetAPIToken(req.Context(), tokenID, userID)
	if err != nil {
		if errors.Is(err, auth.ErrTokenNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		} else {
			r.logger.Error("looking up api token for archive", "token_id", tokenID, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to archive token"})
		}
		return
	}

	if err := r.authService.ArchiveAPIToken(req.Context(), tokenID, userID); err != nil {
		switch {
		case errors.Is(err, auth.ErrTokenNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		case errors.Is(err, auth.ErrTokenActive):
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		case errors.Is(err, auth.ErrTokenNotRevoked):
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		default:
			r.logger.Error("archiving api token", slog.String("token_id", tokenID), slog.Any("error", err))
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to archive token"})
		}
		return
	}

	if logErr := r.authService.WriteAuditLog(req.Context(), "token_archived", tokenID, tok.Name, userID, ""); logErr != nil {
		r.logger.Warn("failed to write audit log for token archive", "error", logErr)
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "archived"})
}

// handleUnarchiveAPIToken restores an archived token to revoked status.
// PATCH /api/v1/auth/tokens/{id}/unarchive
func (r *Router) handleUnarchiveAPIToken(w http.ResponseWriter, req *http.Request) {
	userID := middleware.UserIDFromContext(req.Context())
	if userID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	tokenID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}

	// Look up token name for audit logging.
	tok, err := r.authService.GetAPIToken(req.Context(), tokenID, userID)
	if err != nil {
		if errors.Is(err, auth.ErrTokenNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		} else {
			r.logger.Error("looking up api token for unarchive", "token_id", tokenID, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to unarchive token"})
		}
		return
	}

	if err := r.authService.UnarchiveAPIToken(req.Context(), tokenID, userID); err != nil {
		switch {
		case errors.Is(err, auth.ErrTokenNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		case errors.Is(err, auth.ErrTokenActive):
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		case errors.Is(err, auth.ErrTokenNotArchived):
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		default:
			r.logger.Error("unarchiving api token", slog.String("token_id", tokenID), slog.Any("error", err))
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to unarchive token"})
		}
		return
	}

	if logErr := r.authService.WriteAuditLog(req.Context(), "token_unarchived", tokenID, tok.Name, userID, ""); logErr != nil {
		r.logger.Warn("failed to write audit log for token unarchive", "error", logErr)
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

// handleDeleteAPIToken permanently deletes a revoked or archived token.
// DELETE /api/v1/auth/tokens/{id}/permanent
func (r *Router) handleDeleteAPIToken(w http.ResponseWriter, req *http.Request) {
	userID := middleware.UserIDFromContext(req.Context())
	if userID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	tokenID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}

	if err := r.authService.DeleteAPIToken(req.Context(), tokenID, userID); err != nil {
		switch {
		case errors.Is(err, auth.ErrTokenNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		case errors.Is(err, auth.ErrTokenActive):
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		default:
			r.logger.Error("deleting api token", slog.String("token_id", tokenID), slog.Any("error", err))
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to delete token"})
		}
		return
	}

	r.logger.Info("api token permanently deleted", slog.String("token_id", tokenID), slog.String("user_id", userID))
	w.WriteHeader(http.StatusNoContent)
}
