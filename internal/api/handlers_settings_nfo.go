package api

import (
	"encoding/json"
	"net/http"

	"github.com/sydlexius/stillwater/internal/nfo"
)

// handleGetNFOOutput returns the current NFO field mapping configuration.
// GET /api/v1/settings/nfo-output
func (r *Router) handleGetNFOOutput(w http.ResponseWriter, req *http.Request) {
	if r.nfoSettingsService == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "NFO settings not available"})
		return
	}

	fm, err := r.nfoSettingsService.GetFieldMap(req.Context())
	if err != nil {
		r.logger.Error("reading NFO output settings", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	writeJSON(w, http.StatusOK, fm)
}

// handleUpdateNFOOutput updates the NFO field mapping configuration.
// PUT /api/v1/settings/nfo-output
func (r *Router) handleUpdateNFOOutput(w http.ResponseWriter, req *http.Request) {
	if r.nfoSettingsService == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "NFO settings not available"})
		return
	}

	var fm nfo.NFOFieldMap
	if err := json.NewDecoder(req.Body).Decode(&fm); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	// Validate genre_sources values
	validSources := map[string]bool{"genres": true, "styles": true, "moods": true}
	for _, src := range fm.GenreSources {
		if !validSources[src] {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "invalid genre_sources value: " + src + "; valid values are genres, styles, moods",
			})
			return
		}
	}

	// Validate advanced_remap keys and values if present
	if fm.AdvancedRemap != nil {
		validElements := map[string]bool{"genre": true, "style": true, "mood": true}
		for element, sources := range fm.AdvancedRemap {
			if !validElements[element] {
				writeJSON(w, http.StatusBadRequest, map[string]string{
					"error": "invalid advanced_remap key: " + element + "; valid keys are genre, style, mood",
				})
				return
			}
			for _, src := range sources {
				if !validSources[src] {
					writeJSON(w, http.StatusBadRequest, map[string]string{
						"error": "invalid advanced_remap source: " + src + "; valid values are genres, styles, moods",
					})
					return
				}
			}
		}
	}

	if err := r.nfoSettingsService.SetFieldMap(req.Context(), fm); err != nil {
		r.logger.Error("updating NFO output settings", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	writeJSON(w, http.StatusOK, fm)
}
