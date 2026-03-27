package emby

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
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
		if fields != "Path,ProviderIds,ImageTags,BackdropImageTags,Overview,Genres,Tags,SortName,PremiereDate,EndDate" {
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

func TestCheckImageFetchersEnabled_WithFetchers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{
				"Name":"Music","CollectionType":"music","ItemId":"lib-001",
				"LibraryOptions":{
					"SaveLocalMetadata":false,"MetadataSavers":[],
					"TypeOptions":[
						{"Type":"MusicArtist","ImageFetchers":["TheAudioDb","FanArt"],"MetadataFetchers":["TheAudioDb"]},
						{"Type":"MusicAlbum","ImageFetchers":["TheAudioDb"],"MetadataFetchers":[]}
					]
				}
			},
			{
				"Name":"Movies","CollectionType":"movies","ItemId":"lib-002",
				"LibraryOptions":{
					"SaveLocalMetadata":false,"MetadataSavers":[],
					"TypeOptions":[
						{"Type":"Movie","ImageFetchers":["TheMovieDb"],"MetadataFetchers":[]}
					]
				}
			}
		]`))
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", "", srv.Client(), testLogger())
	statuses, err := c.CheckImageFetchersEnabled(context.Background())
	if err != nil {
		t.Fatalf("CheckImageFetchersEnabled failed: %v", err)
	}
	if len(statuses) != 1 {
		t.Fatalf("got %d statuses, want 1 (only MusicArtist in music library)", len(statuses))
	}
	s := statuses[0]
	if s.LibraryName != "Music" {
		t.Errorf("LibraryName = %q, want Music", s.LibraryName)
	}
	if s.LibraryID != "lib-001" {
		t.Errorf("LibraryID = %q, want lib-001", s.LibraryID)
	}
	if len(s.FetcherNames) != 2 || s.FetcherNames[0] != "TheAudioDb" || s.FetcherNames[1] != "FanArt" {
		t.Errorf("FetcherNames = %v, want [TheAudioDb FanArt]", s.FetcherNames)
	}
	if s.RiskLevel != "warn" {
		t.Errorf("RiskLevel = %q, want warn", s.RiskLevel)
	}
}

func TestCheckImageFetchersEnabled_NoFetchers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{
				"Name":"Music","CollectionType":"music","ItemId":"lib-001",
				"LibraryOptions":{
					"SaveLocalMetadata":false,"MetadataSavers":[],
					"TypeOptions":[
						{"Type":"MusicArtist","ImageFetchers":[],"MetadataFetchers":["TheAudioDb"]}
					]
				}
			}
		]`))
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", "", srv.Client(), testLogger())
	statuses, err := c.CheckImageFetchersEnabled(context.Background())
	if err != nil {
		t.Fatalf("CheckImageFetchersEnabled failed: %v", err)
	}
	if len(statuses) != 0 {
		t.Errorf("got %d statuses, want 0 (no image fetchers enabled)", len(statuses))
	}
}

func TestCheckImageFetchersEnabled_NonMusicIgnored(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{
				"Name":"Movies","CollectionType":"movies","ItemId":"lib-002",
				"LibraryOptions":{
					"SaveLocalMetadata":false,"MetadataSavers":[],
					"TypeOptions":[
						{"Type":"Movie","ImageFetchers":["TheMovieDb"],"MetadataFetchers":[]}
					]
				}
			}
		]`))
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", "", srv.Client(), testLogger())
	statuses, err := c.CheckImageFetchersEnabled(context.Background())
	if err != nil {
		t.Fatalf("CheckImageFetchersEnabled failed: %v", err)
	}
	if len(statuses) != 0 {
		t.Errorf("got %d statuses, want 0 (non-music libraries ignored)", len(statuses))
	}
}

func TestCheckImageFetchersEnabled_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", "", srv.Client(), testLogger())
	statuses, err := c.CheckImageFetchersEnabled(context.Background())
	if err == nil {
		t.Fatal("expected error on server error, got nil")
	}
	if statuses != nil {
		t.Errorf("expected nil statuses on error, got %v", statuses)
	}
}

func TestPushMetadata(t *testing.T) {
	bodyCh := make(chan itemUpdateBody, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// PushMetadata now also calls refreshItem (POST /Items/{id}/Refresh).
		if strings.HasSuffix(r.URL.Path, "/Refresh") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.URL.Path != "/Items/emby-artist-1" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("content-type = %s, want application/json", r.Header.Get("Content-Type"))
		}
		var b itemUpdateBody
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			t.Errorf("decoding body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		bodyCh <- b
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
	gotBody := <-bodyCh
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

// TestPushMetadata_TagItems verifies that styles and moods are sent as TagItems
// (Emby's {Name} object format) rather than flat Tags strings.
func TestPushMetadata_TagItems(t *testing.T) {
	bodyCh := make(chan itemUpdateBody, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/Refresh") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var b itemUpdateBody
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			t.Errorf("decoding body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		bodyCh <- b
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", "", srv.Client(), testLogger())
	data := connection.ArtistPushData{
		Name:   "Test",
		Styles: []string{"Shoegaze", "Dream Pop"},
		Moods:  []string{"Melancholy"},
	}
	if err := c.PushMetadata(context.Background(), "emby-001", data); err != nil {
		t.Fatalf("PushMetadata failed: %v", err)
	}
	gotBody := <-bodyCh
	if len(gotBody.TagItems) != 3 {
		t.Fatalf("got %d TagItems, want 3", len(gotBody.TagItems))
	}
	if gotBody.TagItems[0].Name != "Shoegaze" {
		t.Errorf("TagItems[0].Name = %q, want Shoegaze", gotBody.TagItems[0].Name)
	}
	if gotBody.TagItems[2].Name != "Melancholy" {
		t.Errorf("TagItems[2].Name = %q, want Melancholy", gotBody.TagItems[2].Name)
	}
}

// TestPushMetadata_RefreshCalled verifies that PushMetadata triggers a metadata
// refresh call after a successful push.
func TestPushMetadata_RefreshCalled(t *testing.T) {
	var refreshCalled atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/Refresh") {
			refreshCalled.Store(true)
			if r.Method != http.MethodPost {
				t.Errorf("refresh method = %s, want POST", r.Method)
			}
			if !strings.Contains(r.URL.RawQuery, "ReplaceAllMetadata=false") {
				t.Errorf("refresh query missing ReplaceAllMetadata=false: %s", r.URL.RawQuery)
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", "", srv.Client(), testLogger())
	if err := c.PushMetadata(context.Background(), "emby-001", connection.ArtistPushData{Name: "Test"}); err != nil {
		t.Fatalf("PushMetadata failed: %v", err)
	}
	if !refreshCalled.Load() {
		t.Error("refreshItem was not called after successful push")
	}
}

// TestPushMetadata_SpecialCharacterID verifies that platformArtistID values
// containing path-breaking characters are correctly escaped in the URL.
func TestPushMetadata_SpecialCharacterID(t *testing.T) {
	pathCh := make(chan string, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case pathCh <- r.URL.EscapedPath():
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", "", srv.Client(), testLogger())
	err := c.PushMetadata(context.Background(), "id/with spaces", connection.ArtistPushData{Name: "Test"})
	if err != nil {
		t.Fatalf("PushMetadata failed: %v", err)
	}
	got := <-pathCh
	if !strings.Contains(got, "id%2Fwith%20spaces") {
		t.Errorf("path = %q, want escaped id containing 'id%%2Fwith%%20spaces'", got)
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
	var reqCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqCount++
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
	if reqCount != 1 {
		t.Fatalf("got %d requests, want exactly 1 (no /Users lookup)", reqCount)
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
		t.Errorf("error = %v, want message about missing user ID", err)
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
			name:         "unparsable date omitted",
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
			bodyCh := make(chan itemUpdateBody, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/Refresh") {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				var b itemUpdateBody
				if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
					t.Errorf("decoding body: %v", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				bodyCh <- b
				w.WriteHeader(http.StatusNoContent)
			}))
			defer srv.Close()

			c := NewWithHTTPClient(srv.URL, "key", "", srv.Client(), testLogger())
			if err := c.PushMetadata(context.Background(), "emby-001", tt.data); err != nil {
				t.Fatalf("PushMetadata failed: %v", err)
			}
			gotBody := <-bodyCh
			if gotBody.PremiereDate != tt.wantPremiere {
				t.Errorf("PremiereDate = %q, want %q", gotBody.PremiereDate, tt.wantPremiere)
			}
			if gotBody.EndDate != tt.wantEnd {
				t.Errorf("EndDate = %q, want %q", gotBody.EndDate, tt.wantEnd)
			}
		})
	}
}

func TestUploadImage_BodyIsBase64(t *testing.T) {
	jpegData := createTestJPEG(t)
	bodyCh := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/Items/emby-001/Images/Primary" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("X-Emby-Token") != "test-key" {
			t.Errorf("missing or wrong auth header: %s", r.Header.Get("X-Emby-Token"))
		}
		if ct := r.Header.Get("Content-Type"); ct != "image/jpeg" {
			t.Errorf("Content-Type = %q, want image/jpeg", ct)
		}
		body, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			t.Errorf("reading request body: %v", readErr)
		}
		bodyCh <- body
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "test-key", "", srv.Client(), testLogger())
	if err := c.UploadImage(context.Background(), "emby-001", "thumb", jpegData, "image/jpeg"); err != nil {
		t.Fatalf("UploadImage failed: %v", err)
	}

	gotBody := <-bodyCh
	decoded, err := base64.StdEncoding.DecodeString(string(gotBody))
	if err != nil {
		t.Fatalf("body is not valid base64: %v", err)
	}
	if !bytes.Equal(decoded, jpegData) {
		t.Errorf("decoded body differs from input: got %d bytes, want %d", len(decoded), len(jpegData))
	}
}

func TestUploadImage_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal"}`))
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "test-key", "", srv.Client(), testLogger())
	if err := c.UploadImage(context.Background(), "emby-001", "thumb", []byte{1, 2, 3}, "image/jpeg"); err == nil {
		t.Fatal("expected error for server error response")
	}
}

func TestUploadImage_UnsupportedType(t *testing.T) {
	c := New("http://localhost", "key", "", testLogger())
	if err := c.UploadImage(context.Background(), "emby-001", "clearart", []byte{1}, "image/jpeg"); err == nil {
		t.Fatal("expected error for unsupported image type")
	}
}

func TestGetArtistBackdrop_Success(t *testing.T) {
	jpegData := createTestJPEG(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/Items/emby-001/Images/Backdrop/2" {
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
	data, contentType, err := c.GetArtistBackdrop(context.Background(), "emby-001", 2)
	if err != nil {
		t.Fatalf("GetArtistBackdrop failed: %v", err)
	}
	if contentType != "image/jpeg" {
		t.Errorf("content-type = %q, want image/jpeg", contentType)
	}
	if !bytes.Equal(data, jpegData) {
		t.Errorf("image data mismatch: got %d bytes, want %d", len(data), len(jpegData))
	}
}

func TestGetArtistBackdrop_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/Items/emby-001/Images/Backdrop/0" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("not found"))
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "test-key", "", srv.Client(), testLogger())
	_, _, err := c.GetArtistBackdrop(context.Background(), "emby-001", 0)
	if err == nil {
		t.Fatal("expected error for 404 response")
	}
}

// --- AuthenticateByName tests ---

func TestAuthenticateByName_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/Users/AuthenticateByName" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method: %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(AuthResult{
			AccessToken: "test-token-abc123",
			User: AuthUser{
				ID:   "user-001",
				Name: "admin",
				Policy: UserPolicy{
					IsAdministrator: true,
				},
			},
		})
	}))
	defer srv.Close()

	result, err := AuthenticateByName(context.Background(), srv.URL, "admin", "pass123", testLogger())
	if err != nil {
		t.Fatalf("AuthenticateByName failed: %v", err)
	}
	if result.AccessToken != "test-token-abc123" {
		t.Errorf("AccessToken = %q, want %q", result.AccessToken, "test-token-abc123")
	}
	if result.User.ID != "user-001" {
		t.Errorf("User.ID = %q, want %q", result.User.ID, "user-001")
	}
	if result.User.Name != "admin" {
		t.Errorf("User.Name = %q, want %q", result.User.Name, "admin")
	}
	if !result.User.Policy.IsAdministrator {
		t.Error("User.Policy.IsAdministrator = false, want true")
	}
}

func TestAuthenticateByName_InvalidCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := AuthenticateByName(context.Background(), srv.URL, "admin", "wrong", testLogger())
	if err == nil {
		t.Fatal("expected error for invalid credentials")
	}
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("error = %v, want ErrInvalidCredentials", err)
	}
}

func TestAuthenticateByName_ServerUnreachable(t *testing.T) {
	// Port 1 is almost certainly not listening, so the connection should fail.
	_, err := AuthenticateByName(context.Background(), "http://127.0.0.1:1", "admin", "pass", testLogger())
	if err == nil {
		t.Fatal("expected error for unreachable server")
	}
}

func TestAuthenticateByName_AuthorizationHeader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(AuthResult{
			AccessToken: "tok",
			User:        AuthUser{ID: "u1", Name: "admin"},
		})
	}))
	defer srv.Close()

	_, err := AuthenticateByName(context.Background(), srv.URL, "admin", "pass", testLogger())
	if err != nil {
		t.Fatalf("AuthenticateByName failed: %v", err)
	}
	if !strings.HasPrefix(gotAuth, `Emby UserId=""`) {
		t.Errorf("Authorization header does not start with Emby UserId: %q", gotAuth)
	}
	if !strings.Contains(gotAuth, `Client="Stillwater"`) {
		t.Errorf("Authorization header missing Client=Stillwater: %q", gotAuth)
	}
	if !strings.Contains(gotAuth, `Device="Server"`) {
		t.Errorf("Authorization header missing Device=Server: %q", gotAuth)
	}
	if !strings.Contains(gotAuth, `DeviceId="`) {
		t.Errorf("Authorization header missing DeviceId: %q", gotAuth)
	}
	if !strings.Contains(gotAuth, `Version="`) {
		t.Errorf("Authorization header missing Version: %q", gotAuth)
	}
}

func TestAuthenticateByName_RequestBody(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		gotBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("reading request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(AuthResult{
			AccessToken: "tok",
			User:        AuthUser{ID: "u1", Name: "admin"},
		})
	}))
	defer srv.Close()

	_, err := AuthenticateByName(context.Background(), srv.URL, "testuser", "testpass", testLogger())
	if err != nil {
		t.Fatalf("AuthenticateByName failed: %v", err)
	}

	var parsed map[string]string
	if err := json.Unmarshal(gotBody, &parsed); err != nil {
		t.Fatalf("parsing request body: %v", err)
	}
	if parsed["Username"] != "testuser" {
		t.Errorf("Username = %q, want %q", parsed["Username"], "testuser")
	}
	if parsed["Pw"] != "testpass" {
		t.Errorf("Pw = %q, want %q", parsed["Pw"], "testpass")
	}
}
