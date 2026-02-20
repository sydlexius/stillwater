package api

import (
	"encoding/json"
	"net/http"

	"github.com/sydlexius/stillwater/internal/artist"
)

func (r *Router) handleListAliases(w http.ResponseWriter, req *http.Request) {
	artistID := req.PathValue("id")
	if artistID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing artist id"})
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

func (r *Router) handleAddAlias(w http.ResponseWriter, req *http.Request) {
	artistID := req.PathValue("id")
	if artistID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing artist id"})
		return
	}

	var body struct {
		Alias  string `json:"alias"`
		Source string `json:"source"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	alias, err := r.artistService.AddAlias(req.Context(), artistID, body.Alias, body.Source)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, alias)
}

func (r *Router) handleRemoveAlias(w http.ResponseWriter, req *http.Request) {
	aliasID := req.PathValue("aliasId")
	if aliasID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing alias id"})
		return
	}

	if err := r.artistService.RemoveAlias(req.Context(), aliasID); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

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
