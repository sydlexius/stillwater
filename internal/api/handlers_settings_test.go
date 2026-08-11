package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandleUpdateSettings_CacheMaxSize_Invalid(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)
	body := `{"cache.image.max_size_mb": "-5"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/settings", strings.NewReader(body))
	w := httptest.NewRecorder()
	r.handleUpdateSettings(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for negative value, got %d", w.Code)
	}
}

func TestHandleUpdateSettings_CacheMaxSize_Valid(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)
	body := `{"cache.image.max_size_mb": "512"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/settings", strings.NewReader(body))
	w := httptest.NewRecorder()
	r.handleUpdateSettings(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleUpdateSettings_CacheMaxSize_Zero(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)
	body := `{"cache.image.max_size_mb": "0"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/settings", strings.NewReader(body))
	w := httptest.NewRecorder()
	r.handleUpdateSettings(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for zero (unlimited), got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleUpdateSettings_Threshold_Invalid(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		value string
	}{
		{"non-integer", "abc"},
		{"negative", "-1"},
		{"above 100", "101"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := testRouter(t)
			body := `{"provider.name_similarity_threshold": "` + tt.value + `"}`
			req := httptest.NewRequest(http.MethodPut, "/api/v1/settings", strings.NewReader(body))
			w := httptest.NewRecorder()
			r.handleUpdateSettings(w, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 for %s (%s), got %d: %s", tt.name, tt.value, w.Code, w.Body.String())
			}
		})
	}
}

// TestHandleUpdateSettings_LocalAuthCannotBeDisabled verifies that any attempt
// to set auth.providers.local.enabled to a falsy value is rejected with 400,
// including case and whitespace variants.
func TestHandleUpdateSettings_LocalAuthCannotBeDisabled(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		value string
	}{
		{"false string", "false"},
		{"zero string", "0"},
		{"uppercase False", "False"},
		{"padded false", " false "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := testRouter(t)
			body := `{"auth.providers.local.enabled": "` + tt.value + `"}`
			req := httptest.NewRequest(http.MethodPut, "/api/v1/settings", strings.NewReader(body))
			w := httptest.NewRecorder()
			r.handleUpdateSettings(w, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 for value %q, got %d: %s", tt.value, w.Code, w.Body.String())
			}
		})
	}
}

// TestHandleUpdateSettings_LocalAuthEnabled verifies that setting
// auth.providers.local.enabled to true is accepted.
func TestHandleUpdateSettings_LocalAuthEnabled(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)
	body := `{"auth.providers.local.enabled": "true"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/settings", strings.NewReader(body))
	w := httptest.NewRecorder()
	r.handleUpdateSettings(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

// TestHandleUpdateSettings_BasePath_Invalid verifies the validation rules
// for the editable SW_BASE_PATH override (#1005). Each case covers a rule
// the API documents: must start with "/", must not end with "/", must use
// the allowed character set.
func TestHandleUpdateSettings_BasePath_Invalid(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{"missing leading slash", "stillwater"},
		{"trailing slash", "/stillwater/"},
		{"disallowed chars (space)", "/still water"},
		{"disallowed chars (dot)", "/v1.2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := testRouter(t)
			body := `{"server.base_path": "` + tt.value + `"}`
			req := httptest.NewRequest(http.MethodPut, "/api/v1/settings", strings.NewReader(body))
			w := httptest.NewRecorder()
			r.handleUpdateSettings(w, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 for %q, got %d: %s", tt.value, w.Code, w.Body.String())
			}
		})
	}
}

// TestHandleUpdateSettings_BasePath_Valid covers the accepted shapes:
// the canonical "/" and a typical sub-path with hyphens/underscores. The
// follow-up GET asserts the canonical persisted form so a regression that
// returns 200 but stores a non-canonical value still fails.
func TestHandleUpdateSettings_BasePath_Valid(t *testing.T) {
	tests := []struct {
		name      string
		value     string
		canonical string // expected persisted value after canonicalization
	}{
		{"root", "/", "/"},
		{"empty (coerced to /)", "", "/"},
		{"simple sub-path", "/stillwater", "/stillwater"},
		{"hyphen sub-path", "/my-app", "/my-app"},
		{"nested", "/apps/stillwater", "/apps/stillwater"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := testRouter(t)
			body := `{"server.base_path": "` + tt.value + `"}`
			req := httptest.NewRequest(http.MethodPut, "/api/v1/settings", strings.NewReader(body))
			w := httptest.NewRecorder()
			r.handleUpdateSettings(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("expected 200 for %q, got %d: %s", tt.value, w.Code, w.Body.String())
			}

			// Follow-up GET to assert the canonical persisted value.
			getReq := httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil)
			getW := httptest.NewRecorder()
			r.handleGetSettings(getW, getReq)
			if getW.Code != http.StatusOK {
				t.Fatalf("GET /settings: status %d, body %s", getW.Code, getW.Body.String())
			}
			var settings map[string]any
			if err := json.Unmarshal(getW.Body.Bytes(), &settings); err != nil {
				t.Fatalf("unmarshal settings: %v", err)
			}
			got, _ := settings["server.base_path"].(string)
			if got != tt.canonical {
				t.Errorf("persisted server.base_path = %q, want canonical %q", got, tt.canonical)
			}
		})
	}
}

func TestHandleUpdateSettings_Threshold_Valid(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		value string
	}{
		{"mid-range", "75"},
		{"lower bound", "0"},
		{"upper bound", "100"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := testRouter(t)
			body := `{"provider.name_similarity_threshold": "` + tt.value + `"}`
			req := httptest.NewRequest(http.MethodPut, "/api/v1/settings", strings.NewReader(body))
			w := httptest.NewRecorder()
			r.handleUpdateSettings(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("expected 200 for %s (%s), got %d: %s", tt.name, tt.value, w.Code, w.Body.String())
			}
		})
	}
}

// TestHandleUpdateSettings_BaselineChoice verifies that the OOBE wizard's
// onboarding.baseline_choice transport key is translated to the derived
// foreign_files.baseline_completed flag and the original key is not persisted:
//   - "yes" -> flag="true", key removed from settings, 200
//   - "no"  -> flag="",     key removed from settings, 200
//   - other -> 400 BadRequest, no settings written
//
// The "other" path enforces an allowlist on the transport key so buggy callers
// can't silently activate the baseline flag with an unexpected value.
func TestHandleUpdateSettings_BaselineChoice(t *testing.T) {
	t.Parallel()

	cases := []struct {
		choice     string
		wantStatus int
		wantFlag   string // value of foreign_files.baseline_completed (empty if unset)
	}{
		{"yes", http.StatusOK, "true"},
		{"no", http.StatusOK, ""},
		// Normalisation: trimmed + lowercased before the switch.
		{"YES", http.StatusOK, "true"},
		{" yes ", http.StatusOK, "true"},
		{"No", http.StatusOK, ""},
	}

	for _, c := range cases {
		c := c
		t.Run("choice="+c.choice, func(t *testing.T) {
			t.Parallel()
			r, _ := testRouter(t)

			payload := map[string]string{
				"onboarding.completed":       "true",
				"onboarding.baseline_choice": c.choice,
			}
			bodyBytes, _ := json.Marshal(payload)
			req := httptest.NewRequest(http.MethodPut, "/api/v1/settings", strings.NewReader(string(bodyBytes)))
			w := httptest.NewRecorder()
			r.handleUpdateSettings(w, req)

			if w.Code != c.wantStatus {
				t.Fatalf("status = %d, want %d; body: %s", w.Code, c.wantStatus, w.Body.String())
			}

			// The transport-only key must never persist as a real setting.
			var transportRows int
			if err := r.db.QueryRowContext(context.Background(),
				`SELECT COUNT(*) FROM settings WHERE key = 'onboarding.baseline_choice'`).Scan(&transportRows); err != nil {
				t.Fatalf("counting transport key rows: %v", err)
			}
			if transportRows != 0 {
				t.Errorf("onboarding.baseline_choice persisted as a setting (rows=%d); transport key should be deleted before upsert", transportRows)
			}

			// Read back the derived flag value.
			var got string
			err := r.db.QueryRowContext(context.Background(),
				`SELECT COALESCE(value, '') FROM settings WHERE key = 'foreign_files.baseline_completed'`).Scan(&got)
			if err != nil {
				t.Fatalf("reading foreign_files.baseline_completed: %v", err)
			}
			if got != c.wantFlag {
				t.Errorf("foreign_files.baseline_completed = %q, want %q", got, c.wantFlag)
			}
		})
	}

	// Unexpected values must reject the entire settings update with 400 so the
	// caller is forced to send a known value rather than having an unknown one
	// silently dropped (which masked the original bug -- see #1698 review).
	// "YES" and "Yes" are NOT in this set -- they normalise to "yes" and pass.
	for _, badChoice := range []string{"", "maybe", "true", "1", "yeah"} {
		badChoice := badChoice
		t.Run("rejects choice="+badChoice, func(t *testing.T) {
			t.Parallel()
			r, _ := testRouter(t)

			payload := map[string]string{
				"onboarding.completed":       "true",
				"onboarding.baseline_choice": badChoice,
			}
			bodyBytes, _ := json.Marshal(payload)
			req := httptest.NewRequest(http.MethodPut, "/api/v1/settings", strings.NewReader(string(bodyBytes)))
			w := httptest.NewRecorder()
			r.handleUpdateSettings(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
			}
			// Nothing should have been persisted, including the otherwise-valid
			// onboarding.completed key -- the whole transaction must abort.
			var completedRows int
			if err := r.db.QueryRowContext(context.Background(),
				`SELECT COUNT(*) FROM settings WHERE key = 'onboarding.completed'`).Scan(&completedRows); err != nil {
				t.Fatalf("counting onboarding.completed rows: %v", err)
			}
			if completedRows != 0 {
				t.Errorf("settings updated despite 400 (onboarding.completed rows=%d)", completedRows)
			}
		})
	}

	// Separate subtest: when the key is entirely absent from the payload (a
	// non-OOBE settings update) the existing baseline flag must be left
	// alone. Guards against a future refactor accidentally clearing the
	// flag on every settings save.
	t.Run("key absent leaves existing flag unchanged", func(t *testing.T) {
		t.Parallel()
		r, _ := testRouter(t)

		_, err := r.db.ExecContext(context.Background(),
			`INSERT INTO settings (key, value, updated_at) VALUES ('foreign_files.baseline_completed', 'true', 'seed')
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`)
		if err != nil {
			t.Fatalf("seeding baseline flag: %v", err)
		}

		payload := map[string]string{
			"onboarding.completed": "true",
		}
		bodyBytes, _ := json.Marshal(payload)
		req := httptest.NewRequest(http.MethodPut, "/api/v1/settings", strings.NewReader(string(bodyBytes)))
		w := httptest.NewRecorder()
		r.handleUpdateSettings(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
		}

		var got string
		if err := r.db.QueryRowContext(context.Background(),
			`SELECT COALESCE(value, '') FROM settings WHERE key = 'foreign_files.baseline_completed'`).Scan(&got); err != nil {
			t.Fatalf("reading foreign_files.baseline_completed: %v", err)
		}
		if got != "true" {
			t.Errorf("foreign_files.baseline_completed = %q, want unchanged %q", got, "true")
		}
	})
}

// TestHandleUpdateSettings_MBIDRevalidate_Invalid covers the #3004 defect: the
// five mbid_revalidate.* keys shipped in #3003 with no settingValidators entry,
// so PUT stored any string with a 200 OK. The boot reader parses with
// fmt.Sscanf("%d"), which stops at the first non-digit and reports success, so
// a stored "0.5" was read back as a valid, in-range 0 -- and a name-similarity
// threshold of 0 matches every name, making the check pass everything.
//
// The fractional cases are the load-bearing ones. "abc" merely falls back to
// the default, which is benign; "0.5" and "25.5" survive the parse as real
// values, so only rejection at the write boundary keeps them out.
func TestHandleUpdateSettings_MBIDRevalidate_Invalid(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		key   string
		value string
	}{
		{"enabled non-boolean", "mbid_revalidate.enabled", "sometimes"},
		{"enabled empty", "mbid_revalidate.enabled", ""},
		{"interval_hours non-integer", "mbid_revalidate.interval_hours", "six"},
		{"interval_hours zero", "mbid_revalidate.interval_hours", "0"},
		{"interval_hours negative", "mbid_revalidate.interval_hours", "-1"},
		{"interval_hours fractional", "mbid_revalidate.interval_hours", "6.5"},
		{"max_per_pass non-integer", "mbid_revalidate.max_per_pass", "twenty"},
		{"max_per_pass zero", "mbid_revalidate.max_per_pass", "0"},
		{"max_per_pass fractional", "mbid_revalidate.max_per_pass", "25.5"},
		{"name_similarity fractional zero", "mbid_revalidate.name_similarity_threshold", "0.5"},
		{"name_similarity fractional", "mbid_revalidate.name_similarity_threshold", "25.5"},
		{"name_similarity scientific", "mbid_revalidate.name_similarity_threshold", "1e3"},
		{"name_similarity negative", "mbid_revalidate.name_similarity_threshold", "-1"},
		{"name_similarity above 100", "mbid_revalidate.name_similarity_threshold", "101"},
		{"catalogue fractional zero", "mbid_revalidate.catalogue_match_percent", "0.5"},
		{"catalogue negative", "mbid_revalidate.catalogue_match_percent", "-5"},
		{"catalogue above 100", "mbid_revalidate.catalogue_match_percent", "101"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r, _ := testRouter(t)
			payload, _ := json.Marshal(map[string]string{tt.key: tt.value})
			req := httptest.NewRequest(http.MethodPut, "/api/v1/settings", strings.NewReader(string(payload)))
			w := httptest.NewRecorder()
			r.handleUpdateSettings(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("PUT %s=%q: status = %d, want 400; body: %s",
					tt.key, tt.value, w.Code, w.Body.String())
			}
			// The error message must name the key, or an operator saving
			// several settings at once cannot tell which one was refused.
			if !strings.Contains(w.Body.String(), tt.key) {
				t.Errorf("PUT %s=%q: error body %s does not name the key",
					tt.key, tt.value, w.Body.String())
			}
			// A rejected value must not reach the settings table. Without
			// this the handler could 400 and still upsert, which is the
			// failure the 400 is supposed to prevent.
			var rows int
			if err := r.db.QueryRowContext(context.Background(),
				`SELECT COUNT(*) FROM settings WHERE key = ?`, tt.key).Scan(&rows); err != nil {
				t.Fatalf("counting rows for %s: %v", tt.key, err)
			}
			if rows != 0 {
				t.Errorf("PUT %s=%q returned 400 but persisted %d row(s)", tt.key, tt.value, rows)
			}
		})
	}
}

// TestHandleUpdateSettings_MBIDRevalidate_Valid asserts the accepted values
// persist in their canonical form. The boolean cases matter beyond acceptance:
// validateBool rewrites "1" and "TRUE" to "true", and the boot reader
// (getDBBoolSetting) tests v == "true" || v == "1", so an un-canonicalised
// "TRUE" would read back as DISABLED -- an operator turning the sweep on and
// silently getting nothing.
func TestHandleUpdateSettings_MBIDRevalidate_Valid(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		key   string
		value string
		want  string
	}{
		{"enabled true", "mbid_revalidate.enabled", "true", "true"},
		{"enabled uppercase canonicalised", "mbid_revalidate.enabled", "TRUE", "true"},
		{"enabled one canonicalised", "mbid_revalidate.enabled", "1", "true"},
		{"enabled false", "mbid_revalidate.enabled", "false", "false"},
		{"interval_hours", "mbid_revalidate.interval_hours", "6", "6"},
		{"max_per_pass", "mbid_revalidate.max_per_pass", "200", "200"},
		{"name_similarity lower bound", "mbid_revalidate.name_similarity_threshold", "0", "0"},
		{"name_similarity upper bound", "mbid_revalidate.name_similarity_threshold", "100", "100"},
		{"catalogue lower bound", "mbid_revalidate.catalogue_match_percent", "0", "0"},
		{"catalogue upper bound", "mbid_revalidate.catalogue_match_percent", "100", "100"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r, _ := testRouter(t)
			payload, _ := json.Marshal(map[string]string{tt.key: tt.value})
			req := httptest.NewRequest(http.MethodPut, "/api/v1/settings", strings.NewReader(string(payload)))
			w := httptest.NewRecorder()
			r.handleUpdateSettings(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("PUT %s=%q: status = %d, want 200; body: %s",
					tt.key, tt.value, w.Code, w.Body.String())
			}
			var got string
			if err := r.db.QueryRowContext(context.Background(),
				`SELECT value FROM settings WHERE key = ?`, tt.key).Scan(&got); err != nil {
				t.Fatalf("reading back %s: %v", tt.key, err)
			}
			if got != tt.want {
				t.Errorf("PUT %s=%q persisted %q, want %q", tt.key, tt.value, got, tt.want)
			}
		})
	}
}

// TestMBIDRevalidateKeysRegistered pins the five mbid_revalidate.* keys as
// registered, and the pre-rename name as absent.
//
// SCOPE: this test knows nothing about what the boot path actually reads -- it
// checks a hand-written list against the map. Deleting a map entry fails it;
// ADDING a sixth key at boot with no validator does NOT, because nothing here
// reads cmd/stillwater. That direction is covered by
// TestMBIDRevalidateSettingKeysMatchValidators in package main, which parses
// the boot package's source and asserts each key it finds against
// HasSettingValidator. (An earlier version of this comment claimed the
// added-key case was covered HERE. It was not, and a review caught it -- a
// guard that overstates its own coverage is worse than no guard, because it
// stops anyone looking for the real one.)
//
// It also pins the rename decided in #3004: mbid_revalidate.name_similarity was
// renamed to ...name_similarity_threshold for consistency with the pre-existing
// provider.name_similarity_threshold, which is a differently-scoped knob that
// operators were liable to confuse with it.
func TestMBIDRevalidateKeysRegistered(t *testing.T) {
	t.Parallel()
	for _, key := range []string{
		"mbid_revalidate.enabled",
		"mbid_revalidate.interval_hours",
		"mbid_revalidate.max_per_pass",
		"mbid_revalidate.name_similarity_threshold",
		"mbid_revalidate.catalogue_match_percent",
	} {
		if !HasSettingValidator(key) {
			t.Errorf("%s is read at boot but has no settingValidators entry: "+
				"PUT would store any string with a 200 OK (#3004)", key)
		}
	}
	// The pre-rename key must NOT be registered: a lingering entry would keep
	// the confusable name alive in the API surface after the rename.
	if HasSettingValidator("mbid_revalidate.name_similarity") {
		t.Error("mbid_revalidate.name_similarity is still registered; " +
			"it was renamed to mbid_revalidate.name_similarity_threshold in #3004")
	}
}

// TestHasSettingValidator covers the exported predicate other packages use to
// assert that a settings key they read is validated at the write boundary.
//
// The cases are chosen so the test fails if the function is ever stubbed to a
// constant: a `return true` passes the registered cases but fails the
// unregistered ones, and a `return false` does the reverse. That matters more
// than usual here, because a caller in another package cannot tell a working
// predicate from a stubbed one -- it would simply report every key as valid
// and go quietly blind, which is the failure mode this whole issue is about.
func TestHasSettingValidator(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		key  string
		want bool
	}{
		// Registered: one of each validator shape, so the test keeps meaning
		// if a single entry is re-typed.
		{"mbid_revalidate.enabled", true},
		{"mbid_revalidate.name_similarity_threshold", true},
		{"provider.name_similarity_threshold", true},
		{"server.base_path", true},
		// Not registered: real keys the boot path reads with no validator
		// today (tracked in #3005), plus the pre-rename name and a key that
		// cannot exist. If any of the first two gain a validator, flip the
		// expectation -- do not delete the case.
		{"logging.level", false},
		{"db_maintenance.enabled", false},
		{"mbid_revalidate.name_similarity", false},
		{"definitely.not.a.real.setting.key", false},
	} {
		if got := HasSettingValidator(c.key); got != c.want {
			t.Errorf("HasSettingValidator(%q) = %v, want %v", c.key, got, c.want)
		}
	}
}

// TestHandleUpdateSettings_BatchRejectionIsAllOrNothing pins the contract the
// OpenAPI description states: every value is validated before any write, so a
// batch containing one bad value persists NONE of it.
//
// The property is worth a test rather than an assumption because it is purely
// positional -- it holds only because the validation loop runs to completion
// and returns before the write loop begins. Moving the upsert into the
// validation loop, or validating lazily per key, would still pass every
// single-key test in this file while silently making a rejected batch write
// its valid prefix. Map iteration order is randomized in Go, so that failure
// would be non-deterministic: the good key lands only when it sorts first.
func TestHandleUpdateSettings_BatchRejectionIsAllOrNothing(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	const goodKey = "mbid_revalidate.interval_hours"
	const badKey = "mbid_revalidate.name_similarity_threshold"
	payload, _ := json.Marshal(map[string]string{
		goodKey: "6",   // valid on its own
		badKey:  "0.5", // rejected, and the reason #3004 exists
	})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/settings", strings.NewReader(string(payload)))
	w := httptest.NewRecorder()
	r.handleUpdateSettings(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("mixed batch: status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
	// The VALID key must not have landed either. This is the assertion the
	// contract rests on -- a per-key write loop would leave this row behind.
	var rows int
	if err := r.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM settings WHERE key = ?`, goodKey).Scan(&rows); err != nil {
		t.Fatalf("counting rows for %s: %v", goodKey, err)
	}
	if rows != 0 {
		t.Errorf("the valid key %q persisted (%d row(s)) despite the batch being rejected; "+
			"validation must complete before any write", goodKey, rows)
	}
}

// TestHandleUpdateSettings_MalformedBody covers the second of the two 400 paths
// this endpoint documents: a body that is not a JSON object of string values.
//
// It exists because the OpenAPI 400 description originally promised that "the
// error message names the offending key" for every 400 -- true of a validation
// failure, false here, where nothing was parsed and there is no key to name.
// A Copilot review caught the overreach. The test pins both halves of the
// corrected contract so the description cannot drift from the handler again:
// the malformed case reports "invalid request body" and names no key, and it
// still persists nothing.
func TestHandleUpdateSettings_MalformedBody(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		body string
	}{
		{"not json", "not json at all"},
		{"json array", `["mbid_revalidate.enabled"]`},
		{"non-string value", `{"mbid_revalidate.enabled": true}`},
		{"truncated object", `{"mbid_revalidate.enabled":`},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r, _ := testRouter(t)
			var before int
			if err := r.db.QueryRowContext(context.Background(),
				`SELECT COUNT(*) FROM settings`).Scan(&before); err != nil {
				t.Fatalf("counting settings rows before the request: %v", err)
			}

			req := httptest.NewRequest(http.MethodPut, "/api/v1/settings", strings.NewReader(c.body))
			w := httptest.NewRecorder()
			r.handleUpdateSettings(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("body %q: status = %d, want 400; body: %s", c.body, w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "invalid request body") {
				t.Errorf("body %q: error %s, want it to report \"invalid request body\"",
					c.body, w.Body.String())
			}
			// Nothing parsed, so nothing may have been written. Compare
			// against a BEFORE count rather than expecting an empty table:
			// migration 001 seeds ~20 default settings rows, so an absolute
			// "want 0" assertion fails on a correct handler (it did, when this
			// test was first written). The delta is the real property.
			var after int
			if err := r.db.QueryRowContext(context.Background(),
				`SELECT COUNT(*) FROM settings`).Scan(&after); err != nil {
				t.Fatalf("counting settings rows: %v", err)
			}
			if after != before {
				t.Errorf("body %q: rejected with 400 but the settings row count moved %d -> %d",
					c.body, before, after)
			}
		})
	}
}
