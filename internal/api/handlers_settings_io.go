package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/settingsio"
)

const maxImportSize = 10 << 20 // 10 MB

func (r *Router) handleSettingsExport(w http.ResponseWriter, req *http.Request) {
	if r.settingsIOService == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "settings export not available"})
		return
	}

	// Read passphrase from POST body (form-encoded or JSON)
	var passphrase string
	ct := req.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "application/json") {
		var body struct {
			Passphrase string `json:"passphrase"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
			return
		}
		passphrase = body.Passphrase
	} else {
		if err := req.ParseForm(); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid form data"})
			return
		}
		passphrase = req.FormValue("passphrase")
	}

	if passphrase == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "passphrase is required"})
		return
	}

	envelope, err := r.settingsIOService.Export(req.Context(), passphrase)
	if err != nil {
		r.logger.Error("settings export failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "export failed"})
		return
	}

	filename := fmt.Sprintf("stillwater-settings-%s.json", time.Now().UTC().Format("20060102-150405"))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	json.NewEncoder(w).Encode(envelope) //nolint:errcheck
}

func (r *Router) handleSettingsImport(w http.ResponseWriter, req *http.Request) {
	if r.settingsIOService == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "settings import not available"})
		return
	}

	var envelope settingsio.Envelope
	var passphrase string

	ct := req.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "application/json") {
		// Direct JSON body with passphrase field
		body, err := io.ReadAll(io.LimitReader(req.Body, maxImportSize+1))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "reading request body"})
			return
		}
		if len(body) > maxImportSize {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "file exceeds 10MB limit"})
			return
		}

		// Expect {"passphrase": "...", "envelope": {...}}
		var payload struct {
			Passphrase string              `json:"passphrase"`
			Envelope   settingsio.Envelope `json:"envelope"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
			return
		}
		envelope = payload.Envelope
		passphrase = payload.Passphrase
	} else {
		// Multipart form upload
		if err := req.ParseMultipartForm(maxImportSize); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "request too large or invalid multipart form"})
			return
		}

		passphrase = req.FormValue("passphrase")

		file, _, err := req.FormFile("file")
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing file field"})
			return
		}
		defer file.Close() //nolint:errcheck

		data, err := io.ReadAll(io.LimitReader(file, maxImportSize+1))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "reading uploaded file"})
			return
		}
		if len(data) > maxImportSize {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "file exceeds 10MB limit"})
			return
		}
		if err := json.Unmarshal(data, &envelope); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON in uploaded file"})
			return
		}
	}

	if passphrase == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "passphrase is required"})
		return
	}

	result, err := r.settingsIOService.Import(req.Context(), &envelope, passphrase)
	if err != nil {
		r.logger.Error("settings import failed", "error", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	if req.Header.Get("HX-Request") == "true" {
		w.Header().Set("Content-Type", "text/html")
		html := fmt.Sprintf(
			`<div class="text-sm text-green-600 dark:text-green-400">`+
				`Import complete: %d settings, %d connections, %d profiles, %d webhooks, %d provider keys, %d priorities.`+
				`</div>`,
			result.Settings, result.Connections, result.Profiles, result.Webhooks, result.ProviderKeys, result.Priorities,
		)
		w.Write([]byte(html)) //nolint:errcheck,gosec // G705: all format args are %d (integers)
		return
	}

	writeJSON(w, http.StatusOK, result)
}
