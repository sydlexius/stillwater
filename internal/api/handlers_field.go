package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/web/templates"
)

// handleFieldDisplay returns the display-mode HTMX fragment for a single field.
// GET /api/v1/artists/{id}/fields/{field}/display
func (r *Router) handleFieldDisplay(w http.ResponseWriter, req *http.Request) {
	artistID := req.PathValue("id")
	field := req.PathValue("field")

	if !artist.IsEditableField(field) {
		writeError(w, req, http.StatusBadRequest, "unknown or non-editable field: "+field)
		return
	}

	a, err := r.artistService.GetByID(req.Context(), artistID)
	if err != nil {
		writeError(w, req, http.StatusNotFound, "artist not found")
		return
	}

	if isHTMXRequest(req) {
		providers := r.fieldProviderNames(req, field)
		renderTempl(w, req, templates.FieldDisplay(a, field, providers))
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"field": field,
		"value": artist.FieldValueFromArtist(a, field),
	})
}

// handleFieldEdit returns the edit-mode HTMX fragment for a single field.
// GET /api/v1/artists/{id}/fields/{field}/edit
func (r *Router) handleFieldEdit(w http.ResponseWriter, req *http.Request) {
	artistID := req.PathValue("id")
	field := req.PathValue("field")

	if !artist.IsEditableField(field) {
		writeError(w, req, http.StatusBadRequest, "unknown or non-editable field: "+field)
		return
	}

	a, err := r.artistService.GetByID(req.Context(), artistID)
	if err != nil {
		writeError(w, req, http.StatusNotFound, "artist not found")
		return
	}

	if isHTMXRequest(req) {
		renderTempl(w, req, templates.FieldEdit(a, field))
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"field": field,
		"value": artist.FieldValueFromArtist(a, field),
	})
}

// handleFieldUpdate saves a single field value.
// PATCH /api/v1/artists/{id}/fields/{field}
func (r *Router) handleFieldUpdate(w http.ResponseWriter, req *http.Request) {
	artistID := req.PathValue("id")
	field := req.PathValue("field")

	if !artist.IsEditableField(field) {
		writeError(w, req, http.StatusBadRequest, "unknown or non-editable field: "+field)
		return
	}

	value := extractFieldValue(req)

	if err := r.artistService.UpdateField(req.Context(), artistID, field, value); err != nil {
		writeError(w, req, http.StatusInternalServerError, "failed to update field")
		return
	}

	// Re-fetch the artist to return updated state
	a, err := r.artistService.GetByID(req.Context(), artistID)
	if err != nil {
		writeError(w, req, http.StatusInternalServerError, "failed to reload artist")
		return
	}

	if isHTMXRequest(req) {
		providers := r.fieldProviderNames(req, field)
		renderTempl(w, req, templates.FieldDisplay(a, field, providers))
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"status": "updated",
		"field":  field,
		"value":  artist.FieldValueFromArtist(a, field),
	})
}

// handleFieldClear clears a single field.
// DELETE /api/v1/artists/{id}/fields/{field}
func (r *Router) handleFieldClear(w http.ResponseWriter, req *http.Request) {
	artistID := req.PathValue("id")
	field := req.PathValue("field")

	if !artist.IsEditableField(field) {
		writeError(w, req, http.StatusBadRequest, "unknown or non-editable field: "+field)
		return
	}

	if err := r.artistService.ClearField(req.Context(), artistID, field); err != nil {
		writeError(w, req, http.StatusInternalServerError, "failed to clear field")
		return
	}

	a, err := r.artistService.GetByID(req.Context(), artistID)
	if err != nil {
		writeError(w, req, http.StatusInternalServerError, "failed to reload artist")
		return
	}

	if isHTMXRequest(req) {
		providers := r.fieldProviderNames(req, field)
		renderTempl(w, req, templates.FieldDisplay(a, field, providers))
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"status": "cleared",
		"field":  field,
	})
}

// handleClearMembers deletes all band members for an artist.
// DELETE /api/v1/artists/{id}/members
func (r *Router) handleClearMembers(w http.ResponseWriter, req *http.Request) {
	artistID := req.PathValue("id")

	if err := r.artistService.DeleteMembersByArtistID(req.Context(), artistID); err != nil {
		writeError(w, req, http.StatusInternalServerError, "failed to clear members")
		return
	}

	if isHTMXRequest(req) {
		providers := r.fieldProviderNames(req, "members")
		renderTempl(w, req, templates.MembersSection(artistID, nil, providers))
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "cleared"})
}

// handleSaveMembers accepts a JSON array of provider MemberInfo objects,
// converts them to BandMember records, and upserts them for the artist.
// POST /api/v1/artists/{id}/members/from-provider
func (r *Router) handleSaveMembers(w http.ResponseWriter, req *http.Request) {
	artistID := req.PathValue("id")

	var members []provider.MemberInfo
	if err := json.NewDecoder(req.Body).Decode(&members); err != nil {
		writeError(w, req, http.StatusBadRequest, "invalid request body")
		return
	}

	bandMembers := convertProviderMembers(artistID, members)
	if err := r.artistService.UpsertMembers(req.Context(), artistID, bandMembers); err != nil {
		writeError(w, req, http.StatusInternalServerError, "failed to save members")
		return
	}

	if isHTMXRequest(req) {
		saved, _ := r.artistService.ListMembersByArtistID(req.Context(), artistID)
		providers := r.fieldProviderNames(req, "members")
		w.Header().Set("HX-Trigger", "hideFieldProviderModal")
		renderTempl(w, req, templates.MembersSection(artistID, saved, providers))
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

// handleFieldProviders fetches a field from all configured providers and returns
// a side-by-side comparison UI.
// GET /api/v1/artists/{id}/fields/{field}/providers
func (r *Router) handleFieldProviders(w http.ResponseWriter, req *http.Request) {
	artistID := req.PathValue("id")
	field := req.PathValue("field")

	a, err := r.artistService.GetByID(req.Context(), artistID)
	if err != nil {
		writeError(w, req, http.StatusNotFound, "artist not found")
		return
	}

	results, err := r.orchestrator.FetchFieldFromProviders(
		req.Context(), a.MusicBrainzID, a.Name, field,
	)
	if err != nil {
		writeError(w, req, http.StatusInternalServerError, "failed to fetch from providers: "+err.Error())
		return
	}

	if isHTMXRequest(req) {
		if allProvidersMatch(field, results, a) {
			renderTempl(w, req, templates.FieldProviderNoChanges(field))
			return
		}
		currentValue := artist.FieldValueFromArtist(a, field)
		renderTempl(w, req, templates.FieldProviderModalContent(a, field, results, currentValue))
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"field":   field,
		"results": results,
	})
}

// extractFieldValue reads the field value from a PATCH request body.
// Supports both form-encoded and JSON payloads.
func extractFieldValue(req *http.Request) string {
	if strings.HasPrefix(req.Header.Get("Content-Type"), "application/json") {
		var body struct {
			Value string `json:"value"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err == nil {
			return body.Value
		}
		return ""
	}
	return req.FormValue("value")
}

// fieldProviderNames returns the provider name strings for a given field,
// used to render the provider logo stack in templates.
func (r *Router) fieldProviderNames(req *http.Request, field string) []string {
	priorities, err := r.providerSettings.GetPriorities(req.Context())
	if err != nil {
		return nil
	}
	for _, pri := range priorities {
		if pri.Field == field {
			names := make([]string, len(pri.Providers))
			for i, p := range pri.Providers {
				names[i] = string(p)
			}
			return names
		}
	}
	return nil
}

// allProvidersMatch returns true when every provider result that has data
// matches the artist's current value for the field. Returns false if no
// provider had data (so the user sees "no data" messages in the modal).
func allProvidersMatch(field string, results []provider.FieldProviderResult, a *artist.Artist) bool {
	// Members are stored in a separate table and returned via r.Members,
	// not r.Value. We cannot compare them here, so always show the modal.
	if field == "members" {
		return false
	}

	anyHasData := false
	for _, r := range results {
		if !r.HasData {
			continue
		}
		anyHasData = true
		if artist.IsSliceField(field) {
			if !slicesEqualIgnoreCase(artist.SliceFieldFromArtist(a, field), r.Values) {
				return false
			}
		} else {
			currentValue := artist.FieldValueFromArtist(a, field)
			if strings.TrimSpace(r.Value) != strings.TrimSpace(currentValue) {
				return false
			}
		}
	}
	return anyHasData
}

// slicesEqualIgnoreCase compares two string slices for equality, ignoring
// order and case.
func slicesEqualIgnoreCase(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	counts := make(map[string]int, len(a))
	for _, v := range a {
		counts[strings.ToLower(strings.TrimSpace(v))]++
	}
	for _, v := range b {
		key := strings.ToLower(strings.TrimSpace(v))
		counts[key]--
		if counts[key] < 0 {
			return false
		}
	}
	return true
}

// buildFieldProvidersMap builds a map of field name -> provider name strings
// for all metadata fields, used by the artist detail page.
func buildFieldProvidersMap(priorities []provider.FieldPriority) map[string][]string {
	m := make(map[string][]string, len(priorities))
	for _, pri := range priorities {
		names := make([]string, len(pri.Providers))
		for i, p := range pri.Providers {
			names[i] = string(p)
		}
		m[pri.Field] = names
	}
	return m
}
