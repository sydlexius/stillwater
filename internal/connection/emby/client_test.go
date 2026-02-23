package emby

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/sydlexius/stillwater/internal/connection"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestTestConnection_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/System/Info" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("X-Emby-Token") != "test-key" {
			t.Errorf("missing or wrong auth header: %s", r.Header.Get("X-Emby-Token"))
		}
		http.ServeFile(w, r, "testdata/system_info.json")
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
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "bad-key", srv.Client(), testLogger())
	if err := c.TestConnection(context.Background()); err == nil {
		t.Fatal("expected error for unauthorized")
	}
}

func TestGetMusicLibraries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/Library/VirtualFolders" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		http.ServeFile(w, r, "testdata/virtual_folders.json")
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "test-key", srv.Client(), testLogger())
	libs, err := c.GetMusicLibraries(context.Background())
	if err != nil {
		t.Fatalf("GetMusicLibraries failed: %v", err)
	}

	if len(libs) != 2 {
		t.Fatalf("got %d music libraries, want 2", len(libs))
	}
	if libs[0].Name != "Music" {
		t.Errorf("first library = %q, want Music", libs[0].Name)
	}
}

func TestGetArtists(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/Artists" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		parentID := r.URL.Query().Get("ParentId")
		if parentID != "lib-001" {
			t.Errorf("ParentId = %q, want lib-001", parentID)
		}
		http.ServeFile(w, r, "testdata/artists.json")
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "test-key", srv.Client(), testLogger())
	resp, err := c.GetArtists(context.Background(), "lib-001", 0, 50)
	if err != nil {
		t.Fatalf("GetArtists failed: %v", err)
	}

	if resp.TotalRecordCount != 2 {
		t.Errorf("TotalRecordCount = %d, want 2", resp.TotalRecordCount)
	}
	if len(resp.Items) != 2 {
		t.Fatalf("got %d items, want 2", len(resp.Items))
	}
	if resp.Items[0].Name != "Radiohead" {
		t.Errorf("first artist = %q, want Radiohead", resp.Items[0].Name)
	}
	if resp.Items[0].ProviderIDs.MusicBrainzArtist != "a74b1b7f-71a5-4011-9441-d0b5e4122711" {
		t.Errorf("unexpected MBID: %s", resp.Items[0].ProviderIDs.MusicBrainzArtist)
	}
}

func TestTriggerLibraryScan(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/Library/Refresh" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "test-key", srv.Client(), testLogger())
	if err := c.TriggerLibraryScan(context.Background()); err != nil {
		t.Fatalf("TriggerLibraryScan failed: %v", err)
	}
}

func TestTriggerArtistRefresh(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/Items/emby-001/Refresh" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "test-key", srv.Client(), testLogger())
	if err := c.TriggerArtistRefresh(context.Background(), "emby-001"); err != nil {
		t.Fatalf("TriggerArtistRefresh failed: %v", err)
	}
}

func TestCheckNFOWriterEnabled_True(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"Name":"Music","CollectionType":"music","ItemId":"lib-001","LibraryOptions":{"SaveLocalMetadata":true,"MetadataSavers":["Nfo saver"]}},
			{"Name":"Movies","CollectionType":"movies","ItemId":"lib-002","LibraryOptions":{"SaveLocalMetadata":false,"MetadataSavers":[]}}
		]`))
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", srv.Client(), testLogger())
	enabled, libName, err := c.CheckNFOWriterEnabled(context.Background())
	if err != nil {
		t.Fatalf("CheckNFOWriterEnabled failed: %v", err)
	}
	if !enabled {
		t.Error("expected NFO writer to be enabled")
	}
	if libName != "Music" {
		t.Errorf("library name = %q, want Music", libName)
	}
}

func TestCheckNFOWriterEnabled_False(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"Name":"Music","CollectionType":"music","ItemId":"lib-001","LibraryOptions":{"SaveLocalMetadata":false,"MetadataSavers":[]}},
			{"Name":"Movies","CollectionType":"movies","ItemId":"lib-002","LibraryOptions":{"SaveLocalMetadata":false,"MetadataSavers":[]}}
		]`))
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", srv.Client(), testLogger())
	enabled, _, err := c.CheckNFOWriterEnabled(context.Background())
	if err != nil {
		t.Fatalf("CheckNFOWriterEnabled failed: %v", err)
	}
	if enabled {
		t.Error("expected NFO writer to be disabled")
	}
}

func TestCheckNFOWriterEnabled_NoMusicLibraries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"Name":"Movies","CollectionType":"movies","ItemId":"lib-002","LibraryOptions":{"SaveLocalMetadata":false,"MetadataSavers":["Nfo saver"]}}
		]`))
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", srv.Client(), testLogger())
	enabled, _, err := c.CheckNFOWriterEnabled(context.Background())
	if err != nil {
		t.Fatalf("CheckNFOWriterEnabled failed: %v", err)
	}
	if enabled {
		t.Error("expected false when NFO saver only on non-music library")
	}
}

func TestCheckNFOWriterEnabled_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", srv.Client(), testLogger())
	enabled, _, err := c.CheckNFOWriterEnabled(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if enabled {
		t.Error("expected false on server error")
	}
}

func TestPushMetadata(t *testing.T) {
	var gotBody itemUpdateBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/Items/emby-artist-1" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("content-type = %s, want application/json", r.Header.Get("Content-Type"))
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decoding body: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "test-key", srv.Client(), testLogger())
	data := connection.ArtistPushData{
		Name:      "Radiohead",
		SortName:  "Radiohead",
		Biography: "English rock band",
		Genres:    []string{"Rock", "Alternative"},
	}
	if err := c.PushMetadata(context.Background(), "emby-artist-1", data); err != nil {
		t.Fatalf("PushMetadata failed: %v", err)
	}
	if gotBody.Name != "Radiohead" {
		t.Errorf("Name = %q, want Radiohead", gotBody.Name)
	}
	if gotBody.ForcedSortName != "Radiohead" {
		t.Errorf("ForcedSortName = %q, want Radiohead", gotBody.ForcedSortName)
	}
	if gotBody.Overview != "English rock band" {
		t.Errorf("Overview = %q, want English rock band", gotBody.Overview)
	}
	if len(gotBody.Genres) != 2 {
		t.Errorf("got %d genres, want 2", len(gotBody.Genres))
	}
}

func TestPushMetadata_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal"}`))
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "test-key", srv.Client(), testLogger())
	err := c.PushMetadata(context.Background(), "emby-001", connection.ArtistPushData{Name: "Test"})
	if err == nil {
		t.Fatal("expected error for server error")
	}
}
