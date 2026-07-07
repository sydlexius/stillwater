package api

import (
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/i18n"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/web/templates"

	"golang.org/x/time/rate"
)

// providerTestFailureMessage maps a TestConnection error to an accurate,
// localized user-facing message. It distinguishes a credential failure
// (missing/invalid key, or a 401/403 from a mirror) from a connectivity/URL
// failure (unreachable host, bad base URL, timeout), so a self-hosted mirror
// that fails because its base URL is wrong no longer reports "credentials"
// (#2278). Anything else falls back to a generic test-failed message. The raw
// error is logged separately by the caller (scrubbed); this only produces the
// display string.
func providerTestFailureMessage(ctx context.Context, testErr error) string {
	tr := i18n.TFromCtx(ctx)
	switch {
	case provider.IsAuthError(testErr):
		return tr.T("settings.provider_keys.test_failed.credentials")
	case provider.IsConnectivityError(testErr):
		return tr.T("settings.provider_keys.test_failed.connectivity")
	default:
		return tr.T("settings.provider_keys.test_failed.generic")
	}
}

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

// providerKeyInput holds the parsed result of a set-provider-key request.
type providerKeyInput struct {
	APIKey   string
	SkipTest bool
}

// buildSpotifyKey encodes a client_id + client_secret pair into the JSON
// blob Stillwater stores as the Spotify API key.
func buildSpotifyKey(clientID, clientSecret string) (string, error) {
	b, err := json.Marshal(map[string]string{
		"client_id":     clientID,
		"client_secret": clientSecret,
	})
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// parseProviderKeyForm parses a form-encoded set-provider-key request body.
// Writes an error response and returns (_, false) on any parse failure.
func parseProviderKeyForm(w http.ResponseWriter, req *http.Request, name provider.ProviderName) (providerKeyInput, bool) {
	if err := req.ParseForm(); err != nil {
		writeError(w, req, http.StatusBadRequest, "invalid form data")
		return providerKeyInput{}, false
	}
	apiKey := req.FormValue("api_key")
	skipTest := req.FormValue("skip_test") == "true"
	// Spotify uses two fields (client_id + client_secret) combined as JSON.
	if name == provider.NameSpotify && apiKey == "" {
		clientID := req.FormValue("client_id")
		clientSecret := req.FormValue("client_secret")
		if clientID != "" && clientSecret != "" {
			key, err := buildSpotifyKey(clientID, clientSecret)
			if err != nil {
				writeError(w, req, http.StatusInternalServerError, "failed to encode credentials")
				return providerKeyInput{}, false
			}
			apiKey = key
		}
	}
	return providerKeyInput{APIKey: apiKey, SkipTest: skipTest}, true
}

// parseProviderKeyJSON parses a JSON-encoded set-provider-key request body.
// Writes an error response and returns (_, false) on any parse failure.
func parseProviderKeyJSON(w http.ResponseWriter, req *http.Request, name provider.ProviderName) (providerKeyInput, bool) {
	var body struct {
		APIKey       string `json:"api_key"`
		SkipTest     bool   `json:"skip_test"`
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	dec := json.NewDecoder(req.Body)
	if err := dec.Decode(&body); err != nil {
		writeError(w, req, http.StatusBadRequest, "invalid request body")
		return providerKeyInput{}, false
	}
	// Reject trailing content after the first document: a clean body decodes to
	// exactly one value, so a second Decode must report io.EOF. Anything else
	// (another JSON document, junk) widens the contract and is rejected.
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeError(w, req, http.StatusBadRequest, "request body must contain a single JSON document")
		return providerKeyInput{}, false
	}
	apiKey := body.APIKey
	// Spotify: combine client_id + client_secret into JSON when api_key absent.
	if name == provider.NameSpotify && apiKey == "" && body.ClientID != "" && body.ClientSecret != "" {
		key, err := buildSpotifyKey(body.ClientID, body.ClientSecret)
		if err != nil {
			writeError(w, req, http.StatusInternalServerError, "failed to encode credentials")
			return providerKeyInput{}, false
		}
		apiKey = key
	}
	return providerKeyInput{APIKey: apiKey, SkipTest: body.SkipTest}, true
}

// parseProviderKeyInput dispatches to parseProviderKeyForm or parseProviderKeyJSON
// based on the request media type. Only form-urlencoded and JSON bodies are
// accepted; any other (or missing) Content-Type is rejected with 415 rather than
// silently falling through to the JSON decoder. Returns (input, true) on success;
// writes an error response and returns (_, false) on failure.
func parseProviderKeyInput(w http.ResponseWriter, req *http.Request, name provider.ProviderName) (providerKeyInput, bool) {
	mediaType, _, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
	if err != nil {
		writeError(w, req, http.StatusUnsupportedMediaType, "unsupported content type")
		return providerKeyInput{}, false
	}
	switch mediaType {
	case "application/x-www-form-urlencoded":
		return parseProviderKeyForm(w, req, name)
	case "application/json":
		return parseProviderKeyJSON(w, req, name)
	default:
		writeError(w, req, http.StatusUnsupportedMediaType, "unsupported content type")
		return providerKeyInput{}, false
	}
}

// testProviderKeyFailed runs a connection test for the given apiKey.
// Returns true and writes the appropriate error response if the test fails.
// Returns false when the test passes or the provider is not testable.
func (r *Router) testProviderKeyFailed(w http.ResponseWriter, req *http.Request, name provider.ProviderName, apiKey string, isOOBE bool) bool {
	p := r.providerRegistry.Get(name)
	if p == nil {
		return false
	}
	testable, ok := p.(provider.TestableProvider)
	if !ok {
		return false
	}
	testCtx, cancel := context.WithTimeout(req.Context(), 15*time.Second)
	defer cancel()
	testCtx = provider.WithAPIKeyOverride(testCtx, name, apiKey)
	testErr := testable.TestConnection(testCtx)
	if testErr == nil {
		return false
	}
	// Scrub the raw error: provider TestConnection failures can wrap a request
	// URL whose query string carries the credential (e.g. Fanart.tv's api_key),
	// which url.Error does not redact. ScrubError redacts sensitive params.
	r.logger.Error("provider key test failed before save", "provider", name, "error", provider.ScrubError(testErr))
	sanitizedMsg := providerTestFailureMessage(req.Context(), testErr)
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
		return true
	}
	writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
		"status": "test_failed",
		"error":  sanitizedMsg,
	})
	return true
}

// persistProviderKeyStatus records "ok" test status after a successful save.
// Only writes if the provider is testable; non-fatal on error.
func (r *Router) persistProviderKeyStatus(ctx context.Context, name provider.ProviderName) {
	p := r.providerRegistry.Get(name)
	if p == nil {
		return
	}
	if _, ok := p.(provider.TestableProvider); !ok {
		return
	}
	if err := r.providerSettings.SetKeyStatus(ctx, name, "ok"); err != nil {
		r.logger.Error("setting key status after save", "provider", name, "error", err)
	}
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

	input, ok := parseProviderKeyInput(w, req, name)
	if !ok {
		return
	}

	if name == provider.NameSpotify && input.APIKey == "" {
		writeError(w, req, http.StatusBadRequest, "both client_id and client_secret are required for Spotify")
		return
	}

	if input.APIKey == "" {
		writeError(w, req, http.StatusBadRequest, "api_key is required")
		return
	}

	isOOBE := strings.Contains(req.Header.Get("HX-Current-URL"), "/setup/wizard")

	// Test-before-save: verify the key works before persisting it.
	// On failure, offer "Save anyway" (skip_test=true).
	if !input.SkipTest && r.testProviderKeyFailed(w, req, name, input.APIKey, isOOBE) {
		return
	}

	if err := r.providerSettings.SetAPIKey(req.Context(), name, input.APIKey); err != nil {
		r.logger.Error("setting provider API key", "provider", name, "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to save API key")
		return
	}

	// Persist "ok" test status only when the test was not skipped.
	if !input.SkipTest {
		r.persistProviderKeyStatus(req.Context(), name)
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
	for i := range statuses {
		if statuses[i].Name == name {
			if isOOBE {
				renderTempl(w, req, templates.OnboardingProviderCard(statuses[i]))
			} else {
				renderTempl(w, req, templates.ProviderKeyCard(statuses[i]))
			}
			return
		}
	}

	r.logger.Error("provider status not found for card render", "provider", name)
	writeError(w, req, http.StatusNotFound, "unknown provider")
}

// handleDeleteProviderKey removes the API key for a provider (#2218). Credential
// scope only: it clears the stored key (and, for Spotify, the combined
// client_id/client_secret blob -- both are stored as one encrypted value, see
// SettingsService.DeleteAPIKey) plus the persisted test status. It does not
// touch priority-list participation; ListProviderKeyStatuses already derives
// the "unconfigured" status implicitly from HasKey, so the provider drops out
// of active lookups without any separate priority mutation here.
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
	r.logger.Info("provider API key cleared", "provider", name)

	if isHTMXRequest(req) {
		isOOBE := strings.Contains(req.Header.Get("HX-Current-URL"), "/setup/wizard")
		w.Header().Set("HX-Retarget", "#provider-card-"+string(name))
		w.Header().Set("HX-Reswap", "innerHTML")
		r.renderProviderCard(w, req, name, isOOBE)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// handleTestProvider tests the connection to a provider and persists the result.
// For HTMX requests: OOBE re-renders the full provider card; non-OOBE returns a
// test result fragment with a status dot update only when persistence succeeds.
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
		failMsg := providerTestFailureMessage(req.Context(), err)
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
				renderTempl(w, req, templates.ProviderTestResultWithDot(string(name), "error", failMsg, "invalid"))
			} else {
				renderTempl(w, req, templates.ProviderTestResult("error", failMsg))
			}
			return
		}
		resp := map[string]any{"status": "error", "error": failMsg}
		if !statusPersisted {
			resp["status_persisted"] = false
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
	resp := map[string]any{"status": "ok"}
	if !statusPersisted {
		resp["status_persisted"] = false
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

// handleResetPriorities deletes all stored provider.priority.* settings rows
// so GetPriorities falls back to DefaultPriorities. For HTMX requests, returns
// the re-rendered priority chip rows fragment; otherwise returns JSON.
func (r *Router) handleResetPriorities(w http.ResponseWriter, req *http.Request) {
	if err := r.providerSettings.ResetPriorities(req.Context()); err != nil {
		r.logger.Error("resetting priorities", "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to reset priorities")
		return
	}

	priorities, err := r.providerSettings.GetPriorities(req.Context())
	if err != nil {
		r.logger.Error("loading priorities after reset", "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to load priorities")
		return
	}

	if req.Header.Get("HX-Request") == "true" {
		keys, err := r.providerSettings.ListProviderKeyStatuses(req.Context())
		if err != nil {
			r.logger.Error("listing provider key statuses after reset", "error", err)
		}
		renderTempl(w, req, templates.PriorityChipRows(priorities, keys))
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"status": "reset", "priorities": priorities})
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

// isValidWebSearchProviderName reports whether name is a known web search
// provider.
func isValidWebSearchProviderName(name provider.ProviderName) bool {
	for _, n := range provider.AllWebSearchProviderNames() {
		if n == name {
			return true
		}
	}
	return false
}

// parseWebSearchEnabledRequest extracts the "enabled" flag from a JSON or
// form-encoded request body. On a parse failure it writes the error
// response itself and returns ok=false.
func parseWebSearchEnabledRequest(w http.ResponseWriter, req *http.Request) (enabled, ok bool) {
	contentType := req.Header.Get("Content-Type")
	if strings.HasPrefix(contentType, "application/json") {
		var body struct {
			Enabled bool `json:"enabled"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			writeError(w, req, http.StatusBadRequest, "invalid request body")
			return false, false
		}
		return body.Enabled, true
	}
	if err := req.ParseForm(); err != nil {
		writeError(w, req, http.StatusBadRequest, "invalid form data")
		return false, false
	}
	return req.FormValue("enabled") == "true", true
}

// respondWebSearchToggle writes the response for a web search toggle: an
// OOB card re-render when called from the onboarding wizard, an HTMX
// refresh trigger for the Settings context, or a plain JSON status for
// non-HTMX callers.
func (r *Router) respondWebSearchToggle(w http.ResponseWriter, req *http.Request, name provider.ProviderName) {
	if !isHTMXRequest(req) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	if strings.Contains(req.Header.Get("HX-Current-URL"), "/setup/wizard") {
		status, err := r.providerSettings.ListWebSearchStatuses(req.Context())
		if err != nil {
			// Full error to slog; the client still gets the generic HX-Refresh
			// fallback below rather than a sanitized error page.
			r.logger.Error("listing web search statuses for onboarding fragment", "provider", name, "error", err)
		} else {
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
}

// handleSetWebSearchEnabled toggles the enabled state of a web search provider.
// When enabled, the provider is added to image field priority lists at lowest position.
// When disabled, it is removed from all priority lists.
// PUT /api/v1/providers/websearch/{name}/toggle
func (r *Router) handleSetWebSearchEnabled(w http.ResponseWriter, req *http.Request) {
	name := provider.ProviderName(req.PathValue("name"))
	if !isValidWebSearchProviderName(name) {
		writeError(w, req, http.StatusBadRequest, "unknown web search provider")
		return
	}

	enabled, ok := parseWebSearchEnabledRequest(w, req)
	if !ok {
		return
	}

	// Persist the enabled flag and sync the image-field priority lists as one
	// serialized operation so concurrent toggles cannot lose each other's
	// updates.
	if err := r.providerSettings.SetWebSearchEnabledAndSyncPriorities(req.Context(), name, enabled); err != nil {
		r.logger.Error("setting web search enabled", "provider", name, "enabled", enabled, "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to update setting")
		return
	}

	r.respondWebSearchToggle(w, req, name)
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
			testError = providerTestFailureMessage(req.Context(), err)
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

	// Clear any persisted test status so it does not resurface if a mirror
	// is later re-added. Non-fatal: the consumer-side gate in
	// ListProviderKeyStatuses also suppresses stale status display.
	if err := r.providerSettings.SetKeyStatus(req.Context(), name, ""); err != nil {
		r.logger.Error("clearing mirror test status", "provider", name, "error", err)
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

// handleGetProviderConfig returns the current field verbosity configuration for
// a provider. The response includes a verbosity map keyed by field name.
//
// GET /api/v1/providers/{name}/config
func (r *Router) handleGetProviderConfig(w http.ResponseWriter, req *http.Request) {
	name := provider.ProviderName(req.PathValue("name"))
	if !isValidProviderName(name) {
		writeError(w, req, http.StatusBadRequest, "unknown provider")
		return
	}

	opts := provider.FieldVerbosityOptions(name)
	values := make(map[string]string, len(opts))
	for _, fv := range opts {
		v, err := r.providerSettings.GetFieldVerbosity(req.Context(), name, fv.Field)
		if err != nil {
			r.logger.Error("reading field verbosity", "provider", name, "field", fv.Field, "error", err)
			writeError(w, req, http.StatusInternalServerError, "failed to read configuration")
			return
		}
		values[fv.Field] = v
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"provider":  string(name),
		"verbosity": values,
	})
}

// handleSetProviderConfig stores field verbosity selections for a provider.
// Accepts form-encoded data (HTMX) or a JSON body. Form fields use the naming
// convention "verbosity_{field}" (e.g. verbosity_biography=full). JSON body
// uses the shape {"verbosity_by_field": {"biography": "full"}}.
//
// PUT /api/v1/providers/{name}/config
func (r *Router) handleSetProviderConfig(w http.ResponseWriter, req *http.Request) {
	name := provider.ProviderName(req.PathValue("name"))
	if !isValidProviderName(name) {
		writeFormError(w, req, http.StatusBadRequest, "Unknown provider.")
		return
	}

	opts := provider.FieldVerbosityOptions(name)
	if len(opts) == 0 {
		writeFormError(w, req, http.StatusBadRequest, "This provider has no configurable field verbosity.")
		return
	}

	// Parse the incoming verbosity values -- supports both form-encoded
	// (from HTMX select onchange) and JSON (for API clients).
	verbosity := make(map[string]string)
	contentType := req.Header.Get("Content-Type")
	if strings.HasPrefix(contentType, "application/x-www-form-urlencoded") {
		if err := req.ParseForm(); err != nil {
			writeFormError(w, req, http.StatusBadRequest, "Invalid form data.")
			return
		}
		// Each verbosity select is named "verbosity_{field}".
		for _, fv := range opts {
			if v := req.FormValue("verbosity_" + fv.Field); v != "" {
				verbosity[fv.Field] = v
			}
		}
	} else {
		var body struct {
			VerbosityByField map[string]string `json:"verbosity_by_field"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			writeFormError(w, req, http.StatusBadRequest, "Invalid request body.")
			return
		}
		verbosity = body.VerbosityByField
	}

	if len(verbosity) == 0 {
		writeFormError(w, req, http.StatusBadRequest, "No verbosity values provided.")
		return
	}

	// Validate every (field, value) against the catalogue before persisting any
	// of them, so that a failure inside the persist loop below is a genuine
	// server error (500), not a client input error (400).
	for field, value := range verbosity {
		fieldOpts := provider.VerbosityOptionsForField(name, field)
		if len(fieldOpts) == 0 {
			writeFormError(w, req, http.StatusBadRequest, "Unknown verbosity field "+field+".")
			return
		}
		if !provider.IsValidVerbosity(fieldOpts, value) {
			writeFormError(w, req, http.StatusBadRequest, "Invalid verbosity value for field "+field+".")
			return
		}
	}

	// Persist each validated field value. An error here is a server-side
	// failure (the values were already validated above).
	for field, value := range verbosity {
		if err := r.providerSettings.SetFieldVerbosity(req.Context(), name, field, value); err != nil {
			r.logger.Error("storing field verbosity", "provider", name, "field", field, "error", err)
			writeFormError(w, req, http.StatusInternalServerError, "Failed to save provider configuration.")
			return
		}
	}

	writeFormSuccess(w, req, "Provider configuration saved.")
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
