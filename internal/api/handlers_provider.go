package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/web/templates"
)

// handleListProviders returns the status of all providers and their API key configuration.
func (r *Router) handleListProviders(w http.ResponseWriter, req *http.Request) {
	statuses, err := r.providerSettings.ListProviderKeyStatuses(req.Context())
	if err != nil {
		r.logger.Error("listing provider statuses", "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to list providers")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": statuses})
}

// handleSetProviderKey stores an encrypted API key for a provider.
// Supports both JSON body and form-encoded data (for HTMX forms).
func (r *Router) handleSetProviderKey(w http.ResponseWriter, req *http.Request) {
	name := provider.ProviderName(req.PathValue("name"))
	if !isValidProviderName(name) {
		writeError(w, req, http.StatusBadRequest, "unknown provider")
		return
	}

	var apiKey string
	contentType := req.Header.Get("Content-Type")
	if strings.HasPrefix(contentType, "application/x-www-form-urlencoded") {
		if err := req.ParseForm(); err != nil {
			writeError(w, req, http.StatusBadRequest, "invalid form data")
			return
		}
		apiKey = req.FormValue("api_key")
	} else {
		var body struct {
			APIKey string `json:"api_key"` //nolint:gosec // G117: not a hardcoded credential, this is user input
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			writeError(w, req, http.StatusBadRequest, "invalid request body")
			return
		}
		apiKey = body.APIKey
	}

	if apiKey == "" {
		writeError(w, req, http.StatusBadRequest, "api_key is required")
		return
	}

	if err := r.providerSettings.SetAPIKey(req.Context(), name, apiKey); err != nil {
		r.logger.Error("setting provider API key", "provider", name, "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to save API key")
		return
	}

	// For HTMX requests, re-render the provider card with updated status
	if req.Header.Get("HX-Request") == "true" {
		statuses, err := r.providerSettings.ListProviderKeyStatuses(req.Context())
		if err == nil {
			for _, s := range statuses {
				if s.Name == name {
					renderTempl(w, req, templates.ProviderKeyCard(s))
					return
				}
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

// handleDeleteProviderKey removes the API key for a provider.
func (r *Router) handleDeleteProviderKey(w http.ResponseWriter, req *http.Request) {
	name := provider.ProviderName(req.PathValue("name"))
	if !isValidProviderName(name) {
		writeError(w, req, http.StatusBadRequest, "unknown provider")
		return
	}

	if err := r.providerSettings.DeleteAPIKey(req.Context(), name); err != nil {
		r.logger.Error("deleting provider API key", "provider", name, "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to delete API key")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// handleTestProvider tests the connection to a provider.
// For HTMX requests, returns an HTML fragment with the test result.
func (r *Router) handleTestProvider(w http.ResponseWriter, req *http.Request) {
	name := provider.ProviderName(req.PathValue("name"))
	p := r.providerRegistry.Get(name)
	if p == nil {
		writeError(w, req, http.StatusBadRequest, "unknown provider")
		return
	}

	testable, ok := p.(provider.TestableProvider)
	if !ok {
		if req.Header.Get("HX-Request") == "true" {
			renderTempl(w, req, templates.ProviderTestResult("ok", ""))
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "message": "provider does not support connection testing"})
		return
	}

	if err := testable.TestConnection(req.Context()); err != nil {
		if req.Header.Get("HX-Request") == "true" {
			renderTempl(w, req, templates.ProviderTestResult("error", err.Error()))
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "error", "error": err.Error()})
		return
	}

	if req.Header.Get("HX-Request") == "true" {
		renderTempl(w, req, templates.ProviderTestResult("ok", ""))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleGetPriorities returns the provider priority configuration.
func (r *Router) handleGetPriorities(w http.ResponseWriter, req *http.Request) {
	priorities, err := r.providerSettings.GetPriorities(req.Context())
	if err != nil {
		r.logger.Error("getting priorities", "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to get priorities")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"priorities": priorities})
}

// handleSetPriorities updates the provider priority configuration.
// For HTMX requests, returns the updated priority row fragment.
func (r *Router) handleSetPriorities(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Priorities []provider.FieldPriority `json:"priorities"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeError(w, req, http.StatusBadRequest, "invalid request body")
		return
	}

	for _, p := range body.Priorities {
		if p.Field == "" {
			writeError(w, req, http.StatusBadRequest, "field name is required")
			return
		}
		if err := r.providerSettings.SetPriority(req.Context(), p.Field, p.Providers); err != nil {
			r.logger.Error("setting priority", "field", p.Field, "error", err)
			writeError(w, req, http.StatusInternalServerError, "failed to set priority")
			return
		}
	}

	// For HTMX requests, return the updated priority row
	if req.Header.Get("HX-Request") == "true" && len(body.Priorities) == 1 {
		renderTempl(w, req, templates.PriorityRow(body.Priorities[0]))
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

// handleProviderSearch searches all providers for an artist by name.
func (r *Router) handleProviderSearch(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeError(w, req, http.StatusBadRequest, "invalid request body")
		return
	}
	if body.Name == "" {
		writeError(w, req, http.StatusBadRequest, "name is required")
		return
	}

	results, err := r.orchestrator.Search(req.Context(), body.Name)
	if err != nil {
		r.logger.Error("provider search", "error", err)
		writeError(w, req, http.StatusInternalServerError, "search failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// handleProviderFetch fetches metadata from providers using the orchestrator.
func (r *Router) handleProviderFetch(w http.ResponseWriter, req *http.Request) {
	var body struct {
		MBID string `json:"mbid"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeError(w, req, http.StatusBadRequest, "invalid request body")
		return
	}
	if body.MBID == "" && body.Name == "" {
		writeError(w, req, http.StatusBadRequest, "mbid or name is required")
		return
	}

	result, err := r.orchestrator.FetchMetadata(req.Context(), body.MBID, body.Name)
	if err != nil {
		r.logger.Error("provider fetch", "error", err)
		writeError(w, req, http.StatusInternalServerError, "fetch failed")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// handleGetWebSearchProviders returns the enabled state of all web search providers.
// GET /api/v1/providers/websearch
func (r *Router) handleGetWebSearchProviders(w http.ResponseWriter, req *http.Request) {
	statuses, err := r.providerSettings.ListWebSearchStatuses(req.Context())
	if err != nil {
		r.logger.Error("listing web search statuses", "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to list web search providers")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": statuses})
}

// handleSetWebSearchEnabled toggles the enabled state of a web search provider.
// When enabled, the provider is added to image field priority lists at lowest position.
// When disabled, it is removed from all priority lists.
// PUT /api/v1/providers/websearch/{name}/toggle
func (r *Router) handleSetWebSearchEnabled(w http.ResponseWriter, req *http.Request) {
	name := provider.ProviderName(req.PathValue("name"))

	valid := false
	for _, n := range provider.AllWebSearchProviderNames() {
		if n == name {
			valid = true
			break
		}
	}
	if !valid {
		writeError(w, req, http.StatusBadRequest, "unknown web search provider")
		return
	}

	var enabled bool
	contentType := req.Header.Get("Content-Type")
	if strings.HasPrefix(contentType, "application/json") {
		var body struct {
			Enabled bool `json:"enabled"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			writeError(w, req, http.StatusBadRequest, "invalid request body")
			return
		}
		enabled = body.Enabled
	} else {
		if err := req.ParseForm(); err != nil {
			writeError(w, req, http.StatusBadRequest, "invalid form data")
			return
		}
		enabled = req.FormValue("enabled") == "true"
	}

	if err := r.providerSettings.SetWebSearchEnabled(req.Context(), name, enabled); err != nil {
		r.logger.Error("setting web search enabled", "provider", name, "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to update setting")
		return
	}

	// Update image field priority lists
	imageFields := []string{"thumb", "fanart", "logo", "banner"}
	priorities, err := r.providerSettings.GetPriorities(req.Context())
	if err != nil {
		r.logger.Error("getting priorities for web search toggle", "error", err)
	} else {
		for _, pri := range priorities {
			isImageField := false
			for _, f := range imageFields {
				if pri.Field == f {
					isImageField = true
					break
				}
			}
			if !isImageField {
				continue
			}

			if enabled {
				// Add at end if not already present
				found := false
				for _, p := range pri.Providers {
					if p == name {
						found = true
						break
					}
				}
				if !found {
					pri.Providers = append(pri.Providers, name)
					if err := r.providerSettings.SetPriority(req.Context(), pri.Field, pri.Providers); err != nil {
						r.logger.Error("adding web search to priority", "field", pri.Field, "error", err)
					}
				}
			} else {
				// Remove from list
				var filtered []provider.ProviderName
				for _, p := range pri.Providers {
					if p != name {
						filtered = append(filtered, p)
					}
				}
				if len(filtered) != len(pri.Providers) {
					if err := r.providerSettings.SetPriority(req.Context(), pri.Field, filtered); err != nil {
						r.logger.Error("removing web search from priority", "field", pri.Field, "error", err)
					}
				}
			}
		}
	}

	// For HTMX, trigger a full page refresh so toggle + priority rows update
	if isHTMXRequest(req) {
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusOK)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// isValidProviderName checks if a provider name is one of the known providers.
func isValidProviderName(name provider.ProviderName) bool {
	for _, n := range provider.AllProviderNames() {
		if n == name {
			return true
		}
	}
	return false
}
