package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sydlexius/stillwater/internal/platform"
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
