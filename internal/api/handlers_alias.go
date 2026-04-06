package api

import (
	"errors"
	"net/http"
	"strings"

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
// Accepts application/x-www-form-urlencoded (fields: alias, source) or
// application/json (fields: alias, source).
func (r *Router) handleAddAlias(w http.ResponseWriter, req *http.Request) {
	artistID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}

	var aliasVal, sourceVal string
	ct := req.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "application/json") {
		var body struct {
			Alias  string `json:"alias"`
			Source string `json:"source"`
		}
		if !DecodeJSON(w, req, &body) {
			return
		}
		aliasVal = body.Alias
		sourceVal = body.Source
	} else {
		if err := req.ParseForm(); err != nil {
			r.logger.Error("parsing alias form", "artist_id", artistID, "error", err)
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid form data"})
			return
		}
		aliasVal = req.FormValue("alias")
		sourceVal = req.FormValue("source")
	}

	aliasVal = strings.TrimSpace(aliasVal)
	if aliasVal == "" {
		r.logger.Warn("add alias: empty alias", "artist_id", artistID)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "alias is required"})
		return
	}

	alias, err := r.artistService.AddAlias(req.Context(), artistID, aliasVal, sourceVal)
	if err != nil {
		if errors.Is(err, artist.ErrNotFound) {
			r.logger.Warn("add alias: artist not found", "artist_id", artistID)
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "artist not found"})
			return
		}
		r.logger.Error("adding alias", "artist_id", artistID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
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
		if errors.Is(err, artist.ErrAliasNotFound) {
			r.logger.Warn("remove alias: not found", "alias_id", aliasID)
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "alias not found"})
			return
		}
		r.logger.Error("removing alias", "alias_id", aliasID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
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
