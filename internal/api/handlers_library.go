package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/sydlexius/stillwater/internal/library"
)

// handleListLibraries returns all libraries as JSON.
// GET /api/v1/libraries
func (r *Router) handleListLibraries(w http.ResponseWriter, req *http.Request) {
	libs, err := r.libraryService.List(req.Context())
	if err != nil {
		r.logger.Error("listing libraries", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, libs)
}

// handleGetLibrary returns a single library with artist count.
// GET /api/v1/libraries/{id}
func (r *Router) handleGetLibrary(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	lib, err := r.libraryService.GetByID(req.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "library not found"})
		return
	}

	count, err := r.libraryService.CountArtists(req.Context(), id)
	if err != nil {
		r.logger.Error("counting library artists", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"library":      lib,
		"artist_count": count,
	})
}

// handleCreateLibrary creates a new library.
// POST /api/v1/libraries
func (r *Router) handleCreateLibrary(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Name string `json:"name"`
		Path string `json:"path"`
		Type string `json:"type"`
	}
	if strings.HasPrefix(req.Header.Get("Content-Type"), "application/json") {
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
			return
		}
	} else {
		body.Name = req.FormValue("name")
		body.Path = req.FormValue("path")
		body.Type = req.FormValue("type")
	}
	if body.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}
	if body.Type == "" {
		body.Type = library.TypeRegular
	}
	if body.Type != library.TypeRegular && body.Type != library.TypeClassical {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "type must be 'regular' or 'classical'"})
		return
	}

	lib := &library.Library{
		Name: body.Name,
		Path: body.Path,
		Type: body.Type,
	}
	if err := r.libraryService.Create(req.Context(), lib); err != nil {
		r.logger.Error("creating library", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusCreated, lib)
}

// handleUpdateLibrary updates an existing library.
// PUT /api/v1/libraries/{id}
func (r *Router) handleUpdateLibrary(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	existing, err := r.libraryService.GetByID(req.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "library not found"})
		return
	}

	var body struct {
		Name string `json:"name"`
		Path string `json:"path"`
		Type string `json:"type"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	if body.Name != "" {
		existing.Name = body.Name
	}
	if body.Path != "" {
		existing.Path = body.Path
	}
	if body.Type != "" {
		if body.Type != library.TypeRegular && body.Type != library.TypeClassical {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "type must be 'regular' or 'classical'"})
			return
		}
		existing.Type = body.Type
	}

	if err := r.libraryService.Update(req.Context(), existing); err != nil {
		r.logger.Error("updating library", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, existing)
}

// handleDeleteLibrary deletes a library if no artists reference it.
// DELETE /api/v1/libraries/{id}
func (r *Router) handleDeleteLibrary(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	if err := r.libraryService.Delete(req.Context(), id); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}
