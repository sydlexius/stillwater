package api

import (
	"net/http"

	"github.com/sydlexius/stillwater/internal/artist"
)

// handleListAliases returns all aliases for an artist.
// GET /api/v1/artists/{id}/aliases
func (r *Router) handleListAliases(w http.ResponseWriter, req *http.Request) {
	artistID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}

	aliases, err := r.artistService.ListAliases(req.Context(), artistID)
	if err != nil {
		r.logger.Error("listing aliases", "artist_id", artistID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if aliases == nil {
		aliases = []artist.Alias{}
	}
	writeJSON(w, http.StatusOK, aliases)
}

// handleAddAlias adds a new alias to an artist.
// POST /api/v1/artists/{id}/aliases
func (r *Router) handleAddAlias(w http.ResponseWriter, req *http.Request) {
	artistID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}

	var body struct {
		Alias  string `json:"alias"`
		Source string `json:"source"`
	}
	if !DecodeJSON(w, req, &body) {
		return
	}

	alias, err := r.artistService.AddAlias(req.Context(), artistID, body.Alias, body.Source)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, alias)
}

// handleRemoveAlias deletes an alias by ID.
// DELETE /api/v1/artists/{id}/aliases/{aliasId}
func (r *Router) handleRemoveAlias(w http.ResponseWriter, req *http.Request) {
	aliasID, ok := RequirePathParam(w, req, "aliasId")
	if !ok {
		return
	}

	if err := r.artistService.RemoveAlias(req.Context(), aliasID); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// handleDuplicates returns groups of artists with matching names or aliases.
// GET /api/v1/artists/duplicates
func (r *Router) handleDuplicates(w http.ResponseWriter, req *http.Request) {
	groups, err := r.artistService.FindDuplicates(req.Context())
	if err != nil {
		r.logger.Error("finding duplicates", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if groups == nil {
		groups = []artist.DuplicateGroup{}
	}
	writeJSON(w, http.StatusOK, groups)
}
