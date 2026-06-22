package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sydlexius/stillwater/internal/connection"
)

// TestHandlePullMetadata_MatchingFieldNotInUpdated verifies that when the
// platform value already matches the stored value, that field is NOT included
// in the "updated" list (the changed bool is false, so the append is skipped).
//
// The test stands up a minimal httptest.Server that mimics just enough of the
// Emby API for GetArtistDetail to succeed, then calls handlePullMetadata and
// checks the JSON response.
func TestHandlePullMetadata_MatchingFieldNotInUpdated(t *testing.T) {
	t.Parallel()
	r, artistSvc, _ := testRouterWithHistory(t)
	ctx := context.Background()

	// Create an artist with a known biography.
	a := addTestArtist(t, artistSvc, "Pull Noop Artist")
	if _, err := artistSvc.UpdateField(ctx, a.ID, "biography", "existing bio"); err != nil {
		t.Fatalf("setting initial biography: %v", err)
	}

	// Stand up a stub Emby server that returns the SAME biography the artist
	// already has. The handler should not include "biography" in updated.
	embyResp := `{"Name":"Pull Noop Artist","SortName":"Pull Noop Artist","Overview":"existing bio","Genres":[],"Tags":[],"ProviderIds":{},"ImageTags":{},"BackdropImageTags":[],"LockData":false,"LockedFields":[]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, embyResp)
	}))
	defer srv.Close()

	// Create a connection pointing to the test server.
	conn := &connection.Connection{
		Name:    "Test Emby",
		Type:    connection.TypeEmby,
		URL:     srv.URL,
		APIKey:  "test-api-key",
		Emby:    &connection.EmbyConfig{PlatformUserID: "user-001"},
		Enabled: true,
	}
	if err := r.connectionService.Create(ctx, conn); err != nil {
		t.Fatalf("creating connection: %v", err)
	}

	// Associate a platform artist ID with this artist on this connection.
	platformID := "emby-artist-001"
	if err := artistSvc.SetPlatformID(ctx, a.ID, conn.ID, platformID); err != nil {
		t.Fatalf("setting platform artist ID: %v", err)
	}

	// Call handlePullMetadata.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/pull?connection_id="+conn.ID, nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()
	r.handlePullMetadata(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	// When updated is nil (no fields changed), the JSON will encode as null.
	// Either null or an empty slice satisfies "biography not in updated".
	if updatedRaw := resp["updated"]; updatedRaw != nil {
		updated, ok := updatedRaw.([]any)
		if !ok {
			t.Fatalf("updated is not an array: %T %v", updatedRaw, updatedRaw)
		}
		for _, v := range updated {
			if v == "biography" {
				t.Errorf("biography appears in updated=%v but the value already matched the platform; want it absent", updated)
			}
		}
	}
}

// TestHandlePullMetadata_ChangedFieldInUpdated verifies that when the platform
// returns a value that differs from the stored value, that field IS included in
// the "updated" list (changed==true).
func TestHandlePullMetadata_ChangedFieldInUpdated(t *testing.T) {
	t.Parallel()
	r, artistSvc, _ := testRouterWithHistory(t)
	ctx := context.Background()

	a := addTestArtist(t, artistSvc, "Pull Changed Artist")
	// Artist starts with no biography / no genres; platform returns new values for
	// both plus dates so all 4 UpdateField branches are exercised.
	embyResp := `{"Name":"Pull Changed Artist","SortName":"Pull Changed Artist","Overview":"new platform bio","Genres":["Rock","Grunge"],"Tags":[],"ProviderIds":{},"ImageTags":{},"BackdropImageTags":[],"LockData":false,"LockedFields":[],"PremiereDate":"1990-01-01T00:00:00.0000000Z","EndDate":"2000-06-01T00:00:00.0000000Z"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, embyResp)
	}))
	defer srv.Close()

	conn := &connection.Connection{
		Name:    "Test Emby Changed",
		Type:    connection.TypeEmby,
		URL:     srv.URL,
		APIKey:  "test-api-key",
		Emby:    &connection.EmbyConfig{PlatformUserID: "user-001"},
		Enabled: true,
	}
	if err := r.connectionService.Create(ctx, conn); err != nil {
		t.Fatalf("creating connection: %v", err)
	}
	if err := artistSvc.SetPlatformID(ctx, a.ID, conn.ID, "emby-artist-002"); err != nil {
		t.Fatalf("setting platform artist ID: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/pull?connection_id="+conn.ID, nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()
	r.handlePullMetadata(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	updatedRaw := resp["updated"]
	updated, ok := updatedRaw.([]any)
	if !ok || len(updated) == 0 {
		t.Fatalf("updated = %v, want [biography] (new platform value differs from stored value)", updatedRaw)
	}
	found := false
	for _, v := range updated {
		if v == "biography" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("biography not in updated=%v, want it present (field actually changed)", updated)
	}
}
