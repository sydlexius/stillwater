package lidarr

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestTestConnection_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/system/status" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("X-Api-Key") != "test-key" {
			t.Errorf("missing or wrong auth header: %s", r.Header.Get("X-Api-Key"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"2.0.0","appName":"Lidarr"}`))
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "test-key", srv.Client(), testLogger())
	if err := c.TestConnection(context.Background()); err != nil {
		t.Fatalf("TestConnection failed: %v", err)
	}
}

func TestTestConnection_Unauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "bad-key", srv.Client(), testLogger())
	if err := c.TestConnection(context.Background()); err == nil {
		t.Fatal("expected error for unauthorized")
	}
}

func TestGetArtists(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"id":1,"artistName":"Radiohead","foreignArtistId":"mbid-001","path":"/music/Radiohead","monitored":true,"metadataProfileId":1},
			{"id":2,"artistName":"Bjork","foreignArtistId":"mbid-002","path":"/music/Bjork","monitored":true,"metadataProfileId":1}
		]`))
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", srv.Client(), testLogger())
	artists, err := c.GetArtists(context.Background())
	if err != nil {
		t.Fatalf("GetArtists failed: %v", err)
	}
	if len(artists) != 2 {
		t.Fatalf("got %d artists, want 2", len(artists))
	}
	if artists[0].ForeignArtistID != "mbid-001" {
		t.Errorf("MBID = %q, want mbid-001", artists[0].ForeignArtistID)
	}
}

func TestGetMetadataProfiles(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":1,"name":"Standard"},{"id":2,"name":"Minimal"}]`))
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", srv.Client(), testLogger())
	profiles, err := c.GetMetadataProfiles(context.Background())
	if err != nil {
		t.Fatalf("GetMetadataProfiles failed: %v", err)
	}
	if len(profiles) != 2 {
		t.Fatalf("got %d profiles, want 2", len(profiles))
	}
}

func TestCheckNFOWriterEnabled_True(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"id":1,"metadataType":"MediaBrowser","consumerId":1,"consumerName":"MediaBrowser","enable":true},
			{"id":2,"metadataType":"Kodi (XBMC) / Emby","consumerId":2,"consumerName":"Kodi (XBMC) / Emby","enable":true}
		]`))
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", srv.Client(), testLogger())
	enabled, err := c.CheckNFOWriterEnabled(context.Background())
	if err != nil {
		t.Fatalf("CheckNFOWriterEnabled failed: %v", err)
	}
	if !enabled {
		t.Error("expected NFO writer to be enabled")
	}
}

func TestCheckNFOWriterEnabled_False(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"id":1,"metadataType":"MediaBrowser","consumerId":1,"consumerName":"MediaBrowser","enable":true},
			{"id":2,"metadataType":"Kodi (XBMC) / Emby","consumerId":2,"consumerName":"Kodi (XBMC) / Emby","enable":false}
		]`))
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", srv.Client(), testLogger())
	enabled, err := c.CheckNFOWriterEnabled(context.Background())
	if err != nil {
		t.Fatalf("CheckNFOWriterEnabled failed: %v", err)
	}
	if enabled {
		t.Error("expected NFO writer to be disabled")
	}
}

func TestTriggerArtistRefresh(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/command" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		var cmd CommandBody
		if err := json.NewDecoder(r.Body).Decode(&cmd); err != nil {
			t.Fatalf("decoding body: %v", err)
		}
		if cmd.Name != "RefreshArtist" {
			t.Errorf("command name = %q, want RefreshArtist", cmd.Name)
		}
		if cmd.ArtistID != 42 {
			t.Errorf("artistId = %d, want 42", cmd.ArtistID)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":1,"name":"RefreshArtist","status":"queued"}`))
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", srv.Client(), testLogger())
	resp, err := c.TriggerArtistRefresh(context.Background(), 42)
	if err != nil {
		t.Fatalf("TriggerArtistRefresh failed: %v", err)
	}
	if resp.Status != "queued" {
		t.Errorf("status = %q, want queued", resp.Status)
	}
}
