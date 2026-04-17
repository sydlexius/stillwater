package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
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
	// Headers are already flushed at this point so we cannot change the status,
	// but a mid-stream encode failure leaves the client with a truncated file
	// and silent 200. Log it so operators can correlate broken downloads.
	if err := json.NewEncoder(w).Encode(envelope); err != nil {
		r.logger.Error("settings export encode failed", "error", err)
	}
}

func (r *Router) handleSettingsImport(w http.ResponseWriter, req *http.Request) {
	// HTMX does not swap on non-2xx by default, so for HX requests every error
	// path must respond with 200 plus a red HTML fragment -- otherwise the
	// #import-result div stays empty and the failure is silent. JSON callers
	// still get the real status code.
	writeImportErr := func(status int, msg string) {
		if req.Header.Get("HX-Request") == "true" {
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprintf(w, `<div class="text-sm text-red-600 dark:text-red-400">%s</div>`, html.EscapeString(msg)) //nolint:errcheck
			return
		}
		writeJSON(w, status, map[string]string{"error": msg})
	}

	if r.settingsIOService == nil {
		writeImportErr(http.StatusServiceUnavailable, "settings import not available")
		return
	}

	var envelope settingsio.Envelope
	var passphrase string

	ct := req.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "application/json") {
		// Direct JSON body with passphrase field
		body, err := io.ReadAll(io.LimitReader(req.Body, maxImportSize+1))
		if err != nil {
			writeImportErr(http.StatusBadRequest, "reading request body")
			return
		}
		if len(body) > maxImportSize {
			writeImportErr(http.StatusRequestEntityTooLarge, "file exceeds 10MB limit")
			return
		}

		// Expect {"passphrase": "...", "envelope": {...}}
		var payload struct {
			Passphrase string              `json:"passphrase"`
			Envelope   settingsio.Envelope `json:"envelope"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			writeImportErr(http.StatusBadRequest, "invalid JSON")
			return
		}
		envelope = payload.Envelope
		passphrase = payload.Passphrase
	} else {
		// Multipart form upload
		if err := req.ParseMultipartForm(maxImportSize); err != nil {
			writeImportErr(http.StatusBadRequest, "request too large or invalid multipart form")
			return
		}

		passphrase = req.FormValue("passphrase")

		file, _, err := req.FormFile("file")
		if err != nil {
			writeImportErr(http.StatusBadRequest, "missing file field")
			return
		}
		defer file.Close() //nolint:errcheck

		data, err := io.ReadAll(io.LimitReader(file, maxImportSize+1))
		if err != nil {
			writeImportErr(http.StatusBadRequest, "reading uploaded file")
			return
		}
		if len(data) > maxImportSize {
			writeImportErr(http.StatusRequestEntityTooLarge, "file exceeds 10MB limit")
			return
		}
		if err := json.Unmarshal(data, &envelope); err != nil {
			writeImportErr(http.StatusBadRequest, "invalid JSON in uploaded file")
			return
		}
	}

	if passphrase == "" {
		writeImportErr(http.StatusBadRequest, "passphrase is required")
		return
	}

	result, err := r.settingsIOService.Import(req.Context(), &envelope, passphrase)
	if err != nil {
		r.logger.Error("settings import failed", "error", err)
		// Build a safe user-facing message: never expose raw internal errors.
		// The passphrase/AES-GCM failure is the one case where a specific hint
		// is more helpful than a generic message; everything else uses a generic
		// "import failed" string so internal details do not leak to clients.
		// Distinguish client errors (400) from internal apply failures (500) so
		// monitoring and the client can tell them apart. Wrong passphrase and
		// unsupported envelope version are both caused by what the client sent.
		var clientMsg string
		status := http.StatusInternalServerError
		switch {
		case errors.Is(err, settingsio.ErrWrongPassphrase):
			clientMsg = "Import failed: incorrect passphrase or corrupted backup file"
			status = http.StatusBadRequest
		case errors.Is(err, settingsio.ErrUnsupportedVersion):
			clientMsg = "Import failed: this backup file uses an unsupported format version"
			status = http.StatusBadRequest
		default:
			clientMsg = "Import failed: see server logs for details"
		}
		writeImportErr(status, clientMsg)
		return
	}

	if req.Header.Get("HX-Request") == "true" {
		w.Header().Set("Content-Type", "text/html")
		// Local name avoids shadowing the html stdlib import used in
		// writeImportErr above.
		fragment := fmt.Sprintf(
			`<div class="text-sm text-green-600 dark:text-green-400">`+
				`Import complete: %d settings, %d connections, %d profiles, %d webhooks, %d provider keys, %d priorities,`+
				` %d rules, %d scraper configs, %d preferences.`+
				`</div>`,
			result.Settings, result.Connections, result.Profiles, result.Webhooks, result.ProviderKeys, result.Priorities,
			result.Rules, result.ScraperConfigs, result.UserPreferences,
		)
		w.Write([]byte(fragment)) //nolint:errcheck,gosec // G705: all format args are %d (integers)
		return
	}

	writeJSON(w, http.StatusOK, result)
}
