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

// -- Unit tests for validator functions --

func TestValidatePositiveInt(t *testing.T) {
	t.Parallel()
	fn := validatePositiveInt("my_key")
	cases := []struct {
		input   string
		wantErr bool
	}{
		{"1", false},
		{"100", false},
		{"0", true},
		{"-1", true},
		{"abc", true},
		{"", true},
	}
	for _, c := range cases {
		canon, err := fn(c.input)
		if c.wantErr && err == nil {
			t.Errorf("input %q: expected error, got canonical %q", c.input, canon)
		}
		if !c.wantErr && err != nil {
			t.Errorf("input %q: unexpected error: %v", c.input, err)
		}
		if !c.wantErr && canon != c.input {
			t.Errorf("input %q: canonical = %q, want %q", c.input, canon, c.input)
		}
	}
}

func TestValidateNonNegativeInt(t *testing.T) {
	t.Parallel()
	fn := validateNonNegativeInt("my_key")
	cases := []struct {
		input   string
		wantErr bool
	}{
		{"0", false},
		{"1", false},
		{"999", false},
		{"-1", true},
		{"abc", true},
		{"", true},
	}
	for _, c := range cases {
		_, err := fn(c.input)
		if c.wantErr && err == nil {
			t.Errorf("input %q: expected error", c.input)
		}
		if !c.wantErr && err != nil {
			t.Errorf("input %q: unexpected error: %v", c.input, err)
		}
	}
}

func TestValidateIntRange(t *testing.T) {
	t.Parallel()
	fn := validateIntRange("my_key", 5, 10)
	cases := []struct {
		input   string
		wantErr bool
	}{
		{"5", false},
		{"7", false},
		{"10", false},
		{"4", true},
		{"11", true},
		{"abc", true},
	}
	for _, c := range cases {
		_, err := fn(c.input)
		if c.wantErr && err == nil {
			t.Errorf("input %q: expected error", c.input)
		}
		if !c.wantErr && err != nil {
			t.Errorf("input %q: unexpected error: %v", c.input, err)
		}
	}
}

// TestOpsSettingsValidators covers the four operational settings surfaced from
// env-only into the UI (#1746, #1753): the registry wiring plus each
// validator's canonicalisation and bounds.
func TestOpsSettingsValidators(t *testing.T) {
	t.Parallel()

	// rule_engine.artist_workers: bounded int 1..64.
	workers, ok := settingValidators["rule_engine.artist_workers"]
	if !ok {
		t.Fatal("rule_engine.artist_workers not registered in settingValidators")
	}
	workerCases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"1", "1", false},
		{"2", "2", false},
		{"64", "64", false},
		{"0", "", true},
		{"65", "", true},
		{"-1", "", true},
		{"abc", "", true},
	}
	for _, c := range workerCases {
		got, err := workers(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("artist_workers(%q): expected error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("artist_workers(%q): unexpected error: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("artist_workers(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	// scanner.exclusions: CSV normaliser -- trim tokens, drop empties, rejoin.
	excl, ok := settingValidators["scanner.exclusions"]
	if !ok {
		t.Fatal("scanner.exclusions not registered in settingValidators")
	}
	exclCases := []struct{ in, want string }{
		{"Various Artists, Soundtrack", "Various Artists, Soundtrack"},
		{" VA ,  , OST ", "VA, OST"},
		{"", ""},
		{"  ,  ", ""},
		{"Single", "Single"},
	}
	for _, c := range exclCases {
		got, err := excl(c.in)
		if err != nil {
			t.Errorf("exclusions(%q): unexpected error: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("exclusions(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	// scanner.mtime_fast_path: bool, canonicalised to true/false.
	mtime, ok := settingValidators["scanner.mtime_fast_path"]
	if !ok {
		t.Fatal("scanner.mtime_fast_path not registered in settingValidators")
	}
	mtimeCases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"true", "true", false},
		{"TRUE", "true", false},
		{" 1 ", "true", false},
		{"false", "false", false},
		{"0", "false", false},
		{"yes", "", true},
		{"", "", true},
	}
	for _, c := range mtimeCases {
		got, err := mtime(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("mtime(%q): expected error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("mtime(%q): unexpected error: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("mtime(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	// backup.interval_hours: positive int (>= 1).
	interval, ok := settingValidators["backup.interval_hours"]
	if !ok {
		t.Fatal("backup.interval_hours not registered in settingValidators")
	}
	for _, c := range []struct {
		in      string
		wantErr bool
	}{
		{"1", false},
		{"24", false},
		{"0", true},
		{"-3", true},
		{"abc", true},
	} {
		_, err := interval(c.in)
		if c.wantErr && err == nil {
			t.Errorf("interval(%q): expected error", c.in)
		}
		if !c.wantErr && err != nil {
			t.Errorf("interval(%q): unexpected error: %v", c.in, err)
		}
	}
}

func TestValidateEnum(t *testing.T) {
	t.Parallel()
	fn := validateEnum("my_key", "alpha", "beta", "gamma")
	cases := []struct {
		input   string
		wantErr bool
	}{
		{"alpha", false},
		{"beta", false},
		{"gamma", false},
		{"delta", true},
		{"", true},
		{"Alpha", true}, // case-sensitive
	}
	for _, c := range cases {
		_, err := fn(c.input)
		if c.wantErr && err == nil {
			t.Errorf("input %q: expected error", c.input)
		}
		if !c.wantErr && err != nil {
			t.Errorf("input %q: unexpected error: %v", c.input, err)
		}
	}
}

func TestValidateRuleScheduleMinutes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		input   string
		wantErr bool
	}{
		{"0", false}, // disabled
		{"5", false}, // minimum non-zero
		{"60", false},
		{"1", true}, // 1-4 rejected
		{"4", true},
		{"-1", true},
		{"abc", true},
	}
	for _, c := range cases {
		_, err := validateRuleScheduleMinutes(c.input)
		if c.wantErr && err == nil {
			t.Errorf("input %q: expected error", c.input)
		}
		if !c.wantErr && err != nil {
			t.Errorf("input %q: unexpected error: %v", c.input, err)
		}
	}
}

func TestValidateBasePath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		input     string
		wantErr   bool
		canonical string
	}{
		{"/", false, "/"},
		{"", false, "/"},    // normalised to /
		{"   ", false, "/"}, // whitespace normalised to /
		{"/app", false, "/app"},
		{"/my-app", false, "/my-app"},
		{"/apps/stillwater", false, "/apps/stillwater"},
		{"app", true, ""},       // missing leading /
		{"/app/", true, ""},     // trailing /
		{"//app", true, ""},     // double leading /
		{"/app name", true, ""}, // space not allowed
		{"/v1.0", true, ""},     // dot not allowed
	}
	for _, c := range cases {
		got, err := validateBasePath(c.input)
		if c.wantErr {
			if err == nil {
				t.Errorf("input %q: expected error, got canonical %q", c.input, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("input %q: unexpected error: %v", c.input, err)
			continue
		}
		if got != c.canonical {
			t.Errorf("input %q: canonical = %q, want %q", c.input, got, c.canonical)
		}
	}
}

func TestValidateLocalAuthEnabled(t *testing.T) {
	t.Parallel()
	cases := []struct {
		input     string
		wantErr   bool
		canonical string
	}{
		{"true", false, "true"},
		{"1", false, "true"},
		{"TRUE", false, "true"},   // normalised
		{" true ", false, "true"}, // trimmed
		{"false", true, ""},
		{"0", true, ""},
		{"False", true, ""},
		{" false ", true, ""},
		{"yes", true, ""},
		{"", true, ""},
	}
	for _, c := range cases {
		got, err := validateLocalAuthEnabled(c.input)
		if c.wantErr {
			if err == nil {
				t.Errorf("input %q: expected error, got canonical %q", c.input, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("input %q: unexpected error: %v", c.input, err)
			continue
		}
		if got != c.canonical {
			t.Errorf("input %q: canonical = %q, want %q", c.input, got, c.canonical)
		}
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
