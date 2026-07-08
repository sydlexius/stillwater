package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/platform"
	"github.com/sydlexius/stillwater/internal/rule"
)

// testRouterWithPlatformForUpdate returns a Router with platform service and
// a custom profile suitable for update tests.
func testRouterWithPlatformForUpdate(t *testing.T) (*Router, *platform.Service, string) {
	t.Helper()
	r, _ := testRouterWithPlatform(t)
	svc := r.platformService

	p := &platform.Profile{
		Name:       "UpdateTest",
		NFOEnabled: true,
		NFOFormat:  "kodi",
		ImageNaming: platform.ImageNaming{
			Thumb:  []string{"folder.jpg"},
			Fanart: []string{"fanart.jpg"},
			Logo:   []string{"logo.png"},
			Banner: []string{"banner.jpg"},
		},
	}
	if err := svc.Create(context.Background(), p); err != nil {
		t.Fatalf("creating test profile: %v", err)
	}
	return r, svc, p.ID
}

func TestUpdatePlatform_PartialUpdate(t *testing.T) {
	t.Parallel()
	r, svc, id := testRouterWithPlatformForUpdate(t)

	// Send only nfo_enabled = false; other fields should remain unchanged.
	body := `{"nfo_enabled": false}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/platforms/"+id, bytes.NewBufferString(body))
	req.SetPathValue("id", id)
	w := httptest.NewRecorder()
	r.handleUpdatePlatform(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	got, err := svc.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.NFOEnabled {
		t.Error("NFOEnabled should be false after partial update")
	}
	if got.Name != "UpdateTest" {
		t.Errorf("Name = %q, want UpdateTest (should be unchanged)", got.Name)
	}
	if got.NFOFormat != "kodi" {
		t.Errorf("NFOFormat = %q, want kodi (should be unchanged)", got.NFOFormat)
	}
	if got.ImageNaming.PrimaryName("thumb") != "folder.jpg" {
		t.Errorf("thumb = %q, want folder.jpg (should be unchanged)",
			got.ImageNaming.PrimaryName("thumb"))
	}
}

func TestUpdatePlatform_EmptyStringIgnored(t *testing.T) {
	t.Parallel()
	r, svc, id := testRouterWithPlatformForUpdate(t)

	// Send name as empty string; should be treated as "not provided".
	body := `{"name": ""}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/platforms/"+id, bytes.NewBufferString(body))
	req.SetPathValue("id", id)
	w := httptest.NewRecorder()
	r.handleUpdatePlatform(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	got, err := svc.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Name != "UpdateTest" {
		t.Errorf("Name = %q, want UpdateTest (empty string should not clear name)", got.Name)
	}
}

func TestUpdatePlatform_BuiltinRenameRejected(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithPlatformForUpdate(t)

	// Attempt to rename the built-in Kodi profile.
	body := `{"name": "MyKodi"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/platforms/kodi", bytes.NewBufferString(body))
	req.SetPathValue("id", "kodi")
	w := httptest.NewRecorder()
	r.handleUpdatePlatform(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp["error"] == "" {
		t.Error("expected error message in response")
	}
}

func TestUpdatePlatform_FullUpdate(t *testing.T) {
	t.Parallel()
	r, svc, id := testRouterWithPlatformForUpdate(t)

	body := `{
		"name": "Renamed",
		"nfo_enabled": false,
		"nfo_format": "emby",
		"image_naming": {
			"thumb": ["artist.jpg"],
			"fanart": ["backdrop.jpg"],
			"logo": ["logo.png"],
			"banner": ["banner.jpg"]
		}
	}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/platforms/"+id, bytes.NewBufferString(body))
	req.SetPathValue("id", id)
	w := httptest.NewRecorder()
	r.handleUpdatePlatform(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	got, err := svc.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Name != "Renamed" {
		t.Errorf("Name = %q, want Renamed", got.Name)
	}
	if got.NFOEnabled {
		t.Error("NFOEnabled should be false")
	}
	if got.NFOFormat != "emby" {
		t.Errorf("NFOFormat = %q, want emby", got.NFOFormat)
	}
	if got.ImageNaming.PrimaryName("thumb") != "artist.jpg" {
		t.Errorf("thumb = %q, want artist.jpg", got.ImageNaming.PrimaryName("thumb"))
	}
	if got.ImageNaming.PrimaryName("fanart") != "backdrop.jpg" {
		t.Errorf("fanart = %q, want backdrop.jpg", got.ImageNaming.PrimaryName("fanart"))
	}
}

func TestUpdatePlatform_NotFound(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithPlatformForUpdate(t)

	body := `{"name": "whatever"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/platforms/nonexistent", bytes.NewBufferString(body))
	req.SetPathValue("id", "nonexistent")
	w := httptest.NewRecorder()
	r.handleUpdatePlatform(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestUpdatePlatform_UseSymlinks(t *testing.T) {
	t.Parallel()
	r, svc, id := testRouterWithPlatformForUpdate(t)

	body := `{"use_symlinks": true}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/platforms/"+id, bytes.NewBufferString(body))
	req.SetPathValue("id", id)
	w := httptest.NewRecorder()
	r.handleUpdatePlatform(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	got, err := svc.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if !got.UseSymlinks {
		t.Error("UseSymlinks should be true after update")
	}
}

func TestUpdatePlatform_UseSymlinks_BuiltinRejected(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithPlatformForUpdate(t)

	// Attempt to set use_symlinks on the built-in Kodi profile (not editable).
	body := `{"use_symlinks": true}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/platforms/kodi", bytes.NewBufferString(body))
	req.SetPathValue("id", "kodi")
	w := httptest.NewRecorder()
	r.handleUpdatePlatform(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp["error"] == "" {
		t.Error("expected error message in response")
	}
}

// TestGetUserBoolPreference_RomanizationFallback verifies the read path that
// the settings page handler uses to populate NameRomanizationFallback in
// SettingsData. The test calls getUserBoolPreference directly so it does not
// need the full suite of services wired up by handleSettingsPage.
func TestGetUserBoolPreference_RomanizationFallback(t *testing.T) {
	t.Parallel()
	r, _, userID := testRouterWithAuth(t)

	// Default (no stored row): must return true.
	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	req = withUserCtx(req, userID)
	ctx := middleware.WithTestRole(req.Context(), "administrator")
	if got := r.getUserBoolPreference(ctx, PrefMetadataNameRomanization, true); !got {
		t.Error("default: expected getUserBoolPreference to return true when no row stored")
	}

	// Store "false" via the preference update handler.
	prefBody := `{"value":"false"}`
	putReq := httptest.NewRequest(http.MethodPut, "/api/v1/preferences/"+PrefMetadataNameRomanization, strings.NewReader(prefBody))
	putReq.SetPathValue("key", PrefMetadataNameRomanization)
	putReq = withUserCtx(putReq, userID)
	pw := httptest.NewRecorder()
	r.handleUpdatePreference(pw, putReq)
	if pw.Code != http.StatusOK {
		t.Fatalf("PUT preference: expected 200, got %d: %s", pw.Code, pw.Body.String())
	}

	// getUserBoolPreference must now return false.
	if got := r.getUserBoolPreference(ctx, PrefMetadataNameRomanization, true); got {
		t.Error("after storing false: expected getUserBoolPreference to return false")
	}
}

// TestSetActivePlatform_CouplesNFORule verifies R5 (#2306): activating a
// Plex-style (NFO-disabled) profile turns the nfo_exists rule off and emits the
// nfoRuleToggled HX-Trigger; activating a non-Plex profile turns it back on
// (auto); and re-activating the already-active state is a silent no-op.
func TestSetActivePlatform_CouplesNFORule(t *testing.T) {
	t.Parallel()
	r, _ := testRouterWithPlatform(t)
	ctx := context.Background()

	profs, err := r.platformService.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var plexID, embyID string
	for _, p := range profs {
		if !p.NFOEnabled && plexID == "" {
			plexID = p.ID
		}
		if p.NFOEnabled && embyID == "" {
			embyID = p.ID
		}
	}
	if plexID == "" || embyID == "" {
		t.Fatalf("need a Plex-style and a non-Plex builtin profile; got %d profiles", len(profs))
	}

	// nfo_exists seeds enabled+auto (R4), so the rule starts on.
	rl, err := r.ruleService.GetByID(ctx, rule.RuleNFOExists)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if !rl.Enabled {
		t.Fatalf("precondition: nfo_exists should start enabled")
	}

	activate := func(id string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/platforms/"+id+"/activate", nil)
		req.SetPathValue("id", id)
		w := httptest.NewRecorder()
		r.handleSetActivePlatform(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("activate %s: status %d (%s)", id, w.Code, w.Body.String())
		}
		return w
	}
	triggerState := func(w *httptest.ResponseRecorder) string {
		t.Helper()
		h := w.Header().Get("HX-Trigger")
		if h == "" {
			return ""
		}
		var payload map[string]map[string]string
		if err := json.Unmarshal([]byte(h), &payload); err != nil {
			t.Fatalf("bad HX-Trigger %q: %v", h, err)
		}
		return payload["nfoRuleToggled"]["state"]
	}
	ruleEnabled := func() bool {
		t.Helper()
		got, err := r.ruleService.GetByID(ctx, rule.RuleNFOExists)
		if err != nil {
			t.Fatalf("GetByID: %v", err)
		}
		return got.Enabled
	}

	// Activate Plex -> rule off + "disabled" trigger.
	if s := triggerState(activate(plexID)); s != "disabled" {
		t.Errorf("plex activate: trigger state = %q, want disabled", s)
	}
	if ruleEnabled() {
		t.Errorf("plex activate: nfo_exists should be disabled")
	}

	// Activate a non-Plex profile -> rule on (auto) + "enabled" trigger.
	if s := triggerState(activate(embyID)); s != "enabled" {
		t.Errorf("non-plex activate: trigger state = %q, want enabled", s)
	}
	got, _ := r.ruleService.GetByID(ctx, rule.RuleNFOExists)
	if !got.Enabled {
		t.Errorf("non-plex activate: nfo_exists should be enabled")
	}
	if got.AutomationMode != rule.AutomationModeAuto {
		t.Errorf("non-plex activate: automation = %q, want auto", got.AutomationMode)
	}

	// Re-activate the same (already-enabled) profile -> no state change, no trigger.
	if h := activate(embyID).Header().Get("HX-Trigger"); h != "" {
		t.Errorf("no-op activate should not set HX-Trigger, got %q", h)
	}
}

// TestSetActivePlatform_InvalidID covers the SetActive error path: activating a
// nonexistent profile returns 400 and never toggles the rule.
func TestSetActivePlatform_InvalidID(t *testing.T) {
	t.Parallel()
	r, _ := testRouterWithPlatform(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/platforms/nope/activate", nil)
	req.SetPathValue("id", "nope")
	w := httptest.NewRecorder()
	r.handleSetActivePlatform(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%q)", w.Code, w.Body.String())
	}
	if h := w.Header().Get("HX-Trigger"); h != "" {
		t.Errorf("failed activation should not set HX-Trigger, got %q", h)
	}
}

// TestSetActivePlatform_NilRuleService is the fail-open path: with no rule
// service wired, activation still succeeds and simply skips the rule coupling.
func TestSetActivePlatform_NilRuleService(t *testing.T) {
	t.Parallel()
	r, _ := testRouterWithPlatform(t)
	r.ruleService = nil
	req := httptest.NewRequest(http.MethodPost, "/api/v1/platforms/plex/activate", nil)
	req.SetPathValue("id", "plex")
	w := httptest.NewRecorder()
	r.handleSetActivePlatform(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", w.Code, w.Body.String())
	}
	if h := w.Header().Get("HX-Trigger"); h != "" {
		t.Errorf("nil rule service should not set HX-Trigger, got %q", h)
	}
}

// TestSetActivePlatform_RuleLookupError is the fail-open path when the
// nfo_exists rule is absent: activation still succeeds, no trigger, no panic.
func TestSetActivePlatform_RuleLookupError(t *testing.T) {
	t.Parallel()
	r, _ := testRouterWithPlatform(t)
	if _, err := r.db.ExecContext(context.Background(), `DELETE FROM rules WHERE id = ?`, rule.RuleNFOExists); err != nil {
		t.Fatalf("deleting nfo_exists rule: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/platforms/plex/activate", nil)
	req.SetPathValue("id", "plex")
	w := httptest.NewRecorder()
	r.handleSetActivePlatform(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", w.Code, w.Body.String())
	}
	if h := w.Header().Get("HX-Trigger"); h != "" {
		t.Errorf("rule lookup error should not set HX-Trigger, got %q", h)
	}
}
