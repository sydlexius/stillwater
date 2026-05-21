package api

// handlers_vocab.go -- REST API for the metadata_vocab tag-filtering
// configuration. Vocab config is stored as a single JSON blob in the settings
// KV table under the key SettingMetadataVocab.
//
// Endpoints:
//   GET  /api/v1/settings/vocab  -- read current config (defaults when unset)
//   PUT  /api/v1/settings/vocab  -- replace config (full replace, not merge)
//
// Both endpoints require admin access (consistent with other settings routes).
// Input validation: the PUT body must be valid JSON that decodes to a
// VocabConfig. Exclude patterns must be non-blank and the per-field caps must
// not be negative. Unknown JSON keys are rejected to guard against typos that
// would otherwise be silently ignored.

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/provider/tagdict"
	"github.com/sydlexius/stillwater/web/templates"
)

// SettingMetadataVocab is the settings table key for the metadata_vocab blob.
// This constant is used by the API handlers and by injectMetadataLanguages to
// read the setting, so changing the key here changes it everywhere.
const SettingMetadataVocab = "metadata_vocab"

// handleGetVocab returns the current metadata_vocab configuration as a JSON
// object. When the setting has never been saved, the response is the default
// config (empty exclude list, zero caps -- a complete no-op).
//
// GET /api/v1/settings/vocab
func (r *Router) handleGetVocab(w http.ResponseWriter, req *http.Request) {
	raw := r.getStringSetting(req.Context(), SettingMetadataVocab, "")
	var cfg *tagdict.VocabConfig
	if raw == "" {
		cfg = tagdict.DefaultVocabConfig()
	} else {
		var err error
		cfg, err = tagdict.ParseVocabConfig(raw)
		// A corrupt stored blob surfaces as a 500 here so an admin sees the
		// problem and can re-save a valid config. The fetch paths
		// (loadVocabConfig) instead degrade to the no-op default, so a corrupt
		// blob never breaks metadata fetches -- only this settings screen.
		if err != nil {
			r.logger.Error("parsing metadata_vocab setting", "error", err)
			writeFormError(w, req, http.StatusInternalServerError, "internal error")
			return
		}
	}
	writeJSON(w, http.StatusOK, cfg)
}

// handlePutVocab replaces the metadata_vocab configuration. The request body
// may be either a JSON object or an application/x-www-form-urlencoded form
// (used by the HTMX Settings > Providers > Tag Sources form).
//
// JSON shape: {"exclude":[...], "max_genres":N, "max_styles":N, "max_moods":N}.
// Form fields: "exclude" (textarea text, split on newlines), "max_genres",
//
//	"max_styles", "max_moods".
//
// All keys are optional; absent keys default to the zero value (empty exclude
// list, unlimited counts).
//
// Validation rules (applied to both input paths):
//   - Each exclude pattern must be non-blank after trimming.
//   - Each per-field cap must be zero or positive.
//   - JSON path additionally rejects unknown top-level keys.
//
// PUT /api/v1/settings/vocab
func (r *Router) handlePutVocab(w http.ResponseWriter, req *http.Request) {
	var cfg tagdict.VocabConfig

	contentType := req.Header.Get("Content-Type")
	if strings.HasPrefix(contentType, "application/x-www-form-urlencoded") {
		// Form-encoded path: the Settings page submits this via HTMX so the UI
		// needs no JavaScript. The "exclude" field is a textarea where each
		// non-blank line becomes one pattern. The three max-count fields are
		// plain number inputs.
		if err := req.ParseForm(); err != nil {
			writeFormError(w, req, http.StatusBadRequest, "invalid form data")
			return
		}

		// Split the textarea on newlines, trim each line, drop blank lines.
		// Case is preserved at rest; pattern matching is case-insensitive at
		// filter time, so the stored value is exactly what the user typed.
		rawExclude := req.FormValue("exclude")
		var patterns []string
		for _, line := range strings.Split(rawExclude, "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				patterns = append(patterns, line)
			}
		}
		cfg.Exclude = patterns

		// Parse the three integer cap fields; absent or empty values default to 0.
		for _, f := range []struct {
			name string
			dest *int
		}{
			{"max_genres", &cfg.MaxGenres},
			{"max_styles", &cfg.MaxStyles},
			{"max_moods", &cfg.MaxMoods},
		} {
			if v := req.FormValue(f.name); v != "" {
				n, err := strconv.Atoi(v)
				if err != nil {
					writeFormError(w, req, http.StatusBadRequest, f.name+" must be an integer")
					return
				}
				*f.dest = n
			}
		}
	} else {
		// JSON path (default): decode with strict unknown-field rejection so
		// typos like "excludes" (instead of "exclude") surface as errors rather
		// than silently dropping data.
		dec := json.NewDecoder(req.Body)
		dec.DisallowUnknownFields()

		if err := dec.Decode(&cfg); err != nil {
			// Keep the client-facing message generic (consistent with the other
			// settings handlers); the decoder error detail is not surfaced.
			writeFormError(w, req, http.StatusBadRequest, "invalid request body")
			return
		}
		// Reject a body with trailing content after the first JSON object so a
		// payload like `{}{"x":1}` cannot smuggle a second object past validation.
		if err := dec.Decode(&struct{}{}); err != io.EOF {
			writeFormError(w, req, http.StatusBadRequest, "invalid request body")
			return
		}
	}

	// Exclude patterns must be non-blank: a blank pattern is meaningless and a
	// stray empty string is more likely a UI bug than intent.
	// (The form path strips blanks before this point, but the check runs for
	// both paths to guard against any future code path.)
	for i, p := range cfg.Exclude {
		if strings.TrimSpace(p) == "" {
			writeFormError(w, req, http.StatusBadRequest, "exclude["+strconv.Itoa(i)+"] must not be blank")
			return
		}
	}

	// Per-field caps must not be negative.
	for _, c := range []struct {
		name string
		val  int
	}{
		{"max_genres", cfg.MaxGenres},
		{"max_styles", cfg.MaxStyles},
		{"max_moods", cfg.MaxMoods},
	} {
		if c.val < 0 {
			writeFormError(w, req, http.StatusBadRequest, c.name+" must not be negative")
			return
		}
	}

	// Normalize Exclude to a non-nil slice so the persisted blob stores [] not
	// null, matching the documented no-op shape and the GET response.
	if cfg.Exclude == nil {
		cfg.Exclude = []string{}
	}
	blob, err := json.Marshal(cfg)
	if err != nil {
		r.logger.Error("marshaling metadata_vocab config", "error", err)
		writeFormError(w, req, http.StatusInternalServerError, "internal error")
		return
	}

	now := time.Now().UTC().Format(time.RFC3339)
	_, err = r.db.ExecContext(req.Context(),
		`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		SettingMetadataVocab, string(blob), now)
	if err != nil {
		r.logger.Error("saving metadata_vocab setting", "error", err)
		writeFormError(w, req, http.StatusInternalServerError, "internal error")
		return
	}

	if isHTMXRequest(req) {
		// The Settings > Providers Tag Sources form swaps this fragment into
		// its status span; show a friendly confirmation, not the raw JSON body.
		renderTempl(w, req, templates.VocabSaveResult())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}
