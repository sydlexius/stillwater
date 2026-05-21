package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/library"
	"github.com/sydlexius/stillwater/internal/watcher"
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
	r.populateFSNotifySupported(libs)
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

	cleanPath, err := library.ValidatePath(body.Path)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if cleanPath != "" {
		if err := library.CheckPathExists(cleanPath); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	}
	body.Path = cleanPath

	lib := &library.Library{
		Name: body.Name,
		Path: body.Path,
		Type: body.Type,
	}
	if err := r.libraryService.Create(req.Context(), lib); err != nil {
		msg := err.Error()
		lower := strings.ToLower(msg)
		if strings.Contains(lower, "duplicate") || strings.Contains(lower, "unique") || strings.Contains(lower, "already exists") {
			writeJSON(w, http.StatusConflict, map[string]string{"error": msg})
			return
		}
		r.logger.Error("creating library", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	if lib.IsClassical() {
		setClassicalDeprecationHeaders(w)
	}
	writeJSON(w, http.StatusCreated, lib)
}

// handleUpdateLibrary updates an existing library.
// PUT /api/v1/libraries/{id}
//
//nolint:gocognit // Multi-field PATCH/form handler; each branch is a necessary tristate guard (JSON pointer vs form present/absent). Extracted as much as possible; further reduction would require a reflection-based mapper that is harder to read.
func (r *Router) handleUpdateLibrary(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	existing, err := r.libraryService.GetByID(req.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "library not found"})
		return
	}

	var body struct {
		Name           string `json:"name"`
		Path           string `json:"path"`
		Type           string `json:"type"`
		FSWatch        *int   `json:"fs_watch"`
		FSPollInterval *int   `json:"fs_poll_interval"`
		NFOLockData    *bool  `json:"nfo_lock_data"`
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
		// Only treat nfo_lock_data as present when the form actually
		// carried the key, so an absent field preserves the existing
		// value (parity with the JSON pointer-as-tristate semantics
		// below). Accept the usual boolean strings plus "on" (browsers
		// post "on" for a checked unnamed-value checkbox). The
		// FormValue calls above have already triggered ParseForm, so
		// req.PostForm is populated; we read it directly to distinguish
		// "absent key" (preserve) from "present and empty" (reject).
		if vs, ok := req.PostForm["nfo_lock_data"]; ok && len(vs) > 0 {
			raw := strings.ToLower(strings.TrimSpace(vs[0]))
			var v bool
			if raw == "on" {
				v = true
			} else if parsed, err := strconv.ParseBool(raw); err == nil {
				v = parsed
			} else {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "nfo_lock_data must be a boolean"})
				return
			}
			body.NFOLockData = &v
		}
	}

	if body.Name != "" {
		existing.Name = body.Name
	}
	if body.Path != "" {
		cleanPath, pathErr := library.ValidatePath(body.Path)
		if pathErr != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": pathErr.Error()})
			return
		}
		if err := library.CheckPathExists(cleanPath); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		existing.Path = cleanPath
	}
	if body.Type != "" {
		if body.Type != library.TypeRegular && body.Type != library.TypeClassical {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "type must be 'regular' or 'classical'"})
			return
		}
		existing.Type = body.Type
	}
	if body.FSWatch != nil {
		v := *body.FSWatch
		if v < 0 || v > 3 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "fs_watch must be 0-3"})
			return
		}
		existing.FSWatch = v
	}
	if body.FSPollInterval != nil {
		if !library.IsValidPollInterval(*body.FSPollInterval) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "fs_poll_interval must be 60, 300, 900, or 1800"})
			return
		}
		existing.FSPollInterval = *body.FSPollInterval
	}
	if body.NFOLockData != nil {
		existing.NFOLockData = *body.NFOLockData
	}

	if err := r.libraryService.Update(req.Context(), existing); err != nil {
		r.logger.Error("updating library", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	if existing.IsClassical() {
		setClassicalDeprecationHeaders(w)
	}
	r.populateFSNotifySupportedPtr(existing)
	writeJSON(w, http.StatusOK, existing)
}

// handlePatchLibrary applies a partial update to an existing library.
// Unlike PUT, only the fields present in the request body are modified.
// Currently supports: type (used by the Convert to Regular action).
// PATCH /api/v1/libraries/{id}
func (r *Router) handlePatchLibrary(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	existing, err := r.libraryService.GetByID(req.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "library not found"})
		return
	}

	var body struct {
		Type *string `json:"type"`
	}
	if strings.HasPrefix(req.Header.Get("Content-Type"), "application/json") {
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
			return
		}
	} else {
		// Use ParseForm so we can distinguish a missing key from an empty
		// value: req.FormValue silently returns "" for both.
		if err := req.ParseForm(); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid form body"})
			return
		}
		if _, present := req.PostForm["type"]; present {
			raw := req.PostForm.Get("type")
			body.Type = &raw
		}
	}

	if body.Type == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "type is required"})
		return
	}
	t := strings.TrimSpace(*body.Type)
	if t == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "type is required"})
		return
	}
	if t != library.TypeRegular && t != library.TypeClassical {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "type must be 'regular' or 'classical'"})
		return
	}
	existing.Type = t

	if err := r.libraryService.Update(req.Context(), existing); err != nil {
		r.logger.Error("patching library", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	if existing.IsClassical() {
		setClassicalDeprecationHeaders(w)
	}
	r.populateFSNotifySupportedPtr(existing)
	writeJSON(w, http.StatusOK, existing)
}

// setClassicalDeprecationHeaders sets RFC 8594/9745 deprecation signaling
// headers on a response that creates or returns a Classical library.
// The Sunset date is stored in library.SunsetClassicalType; update that
// constant once v1.3.0 has a firm release date.
func setClassicalDeprecationHeaders(w http.ResponseWriter) {
	w.Header().Set("Deprecation", "true")
	w.Header().Set("Sunset", library.SunsetClassicalType)
}

// handleDeleteLibrary deletes a library. When ?deleteArtists=true is set, all
// artists belonging to the library are also deleted; otherwise they are
// dereferenced (library_id set to NULL).
// DELETE /api/v1/libraries/{id}
func (r *Router) handleDeleteLibrary(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")

	var err error
	if req.URL.Query().Get("deleteArtists") == "true" {
		err = r.libraryService.DeleteWithArtists(req.Context(), id)
	} else {
		// Dismiss active violations before the delete NULLs library_id
		// (after which the association is lost and cleanup is impossible).
		if _, dismissErr := r.ruleService.DismissViolationsForLibrary(req.Context(), id); dismissErr != nil {
			r.logger.Error("dismissing violations for library removal", "library_id", id, "error", dismissErr)
			writeJSON(w, http.StatusInternalServerError, map[string]string{
				"error": "failed to clean up library violations; deletion was not performed",
			})
			return
		}
		err = r.libraryService.Delete(req.Context(), id)
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	// When the last local library is removed, auto-disable filesystem-dependent
	// rules so they do not evaluate against artists without filesystem paths.
	r.maybeDisableFilesystemRules(req.Context())

	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// maybeDisableFilesystemRules checks whether any local library (with a filesystem
// path) still exists. If none remain, all enabled filesystem-dependent rules are
// automatically disabled so they do not produce false violations for API-only artists.
func (r *Router) maybeDisableFilesystemRules(ctx context.Context) {
	hasLocal, err := r.libraryService.HasLocalLibrary(ctx)
	if err != nil {
		r.logger.Error("checking for local libraries after delete", "error", err)
		return
	}
	if hasLocal {
		return // at least one local library still exists; nothing to do
	}

	count, err := r.ruleService.DisableFilesystemRules(ctx)
	if err != nil {
		r.logger.Error("auto-disabling filesystem rules", "error", err)
		return
	}
	if count > 0 {
		r.logger.Info("auto-disabled filesystem-dependent rules because no local library remains",
			"rules_disabled", count)
		if r.ruleEngine != nil {
			r.ruleEngine.InvalidateRuleCache()
		}
	}
}

// populateFSNotifySupported sets the FSNotifySupported field on each library
// from the probe cache. This is a runtime-only field not stored in the DB.
// For paths not yet probed, an on-demand probe is run and cached.
func (r *Router) populateFSNotifySupported(libs []library.Library) {
	if r.probeCache == nil {
		return
	}
	for i := range libs {
		if libs[i].IsPathless() {
			continue
		}
		r.resolveProbe(&libs[i])
	}
}

// populateFSNotifySupportedPtr is the single-library pointer variant used
// by update handlers so the caller's struct is mutated directly.
func (r *Router) populateFSNotifySupportedPtr(lib *library.Library) {
	if r.probeCache == nil || lib.IsPathless() {
		return
	}
	r.resolveProbe(lib)
}

// resolveProbe sets FSNotifySupported from the probe cache, running an
// on-demand probe when no cached result exists for the path.
func (r *Router) resolveProbe(lib *library.Library) {
	supported, ok := r.probeCache.Get(lib.Path)
	if !ok {
		supported = watcher.ProbeFSNotify(lib.Path, 2*time.Second)
		r.probeCache.Set(lib.Path, supported)
	}
	lib.FSNotifySupported = supported
}
