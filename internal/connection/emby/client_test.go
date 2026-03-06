package emby

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
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

	c := NewWithHTTPClient(srv.URL, "test-key", "", srv.Client(), testLogger())
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

	c := NewWithHTTPClient(srv.URL, "bad-key", "", srv.Client(), testLogger())
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

	c := NewWithHTTPClient(srv.URL, "test-key", "", srv.Client(), testLogger())
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
		if r.URL.Path != "/Artists/AlbumArtists" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		parentID := r.URL.Query().Get("ParentId")
		if parentID != "lib-001" {
			t.Errorf("ParentId = %q, want lib-001", parentID)
		}
		fields := r.URL.Query().Get("Fields")
		if fields != "Path,ProviderIds,ImageTags,Overview,Genres,Tags,SortName,PremiereDate,EndDate" {
			t.Errorf("Fields = %q, want expanded field list", fields)
		}
		http.ServeFile(w, r, "testdata/artists.json")
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "test-key", "", srv.Client(), testLogger())
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

	rh := resp.Items[0]
	if rh.Name != "Radiohead" {
		t.Errorf("first artist = %q, want Radiohead", rh.Name)
	}
	if rh.SortName != "Radiohead" {
		t.Errorf("SortName = %q, want Radiohead", rh.SortName)
	}
	if rh.ProviderIDs.MusicBrainzArtist != "a74b1b7f-71a5-4011-9441-d0b5e4122711" {
		t.Errorf("unexpected MBID: %s", rh.ProviderIDs.MusicBrainzArtist)
	}
	if rh.Overview != "English rock band formed in 1985." {
		t.Errorf("Overview = %q, want biography text", rh.Overview)
	}
	if len(rh.Genres) != 2 || rh.Genres[0] != "Rock" {
		t.Errorf("Genres = %v, want [Rock Alternative]", rh.Genres)
	}
	if len(rh.Tags) != 2 || rh.Tags[0] != "Experimental" {
		t.Errorf("Tags = %v, want [Experimental Art Rock]", rh.Tags)
	}
	if rh.PremiereDate != "1985-01-01T00:00:00.0000000Z" {
		t.Errorf("PremiereDate = %q, want 1985 date", rh.PremiereDate)
	}
	if rh.ImageTags["Primary"] != "abc123" {
		t.Errorf("ImageTags[Primary] = %q, want abc123", rh.ImageTags["Primary"])
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

	c := NewWithHTTPClient(srv.URL, "test-key", "", srv.Client(), testLogger())
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

	c := NewWithHTTPClient(srv.URL, "test-key", "", srv.Client(), testLogger())
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

	c := NewWithHTTPClient(srv.URL, "key", "", srv.Client(), testLogger())
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

	c := NewWithHTTPClient(srv.URL, "key", "", srv.Client(), testLogger())
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

	c := NewWithHTTPClient(srv.URL, "key", "", srv.Client(), testLogger())
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

	c := NewWithHTTPClient(srv.URL, "key", "", srv.Client(), testLogger())
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

	c := NewWithHTTPClient(srv.URL, "test-key", "", srv.Client(), testLogger())
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

	c := NewWithHTTPClient(srv.URL, "test-key", "", srv.Client(), testLogger())
	err := c.PushMetadata(context.Background(), "emby-001", connection.ArtistPushData{Name: "Test"})
	if err == nil {
		t.Fatal("expected error for server error")
	}
}

// createTestJPEG generates a minimal 1x1 JPEG image for testing.
func createTestJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.RGBA{R: 255, G: 0, B: 0, A: 255})
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatalf("encoding test jpeg: %v", err)
	}
	return buf.Bytes()
}

func TestGetArtistImage_Success(t *testing.T) {
	jpegData := createTestJPEG(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/Items/emby-001/Images/Primary" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("X-Emby-Token") != "test-key" {
			t.Errorf("missing or wrong auth header")
		}
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(jpegData)
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "test-key", "", srv.Client(), testLogger())
	data, contentType, err := c.GetArtistImage(context.Background(), "emby-001", "thumb")
	if err != nil {
		t.Fatalf("GetArtistImage failed: %v", err)
	}
	if contentType != "image/jpeg" {
		t.Errorf("content-type = %q, want image/jpeg", contentType)
	}
	if !bytes.Equal(data, jpegData) {
		t.Errorf("image data mismatch: got %d bytes, want %d", len(data), len(jpegData))
	}
}

func TestGetArtistImage_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("not found"))
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "test-key", "", srv.Client(), testLogger())
	_, _, err := c.GetArtistImage(context.Background(), "emby-001", "thumb")
	if err == nil {
		t.Fatal("expected error for 404 response")
	}
}

func TestGetArtistImage_UnsupportedType(t *testing.T) {
	c := New("http://localhost", "key", "", testLogger())
	_, _, err := c.GetArtistImage(context.Background(), "emby-001", "clearart")
	if err == nil {
		t.Fatal("expected error for unsupported image type")
	}
}

func TestGetRaw_OversizedImage(t *testing.T) {
	// Return exactly 25 MB + 1 byte to trigger the size check.
	const maxImageSize = 25 << 20
	oversized := make([]byte, maxImageSize+1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(oversized)
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", "", srv.Client(), testLogger())
	_, _, err := c.GetArtistImage(context.Background(), "emby-001", "thumb")
	if err == nil {
		t.Fatal("expected error for oversized image")
	}
	if !strings.Contains(err.Error(), "exceeds 25 MB") {
		t.Errorf("error = %q, want message about exceeding 25 MB limit", err)
	}
}

func TestGetRaw_ErrorBodyLimited(t *testing.T) {
	// Return a 500 with a body larger than 1 KB to verify it gets truncated.
	largeBody := strings.Repeat("x", 4096)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(largeBody))
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", "", srv.Client(), testLogger())
	_, _, err := c.GetArtistImage(context.Background(), "emby-001", "thumb")
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
	// The error message format is "unexpected status 500: <body>".
	// Verify the included body is bounded (not the full 4096 bytes).
	errMsg := err.Error()
	if len(errMsg) > 1100 {
		t.Errorf("error message length = %d, want bounded (body should be limited to 1024 bytes)", len(errMsg))
	}
}

func TestGet_ErrorBodyLimited(t *testing.T) {
	largeBody := strings.Repeat("a", 4096)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(largeBody))
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", "", srv.Client(), testLogger())
	err := c.TestConnection(context.Background()) // uses get()
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
	errMsg := err.Error()
	if len(errMsg) > 1100 {
		t.Errorf("error message length = %d, want bounded (body should be limited to 1024 bytes)", len(errMsg))
	}
}

func TestPost_ErrorBodyLimited(t *testing.T) {
	largeBody := strings.Repeat("b", 4096)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(largeBody))
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", "", srv.Client(), testLogger())
	err := c.TriggerLibraryScan(context.Background()) // uses post()
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
	errMsg := err.Error()
	if len(errMsg) > 1100 {
		t.Errorf("error message length = %d, want bounded (body should be limited to 1024 bytes)", len(errMsg))
	}
}

func TestGetArtistDetail_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Emby-Token") != "test-key" {
			t.Errorf("missing or wrong auth header: %s", r.Header.Get("X-Emby-Token"))
		}
		if r.URL.Path != "/Users/user-001/Items/emby-001" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fields := r.URL.Query().Get("Fields")
		if fields == "" {
			t.Errorf("Fields query param missing")
		}
		http.ServeFile(w, r, "testdata/artist_detail.json")
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "test-key", "user-001", srv.Client(), testLogger())
	state, err := c.GetArtistDetail(context.Background(), "emby-001")
	if err != nil {
		t.Fatalf("GetArtistDetail failed: %v", err)
	}
	if state.Name != "Radiohead" {
		t.Errorf("Name = %q, want Radiohead", state.Name)
	}
	if state.Biography == "" {
		t.Error("Biography should not be empty")
	}
	if state.MusicBrainzID != "a74b1b7f-71a5-4011-9441-d0b5e4122711" {
		t.Errorf("MusicBrainzID = %q, want a74b1b7f-71a5-4011-9441-d0b5e4122711", state.MusicBrainzID)
	}
	if !state.HasThumb {
		t.Error("HasThumb should be true")
	}
	if !state.HasFanart {
		t.Error("HasFanart should be true")
	}
	if !state.HasLogo {
		t.Error("HasLogo should be true")
	}
	if !state.HasBanner {
		t.Error("HasBanner should be true")
	}
	if state.IsLocked {
		t.Error("IsLocked should be false")
	}
	if len(state.Genres) != 2 {
		t.Errorf("got %d genres, want 2", len(state.Genres))
	}
}

func TestGetArtistDetail_NotFound(t *testing.T) {
	var reqCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqCount++
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("not found"))
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "test-key", "user-001", srv.Client(), testLogger())
	_, err := c.GetArtistDetail(context.Background(), "emby-999")
	if err == nil {
		t.Fatal("expected error for 404 response")
	}
	if reqCount != 1 {
		t.Fatalf("got %d requests, want exactly 1 (no /Users lookup)", reqCount)
	}
}

func TestGetArtistDetail_EmptyUserID(t *testing.T) {
	c := New("http://localhost", "key", "", testLogger())
	_, err := c.GetArtistDetail(context.Background(), "emby-001")
	if err == nil {
		t.Fatal("expected error for empty user ID")
	}
	if !strings.Contains(err.Error(), "no user ID configured") {
		t.Errorf("error = %q, want message about missing user ID", err)
	}
}

func TestGetFirstUserID_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/Users" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"Id":"user-001","Name":"Admin"},{"Id":"user-002","Name":"Other"}]`))
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "test-key", "", srv.Client(), testLogger())
	uid, err := c.GetFirstUserID(context.Background())
	if err != nil {
		t.Fatalf("GetFirstUserID failed: %v", err)
	}
	if uid != "user-001" {
		t.Errorf("user ID = %q, want user-001", uid)
	}
}

func TestGetFirstUserID_NoUsers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "test-key", "", srv.Client(), testLogger())
	_, err := c.GetFirstUserID(context.Background())
	if err == nil {
		t.Fatal("expected error for empty users list")
	}
}

func TestDeleteImage_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/Items/emby-001/Images/Primary" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s, want DELETE", r.Method)
		}
		if r.Header.Get("X-Emby-Token") != "test-key" {
			t.Errorf("missing or wrong auth header: %s", r.Header.Get("X-Emby-Token"))
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "test-key", "", srv.Client(), testLogger())
	if err := c.DeleteImage(context.Background(), "emby-001", "thumb"); err != nil {
		t.Fatalf("DeleteImage failed: %v", err)
	}
}

func TestDeleteImage_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal"}`))
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "test-key", "", srv.Client(), testLogger())
	if err := c.DeleteImage(context.Background(), "emby-001", "thumb"); err == nil {
		t.Fatal("expected error for server error response")
	}
}

func TestDeleteImage_UnsupportedType(t *testing.T) {
	c := New("http://localhost", "key", "", testLogger())
	if err := c.DeleteImage(context.Background(), "emby-001", "clearart"); err == nil {
		t.Fatal("expected error for unsupported image type")
	}
}

func TestPushMetadata_DateNormalization(t *testing.T) {
	tests := []struct {
		name         string
		data         connection.ArtistPushData
		wantPremiere string
		wantEnd      string
	}{
		{
			name:         "year-only born",
			data:         connection.ArtistPushData{Name: "Test", Born: "1985"},
			wantPremiere: "1985-01-01",
		},
		{
			name:         "year-month formed",
			data:         connection.ArtistPushData{Name: "Test", Formed: "1991-05"},
			wantPremiere: "1991-05-01",
		},
		{
			name:         "full date born",
			data:         connection.ArtistPushData{Name: "Test", Born: "1946-10-14"},
			wantPremiere: "1946-10-14",
		},
		{
			name:         "ISO 8601 passthrough",
			data:         connection.ArtistPushData{Name: "Test", Formed: "1985-01-01T00:00:00.0000000Z"},
			wantPremiere: "1985-01-01T00:00:00.0000000Z",
		},
		{
			name:         "unparseable date omitted",
			data:         connection.ArtistPushData{Name: "Test", Born: "not a date"},
			wantPremiere: "",
		},
		{
			name:         "born takes precedence over formed",
			data:         connection.ArtistPushData{Name: "Test", Born: "1946", Formed: "1985"},
			wantPremiere: "1946-01-01",
		},
		{
			name:    "died year-only",
			data:    connection.ArtistPushData{Name: "Test", Died: "2016"},
			wantEnd: "2016-01-01",
		},
		{
			name:    "disbanded year-only",
			data:    connection.ArtistPushData{Name: "Test", Disbanded: "2003"},
			wantEnd: "2003-01-01",
		},
		{
			name:    "died takes precedence over disbanded",
			data:    connection.ArtistPushData{Name: "Test", Died: "2016", Disbanded: "2003"},
			wantEnd: "2016-01-01",
		},
		{
			name:         "named month with location",
			data:         connection.ArtistPushData{Name: "Test", Born: "October 14, 1946 in Abingdon, England"},
			wantPremiere: "1946-10-14",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotBody itemUpdateBody
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
					t.Fatalf("decoding body: %v", err)
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer srv.Close()

			c := NewWithHTTPClient(srv.URL, "key", "", srv.Client(), testLogger())
			if err := c.PushMetadata(context.Background(), "emby-001", tt.data); err != nil {
				t.Fatalf("PushMetadata failed: %v", err)
			}
			if gotBody.PremiereDate != tt.wantPremiere {
				t.Errorf("PremiereDate = %q, want %q", gotBody.PremiereDate, tt.wantPremiere)
			}
			if gotBody.EndDate != tt.wantEnd {
				t.Errorf("EndDate = %q, want %q", gotBody.EndDate, tt.wantEnd)
			}
		})
	}
}
