package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
)

// TestHandleFieldUpdate_Name verifies that updating the "name" field
// persists correctly through the API.
func TestHandleFieldUpdate_Name(t *testing.T) {
	r, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Old Name")

	body := strings.NewReader("value=New Name")
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/artists/"+a.ID+"/fields/name", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("id", a.ID)
	req.SetPathValue("field", "name")
	w := httptest.NewRecorder()

	r.handleFieldUpdate(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	got, err := artistSvc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Name != "New Name" {
		t.Errorf("Name = %q, want 'New Name'", got.Name)
	}
}

// TestHandleFieldUpdate_NameEmpty verifies that an empty name is rejected with 400.
func TestHandleFieldUpdate_NameEmpty(t *testing.T) {
	r, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Some Artist")

	body := strings.NewReader("value=")
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/artists/"+a.ID+"/fields/name", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("id", a.ID)
	req.SetPathValue("field", "name")
	w := httptest.NewRecorder()

	r.handleFieldUpdate(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

// TestHandleFieldUpdate_SortName verifies that sort_name is editable.
func TestHandleFieldUpdate_SortName(t *testing.T) {
	r, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "The Beatles")

	body := strings.NewReader("value=Beatles, The")
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/artists/"+a.ID+"/fields/sort_name", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("id", a.ID)
	req.SetPathValue("field", "sort_name")
	w := httptest.NewRecorder()

	r.handleFieldUpdate(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	got, err := artistSvc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.SortName != "Beatles, The" {
		t.Errorf("SortName = %q, want 'Beatles, The'", got.SortName)
	}
}

// TestHandleFieldUpdate_Disambiguation verifies that disambiguation is editable.
func TestHandleFieldUpdate_Disambiguation(t *testing.T) {
	r, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Genesis")

	body := strings.NewReader("value=the prog rock band")
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/artists/"+a.ID+"/fields/disambiguation", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("id", a.ID)
	req.SetPathValue("field", "disambiguation")
	w := httptest.NewRecorder()

	r.handleFieldUpdate(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	got, err := artistSvc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Disambiguation != "the prog rock band" {
		t.Errorf("Disambiguation = %q, want 'the prog rock band'", got.Disambiguation)
	}
}

// TestHandleFieldUpdate_MusicBrainzID verifies that musicbrainz_id is
// editable via the API and persists through the normalized provider_ids table.
func TestHandleFieldUpdate_MusicBrainzID(t *testing.T) {
	r, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Radiohead")

	validMBID := "a74b1b7f-71a5-4011-9441-d0b5e4122711"
	body := strings.NewReader("value=" + validMBID)
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/artists/"+a.ID+"/fields/musicbrainz_id", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("id", a.ID)
	req.SetPathValue("field", "musicbrainz_id")
	w := httptest.NewRecorder()

	r.handleFieldUpdate(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	got, err := artistSvc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.MusicBrainzID != validMBID {
		t.Errorf("MusicBrainzID = %q, want %q", got.MusicBrainzID, validMBID)
	}

	// Verify JSON response contains the updated value
	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp["value"] != validMBID {
		t.Errorf("response value = %q, want %q", resp["value"], validMBID)
	}
}

// TestHandleFieldUpdate_MusicBrainzID_InvalidFormat verifies that an invalid
// UUID format is rejected with 400.
func TestHandleFieldUpdate_MusicBrainzID_InvalidFormat(t *testing.T) {
	r, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Test Artist")

	body := strings.NewReader("value=not-a-valid-uuid")
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/artists/"+a.ID+"/fields/musicbrainz_id", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("id", a.ID)
	req.SetPathValue("field", "musicbrainz_id")
	w := httptest.NewRecorder()

	r.handleFieldUpdate(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

// TestHandleFieldUpdate_AudioDBID verifies that audiodb_id is editable.
func TestHandleFieldUpdate_AudioDBID(t *testing.T) {
	r, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Tool")

	body := strings.NewReader("value=111239")
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/artists/"+a.ID+"/fields/audiodb_id", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("id", a.ID)
	req.SetPathValue("field", "audiodb_id")
	w := httptest.NewRecorder()

	r.handleFieldUpdate(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	got, err := artistSvc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.AudioDBID != "111239" {
		t.Errorf("AudioDBID = %q, want '111239'", got.AudioDBID)
	}
}

// TestHandleFieldClear_MusicBrainzID verifies that clearing musicbrainz_id
// removes it from the provider_ids table.
func TestHandleFieldClear_MusicBrainzID(t *testing.T) {
	r, artistSvc := testRouter(t)

	a := addTestArtist(t, artistSvc, "Led Zeppelin")
	a.MusicBrainzID = "678d88b2-87b0-403b-b63d-5da7465aecc3"
	if err := artistSvc.Update(context.Background(), a); err != nil {
		t.Fatalf("updating artist: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/artists/"+a.ID+"/fields/musicbrainz_id", nil)
	req.SetPathValue("id", a.ID)
	req.SetPathValue("field", "musicbrainz_id")
	w := httptest.NewRecorder()

	r.handleFieldClear(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	got, err := artistSvc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.MusicBrainzID != "" {
		t.Errorf("MusicBrainzID = %q after clear, want empty", got.MusicBrainzID)
	}
}

// TestHandleFieldUpdate_ProviderIDFields_AllEditable verifies that all five
// provider ID fields are accepted by IsEditableField and the handler routes them
// correctly without returning 400 "unknown field".
func TestHandleFieldUpdate_ProviderIDFields_AllEditable(t *testing.T) {
	r, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Pink Floyd")

	providerFields := map[string]string{
		"discogs_id":  "45467",
		"wikidata_id": "Q2306",
		"deezer_id":   "9889",
	}

	for field, value := range providerFields {
		t.Run(field, func(t *testing.T) {
			body := strings.NewReader("value=" + value)
			req := httptest.NewRequest(http.MethodPatch, "/api/v1/artists/"+a.ID+"/fields/"+field, body)
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.SetPathValue("id", a.ID)
			req.SetPathValue("field", field)
			w := httptest.NewRecorder()

			r.handleFieldUpdate(w, req)

			if w.Code != http.StatusOK {
				t.Errorf("field %q: status = %d, want %d; body: %s", field, w.Code, http.StatusOK, w.Body.String())
				return
			}

			got, err := artistSvc.GetByID(context.Background(), a.ID)
			if err != nil {
				t.Fatalf("GetByID: %v", err)
			}
			if artist.FieldValueFromArtist(got, field) != value {
				t.Errorf("field %q: got %q, want %q", field, artist.FieldValueFromArtist(got, field), value)
			}
		})
	}
}

// TestHandleFieldDisplay_NewFields verifies that the display handler accepts
// all new field names without returning 400.
func TestHandleFieldDisplay_NewFields(t *testing.T) {
	r, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Artist For Display")

	newFields := []string{
		"name", "sort_name", "disambiguation",
		"musicbrainz_id", "audiodb_id", "discogs_id", "wikidata_id", "deezer_id",
	}

	for _, field := range newFields {
		t.Run(field, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/"+a.ID+"/fields/"+field+"/display", nil)
			req.SetPathValue("id", a.ID)
			req.SetPathValue("field", field)
			w := httptest.NewRecorder()

			r.handleFieldDisplay(w, req)

			if w.Code != http.StatusOK {
				t.Errorf("field %q: status = %d, want %d; body: %s", field, w.Code, http.StatusOK, w.Body.String())
			}
		})
	}
}
