package api

import (
	"encoding/json"
	"net/http"
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
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if body.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}

	// Validate scopes
	if body.Scopes == "" {
		body.Scopes = "read"
	}
	for _, s := range strings.Split(body.Scopes, ",") {
		scope := auth.TokenScope(strings.TrimSpace(s))
		if !auth.ValidScopes[scope] {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid scope: " + string(scope)})
			return
		}
	}

	plaintext, id, err := r.authService.CreateAPIToken(req.Context(), userID, body.Name, body.Scopes)
	if err != nil {
		r.logger.Error("creating api token", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create token"})
		return
	}

	writeJSON(w, http.StatusCreated, map[string]string{
		"id":    id,
		"token": plaintext,
		"name":  body.Name,
	})
}

// handleListAPITokens lists all tokens for the authenticated user.
// GET /api/v1/auth/tokens
func (r *Router) handleListAPITokens(w http.ResponseWriter, req *http.Request) {
	userID := middleware.UserIDFromContext(req.Context())
	if userID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	tokens, err := r.authService.ListAPITokens(req.Context(), userID)
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

	tokenID := req.PathValue("id")
	if tokenID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "token id required"})
		return
	}

	if err := r.authService.RevokeAPIToken(req.Context(), tokenID, userID); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}
