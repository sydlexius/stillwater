package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/artist"
)

func TestGetPreferences_ReturnsDefaults(t *testing.T) {
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

	// Verify the wire contract returns exactly 14 keys.
	if len(prefs) != 14 {
		t.Errorf("expected 14 keys, got %d", len(prefs))
	}
}

func TestUpdatePreference_ThenGet(t *testing.T) {
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
	if prefs["glass_intensity"] != "medium" {
		t.Errorf("expected glass_intensity=medium, got %q", prefs["glass_intensity"])
	}
}

func TestUpdatePreference_RejectsInvalidKey(t *testing.T) {
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

// -- metadata_languages preference tests --

func TestMetadataLanguagesPref_DefaultReturned(t *testing.T) {
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

func TestMetadataLanguagesPref_RejectsInvalid(t *testing.T) {
	r, _, userID := testRouterWithAuth(t)

	cases := []struct {
		name  string
		value string
	}{
		{"not json", `not json`},
		{"empty array", `[]`},
		{"too many entries", `["a","b","c","d","e","f","g","h","i","j","k","l","m","n","o","p","q","r","s","t","u"]`},
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

func TestMetadataLanguagesPref_AcceptsValid(t *testing.T) {
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

func TestIsValidLanguageTag(t *testing.T) {
	valid := []string{"en", "en-GB", "zh-Hant-TW", "ja", "fr"}
	invalid := []string{"", "en@gb", "en gb", "a-", "-en", "toooooolong123456789012345678901234567"}

	for _, s := range valid {
		if !isValidLanguageTag(s) {
			t.Errorf("expected %q to be valid", s)
		}
	}
	for _, s := range invalid {
		if isValidLanguageTag(s) {
			t.Errorf("expected %q to be invalid", s)
		}
	}
}
