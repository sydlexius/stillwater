package jellyfin

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
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, `MediaBrowser Token="`) {
			t.Errorf("unexpected auth header: %s", auth)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ServerName":"Test Jellyfin","Version":"10.8.0","Id":"jf-001"}`))
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

func TestAuthHeader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ServerName":"Test","Version":"10.8.0","Id":"jf-001"}`))
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "my-api-key", srv.Client(), testLogger())
	_ = c.TestConnection(context.Background())

	expected := `MediaBrowser Token="my-api-key"`
	if gotAuth != expected {
		t.Errorf("auth = %q, want %q", gotAuth, expected)
	}
}

func TestGetMusicLibraries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"Name":"Music","CollectionType":"music","ItemId":"lib-001"},
			{"Name":"Movies","CollectionType":"movies","ItemId":"lib-002"}
		]`))
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", srv.Client(), testLogger())
	libs, err := c.GetMusicLibraries(context.Background())
	if err != nil {
		t.Fatalf("GetMusicLibraries failed: %v", err)
	}
	if len(libs) != 1 {
		t.Fatalf("got %d music libraries, want 1", len(libs))
	}
}

func TestGetArtists(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/Artists/AlbumArtists" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		fields := r.URL.Query().Get("Fields")
		if fields != "Path,ProviderIds,ImageTags,Overview,Genres,Tags,SortName,PremiereDate,EndDate" {
			t.Errorf("Fields = %q, want expanded field list", fields)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"Items":[{
				"Name":"Radiohead",
				"SortName":"Radiohead",
				"Id":"jf-001",
				"Path":"/music/Radiohead",
				"Overview":"English rock band formed in 1985.",
				"Genres":["Rock","Alternative"],
				"Tags":["Experimental"],
				"PremiereDate":"1985-01-01T00:00:00.0000000Z",
				"EndDate":"",
				"ProviderIds":{"MusicBrainzArtist":"mbid-001"},
				"ImageTags":{"Primary":"tag1"}
			}],
			"TotalRecordCount":1
		}`))
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", srv.Client(), testLogger())
	resp, err := c.GetArtists(context.Background(), "lib-001", 0, 50)
	if err != nil {
		t.Fatalf("GetArtists failed: %v", err)
	}
	if resp.TotalRecordCount != 1 {
		t.Errorf("TotalRecordCount = %d, want 1", resp.TotalRecordCount)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("got %d items, want 1", len(resp.Items))
	}

	rh := resp.Items[0]
	if rh.Name != "Radiohead" {
		t.Errorf("Name = %q, want Radiohead", rh.Name)
	}
	if rh.SortName != "Radiohead" {
		t.Errorf("SortName = %q, want Radiohead", rh.SortName)
	}
	if rh.Overview != "English rock band formed in 1985." {
		t.Errorf("Overview = %q, want biography text", rh.Overview)
	}
	if len(rh.Genres) != 2 || rh.Genres[0] != "Rock" {
		t.Errorf("Genres = %v, want [Rock Alternative]", rh.Genres)
	}
	if len(rh.Tags) != 1 || rh.Tags[0] != "Experimental" {
		t.Errorf("Tags = %v, want [Experimental]", rh.Tags)
	}
	if rh.PremiereDate != "1985-01-01T00:00:00.0000000Z" {
		t.Errorf("PremiereDate = %q, want 1985 date", rh.PremiereDate)
	}
}

func TestTriggerLibraryScan(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/Library/Refresh" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", srv.Client(), testLogger())
	if err := c.TriggerLibraryScan(context.Background()); err != nil {
		t.Fatalf("TriggerLibraryScan failed: %v", err)
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

func TestGetArtistDetail_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/Items/jf-001" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, `MediaBrowser Token="`) {
			t.Errorf("unexpected auth header: %s", auth)
		}
		fields := r.URL.Query().Get("Fields")
		if fields == "" {
			t.Errorf("Fields query param missing")
		}
		http.ServeFile(w, r, "testdata/artist_detail.json")
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "test-key", srv.Client(), testLogger())
	state, err := c.GetArtistDetail(context.Background(), "jf-001")
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("not found"))
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "test-key", srv.Client(), testLogger())
	_, err := c.GetArtistDetail(context.Background(), "jf-999")
	if err == nil {
		t.Fatal("expected error for 404 response")
	}
}

func TestPushMetadata(t *testing.T) {
	var gotBody itemUpdateBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/Items/jf-artist-1" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("content-type = %s, want application/json", r.Header.Get("Content-Type"))
		}
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, `MediaBrowser Token="`) {
			t.Errorf("unexpected auth header: %s", auth)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decoding body: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", srv.Client(), testLogger())
	data := connection.ArtistPushData{
		Name:      "Bjork",
		SortName:  "Bjork",
		Biography: "Icelandic singer",
		Genres:    []string{"Electronic", "Art Pop"},
	}
	if err := c.PushMetadata(context.Background(), "jf-artist-1", data); err != nil {
		t.Fatalf("PushMetadata failed: %v", err)
	}
	if gotBody.Name != "Bjork" {
		t.Errorf("Name = %q, want Bjork", gotBody.Name)
	}
	if gotBody.Overview != "Icelandic singer" {
		t.Errorf("Overview = %q, want Icelandic singer", gotBody.Overview)
	}
}

func TestPushMetadata_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", srv.Client(), testLogger())
	err := c.PushMetadata(context.Background(), "jf-001", connection.ArtistPushData{Name: "Test"})
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
		if r.URL.Path != "/Items/jf-001/Images/Primary" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, `MediaBrowser Token="`) {
			t.Errorf("unexpected auth header: %s", auth)
		}
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(jpegData)
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "test-key", srv.Client(), testLogger())
	data, contentType, err := c.GetArtistImage(context.Background(), "jf-001", "thumb")
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

	c := NewWithHTTPClient(srv.URL, "test-key", srv.Client(), testLogger())
	_, _, err := c.GetArtistImage(context.Background(), "jf-001", "thumb")
	if err == nil {
		t.Fatal("expected error for 404 response")
	}
}

func TestGetArtistImage_UnsupportedType(t *testing.T) {
	c := New("http://localhost", "key", testLogger())
	_, _, err := c.GetArtistImage(context.Background(), "jf-001", "clearart")
	if err == nil {
		t.Fatal("expected error for unsupported image type")
	}
}

func TestGetRaw_OversizedImage(t *testing.T) {
	const maxImageSize = 25 << 20
	oversized := make([]byte, maxImageSize+1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(oversized)
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", srv.Client(), testLogger())
	_, _, err := c.GetArtistImage(context.Background(), "jf-001", "thumb")
	if err == nil {
		t.Fatal("expected error for oversized image")
	}
	if !strings.Contains(err.Error(), "exceeds 25 MB") {
		t.Errorf("error = %q, want message about exceeding 25 MB limit", err)
	}
}

func TestGetRaw_ErrorBodyLimited(t *testing.T) {
	largeBody := strings.Repeat("x", 4096)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(largeBody))
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", srv.Client(), testLogger())
	_, _, err := c.GetArtistImage(context.Background(), "jf-001", "thumb")
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
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

	c := NewWithHTTPClient(srv.URL, "key", srv.Client(), testLogger())
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

	c := NewWithHTTPClient(srv.URL, "key", srv.Client(), testLogger())
	err := c.TriggerLibraryScan(context.Background()) // uses post()
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
	errMsg := err.Error()
	if len(errMsg) > 1100 {
		t.Errorf("error message length = %d, want bounded (body should be limited to 1024 bytes)", len(errMsg))
	}
}

func TestDeleteImage_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/Items/jf-001/Images/Primary" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s, want DELETE", r.Method)
		}
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, `MediaBrowser Token="`) {
			t.Errorf("unexpected auth header: %s", auth)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "test-key", srv.Client(), testLogger())
	if err := c.DeleteImage(context.Background(), "jf-001", "thumb"); err != nil {
		t.Fatalf("DeleteImage failed: %v", err)
	}
}

func TestDeleteImage_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal"}`))
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "test-key", srv.Client(), testLogger())
	if err := c.DeleteImage(context.Background(), "jf-001", "thumb"); err == nil {
		t.Fatal("expected error for server error response")
	}
}

func TestDeleteImage_UnsupportedType(t *testing.T) {
	c := New("http://localhost", "key", testLogger())
	if err := c.DeleteImage(context.Background(), "jf-001", "clearart"); err == nil {
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

			c := NewWithHTTPClient(srv.URL, "key", srv.Client(), testLogger())
			if err := c.PushMetadata(context.Background(), "jf-001", tt.data); err != nil {
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
