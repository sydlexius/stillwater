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
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/provider/tagdict"
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
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
	}
	writeJSON(w, http.StatusOK, cfg)
}

// handlePutVocab replaces the metadata_vocab configuration. The request body
// must be a JSON object matching the VocabConfig shape: an "exclude" array of
// patterns and the per-field caps "max_genres", "max_styles", "max_moods".
// All keys are optional; absent keys default to the zero value (empty exclude
// list, unlimited counts).
//
// Validation rules:
//   - Body must be valid JSON; unknown keys are rejected.
//   - Each exclude pattern must be non-blank after trimming.
//   - Each per-field cap must be zero or positive.
//
// PUT /api/v1/settings/vocab
func (r *Router) handlePutVocab(w http.ResponseWriter, req *http.Request) {
	// Decode with strict unknown-field rejection so typos like "excludes"
	// (instead of "exclude") surface as errors rather than silently dropping.
	dec := json.NewDecoder(req.Body)
	dec.DisallowUnknownFields()

	var cfg tagdict.VocabConfig
	if err := dec.Decode(&cfg); err != nil {
		// Keep the client-facing message generic (consistent with the other
		// settings handlers); the decoder error detail is not surfaced.
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	// Exclude patterns must be non-blank: a blank pattern is meaningless and a
	// stray empty string is more likely a UI bug than intent.
	for i, p := range cfg.Exclude {
		if strings.TrimSpace(p) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "exclude[" + strconv.Itoa(i) + "] must not be blank",
			})
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
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": c.name + " must not be negative",
			})
			return
		}
	}

	blob, err := json.Marshal(cfg)
	if err != nil {
		r.logger.Error("marshaling metadata_vocab config", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	now := time.Now().UTC().Format(time.RFC3339)
	_, err = r.db.ExecContext(req.Context(),
		`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		SettingMetadataVocab, string(blob), now)
	if err != nil {
		r.logger.Error("saving metadata_vocab setting", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}
