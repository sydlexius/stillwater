package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
)

func TestHandleGetMBDiffs_ArtistNotFound(t *testing.T) {
	r, _, _ := testRouterWithHistory(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/no-such-artist/musicbrainz/diffs", nil)
	req.SetPathValue("id", "no-such-artist")
	w := httptest.NewRecorder()

	r.handleGetMBDiffs(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestHandleGetMBDiffs_NoMBID(t *testing.T) {
	r, artistSvc, _ := testRouterWithHistory(t)

	a := addTestArtist(t, artistSvc, "No MBID Artist")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/"+a.ID+"/musicbrainz/diffs", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleGetMBDiffs(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d; body: %s", w.Code, http.StatusNotFound, w.Body.String())
	}
}

func TestHandleGetMBDiffs_EmptyDiffs(t *testing.T) {
	r, artistSvc, _ := testRouterWithHistory(t)

	a := &artist.Artist{
		Name:          "ABBA",
		SortName:      "ABBA",
		Type:          "group",
		MusicBrainzID: "d87e52c5-bb8d-4da8-b941-9f4928627dc8",
		Path:          "/music/ABBA",
		Genres:        []string{"Pop"},
	}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/"+a.ID+"/musicbrainz/diffs", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleGetMBDiffs(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	if resp["musicbrainz_id"] != "d87e52c5-bb8d-4da8-b941-9f4928627dc8" {
		t.Errorf("musicbrainz_id = %v, want ABBA's MBID", resp["musicbrainz_id"])
	}

	diffs, ok := resp["diffs"].([]any)
	if !ok {
		// diffs may be null when no snapshots exist; that's valid.
		if resp["diffs"] != nil {
			t.Fatalf("diffs field unexpected type: %T", resp["diffs"])
		}
	} else if len(diffs) != 0 {
		t.Errorf("len(diffs) = %d, want 0", len(diffs))
	}

	if resp["contribution_mode"] != "disabled" {
		t.Errorf("contribution_mode = %v, want %q", resp["contribution_mode"], "disabled")
	}
}

func TestHandleGetMBDiffs_WithDiffs(t *testing.T) {
	r, artistSvc, _ := testRouterWithHistory(t)

	a := &artist.Artist{
		Name:            "Bjork",
		SortName:        "Bjork",
		Type:            "person",
		MusicBrainzID:   "97fa3f6e-557c-4227-bc0e-95a7f9f3b56c",
		Path:            "/music/Bjork",
		Genres:          []string{"electronic", "art pop", "trip hop"},
		MetadataSources: map[string]string{"genres": "audiodb", "type": "discogs"},
	}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// Insert MB snapshots with different values.
	snapshots := []artist.MBSnapshot{
		{ArtistID: a.ID, Field: "genres", MBValue: `["electronic","art pop"]`},
		{ArtistID: a.ID, Field: "type", MBValue: "person"},
	}
	if err := artistSvc.UpsertMBSnapshots(context.Background(), a.ID, snapshots); err != nil {
		t.Fatalf("upserting snapshots: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/"+a.ID+"/musicbrainz/diffs", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleGetMBDiffs(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	diffs, ok := resp["diffs"].([]any)
	if !ok {
		t.Fatalf("diffs field missing or wrong type: %T", resp["diffs"])
	}

	// "type" is "person" in both, so no diff. Only "genres" should differ
	// (Stillwater has "trip hop" in addition to MB's genres).
	if len(diffs) != 1 {
		t.Fatalf("len(diffs) = %d, want 1; diffs = %v", len(diffs), diffs)
	}

	diff := diffs[0].(map[string]any)
	if diff["field"] != "genres" {
		t.Errorf("diff field = %v, want %q", diff["field"], "genres")
	}
	if diff["source"] != "audiodb" {
		t.Errorf("diff source = %v, want %q", diff["source"], "audiodb")
	}
}
