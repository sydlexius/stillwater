package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/web/templates"

	"golang.org/x/time/rate"
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
// By default, the key is tested before saving. If the test fails, an error is
// returned with a "Save anyway" option (skip_test=true). Supports both JSON
// body and form-encoded data (for HTMX forms).
func (r *Router) handleSetProviderKey(w http.ResponseWriter, req *http.Request) {
	name := provider.ProviderName(req.PathValue("name"))
	if !isValidProviderName(name) {
		writeError(w, req, http.StatusBadRequest, "unknown provider")
		return
	}

	var apiKey string
	var skipTest bool
	contentType := req.Header.Get("Content-Type")
	if strings.HasPrefix(contentType, "application/x-www-form-urlencoded") {
		if err := req.ParseForm(); err != nil {
			writeError(w, req, http.StatusBadRequest, "invalid form data")
			return
		}
		apiKey = req.FormValue("api_key")
		skipTest = req.FormValue("skip_test") == "true"
		// Spotify uses two fields (client_id + client_secret) combined as JSON
		if name == provider.NameSpotify && apiKey == "" {
			clientID := req.FormValue("client_id")
			clientSecret := req.FormValue("client_secret")
			if clientID != "" && clientSecret != "" {
				combined, err := json.Marshal(map[string]string{
					"client_id":     clientID,
					"client_secret": clientSecret,
				})
				if err != nil {
					writeError(w, req, http.StatusInternalServerError, "failed to encode credentials")
					return
				}
				apiKey = string(combined)
			}
		}
	} else {
		var body struct {
			APIKey       string `json:"api_key"`       //nolint:gosec // G117: not a hardcoded credential, this is user input
			SkipTest     bool   `json:"skip_test"`     //nolint:gosec // G101: not a credential
			ClientID     string `json:"client_id"`     //nolint:gosec // G101: not a credential
			ClientSecret string `json:"client_secret"` //nolint:gosec // G101: not a credential
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			writeError(w, req, http.StatusBadRequest, "invalid request body")
			return
		}
		apiKey = body.APIKey
		skipTest = body.SkipTest
		// Spotify: combine client_id + client_secret into JSON
		if name == provider.NameSpotify && apiKey == "" && body.ClientID != "" && body.ClientSecret != "" {
			combined, err := json.Marshal(map[string]string{
				"client_id":     body.ClientID,
				"client_secret": body.ClientSecret,
			})
			if err != nil {
				writeError(w, req, http.StatusInternalServerError, "failed to encode credentials")
				return
			}
			apiKey = string(combined)
		}
	}

	if name == provider.NameSpotify && apiKey == "" {
		writeError(w, req, http.StatusBadRequest, "both client_id and client_secret are required for Spotify")
		return
	}

	if apiKey == "" {
		writeError(w, req, http.StatusBadRequest, "api_key is required")
		return
	}

	isOOBE := strings.Contains(req.Header.Get("HX-Current-URL"), "/setup/wizard")

	// Test-before-save: if the provider supports testing, verify the key works
	// before persisting it. On failure, offer "Save anyway" (skip_test).
	if !skipTest {
		if p := r.providerRegistry.Get(name); p != nil {
			if testable, ok := p.(provider.TestableProvider); ok {
				testCtx, cancel := context.WithTimeout(req.Context(), 15*time.Second)
				defer cancel()
				testCtx = provider.WithAPIKeyOverride(testCtx, name, apiKey)
				if testErr := testable.TestConnection(testCtx); testErr != nil {
					r.logger.Error("provider key test failed before save", "provider", name, "error", testErr)
					sanitizedMsg := "Unable to verify provider credentials"
					if isHTMXRequest(req) {
						if isOOBE {
							w.Header().Set("HX-Retarget", "#ob-provider-card-"+string(name))
							w.Header().Set("HX-Reswap", "outerHTML")
						} else {
							w.Header().Set("HX-Retarget", "#provider-save-result-"+string(name))
							w.Header().Set("HX-Reswap", "innerHTML")
						}
						w.WriteHeader(http.StatusUnprocessableEntity)
						renderTempl(w, req, templates.ProviderTestSaveFailure(name, apiKey, sanitizedMsg, isOOBE))
						return
					}
					writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
						"status": "test_failed",
						"error":  sanitizedMsg,
					})
					return
				}
			}
		}
	}

	if err := r.providerSettings.SetAPIKey(req.Context(), name, apiKey); err != nil {
		r.logger.Error("setting provider API key", "provider", name, "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to save API key")
		return
	}

	// Persist test status: "ok" if we tested successfully, leave as "untested"
	// if we skipped the test or if the provider is not testable.
	if !skipTest {
		if p := r.providerRegistry.Get(name); p != nil {
			if _, ok := p.(provider.TestableProvider); ok {
				if err := r.providerSettings.SetKeyStatus(req.Context(), name, "ok"); err != nil {
					r.logger.Error("setting key status after save", "provider", name, "error", err)
				}
			}
		}
	}

	// For HTMX requests, re-render the provider card with updated status.
	if isHTMXRequest(req) {
		r.renderProviderCard(w, req, name, isOOBE)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

// renderProviderCard renders the appropriate provider card (Settings or OOBE).
func (r *Router) renderProviderCard(w http.ResponseWriter, req *http.Request, name provider.ProviderName, isOOBE bool) {
	statuses, err := r.providerSettings.ListProviderKeyStatuses(req.Context())
	if err != nil {
		r.logger.Error("listing provider statuses for card render", "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to list providers")
		return
	}
	for _, s := range statuses {
		if s.Name == name {
			if isOOBE {
				renderTempl(w, req, templates.OnboardingProviderCard(s))
			} else {
				renderTempl(w, req, templates.ProviderKeyCard(s))
			}
			return
		}
	}

	r.logger.Error("provider status not found for card render", "provider", name)
	writeError(w, req, http.StatusNotFound, "unknown provider")
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

// handleTestProvider tests the connection to a provider and persists the result.
// For HTMX requests, returns a test result fragment with an OOB status dot update.
func (r *Router) handleTestProvider(w http.ResponseWriter, req *http.Request) {
	name := provider.ProviderName(req.PathValue("name"))
	p := r.providerRegistry.Get(name)
	if p == nil {
		writeError(w, req, http.StatusBadRequest, "unknown provider")
		return
	}

	testable, ok := p.(provider.TestableProvider)
	if !ok {
		if isHTMXRequest(req) {
			renderTempl(w, req, templates.ProviderTestResult("ok", ""))
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "message": "provider does not support connection testing"})
		return
	}

	isOOBE := strings.Contains(req.Header.Get("HX-Current-URL"), "/setup/wizard")

	if err := testable.TestConnection(req.Context()); err != nil {
		r.logger.Error("provider test failed", "provider", name, "error", err)
		statusPersisted := true
		if setErr := r.providerSettings.SetKeyStatus(req.Context(), name, "invalid"); setErr != nil {
			r.logger.Error("persisting provider test failure status", "provider", name, "error", setErr)
			statusPersisted = false
		}
		if isHTMXRequest(req) {
			if isOOBE {
				// OOBE needs the full card re-render for layout.
				w.Header().Set("HX-Retarget", "#ob-provider-card-"+string(name))
				w.Header().Set("HX-Reswap", "outerHTML")
				r.renderProviderCard(w, req, name, isOOBE)
				return
			}
			// Only update the status dot if the status was actually persisted.
			if statusPersisted {
				renderTempl(w, req, templates.ProviderTestResultWithDot(string(name), "error", "Unable to verify provider credentials", "invalid"))
			} else {
				renderTempl(w, req, templates.ProviderTestResult("error", "Unable to verify provider credentials"))
			}
			return
		}
		resp := map[string]string{"status": "error", "error": "Unable to verify provider credentials"}
		if !statusPersisted {
			resp["status_persisted"] = "false"
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}

	statusPersisted := true
	if setErr := r.providerSettings.SetKeyStatus(req.Context(), name, "ok"); setErr != nil {
		r.logger.Error("persisting provider test success status", "provider", name, "error", setErr)
		statusPersisted = false
	}
	if isHTMXRequest(req) {
		if isOOBE {
			w.Header().Set("HX-Retarget", "#ob-provider-card-"+string(name))
			w.Header().Set("HX-Reswap", "outerHTML")
			r.renderProviderCard(w, req, name, isOOBE)
			return
		}
		// Only update the status dot if the status was actually persisted.
		if statusPersisted {
			renderTempl(w, req, templates.ProviderTestResultWithDot(string(name), "ok", "", "ok"))
		} else {
			renderTempl(w, req, templates.ProviderTestResult("ok", ""))
		}
		return
	}
	resp := map[string]string{"status": "ok"}
	if !statusPersisted {
		resp["status_persisted"] = "false"
	}
	writeJSON(w, http.StatusOK, resp)
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

	// Two-pass: validate all entries before persisting any, so a validation
	// failure on a later entry does not leave earlier entries partially saved.
	for _, p := range body.Priorities {
		if p.Field == "" {
			writeError(w, req, http.StatusBadRequest, "field name is required")
			return
		}
	}
	for _, p := range body.Priorities {
		if err := r.providerSettings.SetPriority(req.Context(), p.Field, p.Providers); err != nil {
			r.logger.Error("setting priority", "field", p.Field, "error", err)
			writeError(w, req, http.StatusInternalServerError, "failed to set priority")
			return
		}
	}

	// For HTMX requests, return the updated priority row
	if req.Header.Get("HX-Request") == "true" && len(body.Priorities) == 1 {
		keys, err := r.providerSettings.ListProviderKeyStatuses(req.Context())
		if err != nil {
			r.logger.Error("listing provider key statuses for priority row", "error", err)
		}
		renderTempl(w, req, templates.PriorityChipRow(body.Priorities[0], keys))
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

// handleToggleFieldProvider enables or disables a provider for a specific field.
// PUT /api/v1/providers/priorities/{field}/{provider}/toggle
func (r *Router) handleToggleFieldProvider(w http.ResponseWriter, req *http.Request) {
	field := req.PathValue("field")
	provName := provider.ProviderName(req.PathValue("provider"))

	if field == "" || provName == "" {
		writeError(w, req, http.StatusBadRequest, "field and provider are required")
		return
	}

	// Load current priorities to find this field.
	priorities, err := r.providerSettings.GetPriorities(req.Context())
	if err != nil {
		r.logger.Error("loading priorities for toggle", "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to load priorities")
		return
	}

	var pri *provider.FieldPriority
	for i := range priorities {
		if priorities[i].Field == field {
			pri = &priorities[i]
			break
		}
	}
	if pri == nil {
		writeError(w, req, http.StatusNotFound, "field not found")
		return
	}

	// Verify the provider is part of this field's priority list.
	provInField := false
	for _, p := range pri.Providers {
		if p == provName {
			provInField = true
			break
		}
	}
	if !provInField {
		writeError(w, req, http.StatusBadRequest, "provider not in field priority list")
		return
	}

	// Toggle: if provider is in the disabled list, remove it (enable).
	// If not in the disabled list, add it (disable).
	found := false
	var newDisabled []provider.ProviderName
	for _, d := range pri.Disabled {
		if d == provName {
			found = true
			continue // Remove from disabled = enable
		}
		newDisabled = append(newDisabled, d)
	}
	if !found {
		newDisabled = append(newDisabled, provName)
	}

	if err := r.providerSettings.SetDisabledProviders(req.Context(), field, newDisabled); err != nil {
		r.logger.Error("toggling field provider", "field", field, "provider", provName, "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to toggle provider")
		return
	}

	// For HTMX requests, return the updated priority row.
	if req.Header.Get("HX-Request") == "true" {
		pri.Disabled = newDisabled
		keys, err := r.providerSettings.ListProviderKeyStatuses(req.Context())
		if err != nil {
			r.logger.Error("listing provider key statuses for toggle", "error", err)
		}
		renderTempl(w, req, templates.PriorityChipRow(*pri, keys))
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "toggled"})
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

	ctx := r.injectMetadataLanguages(req.Context())
	result, err := r.orchestrator.FetchMetadata(ctx, body.MBID, body.Name, nil)
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
		writeError(w, req, http.StatusInternalServerError, "failed to update priorities")
		return
	}
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
					writeError(w, req, http.StatusInternalServerError, "failed to update priorities")
					return
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
					writeError(w, req, http.StatusInternalServerError, "failed to update priorities")
					return
				}
			}
		}
	}

	// For HTMX in OOBE context, re-render just the toggle card (avoids full page reload).
	// In Settings context, trigger a full page refresh so toggle + priority rows update.
	if isHTMXRequest(req) {
		if strings.Contains(req.Header.Get("HX-Current-URL"), "/setup/wizard") {
			status, err := r.providerSettings.ListWebSearchStatuses(req.Context())
			if err == nil {
				for _, s := range status {
					if s.Name == name {
						renderTempl(w, req, templates.OnboardingWebSearchToggle(s))
						return
					}
				}
			}
		}
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusOK)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleSetMirror configures a mirror base URL and optional rate limit for a provider.
// PUT /api/v1/providers/{name}/mirror
func (r *Router) handleSetMirror(w http.ResponseWriter, req *http.Request) {
	name := provider.ProviderName(req.PathValue("name"))
	if !isValidProviderName(name) {
		writeError(w, req, http.StatusBadRequest, "unknown provider")
		return
	}

	p := r.providerRegistry.Get(name)
	if p == nil {
		writeError(w, req, http.StatusBadRequest, "unknown provider")
		return
	}
	mirrorable, ok := p.(provider.MirrorableProvider)
	if !ok {
		writeError(w, req, http.StatusBadRequest, "provider does not support mirror configuration")
		return
	}

	var baseURL string
	var rateLimit float64
	contentType := req.Header.Get("Content-Type")
	if strings.HasPrefix(contentType, "application/x-www-form-urlencoded") {
		if err := req.ParseForm(); err != nil {
			writeError(w, req, http.StatusBadRequest, "invalid form data")
			return
		}
		baseURL = req.FormValue("base_url")
		if rl := req.FormValue("rate_limit"); rl != "" {
			parsed, err := strconv.ParseFloat(rl, 64)
			if err != nil {
				writeError(w, req, http.StatusBadRequest, "rate_limit must be a number")
				return
			}
			rateLimit = parsed
		}
	} else {
		var body struct {
			BaseURL   string  `json:"base_url"`
			RateLimit float64 `json:"rate_limit"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			writeError(w, req, http.StatusBadRequest, "invalid request body")
			return
		}
		baseURL = body.BaseURL
		rateLimit = body.RateLimit
	}

	if baseURL == "" {
		writeError(w, req, http.StatusBadRequest, "base_url is required")
		return
	}

	// Validate URL.
	parsed, err := url.Parse(baseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		writeError(w, req, http.StatusBadRequest, "base_url must be a valid http or https URL")
		return
	}
	baseURL = strings.TrimRight(baseURL, "/")

	// Validate rate limit: must be positive and capped at 100.
	if rateLimit <= 0 {
		rateLimit = 10 // default for mirrors
	}
	if rateLimit > 100 {
		rateLimit = 100
	}

	// Persist settings.
	if err := r.providerSettings.SetBaseURL(req.Context(), name, baseURL); err != nil {
		r.logger.Error("saving mirror base URL", "provider", name, "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to save mirror URL")
		return
	}
	if err := r.providerSettings.SetRateLimit(req.Context(), name, rateLimit); err != nil {
		r.logger.Error("saving mirror rate limit", "provider", name, "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to save rate limit")
		return
	}

	// Apply changes to the live adapter and rate limiter.
	mirrorable.SetBaseURL(baseURL)
	r.rateLimiters.SetLimit(name, rate.Limit(rateLimit))
	r.logger.Info("mirror configured", "provider", name,
		"base_url", baseURL, "rate_limit", rateLimit)

	// For HTMX requests, re-render the provider card. Mirrors can be tested
	// separately via POST /api/v1/providers/{name}/test.
	if isHTMXRequest(req) {
		isOOBE := strings.Contains(req.Header.Get("HX-Current-URL"), "/setup/wizard")
		w.Header().Set("HX-Retarget", "#provider-card-"+string(name))
		w.Header().Set("HX-Reswap", "innerHTML")
		r.renderProviderCard(w, req, name, isOOBE)
		return
	}

	// For JSON API consumers, auto-test the connection.
	testResult := "ok"
	testError := ""
	if testable, ok := p.(provider.TestableProvider); ok {
		testCtx, cancel := context.WithTimeout(req.Context(), 15*time.Second)
		defer cancel()
		if err := testable.TestConnection(testCtx); err != nil {
			r.logger.Error("mirror auto-test failed", "provider", name, "error", err)
			testResult = "error"
			testError = "Unable to verify provider credentials"
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"status":     "saved",
		"test":       testResult,
		"test_error": testError,
	})
}

// handleDeleteMirror removes the mirror configuration for a provider, reverting to defaults.
// DELETE /api/v1/providers/{name}/mirror
func (r *Router) handleDeleteMirror(w http.ResponseWriter, req *http.Request) {
	name := provider.ProviderName(req.PathValue("name"))
	if !isValidProviderName(name) {
		writeError(w, req, http.StatusBadRequest, "unknown provider")
		return
	}

	p := r.providerRegistry.Get(name)
	if p == nil {
		writeError(w, req, http.StatusBadRequest, "unknown provider")
		return
	}
	mirrorable, ok := p.(provider.MirrorableProvider)
	if !ok {
		writeError(w, req, http.StatusBadRequest, "provider does not support mirror configuration")
		return
	}

	// Clear persisted settings.
	if err := r.providerSettings.DeleteBaseURL(req.Context(), name); err != nil {
		r.logger.Error("deleting mirror base URL", "provider", name, "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to clear mirror URL")
		return
	}
	if err := r.providerSettings.DeleteRateLimit(req.Context(), name); err != nil {
		r.logger.Error("deleting mirror rate limit", "provider", name, "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to clear rate limit")
		return
	}

	// Revert adapter and rate limiter to defaults.
	mirrorable.SetBaseURL(mirrorable.DefaultBaseURL())
	defaultRL := provider.DefaultLimit(name)
	if defaultRL > 0 {
		r.rateLimiters.SetLimit(name, defaultRL)
	}
	r.logger.Info("mirror cleared", "provider", name)

	if isHTMXRequest(req) {
		isOOBE := strings.Contains(req.Header.Get("HX-Current-URL"), "/setup/wizard")
		w.Header().Set("HX-Retarget", "#provider-card-"+string(name))
		w.Header().Set("HX-Reswap", "innerHTML")
		r.renderProviderCard(w, req, name, isOOBE)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "cleared"})
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
