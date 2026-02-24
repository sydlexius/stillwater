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

func TestHandleClearMembers_JSON(t *testing.T) {
	r, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "The Beatles")

	// Seed some members first.
	members := []artist.BandMember{
		{MemberName: "John Lennon", SortOrder: 0},
		{MemberName: "Paul McCartney", SortOrder: 1},
	}
	if err := artistSvc.UpsertMembers(context.Background(), a.ID, members); err != nil {
		t.Fatalf("seeding members: %v", err)
	}

	// Verify members exist.
	before, _ := artistSvc.ListMembersByArtistID(context.Background(), a.ID)
	if len(before) != 2 {
		t.Fatalf("expected 2 members before clear, got %d", len(before))
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/artists/"+a.ID+"/members", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleClearMembers(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp["status"] != "cleared" {
		t.Errorf("status = %q, want %q", resp["status"], "cleared")
	}

	// Verify members are gone.
	after, _ := artistSvc.ListMembersByArtistID(context.Background(), a.ID)
	if len(after) != 0 {
		t.Errorf("expected 0 members after clear, got %d", len(after))
	}
}

func TestHandleClearMembers_HTMX(t *testing.T) {
	r, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Radiohead")

	members := []artist.BandMember{
		{MemberName: "Thom Yorke", SortOrder: 0},
	}
	if err := artistSvc.UpsertMembers(context.Background(), a.ID, members); err != nil {
		t.Fatalf("seeding members: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/artists/"+a.ID+"/members", nil)
	req.SetPathValue("id", a.ID)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()

	r.handleClearMembers(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	ct := w.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}

	body := w.Body.String()
	if !strings.Contains(body, "No members.") {
		t.Errorf("expected 'No members.' in HTML response, got: %s", body)
	}

	// Verify members are gone.
	after, _ := artistSvc.ListMembersByArtistID(context.Background(), a.ID)
	if len(after) != 0 {
		t.Errorf("expected 0 members after clear, got %d", len(after))
	}
}

func TestHandleSaveMembers_JSON(t *testing.T) {
	r, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Nirvana")

	body := `[
		{"name": "Kurt Cobain", "instruments": ["guitar", "vocals"]},
		{"name": "Krist Novoselic", "instruments": ["bass"]},
		{"name": "Dave Grohl", "instruments": ["drums"]}
	]`

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/members/from-provider",
		strings.NewReader(body))
	req.SetPathValue("id", a.ID)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleSaveMembers(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp["status"] != "saved" {
		t.Errorf("status = %q, want %q", resp["status"], "saved")
	}

	// Verify members were persisted.
	saved, err := artistSvc.ListMembersByArtistID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("listing members: %v", err)
	}
	if len(saved) != 3 {
		t.Fatalf("expected 3 members, got %d", len(saved))
	}
	if saved[0].MemberName != "Kurt Cobain" {
		t.Errorf("first member = %q, want %q", saved[0].MemberName, "Kurt Cobain")
	}
}

func TestHandleSaveMembers_HTMX(t *testing.T) {
	r, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Led Zeppelin")

	body := `[
		{"name": "Jimmy Page", "instruments": ["guitar"]},
		{"name": "Robert Plant", "instruments": ["vocals"]}
	]`

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/members/from-provider",
		strings.NewReader(body))
	req.SetPathValue("id", a.ID)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()

	r.handleSaveMembers(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	ct := w.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}

	// Check that the HX-Trigger header is set.
	if trigger := w.Header().Get("HX-Trigger"); trigger != "hideFieldProviderModal" {
		t.Errorf("HX-Trigger = %q, want %q", trigger, "hideFieldProviderModal")
	}

	// HTML should contain the member names.
	html := w.Body.String()
	if !strings.Contains(html, "Jimmy Page") {
		t.Errorf("expected 'Jimmy Page' in HTML response")
	}
	if !strings.Contains(html, "Robert Plant") {
		t.Errorf("expected 'Robert Plant' in HTML response")
	}
}

func TestHandleSaveMembers_InvalidBody(t *testing.T) {
	r, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Pink Floyd")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/members/from-provider",
		strings.NewReader("not json"))
	req.SetPathValue("id", a.ID)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleSaveMembers(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleSaveMembers_ReplacesExisting(t *testing.T) {
	r, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Queen")

	// Seed initial members.
	initial := []artist.BandMember{
		{MemberName: "Freddie Mercury", SortOrder: 0},
		{MemberName: "Brian May", SortOrder: 1},
	}
	if err := artistSvc.UpsertMembers(context.Background(), a.ID, initial); err != nil {
		t.Fatalf("seeding members: %v", err)
	}

	// Save new members from provider (should replace, not append).
	body := `[{"name": "Roger Taylor", "instruments": ["drums"]}]`

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/members/from-provider",
		strings.NewReader(body))
	req.SetPathValue("id", a.ID)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleSaveMembers(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	saved, _ := artistSvc.ListMembersByArtistID(context.Background(), a.ID)
	if len(saved) != 1 {
		t.Fatalf("expected 1 member after replace, got %d", len(saved))
	}
	if saved[0].MemberName != "Roger Taylor" {
		t.Errorf("member = %q, want %q", saved[0].MemberName, "Roger Taylor")
	}
}
