package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/event"
	"github.com/sydlexius/stillwater/internal/provider"
)

func TestGetPreferences_ReturnsDefaults(t *testing.T) {
	t.Parallel()
	r, _, userID := testRouterWithAuth(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/preferences", nil)
	req = withUserCtx(req, userID)
	w := httptest.NewRecorder()

	r.handleGetPreferences(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var prefs map[string]string
	if err := json.NewDecoder(w.Body).Decode(&prefs); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	// Verify all default keys are present with correct values.
	for key, def := range preferenceDefaults {
		got, ok := prefs[key]
		if !ok {
			t.Errorf("missing default key %q", key)
			continue
		}
		if got != def.defaultValue {
			t.Errorf("key %q: expected default %q, got %q", key, def.defaultValue, got)
		}
	}

	// Verify page_size is present with its default value.
	if got, ok := prefs[PrefPageSize]; !ok {
		t.Error("missing default key \"page_size\"")
	} else if got != "50" {
		t.Errorf("key %q: expected default %q, got %q", PrefPageSize, "50", got)
	}

	// Verify bg_opacity is present with its default value.
	if got, ok := prefs[PrefBgOpacity]; !ok {
		t.Error("missing default key \"bg_opacity\"")
	} else if got != "85" {
		t.Errorf("key %q: expected default %q, got %q", PrefBgOpacity, "85", got)
	}

	// Verify the romanization fallback preference is present with its default value.
	if got, ok := prefs[PrefMetadataNameRomanization]; !ok {
		t.Error("missing default key \"metadata_name_romanization_fallback\"")
	} else if got != "true" {
		t.Errorf("key %q: expected default %q, got %q", PrefMetadataNameRomanization, "true", got)
	}

	// Verify the wire contract returns every default key plus the three
	// non-default keys (page_size, bg_opacity, metadata_languages). Derived
	// from preferenceDefaults so adding a new default key does not break this.
	expected := len(preferenceDefaults) + 3
	if len(prefs) != expected {
		t.Errorf("expected %d keys, got %d", expected, len(prefs))
	}
}

func TestUpdatePreference_ThenGet(t *testing.T) {
	t.Parallel()
	r, _, userID := testRouterWithAuth(t)

	// PUT a preference.
	body := `{"value":"light"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/preferences/theme", strings.NewReader(body))
	req.SetPathValue("key", "theme")
	req = withUserCtx(req, userID)
	w := httptest.NewRecorder()

	r.handleUpdatePreference(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("PUT expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// GET and verify the updated value.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/preferences", nil)
	req = withUserCtx(req, userID)
	w = httptest.NewRecorder()

	r.handleGetPreferences(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var prefs map[string]string
	if err := json.NewDecoder(w.Body).Decode(&prefs); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	if prefs["theme"] != "light" {
		t.Errorf("expected theme=light, got %q", prefs["theme"])
	}

	// Other defaults should still be present.
	if prefs["sidebar_state"] != "full" {
		t.Errorf("expected sidebar_state=full, got %q", prefs["sidebar_state"])
	}
}

func TestValidateSectionList(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		in     string
		want   string
		wantOK bool
	}{
		{"valid array", `["bio","artwork"]`, `["bio","artwork"]`, true},
		{"empty array", `[]`, `[]`, true},
		{"json null normalizes to empty", `null`, `[]`, true},
		{"not an array", `"bio"`, "", false},
		{"non-string element", `["bio",1]`, "", false},
		{"empty-string element", `["bio",""]`, "", false},
		{"malformed json", `[`, "", false},
		{"too many entries", `[` + strings.TrimSuffix(strings.Repeat(`"x",`, 51), ",") + `]`, "", false},
		{"entry too long", `["` + strings.Repeat("a", 65) + `"]`, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := validateSectionList(tt.in)
			if ok != tt.wantOK || (ok && got != tt.want) {
				t.Errorf("validateSectionList(%q) = (%q, %v), want (%q, %v)", tt.in, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestValidateScalarPref(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		key    string
		val    string
		want   string
		wantOK bool
	}{
		{"known valid", PrefTheme, "dark", "dark", true},
		{"known invalid", PrefTheme, "neon", "", false},
		{"suppress true", "suppress_confirm_delete", "true", "true", true},
		{"suppress bad", "suppress_confirm_delete", "maybe", "", false},
		{"page_size in range normalized", PrefPageSize, "010", "10", true},
		{"page_size out of range", PrefPageSize, "5", "", false},
		{"page_size non-int", PrefPageSize, "lots", "", false},
		{"bg_opacity in range", PrefBgOpacity, "65", "65", true},
		{"bg_opacity out of range", PrefBgOpacity, "5", "", false},
		{"unknown key", "totally_unknown", "x", "", false},
		{"metadata_languages not patchable here", PrefMetadataLanguages, `["en"]`, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := validateScalarPref(tt.key, tt.val)
			if ok != tt.wantOK || (ok && got != tt.want) {
				t.Errorf("validateScalarPref(%q,%q) = (%q,%v), want (%q,%v)", tt.key, tt.val, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

// TestPatchPreferences_PublishesSettingsChanged verifies a successful merge
// publishes a settings.changed event (so other open tabs can refetch/toast),
// carrying the user that made the change, and that a rejected PATCH publishes
// nothing.
func TestPatchPreferences_PublishesSettingsChanged(t *testing.T) {
	t.Parallel()
	r, _, userID := testRouterWithAuth(t)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	bus := event.NewBus(logger, 16)
	r.eventBus = bus

	got := make(chan event.Event, 1)
	bus.Subscribe(event.SettingsChanged, func(e event.Event) { got <- e })
	go bus.Start()
	defer bus.Stop()

	patch := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPatch, "/api/v1/preferences", strings.NewReader(body))
		req = withUserCtx(req, userID)
		w := httptest.NewRecorder()
		r.handlePatchPreferences(w, req)
		return w
	}

	if w := patch(`{"theme":"dark"}`); w.Code != http.StatusOK {
		t.Fatalf("PATCH expected 200, got %d: %s", w.Code, w.Body.String())
	}
	select {
	case e := <-got:
		sectionID, ok := e.Data["sectionId"].(string)
		if !ok || sectionID != "preferences" {
			t.Errorf("sectionId = %v (type %T), want %q", e.Data["sectionId"], e.Data["sectionId"], "preferences")
		}
		// settings.changed is broadcast to every client, so it must not leak
		// the actor's user id to other users.
		if _, leaked := e.Data["updatedBy"]; leaked {
			t.Errorf("settings.changed must not carry updatedBy (cross-user broadcast leak), got %v", e.Data["updatedBy"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("settings.changed not published after a successful PATCH")
	}

	// A rejected PATCH (unknown key) must not publish settings.changed.
	if w := patch(`{"totally_unknown_key":"x"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown-key PATCH expected 400, got %d", w.Code)
	}
	select {
	case e := <-got:
		t.Errorf("unexpected settings.changed published after a rejected PATCH: %+v", e.Data)
	case <-time.After(200 * time.Millisecond):
		// expected: nothing published
	}
}

func TestPatchPreferences_ErrorPaths(t *testing.T) {
	t.Parallel()
	r, _, userID := testRouterWithAuth(t)

	// Unauthenticated: no user in context.
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/preferences", strings.NewReader(`{"theme":"dark"}`))
	w := httptest.NewRecorder()
	r.handlePatchPreferences(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated PATCH expected 401, got %d", w.Code)
	}

	// Malformed JSON body.
	req = httptest.NewRequest(http.MethodPatch, "/api/v1/preferences", strings.NewReader(`{not json`))
	req = withUserCtx(req, userID)
	w = httptest.NewRecorder()
	r.handlePatchPreferences(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("malformed-JSON PATCH expected 400, got %d", w.Code)
	}
}

func TestPreferenceLayoutKey_GetOneAndPutOne(t *testing.T) {
	t.Parallel()
	r, _, userID := testRouterWithAuth(t)

	getOne := func(key string) (int, string) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/preferences/"+key, nil)
		req.SetPathValue("key", key)
		req = withUserCtx(req, userID)
		w := httptest.NewRecorder()
		r.handleGetPreference(w, req)
		var resp map[string]string
		_ = json.NewDecoder(w.Body).Decode(&resp)
		return w.Code, resp["value"]
	}
	putOne := func(key, value string) int {
		req := httptest.NewRequest(http.MethodPut, "/api/v1/preferences/"+key, strings.NewReader(`{"value":`+value+`}`))
		req.SetPathValue("key", key)
		req = withUserCtx(req, userID)
		w := httptest.NewRecorder()
		r.handleUpdatePreference(w, req)
		return w.Code
	}

	// No stored row -> default empty array.
	if code, val := getOne(PrefArtistDetailSectionOrder); code != http.StatusOK || val != "[]" {
		t.Errorf("GET-one no-row = (%d,%q), want (200,\"[]\")", code, val)
	}

	// PUT a valid array (value is a JSON-array string), then GET reflects canonical form.
	if code := putOne(PrefArtistDetailSectionOrder, `"[\"bio\",\"artwork\"]"`); code != http.StatusOK {
		t.Fatalf("PUT valid layout array expected 200, got %d", code)
	}
	if code, val := getOne(PrefArtistDetailSectionOrder); code != http.StatusOK || val != `["bio","artwork"]` {
		t.Errorf("GET-one stored = (%d,%q), want (200,[\"bio\",\"artwork\"])", code, val)
	}

	// PUT an invalid (non-array) value -> 400.
	if code := putOne(PrefArtistDetailHiddenSections, `"notanarray"`); code != http.StatusBadRequest {
		t.Errorf("PUT invalid layout value expected 400, got %d", code)
	}

	// A malformed stored row canonicalizes to [] on read.
	if _, err := r.db.ExecContext(context.Background(),
		`INSERT INTO user_preferences (user_id, key, value, updated_at) VALUES (?, ?, ?, datetime('now'))
		 ON CONFLICT(user_id, key) DO UPDATE SET value = excluded.value`,
		userID, PrefArtistDetailHiddenSections, "garbage{"); err != nil {
		t.Fatalf("seeding malformed row: %v", err)
	}
	if code, val := getOne(PrefArtistDetailHiddenSections); code != http.StatusOK || val != "[]" {
		t.Errorf("GET-one malformed = (%d,%q), want (200,\"[]\")", code, val)
	}
}

func TestPatchPreferences_MergesAndValidates(t *testing.T) {
	t.Parallel()
	r, _, userID := testRouterWithAuth(t)

	patch := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPatch, "/api/v1/preferences", strings.NewReader(body))
		req = withUserCtx(req, userID)
		w := httptest.NewRecorder()
		r.handlePatchPreferences(w, req)
		return w
	}
	getPrefs := func() map[string]string {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/preferences", nil)
		req = withUserCtx(req, userID)
		w := httptest.NewRecorder()
		r.handleGetPreferences(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("GET preferences expected 200, got %d: %s", w.Code, w.Body.String())
		}
		var prefs map[string]string
		if err := json.NewDecoder(w.Body).Decode(&prefs); err != nil {
			t.Fatalf("decoding preferences: %v", err)
		}
		return prefs
	}

	// 1. PATCH a scalar pref and an array-valued pref together.
	if w := patch(`{"theme":"light","artist_detail_section_order":["bio","artwork"]}`); w.Code != http.StatusOK {
		t.Fatalf("PATCH expected 200, got %d: %s", w.Code, w.Body.String())
	}
	prefs := getPrefs()
	if prefs["theme"] != "light" {
		t.Errorf("theme = %q, want light", prefs["theme"])
	}
	if prefs["artist_detail_section_order"] != `["bio","artwork"]` {
		t.Errorf("artist_detail_section_order = %q, want [\"bio\",\"artwork\"]", prefs["artist_detail_section_order"])
	}
	// Merge must not clobber untouched defaults.
	if prefs["sidebar_state"] != "full" {
		t.Errorf("sidebar_state = %q, want full (untouched default)", prefs["sidebar_state"])
	}

	// 2. A second PATCH updates only theme; the array key must persist (merge).
	if w := patch(`{"theme":"dark"}`); w.Code != http.StatusOK {
		t.Fatalf("second PATCH expected 200, got %d: %s", w.Code, w.Body.String())
	}
	prefs = getPrefs()
	if prefs["theme"] != "dark" {
		t.Errorf("theme = %q, want dark", prefs["theme"])
	}
	if prefs["artist_detail_section_order"] != `["bio","artwork"]` {
		t.Errorf("array key not preserved across merge: got %q", prefs["artist_detail_section_order"])
	}

	// 3. An unknown key rejects the whole request (atomic; nothing written).
	if w := patch(`{"bogus_key":"x"}`); w.Code != http.StatusBadRequest {
		t.Errorf("unknown-key PATCH expected 400, got %d: %s", w.Code, w.Body.String())
	}

	// 3b. A mixed valid+invalid request rejects atomically: the valid key must
	// NOT be written when another key in the same request is invalid. theme is
	// "dark" from step 2; the rejected PATCH must leave it unchanged.
	if w := patch(`{"theme":"light","bogus_key":"x"}`); w.Code != http.StatusBadRequest {
		t.Errorf("mixed valid+invalid PATCH expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if prefs := getPrefs(); prefs["theme"] != "dark" {
		t.Errorf("theme = %q after rejected mixed PATCH, want dark (no partial write)", prefs["theme"])
	}

	// 4. A non-array value for an array key is rejected.
	if w := patch(`{"artist_detail_hidden_sections":"notanarray"}`); w.Code != http.StatusBadRequest {
		t.Errorf("invalid array value expected 400, got %d: %s", w.Code, w.Body.String())
	}

	// 5. Empty body is a no-op success (nothing to merge).
	if w := patch(`{}`); w.Code != http.StatusOK {
		t.Errorf("empty PATCH expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUpdatePreference_RejectsInvalidKey(t *testing.T) {
	t.Parallel()
	r, _, userID := testRouterWithAuth(t)

	body := `{"value":"anything"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/preferences/nonexistent_key", strings.NewReader(body))
	req.SetPathValue("key", "nonexistent_key")
	req = withUserCtx(req, userID)
	w := httptest.NewRecorder()

	r.handleUpdatePreference(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp["error"] != "unknown preference key" {
		t.Errorf("unexpected error message: %q", resp["error"])
	}
}

func TestUpdatePreference_UpsertOverwrites(t *testing.T) {
	t.Parallel()
	r, _, userID := testRouterWithAuth(t)

	// First PUT: set theme to light.
	body := `{"value":"light"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/preferences/theme", strings.NewReader(body))
	req.SetPathValue("key", "theme")
	req = withUserCtx(req, userID)
	w := httptest.NewRecorder()
	r.handleUpdatePreference(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("first PUT expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Second PUT: overwrite to system.
	body = `{"value":"system"}`
	req = httptest.NewRequest(http.MethodPut, "/api/v1/preferences/theme", strings.NewReader(body))
	req.SetPathValue("key", "theme")
	req = withUserCtx(req, userID)
	w = httptest.NewRecorder()
	r.handleUpdatePreference(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("second PUT expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// GET and verify the latest value wins.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/preferences", nil)
	req = withUserCtx(req, userID)
	w = httptest.NewRecorder()
	r.handleGetPreferences(w, req)

	var prefs map[string]string
	if err := json.NewDecoder(w.Body).Decode(&prefs); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if prefs["theme"] != "system" {
		t.Errorf("expected theme=system after upsert, got %q", prefs["theme"])
	}
}

func TestGetPreferences_Unauthenticated(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithAuth(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/preferences", nil)
	// No withUserCtx -- unauthenticated.
	w := httptest.NewRecorder()
	r.handleGetPreferences(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUpdatePreference_Unauthenticated(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithAuth(t)

	body := `{"value":"light"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/preferences/theme", strings.NewReader(body))
	req.SetPathValue("key", "theme")
	// No withUserCtx -- unauthenticated.
	w := httptest.NewRecorder()
	r.handleUpdatePreference(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUpdatePreference_RejectsInvalidValue(t *testing.T) {
	t.Parallel()
	r, _, userID := testRouterWithAuth(t)

	body := `{"value":"neon_pink"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/preferences/theme", strings.NewReader(body))
	req.SetPathValue("key", "theme")
	req = withUserCtx(req, userID)
	w := httptest.NewRecorder()

	r.handleUpdatePreference(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp["error"] != "invalid value for preference theme" {
		t.Errorf("unexpected error message: %q", resp["error"])
	}
}

// -- handleGetPreference (single key) tests --

func TestGetPreference_ReturnsDefault(t *testing.T) {
	t.Parallel()
	r, _, userID := testRouterWithAuth(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/preferences/theme", nil)
	req.SetPathValue("key", "theme")
	req = withUserCtx(req, userID)
	w := httptest.NewRecorder()

	r.handleGetPreference(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp["key"] != "theme" {
		t.Errorf("expected key=theme, got %q", resp["key"])
	}
	if resp["value"] != "dark" {
		t.Errorf("expected value=dark (default), got %q", resp["value"])
	}
}

func TestGetPreference_ReturnsStoredValue(t *testing.T) {
	t.Parallel()
	r, _, userID := testRouterWithAuth(t)

	// Store a non-default value.
	body := `{"value":"light"}`
	putReq := httptest.NewRequest(http.MethodPut, "/api/v1/preferences/theme", strings.NewReader(body))
	putReq.SetPathValue("key", "theme")
	putReq = withUserCtx(putReq, userID)
	putW := httptest.NewRecorder()
	r.handleUpdatePreference(putW, putReq)
	if putW.Code != http.StatusOK {
		t.Fatalf("PUT expected 200, got %d: %s", putW.Code, putW.Body.String())
	}

	// GET single key and verify it reflects the stored value.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/preferences/theme", nil)
	req.SetPathValue("key", "theme")
	req = withUserCtx(req, userID)
	w := httptest.NewRecorder()

	r.handleGetPreference(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp["value"] != "light" {
		t.Errorf("expected value=light after update, got %q", resp["value"])
	}
}

func TestGetPreference_UnknownKey(t *testing.T) {
	t.Parallel()
	r, _, userID := testRouterWithAuth(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/preferences/nonexistent", nil)
	req.SetPathValue("key", "nonexistent")
	req = withUserCtx(req, userID)
	w := httptest.NewRecorder()

	r.handleGetPreference(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetPreference_SuppressConfirmDefaultsFalse(t *testing.T) {
	t.Parallel()
	r, _, userID := testRouterWithAuth(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/preferences/suppress_confirm_delete_artist", nil)
	req.SetPathValue("key", "suppress_confirm_delete_artist")
	req = withUserCtx(req, userID)
	w := httptest.NewRecorder()

	r.handleGetPreference(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp["value"] != "false" {
		t.Errorf("expected value=false for unset suppress key, got %q", resp["value"])
	}
}

func TestUpdatePreference_SuppressConfirmAcceptsTrueAndFalse(t *testing.T) {
	t.Parallel()
	r, _, userID := testRouterWithAuth(t)

	// Suppress the action.
	body := `{"value":"true"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/preferences/suppress_confirm_delete_artist", strings.NewReader(body))
	req.SetPathValue("key", "suppress_confirm_delete_artist")
	req = withUserCtx(req, userID)
	w := httptest.NewRecorder()
	r.handleUpdatePreference(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("PUT true expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Verify GET returns true.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/preferences/suppress_confirm_delete_artist", nil)
	req.SetPathValue("key", "suppress_confirm_delete_artist")
	req = withUserCtx(req, userID)
	w = httptest.NewRecorder()
	r.handleGetPreference(w, req)

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp["value"] != "true" {
		t.Errorf("expected value=true after suppress, got %q", resp["value"])
	}

	// Unsuppress.
	body = `{"value":"false"}`
	req = httptest.NewRequest(http.MethodPut, "/api/v1/preferences/suppress_confirm_delete_artist", strings.NewReader(body))
	req.SetPathValue("key", "suppress_confirm_delete_artist")
	req = withUserCtx(req, userID)
	w = httptest.NewRecorder()
	r.handleUpdatePreference(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("PUT false expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Verify the stored value is now "false".
	req = httptest.NewRequest(http.MethodGet, "/api/v1/preferences/suppress_confirm_delete_artist", nil)
	req.SetPathValue("key", "suppress_confirm_delete_artist")
	req = withUserCtx(req, userID)
	w = httptest.NewRecorder()
	r.handleGetPreference(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET after PUT false: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var getResp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&getResp); err != nil {
		t.Fatalf("decoding GET response: %v", err)
	}
	if getResp["value"] != "false" {
		t.Errorf("expected value=false after unsuppress, got %q", getResp["value"])
	}
}

func TestUpdatePreference_SuppressConfirmRejectsInvalidValue(t *testing.T) {
	t.Parallel()
	r, _, userID := testRouterWithAuth(t)

	body := `{"value":"yes"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/preferences/suppress_confirm_delete", strings.NewReader(body))
	req.SetPathValue("key", "suppress_confirm_delete")
	req = withUserCtx(req, userID)
	w := httptest.NewRecorder()
	r.handleUpdatePreference(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetPreference_Unauthenticated(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithAuth(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/preferences/theme", nil)
	req.SetPathValue("key", "theme")
	// No withUserCtx -- unauthenticated.
	w := httptest.NewRecorder()
	r.handleGetPreference(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", w.Code, w.Body.String())
	}
}

// -- page_size preference tests --

func TestPageSizePref_DefaultReturned(t *testing.T) {
	t.Parallel()
	r, _, userID := testRouterWithAuth(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/preferences/page_size", nil)
	req.SetPathValue("key", "page_size")
	req = withUserCtx(req, userID)
	w := httptest.NewRecorder()

	r.handleGetPreference(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	want := fmt.Sprintf("%d", PageSizeDefault)
	if resp["value"] != want {
		t.Errorf("expected value=%s (default), got %q", want, resp["value"])
	}
}

func TestPageSizePref_StoreAndRetrieve(t *testing.T) {
	t.Parallel()
	r, _, userID := testRouterWithAuth(t)

	// PUT a valid page_size value.
	body := `{"value":"100"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/preferences/page_size", strings.NewReader(body))
	req.SetPathValue("key", "page_size")
	req = withUserCtx(req, userID)
	w := httptest.NewRecorder()
	r.handleUpdatePreference(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("PUT expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// GET and verify the stored value.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/preferences/page_size", nil)
	req.SetPathValue("key", "page_size")
	req = withUserCtx(req, userID)
	w = httptest.NewRecorder()
	r.handleGetPreference(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp["value"] != "100" {
		t.Errorf("expected value=100, got %q", resp["value"])
	}
}

func TestPageSizePref_RejectsOutOfRange(t *testing.T) {
	t.Parallel()
	r, _, userID := testRouterWithAuth(t)

	cases := []struct {
		value string
	}{
		{"9"},
		{"501"},
		{"0"},
		{"-1"},
		{"not_a_number"},
		{""},
	}

	for _, tc := range cases {
		t.Run("value_"+tc.value, func(t *testing.T) {
			body := fmt.Sprintf(`{"value":%q}`, tc.value)
			req := httptest.NewRequest(http.MethodPut, "/api/v1/preferences/page_size", strings.NewReader(body))
			req.SetPathValue("key", "page_size")
			req = withUserCtx(req, userID)
			w := httptest.NewRecorder()
			r.handleUpdatePreference(w, req)

			if w.Code != http.StatusBadRequest {
				t.Errorf("expected 400 for value %q, got %d: %s", tc.value, w.Code, w.Body.String())
			}
		})
	}
}

func TestPageSizePref_AcceptsBoundaryValues(t *testing.T) {
	t.Parallel()
	r, _, userID := testRouterWithAuth(t)

	for _, v := range []string{"10", "500"} {
		t.Run("value_"+v, func(t *testing.T) {
			body := fmt.Sprintf(`{"value":%q}`, v)
			req := httptest.NewRequest(http.MethodPut, "/api/v1/preferences/page_size", strings.NewReader(body))
			req.SetPathValue("key", "page_size")
			req = withUserCtx(req, userID)
			w := httptest.NewRecorder()
			r.handleUpdatePreference(w, req)

			if w.Code != http.StatusOK {
				t.Errorf("expected 200 for boundary value %q, got %d: %s", v, w.Code, w.Body.String())
			}
		})
	}
}

// TestPageSizePref_UsedByArtistList verifies that the page_size preference is
// respected by the artist list API endpoint when no query param is provided.
func TestPageSizePref_UsedByArtistList(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)

	// Establish a known user ID that we will use for the preference.
	const testUserID = "test-user-pagesize"

	// Create more artists than the stored page size so the cap is observable.
	for i := 0; i < 15; i++ {
		a := &artist.Artist{Name: fmt.Sprintf("Test Artist %02d", i)}
		if err := artistSvc.Create(context.Background(), a); err != nil {
			t.Fatalf("creating artist: %v", err)
		}
	}

	// Store page_size=10 directly in the DB for the test user.
	_, err := r.db.ExecContext(context.Background(),
		`INSERT INTO user_preferences (user_id, key, value, updated_at)
		 VALUES (?, 'page_size', '10', datetime('now'))
		 ON CONFLICT(user_id, key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		testUserID)
	if err != nil {
		t.Fatalf("storing page_size pref: %v", err)
	}

	// Call handleListArtists without explicit page_size param.
	ctx := middleware.WithTestUserID(context.Background(), testUserID)
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/artists", nil)
	w := httptest.NewRecorder()
	r.handleListArtists(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	// The response should reflect page_size=10 from the preference.
	pageSize, ok := resp["page_size"].(float64)
	if !ok {
		t.Fatalf("page_size not present or not a number in response")
	}
	if int(pageSize) != 10 {
		t.Errorf("expected page_size=10 from preference, got %d", int(pageSize))
	}

	// The returned artists slice should be capped at 10.
	artists, ok := resp["artists"].([]any)
	if !ok {
		t.Fatalf("artists not present or not an array in response")
	}
	if len(artists) > 10 {
		t.Errorf("expected at most 10 artists, got %d", len(artists))
	}
}

// TestPageSizePref_QueryParamOverridesPref verifies that an explicit page_size
// query parameter takes precedence over the stored user preference.
func TestPageSizePref_QueryParamOverridesPref(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	const testUserID = "test-user-qparam"

	// Store page_size=10 directly in the DB for the test user.
	_, err := r.db.ExecContext(context.Background(),
		`INSERT INTO user_preferences (user_id, key, value, updated_at)
		 VALUES (?, 'page_size', '10', datetime('now'))
		 ON CONFLICT(user_id, key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		testUserID)
	if err != nil {
		t.Fatalf("storing page_size pref: %v", err)
	}

	// Call handleListArtists with page_size=25 in the query param.
	ctx := middleware.WithTestUserID(context.Background(), testUserID)
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/artists?page_size=25", nil)
	w := httptest.NewRecorder()
	r.handleListArtists(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	// The query param value (25) should override the stored preference (10).
	pageSize, ok := resp["page_size"].(float64)
	if !ok {
		t.Fatalf("page_size not present or not a number in response")
	}
	if int(pageSize) != 25 {
		t.Errorf("expected page_size=25 from query override, got %d", int(pageSize))
	}
}

func TestIsSuppressConfirmKey(t *testing.T) {
	t.Parallel()
	valid := []string{
		"suppress_confirm_delete",
		"suppress_confirm_delete_artist",
		"suppress_confirm_bulk_fix_all",
		"suppress_confirm_a",
		"suppress_confirm_x1",
	}
	invalid := []string{
		"suppress_confirm_",
		"suppress_confirm",
		"theme",
		"suppress_confirm_DELETE",
		"suppress_confirm_has-dash",
		"suppress_confirm_has space",
		"",
	}

	for _, k := range valid {
		if !isSuppressConfirmKey(k) {
			t.Errorf("expected %q to be a valid suppress_confirm key", k)
		}
	}
	for _, k := range invalid {
		if isSuppressConfirmKey(k) {
			t.Errorf("expected %q to be an invalid suppress_confirm key", k)
		}
	}
}

// -- bg_opacity preference tests --

func TestBgOpacityPref_DefaultReturned(t *testing.T) {
	t.Parallel()
	r, _, userID := testRouterWithAuth(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/preferences/bg_opacity", nil)
	req.SetPathValue("key", "bg_opacity")
	req = withUserCtx(req, userID)
	w := httptest.NewRecorder()

	r.handleGetPreference(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	want := fmt.Sprintf("%d", BgOpacityDefault)
	if resp["value"] != want {
		t.Errorf("expected value=%s (default), got %q", want, resp["value"])
	}
}

func TestBgOpacityPref_StoreAndRetrieve(t *testing.T) {
	t.Parallel()
	r, _, userID := testRouterWithAuth(t)

	body := `{"value":"80"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/preferences/bg_opacity", strings.NewReader(body))
	req.SetPathValue("key", "bg_opacity")
	req = withUserCtx(req, userID)
	w := httptest.NewRecorder()
	r.handleUpdatePreference(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("PUT expected 200, got %d: %s", w.Code, w.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/preferences/bg_opacity", nil)
	req.SetPathValue("key", "bg_opacity")
	req = withUserCtx(req, userID)
	w = httptest.NewRecorder()
	r.handleGetPreference(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp["value"] != "80" {
		t.Errorf("expected value=80, got %q", resp["value"])
	}
}

func TestBgOpacityPref_RejectsOutOfRange(t *testing.T) {
	t.Parallel()
	r, _, userID := testRouterWithAuth(t)

	cases := []struct {
		value string
	}{
		{"19"},
		{"101"},
		{"0"},
		{"-1"},
		{"not_a_number"},
		{""},
	}

	for _, tc := range cases {
		t.Run("value_"+tc.value, func(t *testing.T) {
			body := fmt.Sprintf(`{"value":%q}`, tc.value)
			req := httptest.NewRequest(http.MethodPut, "/api/v1/preferences/bg_opacity", strings.NewReader(body))
			req.SetPathValue("key", "bg_opacity")
			req = withUserCtx(req, userID)
			w := httptest.NewRecorder()
			r.handleUpdatePreference(w, req)

			if w.Code != http.StatusBadRequest {
				t.Errorf("expected 400 for value %q, got %d: %s", tc.value, w.Code, w.Body.String())
			}
		})
	}
}

func TestBgOpacityPref_AcceptsBoundaryValues(t *testing.T) {
	t.Parallel()
	r, _, userID := testRouterWithAuth(t)

	for _, v := range []string{fmt.Sprintf("%d", BgOpacityMin), fmt.Sprintf("%d", BgOpacityMax)} {
		t.Run("value_"+v, func(t *testing.T) {
			body := fmt.Sprintf(`{"value":%q}`, v)
			req := httptest.NewRequest(http.MethodPut, "/api/v1/preferences/bg_opacity", strings.NewReader(body))
			req.SetPathValue("key", "bg_opacity")
			req = withUserCtx(req, userID)
			w := httptest.NewRecorder()
			r.handleUpdatePreference(w, req)

			if w.Code != http.StatusOK {
				t.Errorf("expected 200 for boundary value %q, got %d: %s", v, w.Code, w.Body.String())
			}
		})
	}
}

// -- auto_fetch_images preference tests --

func TestAutoFetchImagesPref_DefaultReturned(t *testing.T) {
	t.Parallel()
	r, _, userID := testRouterWithAuth(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/preferences/auto_fetch_images", nil)
	req.SetPathValue("key", "auto_fetch_images")
	req = withUserCtx(req, userID)
	w := httptest.NewRecorder()

	r.handleGetPreference(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	// Default is "false" when no app-level setting is configured.
	if resp["value"] != "false" {
		t.Errorf("expected value=false (default), got %q", resp["value"])
	}
}

func TestAutoFetchImagesPref_StoreAndRetrieve(t *testing.T) {
	t.Parallel()
	r, _, userID := testRouterWithAuth(t)

	body := `{"value":"true"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/preferences/auto_fetch_images", strings.NewReader(body))
	req.SetPathValue("key", "auto_fetch_images")
	req = withUserCtx(req, userID)
	w := httptest.NewRecorder()
	r.handleUpdatePreference(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("PUT expected 200, got %d: %s", w.Code, w.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/preferences/auto_fetch_images", nil)
	req.SetPathValue("key", "auto_fetch_images")
	req = withUserCtx(req, userID)
	w = httptest.NewRecorder()
	r.handleGetPreference(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp["value"] != "true" {
		t.Errorf("expected value=true, got %q", resp["value"])
	}
}

func TestAutoFetchImagesPref_RejectsInvalidValue(t *testing.T) {
	t.Parallel()
	r, _, userID := testRouterWithAuth(t)

	for _, bad := range []string{"yes", "1", "on", ""} {
		t.Run("value_"+bad, func(t *testing.T) {
			body := fmt.Sprintf(`{"value":%q}`, bad)
			req := httptest.NewRequest(http.MethodPut, "/api/v1/preferences/auto_fetch_images", strings.NewReader(body))
			req.SetPathValue("key", "auto_fetch_images")
			req = withUserCtx(req, userID)
			w := httptest.NewRecorder()
			r.handleUpdatePreference(w, req)

			if w.Code != http.StatusBadRequest {
				t.Errorf("expected 400 for value %q, got %d: %s", bad, w.Code, w.Body.String())
			}
		})
	}
}

// -- metadata_languages preference tests --

func TestMetadataLanguagesPref_DefaultReturned(t *testing.T) {
	t.Parallel()
	r, _, userID := testRouterWithAuth(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/preferences/metadata_languages", nil)
	req.SetPathValue("key", "metadata_languages")
	req = withUserCtx(req, userID)
	w := httptest.NewRecorder()

	r.handleGetPreference(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp["value"] != `["en"]` {
		t.Errorf("expected default [\"en\"], got %q", resp["value"])
	}
}

func TestMetadataLanguagesPref_StoreAndRetrieve(t *testing.T) {
	t.Parallel()
	r, _, userID := testRouterWithAuth(t)

	body := `{"value":"[\"en-GB\",\"en\",\"ja\"]"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/preferences/metadata_languages", strings.NewReader(body))
	req.SetPathValue("key", "metadata_languages")
	req = withUserCtx(req, userID)
	w := httptest.NewRecorder()
	r.handleUpdatePreference(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("PUT expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// GET and verify the stored value.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/preferences/metadata_languages", nil)
	req.SetPathValue("key", "metadata_languages")
	req = withUserCtx(req, userID)
	w = httptest.NewRecorder()
	r.handleGetPreference(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp["value"] != `["en-GB","en","ja"]` {
		t.Errorf("expected stored value, got %q", resp["value"])
	}
}

func TestMetadataLanguagesPref_CanonicalizesCase(t *testing.T) {
	t.Parallel()
	r, _, userID := testRouterWithAuth(t)

	// PUT with mixed-case tags.
	body := `{"value":"[\"EN-gb\",\"JA\"]"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/preferences/metadata_languages", strings.NewReader(body))
	req.SetPathValue("key", "metadata_languages")
	req = withUserCtx(req, userID)
	w := httptest.NewRecorder()
	r.handleUpdatePreference(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("PUT expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// GET and verify casing was canonicalized.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/preferences/metadata_languages", nil)
	req.SetPathValue("key", "metadata_languages")
	req = withUserCtx(req, userID)
	w = httptest.NewRecorder()
	r.handleGetPreference(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp["value"] != `["en-GB","ja"]` {
		t.Errorf("expected canonicalized [\"en-GB\",\"ja\"], got %q", resp["value"])
	}
}

func TestMetadataLanguagesPref_RejectsInvalid(t *testing.T) {
	t.Parallel()
	r, _, userID := testRouterWithAuth(t)

	cases := []struct {
		name  string
		value string
	}{
		{"not json", `not json`},
		// Empty array is accepted as an explicit "unset to default"
		// signal (see TestMetadataLanguagesPref_EmptyArrayResetsToDefault)
		// and is therefore no longer in this "rejects" list. See #1138.
		{"too many entries", `["en","fr","de","es","it","pt","ja","ko","zh","ru","nl","sv","no","da","fi","pl","tr","ar","he","cs","hu"]`},
		{"empty string tag", `["en",""]`},
		{"invalid chars", `["en@GB"]`},
		{"duplicate tags", `["en","en"]`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"value":%q}`, tc.value)
			req := httptest.NewRequest(http.MethodPut, "/api/v1/preferences/metadata_languages", strings.NewReader(body))
			req.SetPathValue("key", "metadata_languages")
			req = withUserCtx(req, userID)
			w := httptest.NewRecorder()
			r.handleUpdatePreference(w, req)

			if w.Code != http.StatusBadRequest {
				t.Errorf("expected 400 for %q, got %d: %s", tc.value, w.Code, w.Body.String())
			}
		})
	}
}

// TestMetadataLanguagesPref_EmptyArray_DeleteFailure covers the handler's
// error branch for the empty-array / reset-to-default path. Simulating a
// DB failure via db.Close() is enough to exercise the write path: the
// langpref.Delete call returns a wrapped error and the handler must
// respond 500 rather than silently 200ing.
func TestMetadataLanguagesPref_EmptyArray_DeleteFailure(t *testing.T) {
	t.Parallel()
	r, _, userID := testRouterWithAuth(t)
	if err := r.db.Close(); err != nil {
		t.Fatalf("closing db for error injection: %v", err)
	}

	body := `{"value":"[]"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/preferences/metadata_languages", strings.NewReader(body))
	req.SetPathValue("key", "metadata_languages")
	req = withUserCtx(req, userID)
	w := httptest.NewRecorder()
	r.handleUpdatePreference(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("empty-array PUT with broken db: expected 500, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding error response: %v", err)
	}
	if resp["error"] != "internal error" {
		t.Errorf("expected sanitized error %q, got %q", "internal error", resp["error"])
	}
}

func TestMetadataLanguagesPref_EmptyArrayResetsToDefault(t *testing.T) {
	t.Parallel()
	r, _, userID := testRouterWithAuth(t)

	// Seed a non-default preference first so we can observe the delete.
	body := `{"value":"[\"ja\",\"en\"]"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/preferences/metadata_languages", strings.NewReader(body))
	req.SetPathValue("key", "metadata_languages")
	req = withUserCtx(req, userID)
	w := httptest.NewRecorder()
	r.handleUpdatePreference(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("seed PUT expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Clear via empty array. Per #1138, the handler deletes the row and
	// echoes "[]" so the UI can keep showing its "using default" state.
	body = `{"value":"[]"}`
	req = httptest.NewRequest(http.MethodPut, "/api/v1/preferences/metadata_languages", strings.NewReader(body))
	req.SetPathValue("key", "metadata_languages")
	req = withUserCtx(req, userID)
	w = httptest.NewRecorder()
	r.handleUpdatePreference(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("clear PUT expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding clear PUT response: %v", err)
	}
	if resp["value"] != "[]" {
		t.Errorf("clear PUT response value = %q, want %q (handler echoes empty intent)", resp["value"], "[]")
	}

	// Follow-up GET must return the default: with no row, the read
	// path falls back to langpref.DefaultJSON.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/preferences/metadata_languages", nil)
	req.SetPathValue("key", "metadata_languages")
	req = withUserCtx(req, userID)
	w = httptest.NewRecorder()
	r.handleGetPreference(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding GET response: %v", err)
	}
	if resp["value"] != `["en"]` {
		t.Errorf("GET after clear = %q, want %q (default fallback)", resp["value"], `["en"]`)
	}
}

func TestMetadataLanguagesPref_AcceptsValid(t *testing.T) {
	t.Parallel()
	r, _, userID := testRouterWithAuth(t)

	cases := []struct {
		name  string
		value string
	}{
		{"single en", `["en"]`},
		{"multiple", `["en-GB","fr","ja"]`},
		{"dialect only", `["zh-Hant-TW"]`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"value":%q}`, tc.value)
			req := httptest.NewRequest(http.MethodPut, "/api/v1/preferences/metadata_languages", strings.NewReader(body))
			req.SetPathValue("key", "metadata_languages")
			req = withUserCtx(req, userID)
			w := httptest.NewRecorder()
			r.handleUpdatePreference(w, req)

			if w.Code != http.StatusOK {
				t.Errorf("expected 200 for %q, got %d: %s", tc.value, w.Code, w.Body.String())
			}
		})
	}
}

func TestMetadataLanguagesPref_IncludedInGetAll(t *testing.T) {
	t.Parallel()
	r, _, userID := testRouterWithAuth(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/preferences", nil)
	req = withUserCtx(req, userID)
	w := httptest.NewRecorder()

	r.handleGetPreferences(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var prefs map[string]string
	if err := json.NewDecoder(w.Body).Decode(&prefs); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	got, ok := prefs["metadata_languages"]
	if !ok {
		t.Fatal("metadata_languages not present in GET /preferences response")
	}
	if got != `["en"]` {
		t.Errorf("expected default [\"en\"], got %q", got)
	}
}

func TestValidateMetadataLanguages(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		valid bool
	}{
		{"valid single", `["en"]`, true},
		{"valid multi", `["en","ja","fr"]`, true},
		{"valid dialect", `["en-GB"]`, true},
		{"empty array", `[]`, false},
		{"not json", `invalid`, false},
		{"empty tag", `[""]`, false},
		{"duplicate", `["en","en"]`, false},
		{"duplicate case insensitive", `["en","EN"]`, false},
		{"invalid char", `["en@gb"]`, false},
		{"too long tag", `["aaaaaaaaa-bbbbbbbbb-ccccccccc-ddddddddd"]`, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, ok := validateMetadataLanguages(tc.input)
			if ok != tc.valid {
				t.Errorf("validateMetadataLanguages(%q) = %v, want %v", tc.input, ok, tc.valid)
			}
		})
	}
}

// NOTE: the low-level tag validator has moved to the internal/langpref
// package and is covered by TestValidate there. The api-package surface
// we care about here is validateMetadataLanguages, tested above.

// TestUserPreferencesPage_RendersWithDefaults exercises the page-render
// handler so the templates.PreferencesData literal at line 639 is covered.
// Asserts a 200 plus markers unique to the preferences page (the
// appearance tab panel and one of its preference inputs) so the test
// fails if the handler accidentally renders the login page or an
// unrelated template.
func TestUserPreferencesPage_RendersWithDefaults(t *testing.T) {
	t.Parallel()
	r, _, userID := testRouterWithAuth(t)

	req := httptest.NewRequest(http.MethodGet, "/preferences", nil)
	req = withUserCtx(req, userID)
	w := httptest.NewRecorder()

	r.handleUserPreferencesPage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, marker := range []string{
		`data-tab-panel="appearance"`,
		`id="pref-theme"`,
	} {
		if !strings.Contains(body, marker) {
			t.Errorf("expected preferences-page marker %q in rendered body", marker)
		}
	}
	if strings.Contains(body, `id="login-result"`) {
		t.Error("expected preferences page, but login-page marker id=\"login-result\" was rendered")
	}
}

// TestUserPreferencesPage_UnauthenticatedRendersLogin verifies the
// short-circuit branch that defers to the login page when no user is
// in context. Together with the authenticated case above this covers
// both arms of the entry guard.
func TestUserPreferencesPage_UnauthenticatedRendersLogin(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithAuth(t)

	req := httptest.NewRequest(http.MethodGet, "/preferences", nil)
	w := httptest.NewRecorder()

	r.handleUserPreferencesPage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (login page), got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `id="login-result"`) {
		t.Errorf("expected login-page marker id=\"login-result\" in rendered body; got %d bytes", w.Body.Len())
	}
	if strings.Contains(body, `data-tab-panel="appearance"`) {
		t.Error("expected login page, but preferences-page marker data-tab-panel=\"appearance\" was rendered")
	}
}

// TestInjectMetadataLanguages_InjectsRomanizationFallback verifies that
// injectMetadataLanguages reads the metadata_name_romanization_fallback
// preference from the database and injects it into the context via
// provider.WithNameRomanizationFallback. When no row is stored the getter
// must return the default (true). When a row with "false" is stored the
// getter must return false.
func TestInjectMetadataLanguages_InjectsRomanizationFallback(t *testing.T) {
	t.Parallel()
	r, _, userID := testRouterWithAuth(t)
	ctx := middleware.WithTestUserID(context.Background(), userID)

	// Default (no stored preference): romanization fallback must default to true.
	injected := r.injectMetadataLanguages(ctx)
	if !provider.NameRomanizationFallback(injected) {
		t.Error("default case: expected NameRomanizationFallback=true when no preference row exists")
	}

	// Store "false" and re-inject.
	body := `{"value":"false"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/preferences/"+PrefMetadataNameRomanization, strings.NewReader(body))
	req.SetPathValue("key", PrefMetadataNameRomanization)
	req = withUserCtx(req, userID)
	w := httptest.NewRecorder()
	r.handleUpdatePreference(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT preference: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Re-inject and check that the false value is picked up.
	injected = r.injectMetadataLanguages(ctx)
	if provider.NameRomanizationFallback(injected) {
		t.Error("after storing false: expected NameRomanizationFallback=false in context")
	}
}

// TestUpdatePreference_RomanizationFallbackRoundtrip verifies that the
// metadata_name_romanization_fallback preference can be written and read back
// via the API.
func TestUpdatePreference_RomanizationFallbackRoundtrip(t *testing.T) {
	t.Parallel()
	r, _, userID := testRouterWithAuth(t)

	// Default GET must return "true".
	req := httptest.NewRequest(http.MethodGet, "/api/v1/preferences/"+PrefMetadataNameRomanization, nil)
	req.SetPathValue("key", PrefMetadataNameRomanization)
	req = withUserCtx(req, userID)
	w := httptest.NewRecorder()
	r.handleGetPreference(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET preference: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding GET response: %v", err)
	}
	if resp["value"] != "true" {
		t.Errorf("default value: expected %q, got %q", "true", resp["value"])
	}

	// PUT "false".
	body := `{"value":"false"}`
	req = httptest.NewRequest(http.MethodPut, "/api/v1/preferences/"+PrefMetadataNameRomanization, strings.NewReader(body))
	req.SetPathValue("key", PrefMetadataNameRomanization)
	req = withUserCtx(req, userID)
	w = httptest.NewRecorder()
	r.handleUpdatePreference(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT preference: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// GET again -- must reflect the update.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/preferences/"+PrefMetadataNameRomanization, nil)
	req.SetPathValue("key", PrefMetadataNameRomanization)
	req = withUserCtx(req, userID)
	w = httptest.NewRecorder()
	r.handleGetPreference(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET preference after update: expected 200, got %d", w.Code)
	}
	// Decode into a fresh map: json.Decoder merges into a reused map, so a
	// missing key would silently keep its prior value and mask a regression.
	resp = map[string]string{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding GET response after update: %v", err)
	}
	if resp["value"] != "false" {
		t.Errorf("after update: expected %q, got %q", "false", resp["value"])
	}

	// PUT "true" explicitly -- an affirmative write must round-trip back, not
	// just the default-when-unset path exercised above.
	body = `{"value":"true"}`
	req = httptest.NewRequest(http.MethodPut, "/api/v1/preferences/"+PrefMetadataNameRomanization, strings.NewReader(body))
	req.SetPathValue("key", PrefMetadataNameRomanization)
	req = withUserCtx(req, userID)
	w = httptest.NewRecorder()
	r.handleUpdatePreference(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT true: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/preferences/"+PrefMetadataNameRomanization, nil)
	req.SetPathValue("key", PrefMetadataNameRomanization)
	req = withUserCtx(req, userID)
	w = httptest.NewRecorder()
	r.handleGetPreference(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET after PUT true: expected 200, got %d", w.Code)
	}
	resp = map[string]string{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding GET response after PUT true: %v", err)
	}
	if resp["value"] != "true" {
		t.Errorf("after PUT true: expected %q, got %q", "true", resp["value"])
	}
}

// TestMetadataNameRomanizationPref_RejectsInvalidValue verifies the
// metadata_name_romanization_fallback preference rejects anything outside its
// {true, false} allowed set, mirroring TestAutoFetchImagesPref_RejectsInvalidValue.
func TestMetadataNameRomanizationPref_RejectsInvalidValue(t *testing.T) {
	t.Parallel()
	r, _, userID := testRouterWithAuth(t)

	for _, bad := range []string{"yes", "1", "on", "maybe", ""} {
		t.Run("value_"+bad, func(t *testing.T) {
			body := fmt.Sprintf(`{"value":%q}`, bad)
			req := httptest.NewRequest(http.MethodPut, "/api/v1/preferences/"+PrefMetadataNameRomanization, strings.NewReader(body))
			req.SetPathValue("key", PrefMetadataNameRomanization)
			req = withUserCtx(req, userID)
			w := httptest.NewRecorder()
			r.handleUpdatePreference(w, req)

			if w.Code != http.StatusBadRequest {
				t.Errorf("expected 400 for value %q, got %d: %s", bad, w.Code, w.Body.String())
			}
		})
	}
}
