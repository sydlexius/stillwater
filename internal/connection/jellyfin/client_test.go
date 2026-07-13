package jellyfin

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
	"sync"
	"testing"

	"github.com/sydlexius/stillwater/internal/connection"
)

// itemUpdateBody is used by tests to decode the Jellyfin POST /Items/{id}
// body into a typed struct for assertion. Production code marshals a
// map[string]any directly (see push.go PushMetadata).
type itemUpdateBody struct {
	Name           string            `json:"Name"`
	ForcedSortName string            `json:"ForcedSortName,omitempty"`
	Overview       string            `json:"Overview,omitempty"`
	Genres         []string          `json:"Genres,omitempty"`
	Tags           []string          `json:"Tags,omitempty"`
	ProviderIds    map[string]string `json:"ProviderIds,omitempty"`
	PremiereDate   string            `json:"PremiereDate,omitempty"`
	EndDate        string            `json:"EndDate,omitempty"`
}

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

	c := NewWithHTTPClient(srv.URL, "test-key", "", srv.Client(), testLogger())
	if err := c.TestConnection(context.Background()); err != nil {
		t.Fatalf("TestConnection failed: %v", err)
	}
}

func TestTestConnection_Unauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "bad-key", "", srv.Client(), testLogger())
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

	c := NewWithHTTPClient(srv.URL, "my-api-key", "", srv.Client(), testLogger())
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

	c := NewWithHTTPClient(srv.URL, "key", "", srv.Client(), testLogger())
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
		if fields != "Path,ProviderIds,ImageTags,BackdropImageTags,Overview,Genres,Tags,SortName,PremiereDate,EndDate" {
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

	c := NewWithHTTPClient(srv.URL, "key", "", srv.Client(), testLogger())
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

	c := NewWithHTTPClient(srv.URL, "key", "", srv.Client(), testLogger())
	if err := c.TriggerLibraryScan(context.Background()); err != nil {
		t.Fatalf("TriggerLibraryScan failed: %v", err)
	}
}

// TestTriggerArtistRefresh verifies Jellyfin's per-artist refresh forces a full
// NFO re-import (#2336): FullRefresh + ReplaceAllMetadata=true are what make the
// server re-read the on-disk NFO (so NFO-only fields like Disambiguation and
// YearsActive land), while ReplaceAllImages=false leaves artwork untouched. The
// artist ID must also be path-escaped into the URL segment.
func TestTriggerArtistRefresh(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/Items/jf-001/Refresh" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		q := r.URL.Query()
		if got := q.Get("MetadataRefreshMode"); got != "FullRefresh" {
			t.Errorf("MetadataRefreshMode = %q, want FullRefresh (query=%q)", got, r.URL.RawQuery)
		}
		if got := q.Get("ReplaceAllMetadata"); got != "true" {
			t.Errorf("ReplaceAllMetadata = %q, want true (query=%q)", got, r.URL.RawQuery)
		}
		if got := q.Get("ReplaceAllImages"); got != "false" {
			t.Errorf("ReplaceAllImages = %q, want false (query=%q)", got, r.URL.RawQuery)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", "", srv.Client(), testLogger())
	if err := c.TriggerArtistRefresh(context.Background(), "jf-001"); err != nil {
		t.Fatalf("TriggerArtistRefresh failed: %v", err)
	}
}

// TestTriggerArtistRefresh_EmptyArtistID covers the defense-in-depth guard:
// an empty/whitespace artistID must be rejected before any request is sent,
// mirroring the sibling UpdateArtistPath guard.
func TestTriggerArtistRefresh_EmptyArtistID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", "", srv.Client(), testLogger())
	if err := c.TriggerArtistRefresh(context.Background(), "   "); err == nil {
		t.Fatal("expected an error for an empty artistID, got nil")
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
	// A peer-side error must propagate so the conflict detector can
	// populate ConnectionState.CheckErr and fail the gate closed. If this
	// method silently swallowed the error and returned (false, "", nil),
	// a transient Jellyfin outage would reopen NFO writes.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", "", srv.Client(), testLogger())
	enabled, _, err := c.CheckNFOWriterEnabled(context.Background())
	if err == nil {
		t.Fatal("expected error from server 500, got nil")
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
					"EnableInternetProviders":true,
					"TypeOptions":[
						{"Type":"MusicArtist","ImageFetchers":["TheAudioDb","FanArt"],"MetadataFetchers":["TheAudioDb"]}
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
	if s.RiskLevel != "critical" {
		t.Errorf("RiskLevel = %q, want critical", s.RiskLevel)
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
					"EnableInternetProviders":true,
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

func TestCheckImageFetchersEnabled_InternetProvidersDisabled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{
				"Name":"Music","CollectionType":"music","ItemId":"lib-001",
				"LibraryOptions":{
					"SaveLocalMetadata":false,"MetadataSavers":[],
					"EnableInternetProviders":false,
					"TypeOptions":[
						{"Type":"MusicArtist","ImageFetchers":["TheAudioDb"],"MetadataFetchers":[]}
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
		t.Errorf("got %d statuses, want 0 (internet providers disabled)", len(statuses))
	}
}

func TestGetArtistDetail_Success(t *testing.T) {
	var reqCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqCount++
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, `MediaBrowser Token="`) {
			t.Errorf("unexpected auth header: %s", auth)
		}
		if r.URL.Path != "/Users/user-001/Items/jf-001" {
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
	state, err := c.GetArtistDetail(context.Background(), "jf-001")
	if err != nil {
		t.Fatalf("GetArtistDetail failed: %v", err)
	}
	if reqCount != 1 {
		t.Errorf("got %d requests, want 1 (no /Users lookup)", reqCount)
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
	_, err := c.GetArtistDetail(context.Background(), "jf-999")
	if err == nil {
		t.Fatal("expected error for 404 response")
	}
	if reqCount != 1 {
		t.Errorf("got %d requests, want 1 (no /Users lookup)", reqCount)
	}
}

func TestGetArtistDetail_EmptyUserID(t *testing.T) {
	c := NewWithHTTPClient("http://localhost", "test-key", "", &http.Client{}, testLogger())
	_, err := c.GetArtistDetail(context.Background(), "jf-001")
	if err == nil {
		t.Fatal("expected error when userID is empty")
	}
	if !strings.Contains(err.Error(), "no user ID configured") {
		t.Errorf("error = %q, want message about no user ID configured", err.Error())
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
		_, _ = w.Write([]byte(`[{"Id":"user-001","Name":"Admin"},{"Id":"user-002","Name":"Guest"}]`))
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "test-key", "", srv.Client(), testLogger())
	uid, err := c.GetFirstUserID(context.Background())
	if err != nil {
		t.Fatalf("GetFirstUserID failed: %v", err)
	}
	if uid != "user-001" {
		t.Errorf("uid = %q, want user-001", uid)
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
		t.Fatal("expected error when no users returned")
	}
}

func TestPushMetadata(t *testing.T) {
	bodyCh := make(chan itemUpdateBody, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// PushMetadata now first fetches the item (GET /Items?Ids=...) then
		// POSTs the merged body back.
		if r.Method == http.MethodGet && r.URL.Path == "/Items" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Items":[{"Name":"Bjork","Id":"jf-artist-1"}]}`))
			return
		}
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
		Name:      "Bjork",
		SortName:  "Bjork",
		Biography: "Icelandic singer",
		Genres:    []string{"Electronic", "Art Pop"},
	}
	if err := c.PushMetadata(context.Background(), "jf-artist-1", data); err != nil {
		t.Fatalf("PushMetadata failed: %v", err)
	}
	gotBody := <-bodyCh
	if gotBody.Name != "Bjork" {
		t.Errorf("Name = %q, want Bjork", gotBody.Name)
	}
	if gotBody.Overview != "Icelandic singer" {
		t.Errorf("Overview = %q, want Icelandic singer", gotBody.Overview)
	}
}

// TestUpdateArtistLocks verifies that the lock-sync path sets LockData on the
// merged item payload, leaves LockedFields untouched (Jellyfin doesn't honor
// per-field locks at the item level), strips read-only fields, and POSTs the
// body back. Mirrors TestUpdateArtistLocks on the Emby client.
func TestUpdateArtistLocks(t *testing.T) {
	bodyCh := make(chan map[string]any, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/Items" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Items":[{"Id":"jf-a1","Name":"Bjork","LockData":false,"LockedFields":["Tags"],"ImageTags":{"Primary":"abc"}}]}`))
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/Items/jf-a1" {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decoding body: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			bodyCh <- body
			w.WriteHeader(http.StatusNoContent)
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", "", srv.Client(), testLogger())
	if err := c.UpdateArtistLocks(context.Background(), "jf-a1", true, []string{"name", "biography"}); err != nil {
		t.Fatalf("UpdateArtistLocks: %v", err)
	}

	got := <-bodyCh
	if lock, _ := got["LockData"].(bool); !lock {
		t.Errorf("LockData = %v, want true", got["LockData"])
	}
	// Per-field LockedFields must be preserved from the fetched state: we
	// intentionally do not overwrite the server-side list.
	lf, _ := got["LockedFields"].([]any)
	if len(lf) != 1 {
		t.Fatalf("LockedFields = %v, want preserved single [\"Tags\"]", got["LockedFields"])
	}
	if s, _ := lf[0].(string); s != "Tags" {
		t.Errorf("LockedFields[0] = %v, want \"Tags\"", lf[0])
	}
	// Read-only fields must be stripped.
	if _, present := got["ImageTags"]; present {
		t.Errorf("read-only ImageTags should have been stripped, got %v", got["ImageTags"])
	}
}

// TestUpdateArtistLocks_EmptyItemID verifies that UpdateArtistLocks rejects
// an empty/whitespace platformArtistID at the boundary. Without the guard,
// fetchItem would issue /Items?Ids= (no value), which Jellyfin accepts and
// answers with the first item in the library - silently corrupting every
// downstream write.
func TestUpdateArtistLocks_EmptyItemID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", "", srv.Client(), testLogger())
	cases := []string{"", "   ", "\t\n "}
	for _, id := range cases {
		if err := c.UpdateArtistLocks(context.Background(), id, true, nil); err == nil {
			t.Errorf("UpdateArtistLocks(%q) = nil, want error", id)
		}
	}
}

// jellyfinItemFake is a STATEFUL stand-in for a Jellyfin item store, and it
// models the one behavior that matters for #2380: POST /Items/{id} applies every
// field of the submitted body EXCEPT Path, which it silently discards, and then
// answers 204.
//
// That is not a caricature. It is what a live Jellyfin 10.11.10 was measured
// doing, and Emby 4.9.5.0 does the same. Jellyfin has NO repath endpoint at all -
// an item's path is derived by the library scanner from the filesystem, so the
// server simply drops a path a client tries to set. The OpenAPI spec actively
// misleads about this: Path is documented on BaseItemDto as "Gets or sets the
// path" and is not marked readOnly.
//
// A fake that answers 204 and stores nothing is NOT a peer. The previous version
// of the test below used one, so it could only assert that the client SENT a Path
// field - and the entire bug passed it. Modeling the discard is what lets a test
// tell "we asked" apart from "it happened".
type jellyfinItemFake struct {
	mu   sync.Mutex
	item map[string]any
	// posted captures the last body the client POSTed, so the marshaling
	// assertions (field preservation, read-only strip) still have something to
	// look at.
	posted map[string]any
}

func (f *jellyfinItemFake) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/Items":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"Items": []any{f.item}})
		case r.Method == http.MethodPost && r.URL.Path == "/Items/jf-a1":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decoding body: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			f.posted = body
			// Apply every field the client sent -- EXCEPT Path. This is the lie.
			for k, v := range body {
				if k == "Path" {
					continue
				}
				f.item[k] = v
			}
			w.WriteHeader(http.StatusNoContent) // and report success anyway
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}
}

// TestUpdateArtistPath_PeerSilentlyDiscardsPath is the rewritten #2380 guard, and
// the reason the old TestUpdateArtistPath_RoundTrip was worthless.
//
// The old test asked "did the client put Path in the body?" against a fake that
// stored nothing. Answer: yes -- and the path never changed on the real server, so
// the shipped code was broken while the test was green. Twice.
//
// This one asks the question that matters: AFTER a successful UpdateArtistPath,
// what does the peer actually say the path is? It must be the OLD one, and
// GetArtistPath must report that, because that read-back is the only mechanism
// that can catch a peer discarding the write.
func TestUpdateArtistPath_PeerSilentlyDiscardsPath(t *testing.T) {
	fake := &jellyfinItemFake{item: map[string]any{
		"Id": "jf-a1", "Name": "Bjork", "Path": "/old/Bjork",
		"Genres": []any{"Pop"}, "ImageTags": map[string]any{"Primary": "abc"},
	}}
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", "", srv.Client(), testLogger())

	// The write "succeeds". It returns nil. It changes nothing.
	if err := c.UpdateArtistPath(context.Background(), "jf-a1", "/new/Bjork"); err != nil {
		t.Fatalf("UpdateArtistPath: %v", err)
	}

	// THE ASSERTION THE OLD TEST COULD NOT MAKE. A nil return above proves only
	// that the request was accepted. Ask the peer what it stored.
	got, err := c.GetArtistPath(context.Background(), "jf-a1")
	if err != nil {
		t.Fatalf("GetArtistPath: %v", err)
	}
	if got != "/old/Bjork" {
		t.Fatalf("peer path = %q, want %q: this fake models Jellyfin DISCARDING the path, "+
			"so if the read-back sees the new path the fake stopped modeling the real server "+
			"and the test has gone vacuous again", got, "/old/Bjork")
	}
	// And that is exactly what the caller must be able to detect: sent != stored.
	if connection.SamePeerPath(got, "/new/Bjork") {
		t.Error("read-back compared EQUAL to the path we sent, so a peer that discards the " +
			"path would read as success -- this is the #2380 defect")
	}

	// The marshaling contract from #1222 still holds and is still worth asserting:
	// the client must send Path, preserve unrelated fields, and strip read-only ones.
	if fake.posted["Path"] != "/new/Bjork" {
		t.Errorf("Path sent = %v, want /new/Bjork", fake.posted["Path"])
	}
	if fake.posted["Name"] != "Bjork" {
		t.Errorf("Name preservation failed: %v", fake.posted["Name"])
	}
	if _, present := fake.posted["ImageTags"]; present {
		t.Errorf("read-only ImageTags should have been stripped, got %v", fake.posted["ImageTags"])
	}
}

// TestGetArtistPath_ReadsBackWhatThePeerStored is the positive control for the
// read-back: against a peer that DOES hold the path, GetArtistPath returns it. A
// detector that always reported "not honored" would pass the test above while
// being useless, so pin both directions.
func TestGetArtistPath_ReadsBackWhatThePeerStored(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/Items" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Items":[{"Id":"jf-a1","Name":"Bjork","Path":"/new/Bjork"}]}`))
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", "", srv.Client(), testLogger())
	got, err := c.GetArtistPath(context.Background(), "jf-a1")
	if err != nil {
		t.Fatalf("GetArtistPath: %v", err)
	}
	if got != "/new/Bjork" {
		t.Errorf("GetArtistPath = %q, want %q", got, "/new/Bjork")
	}
	if !connection.SamePeerPath(got, "/new/Bjork") {
		t.Error("a peer that DID store the path must verify as a match")
	}
}

// TestUpdateArtistPath_EmptyItemID mirrors the lock test's guard: an empty
// platformArtistID would build /Items?Ids= and silently target the wrong
// item. The fetch must reject before issuing the request.
func TestUpdateArtistPath_EmptyItemID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", "", srv.Client(), testLogger())
	if err := c.UpdateArtistPath(context.Background(), "", "/new"); err == nil {
		t.Fatal("expected error on empty item id")
	}
}

// TestUpdateArtistPath_EmptyNewPath rejects a blank target path before any
// fetch / POST. Pushing "" as Path would silently overwrite Jellyfin's record
// with an invalid value and orphan the artist on the peer.
func TestUpdateArtistPath_EmptyNewPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", "", srv.Client(), testLogger())
	if err := c.UpdateArtistPath(context.Background(), "jf-pid", "   "); err == nil {
		t.Fatal("expected error on whitespace-only newPath")
	}
}

// TestPushMetadata_ClearsFields verifies that empty values in the push data
// overwrite existing Jellyfin values, allowing field clears to propagate.
func TestPushMetadata_ClearsFields(t *testing.T) {
	bodyCh := make(chan map[string]any, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/Items" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Items":[{
				"Name":"Old Name","Id":"jf-clear-1",
				"Overview":"Old bio","ForcedSortName":"Old Sort",
				"Genres":["Rock"],"Tags":["Grunge"],
				"PremiereDate":"1985-01-01","EndDate":"2003-01-01"
			}]}`))
			return
		}
		var m map[string]any
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
			t.Errorf("decoding body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		bodyCh <- m
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", "", srv.Client(), testLogger())
	// Push with empty Biography, SortName, and nil Genres/Styles/Moods.
	data := connection.ArtistPushData{Name: "New Name"}
	if err := c.PushMetadata(context.Background(), "jf-clear-1", data); err != nil {
		t.Fatalf("PushMetadata failed: %v", err)
	}
	got := <-bodyCh
	if overview, _ := got["Overview"].(string); overview != "" {
		t.Errorf("Overview = %q, want empty (field should be cleared)", overview)
	}
	if sortName, _ := got["ForcedSortName"].(string); sortName != "" {
		t.Errorf("ForcedSortName = %q, want empty (field should be cleared)", sortName)
	}
	// Genres and Tags must be present as explicit clears (empty array), not omitted.
	genres, ok := got["Genres"]
	if !ok {
		t.Fatal("Genres key missing from POST body")
	}
	genreVals, ok := genres.([]any)
	if !ok {
		t.Fatalf("Genres = %T, want []any", genres)
	}
	if len(genreVals) != 0 {
		t.Errorf("Genres = %v, want empty array", genres)
	}

	tags, ok := got["Tags"]
	if !ok {
		t.Fatal("Tags key missing from POST body")
	}
	tagVals, ok := tags.([]any)
	if !ok {
		t.Fatalf("Tags = %T, want []any", tags)
	}
	if len(tagVals) != 0 {
		t.Errorf("Tags = %v, want empty array", tags)
	}

	// PremiereDate and EndDate must be cleared when all date sources are empty.
	if premiere, _ := got["PremiereDate"].(string); premiere != "" {
		t.Errorf("PremiereDate = %q, want empty (date should be cleared)", premiere)
	}
	if endDate, _ := got["EndDate"].(string); endDate != "" {
		t.Errorf("EndDate = %q, want empty (date should be cleared)", endDate)
	}
}

func TestPushMetadata_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Serve a valid item for the GET fetch, return 500 only for the POST.
		if r.Method == http.MethodGet && r.URL.Path == "/Items" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Items":[{"Name":"Test","Id":"jf-001"}]}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", "", srv.Client(), testLogger())
	err := c.PushMetadata(context.Background(), "jf-001", connection.ArtistPushData{Name: "Test"})
	if err == nil {
		t.Fatal("expected error for server error")
	}
	if !strings.Contains(err.Error(), "push failed with status 500") {
		t.Errorf("error = %q, want message about push failed with status 500", err.Error())
	}
}

// TestPushMetadata_SpecialCharacterID verifies that platformArtistID values
// containing path-breaking characters are correctly escaped in the URL.
func TestPushMetadata_SpecialCharacterID(t *testing.T) {
	pathCh := make(chan string, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/Items" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Items":[{"Name":"Test","Id":"jf-001"}]}`))
			return
		}
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

// TestPushMetadata_FetchItemError verifies that PushMetadata returns an error
// when the initial item fetch fails (e.g. item not found).
func TestPushMetadata_FetchItemError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/Items" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Items":[]}`))
			return
		}
		t.Error("unexpected request after fetch failure")
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", "", srv.Client(), testLogger())
	err := c.PushMetadata(context.Background(), "nonexistent", connection.ArtistPushData{Name: "Test"})
	if err == nil {
		t.Fatal("expected error when item not found")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %q, want it to contain 'not found'", err.Error())
	}
}

// TestPushMetadata_FetchItemHTTPError verifies that PushMetadata returns a clear
// error when the GET /Items call itself returns a non-2xx status.
func TestPushMetadata_FetchItemHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("internal server error"))
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", "", srv.Client(), testLogger())
	err := c.PushMetadata(context.Background(), "jf-001", connection.ArtistPushData{Name: "Test"})
	if err == nil {
		t.Fatal("expected error when fetch returns 500")
	}
	if !strings.Contains(err.Error(), "fetch failed with status 500") {
		t.Errorf("error = %q, want message about fetch failed with status 500", err.Error())
	}
}

// TestPushMetadata_StripsReadOnlyFields verifies that read-only fields from the
// fetched item are removed before POSTing the merged body.
func TestPushMetadata_StripsReadOnlyFields(t *testing.T) {
	bodyCh := make(chan map[string]any, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/Items" {
			w.Header().Set("Content-Type", "application/json")
			// Include read-only fields that should be stripped.
			_, _ = w.Write([]byte(`{"Items":[{
				"Name":"Test","Id":"jf-001","ServerId":"abc123",
				"ImageBlurHashes":{"Primary":{"abc":"def"}},
				"ImageTags":{"Primary":"abc"},
				"LocationType":"FileSystem"
			}]}`))
			return
		}
		var b map[string]any
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			t.Errorf("decoding post body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		bodyCh <- b
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", "", srv.Client(), testLogger())
	err := c.PushMetadata(context.Background(), "jf-001", connection.ArtistPushData{Name: "Test"})
	if err != nil {
		t.Fatalf("PushMetadata failed: %v", err)
	}

	postedBody := <-bodyCh
	for _, field := range []string{"ServerId", "ImageBlurHashes", "ImageTags", "LocationType"} {
		if _, ok := postedBody[field]; ok {
			t.Errorf("read-only field %q was not stripped from POST body", field)
		}
	}
	if postedBody["Name"] != "Test" {
		t.Errorf("Name = %v, want Test", postedBody["Name"])
	}
}

// TestPushMetadata_MergesTags verifies that styles and moods are merged as Tags
// into the existing item body.
func TestPushMetadata_MergesTags(t *testing.T) {
	bodyCh := make(chan map[string]any, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/Items" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Items":[{"Name":"Existing","Id":"jf-001"}]}`))
			return
		}
		var b map[string]any
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			t.Errorf("decoding post body: %v", err)
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
	if err := c.PushMetadata(context.Background(), "jf-001", data); err != nil {
		t.Fatalf("PushMetadata failed: %v", err)
	}

	postedBody := <-bodyCh
	tags, ok := postedBody["Tags"]
	if !ok {
		t.Fatal("Tags field missing from POST body")
	}
	tagSlice, ok := tags.([]any)
	if !ok {
		t.Fatalf("Tags is %T, want []any", tags)
	}
	if len(tagSlice) != 3 {
		t.Errorf("got %d tags, want 3", len(tagSlice))
	}
}

// TestPushMetadata_MergesProviderIds verifies that PushMetadata merges the
// MusicBrainzArtist ID into existing ProviderIds rather than replacing them.
func TestPushMetadata_MergesProviderIds(t *testing.T) {
	bodyCh := make(chan map[string]any, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/Items" {
			w.Header().Set("Content-Type", "application/json")
			// Existing item has TheAudioDb and Discogs provider IDs.
			_, _ = w.Write([]byte(`{"Items":[{
				"Name":"Existing","Id":"jf-001",
				"ProviderIds":{"TheAudioDb":"111","Discogs":"222"}
			}]}`))
			return
		}
		var b map[string]any
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			t.Errorf("decoding post body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		bodyCh <- b
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", "", srv.Client(), testLogger())
	data := connection.ArtistPushData{
		Name:          "Test",
		MusicBrainzID: "mbid-999",
	}
	if err := c.PushMetadata(context.Background(), "jf-001", data); err != nil {
		t.Fatalf("PushMetadata failed: %v", err)
	}

	postedBody := <-bodyCh
	pids, ok := postedBody["ProviderIds"].(map[string]any)
	if !ok {
		t.Fatal("ProviderIds missing or wrong type in POST body")
	}
	// All three IDs should be present: the two existing ones plus the new MBID.
	if pids["MusicBrainzArtist"] != "mbid-999" {
		t.Errorf("MusicBrainzArtist = %v, want mbid-999", pids["MusicBrainzArtist"])
	}
	if pids["TheAudioDb"] != "111" {
		t.Errorf("TheAudioDb = %v, want 111 (existing ID was lost)", pids["TheAudioDb"])
	}
	if pids["Discogs"] != "222" {
		t.Errorf("Discogs = %v, want 222 (existing ID was lost)", pids["Discogs"])
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

	c := NewWithHTTPClient(srv.URL, "test-key", "", srv.Client(), testLogger())
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

	c := NewWithHTTPClient(srv.URL, "test-key", "", srv.Client(), testLogger())
	_, _, err := c.GetArtistImage(context.Background(), "jf-001", "thumb")
	if err == nil {
		t.Fatal("expected error for 404 response")
	}
}

func TestGetArtistImage_UnsupportedType(t *testing.T) {
	c := New("http://localhost", "key", "", testLogger())
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

	c := NewWithHTTPClient(srv.URL, "key", "", srv.Client(), testLogger())
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

	c := NewWithHTTPClient(srv.URL, "key", "", srv.Client(), testLogger())
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

	c := NewWithHTTPClient(srv.URL, "test-key", "", srv.Client(), testLogger())
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

	c := NewWithHTTPClient(srv.URL, "test-key", "", srv.Client(), testLogger())
	if err := c.DeleteImage(context.Background(), "jf-001", "thumb"); err == nil {
		t.Fatal("expected error for server error response")
	}
}

func TestDeleteImage_UnsupportedType(t *testing.T) {
	c := New("http://localhost", "key", "", testLogger())
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
				if r.Method == http.MethodGet && r.URL.Path == "/Items" {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"Items":[{"Name":"Existing","Id":"jf-001"}]}`))
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
			if err := c.PushMetadata(context.Background(), "jf-001", tt.data); err != nil {
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
		if r.Method != http.MethodPost || r.URL.Path != "/Items/jf-001/Images/Primary" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		body, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			t.Errorf("reading request body: %v", readErr)
		}
		bodyCh <- body
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", "", srv.Client(), testLogger())
	if err := c.UploadImage(context.Background(), "jf-001", "thumb", jpegData, "image/jpeg"); err != nil {
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

	c := NewWithHTTPClient(srv.URL, "key", "", srv.Client(), testLogger())
	if err := c.UploadImage(context.Background(), "jf-001", "thumb", []byte("data"), "image/jpeg"); err == nil {
		t.Fatal("expected error for server error response")
	}
}

func TestUploadImage_UnsupportedType(t *testing.T) {
	c := New("http://localhost", "key", "", testLogger())
	if err := c.UploadImage(context.Background(), "jf-001", "clearart", []byte("data"), "image/jpeg"); err == nil {
		t.Fatal("expected error for unsupported image type")
	}
}

func TestGetArtistBackdrop_Success(t *testing.T) {
	jpegData := createTestJPEG(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/Items/jf-001/Images/Backdrop/2" {
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

	c := NewWithHTTPClient(srv.URL, "test-key", "", srv.Client(), testLogger())
	data, contentType, err := c.GetArtistBackdrop(context.Background(), "jf-001", 2)
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
		if r.URL.Path != "/Items/jf-001/Images/Backdrop/0" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("not found"))
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "test-key", "", srv.Client(), testLogger())
	_, _, err := c.GetArtistBackdrop(context.Background(), "jf-001", 0)
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
	// Jellyfin uses MediaBrowser prefix without UserId.
	if !strings.HasPrefix(gotAuth, `MediaBrowser Client="Stillwater"`) {
		t.Errorf("Authorization header does not start with MediaBrowser Client=Stillwater: %q", gotAuth)
	}
	// Verify no UserId field is present (Emby-only).
	if strings.Contains(gotAuth, "UserId") {
		t.Errorf("Authorization header should not contain UserId for Jellyfin: %q", gotAuth)
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

func TestGetLibrarySettings(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{
				"Name": "Music",
				"CollectionType": "music",
				"ItemId": "lib-001",
				"LibraryOptions": {
					"SaveLocalMetadata": false,
					"MetadataSavers": [],
					"EnableInternetProviders": true,
					"TypeOptions": [
						{
							"Type": "MusicArtist",
							"ImageFetchers": ["FanArt"],
							"MetadataFetchers": ["MusicBrainz"]
						}
					]
				}
			},
			{
				"Name": "Podcasts",
				"CollectionType": "music",
				"ItemId": "lib-002",
				"LibraryOptions": {
					"SaveLocalMetadata": false,
					"MetadataSavers": [],
					"EnableInternetProviders": false,
					"TypeOptions": [
						{
							"Type": "MusicArtist",
							"ImageFetchers": ["FanArt"],
							"MetadataFetchers": []
						}
					]
				}
			}
		]`))
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "test-key", "", srv.Client(), testLogger())
	settings, err := c.GetLibrarySettings(context.Background())
	if err != nil {
		t.Fatalf("GetLibrarySettings: %v", err)
	}
	if len(settings) != 2 {
		t.Fatalf("got %d libraries, want 2", len(settings))
	}

	// First library has internet providers enabled with active fetchers.
	if !settings[0].HasConflicts {
		t.Error("expected first library to have conflicts")
	}
	// This library's MetadataSavers is EMPTY, so no lockdata is needed. The
	// assertion here used to be an unconditional "NeedsLockdata must be true for
	// Jellyfin", on the claim that Jellyfin ignores MetadataSavers=[] and lockdata
	// injection is the only NFO protection. That claim is false (#2420): clearing
	// the saver list is what stops the writes. Lockdata is only still needed where
	// a saver remains ARMED -- see TestGetLibrarySettings_NeedsLockdataOnlyWhenSaverArmed.
	if settings[0].NeedsLockdata {
		t.Errorf("NeedsLockdata = true for a library whose MetadataSavers is empty (%v); "+
			"an empty saver list needs no lockdata workaround", settings[0].MetadataSavers)
	}

	// Second library has internet providers disabled, so no conflicts despite having fetchers.
	if settings[1].HasConflicts {
		t.Error("expected second library to have no conflicts (internet providers disabled)")
	}
}

// TestGetLibrarySettings_NeedsLockdataOnlyWhenSaverArmed pins the real contract:
// lockdata is a workaround for an ARMED NFO saver, not a permanent fact of life on
// Jellyfin. NeedsLockdata was hardcoded true on the claim that Jellyfin ignores
// MetadataSavers=[]; that is false on 10.11.10 (#2420), and reporting lockdata as
// required on a library we have already disarmed tells operators to go work around
// a problem that no longer exists.
func TestGetLibrarySettings_NeedsLockdataOnlyWhenSaverArmed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[
			{"Name":"Armed","CollectionType":"music","ItemId":"lib-armed",
			 "LibraryOptions":{"MetadataSavers":["Nfo"],"EnableInternetProviders":false,"TypeOptions":[]}},
			{"Name":"Disarmed","CollectionType":"music","ItemId":"lib-disarmed",
			 "LibraryOptions":{"MetadataSavers":[],"EnableInternetProviders":false,"TypeOptions":[]}}
		]`))
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "test-key", "", srv.Client(), testLogger())
	settings, err := c.GetLibrarySettings(context.Background())
	if err != nil {
		t.Fatalf("GetLibrarySettings: %v", err)
	}
	if len(settings) != 2 {
		t.Fatalf("got %d libraries, want 2", len(settings))
	}
	if !settings[0].NeedsLockdata {
		t.Error("a library with an ARMED Nfo saver still needs lockdata; got NeedsLockdata=false")
	}
	if settings[1].NeedsLockdata {
		t.Error("a library with NO savers does not need lockdata; got NeedsLockdata=true")
	}
}

func TestDisableConflictingSettings_Jellyfin(t *testing.T) {
	bodyCh := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/Library/VirtualFolders" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[
				{
					"Name": "Music",
					"CollectionType": "music",
					"ItemId": "lib-001",
					"LibraryOptions": {
						"SaveLocalMetadata": false,
						"MetadataSavers": ["Nfo"],
						"EnableInternetProviders": true,
						"TypeOptions": [
							{
								"Type": "MusicArtist",
								"ImageFetchers": ["FanArt"],
								"MetadataFetchers": ["MusicBrainz"]
							}
						]
					}
				}
			]`))
		case r.URL.Path == "/Library/VirtualFolders/LibraryOptions" && r.Method == http.MethodPost:
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("reading body: %v", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			bodyCh <- body
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "test-key", "", srv.Client(), testLogger())
	if err := c.DisableConflictingSettings(context.Background(), "lib-001"); err != nil {
		t.Fatalf("DisableConflictingSettings: %v", err)
	}

	// Verify the request body clears fetchers (but NOT MetadataSavers for Jellyfin).
	receivedBody := <-bodyCh
	var opts LibraryOptions
	if err := json.Unmarshal(receivedBody, &opts); err != nil {
		t.Fatalf("parsing sent body: %v", err)
	}
	// Jellyfin's DisableConflictingSettings does NOT clear MetadataSavers because
	// it does not reliably prevent NFO writes. Lockdata injection is needed instead.
	for _, to := range opts.TypeOptions {
		if to.Type == "MusicArtist" {
			if len(to.ImageFetchers) != 0 {
				t.Errorf("ImageFetchers = %v, want empty", to.ImageFetchers)
			}
			if len(to.MetadataFetchers) != 0 {
				t.Errorf("MetadataFetchers = %v, want empty", to.MetadataFetchers)
			}
		}
	}
}

// TestUpdateArtistPath_AuthClass401 verifies that a 401 response (auth-class)
// from the POST half of UpdateArtistPath is wrapped with the ErrAuthRequired sentinel.
// The publish layer uses errors.Is(err, jellyfin.ErrAuthRequired) to route the
// failure to a per-connection re-auth UI signal (per issue #1639).
func TestUpdateArtistPath_AuthClass401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Items":[{"Id":"jf-a1","Name":"Test","Path":"/old"}]}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "k", "", srv.Client(), testLogger())
	err := c.UpdateArtistPath(context.Background(), "jf-a1", "/new")
	if err == nil {
		t.Fatal("expected error on 401")
	}
	if !errors.Is(err, ErrAuthRequired) {
		t.Errorf("errors.Is(err, ErrAuthRequired) = false; want true. err = %v", err)
	}
}

// TestUpdateArtistPath_AuthClass403 mirrors AuthClass401 for the 403 branch
// so the publish layer can rely on both codes wrapping with ErrAuthRequired.
func TestUpdateArtistPath_AuthClass403(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Items":[{"Id":"jf-a1","Name":"Test","Path":"/old"}]}`))
			return
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "k", "", srv.Client(), testLogger())
	err := c.UpdateArtistPath(context.Background(), "jf-a1", "/new")
	if err == nil {
		t.Fatal("expected error on 403")
	}
	if !errors.Is(err, ErrAuthRequired) {
		t.Errorf("errors.Is(err, ErrAuthRequired) = false; want true. err = %v", err)
	}
}

// TestUpdateArtistPath_NonAuthErrorNotWrapped guards the negative branch:
// non-auth status codes (5xx) must NOT wrap with ErrAuthRequired so the publish
// layer routes 5xx to its own toast class (server_error) rather than the
// re-auth signal.
func TestUpdateArtistPath_NonAuthErrorNotWrapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Items":[{"Id":"jf-a1","Name":"Test","Path":"/old"}]}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "k", "", srv.Client(), testLogger())
	err := c.UpdateArtistPath(context.Background(), "jf-a1", "/new")
	if err == nil {
		t.Fatal("expected error on 500")
	}
	if errors.Is(err, ErrAuthRequired) {
		t.Errorf("errors.Is(err, ErrAuthRequired) = true on 500; want false")
	}
}

// TestUploadImage_AuthClass401 covers the image-write surface. Image syncs
// share the per-connection observability path with PushMetadata, so a 401
// here must wrap with ErrAuthRequired alongside the metadata write methods.
func TestUploadImage_AuthClass401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "k", "", srv.Client(), testLogger())
	err := c.UploadImage(context.Background(), "jf-001", "thumb", []byte{1, 2, 3}, "image/jpeg")
	if err == nil {
		t.Fatal("expected error on 401")
	}
	if !errors.Is(err, ErrAuthRequired) {
		t.Errorf("errors.Is(err, ErrAuthRequired) = false; want true. err = %v", err)
	}
}

// TestPushMetadata_LockSortName_Ignored verifies that data.LockSortName=true
// on the Jellyfin path does NOT cause "SortName" to land in LockedFields.
// Jellyfin's MetadataField enum has no SortName member (the platform only
// supports a whole-item LockData boolean, not per-field locks), so sending
// "SortName" returns HTTP 400 and fails the entire push. ForcedSortName
// persists across metadata refresh on Jellyfin without any lock, so the
// LockSortName signal is consumed only by the Emby push path.
//
// The pre-existing user-set lock on Tags must still round-trip verbatim
// so the user's Jellyfin-UI choices survive the push.
func TestPushMetadata_LockSortName_Ignored(t *testing.T) {
	bodyCh := make(chan map[string]any, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/Items" {
			if got := r.URL.Query().Get("Ids"); got != "jf-numeric-1" {
				t.Errorf("Ids query = %q, want jf-numeric-1", got)
			}
			if fields := r.URL.Query().Get("Fields"); !strings.Contains(fields, "LockedFields") {
				t.Errorf("Fields query = %q, want to include LockedFields", fields)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Items":[{"Id":"jf-numeric-1","Name":"12 Pebbles","LockedFields":["Tags"]}]}`))
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/Items/jf-numeric-1" {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			bodyCh <- body
			w.WriteHeader(http.StatusNoContent)
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", "", srv.Client(), testLogger())
	data := connection.ArtistPushData{
		Name:         "12 Pebbles",
		SortName:     "0000000012 Pebbles",
		LockSortName: true,
	}
	if err := c.PushMetadata(context.Background(), "jf-numeric-1", data); err != nil {
		t.Fatalf("PushMetadata: %v", err)
	}
	got := <-bodyCh
	if fs, _ := got["ForcedSortName"].(string); fs != "0000000012 Pebbles" {
		t.Errorf("ForcedSortName = %q, want zero-padded derived value", fs)
	}
	locks := stringSliceFromAny(got["LockedFields"])
	if !sliceContains(locks, "Tags") {
		t.Errorf("LockedFields = %v, must preserve pre-existing 'Tags' lock", locks)
	}
	if sliceContains(locks, "SortName") {
		t.Errorf("LockedFields = %v, must NOT include SortName -- Jellyfin rejects it with HTTP 400", locks)
	}
}

// TestPushMetadata_LocksRoundTripVerbatim verifies that pre-existing per-field
// locks on the Jellyfin item round-trip unchanged through PushMetadata,
// regardless of the LockSortName signal. The push must never silently
// re-author the platform-side lock list.
func TestPushMetadata_LocksRoundTripVerbatim(t *testing.T) {
	bodyCh := make(chan map[string]any, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/Items" {
			if got := r.URL.Query().Get("Ids"); got != "jf-alpha-1" {
				t.Errorf("Ids query = %q, want jf-alpha-1", got)
			}
			if fields := r.URL.Query().Get("Fields"); !strings.Contains(fields, "LockedFields") {
				t.Errorf("Fields query = %q, want to include LockedFields", fields)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Items":[{"Id":"jf-alpha-1","Name":"Bjork","LockedFields":["Tags","Overview"]}]}`))
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/Items/jf-alpha-1" {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			bodyCh <- body
			w.WriteHeader(http.StatusNoContent)
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", "", srv.Client(), testLogger())
	data := connection.ArtistPushData{Name: "Bjork", SortName: "Bjork"}
	if err := c.PushMetadata(context.Background(), "jf-alpha-1", data); err != nil {
		t.Fatalf("PushMetadata: %v", err)
	}
	got := <-bodyCh
	locks := stringSliceFromAny(got["LockedFields"])
	if len(locks) != 2 {
		t.Errorf("LockedFields = %v, want preserved length 2", locks)
	}
	if !sliceContains(locks, "Tags") || !sliceContains(locks, "Overview") {
		t.Errorf("LockedFields = %v, want both Tags and Overview preserved", locks)
	}
	if sliceContains(locks, "SortName") {
		t.Errorf("LockedFields = %v, must NOT include SortName -- Jellyfin rejects it", locks)
	}
}

// stringSliceFromAny coerces a JSON-decoded LockedFields value ([]any)
// into a []string slice, dropping any non-string elements silently. The
// generic JSON decoder always returns array-of-any for unknown shapes,
// so the assertion side has to do the type assertion explicitly.
func stringSliceFromAny(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		if strs, ok := v.([]string); ok {
			return strs
		}
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// sliceContains reports whether `needle` appears anywhere in `haystack`.
// Used by the locked-fields assertions where ordering is not part of the
// contract -- the platform stores the array in arbitrary order.
func sliceContains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
