package lidarr

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Tests for the three peer-facing methods #2380 added: GetRootFolders (the
// pre-flight root guard's evidence), GetArtistPath (the read-back verifier), and
// ListLibraryArtists (the relink's enumeration).
//
// Each server double answers with a real Lidarr-shaped body rather than an empty
// 200, so an implementation that ignored the response and returned a zero value
// would fail rather than pass.

func TestGetRootFolders(t *testing.T) {
	var gotPath, gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotKey = r.URL.Path, r.Header.Get("X-Api-Key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":1,"path":"/music"},{"id":2,"path":"/classical"}]`))
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "test-key", srv.Client(), testLogger())
	folders, err := c.GetRootFolders(context.Background())
	if err != nil {
		t.Fatalf("GetRootFolders: %v", err)
	}

	if gotPath != "/api/v1/rootfolder" {
		t.Errorf("called %q, want /api/v1/rootfolder", gotPath)
	}
	if gotKey != "test-key" {
		t.Errorf("X-Api-Key = %q, want test-key", gotKey)
	}
	// Both roots must survive: the guard fails CLOSED on an empty root list, so an
	// implementation that dropped entries would refuse every legitimate push.
	if len(folders) != 2 {
		t.Fatalf("got %d root folders, want 2: %+v", len(folders), folders)
	}
	if folders[0].Path != "/music" || folders[1].Path != "/classical" {
		t.Errorf("roots = %q,%q, want /music,/classical", folders[0].Path, folders[1].Path)
	}
}

// A root fetch that fails must ERROR, never return an empty list: the guard treats
// zero roots as "cannot verify" and refuses the push, but an error carries the
// reason the operator needs.
func TestGetRootFolders_Unauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "bad-key", srv.Client(), testLogger())
	folders, err := c.GetRootFolders(context.Background())
	if err == nil {
		t.Fatalf("GetRootFolders returned %+v and no error on a 401", folders)
	}
	if folders != nil {
		t.Errorf("folders = %+v on error, want nil", folders)
	}
}

func TestGetArtistPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/artist/42" {
			t.Errorf("called %q, want /api/v1/artist/42", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":42,"artistName":"Artist A","path":"/music/Artist A"}`))
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "test-key", srv.Client(), testLogger())
	path, err := c.GetArtistPath(context.Background(), "42")
	if err != nil {
		t.Fatalf("GetArtistPath: %v", err)
	}
	if path != "/music/Artist A" {
		t.Errorf("path = %q, want /music/Artist A", path)
	}
}

// A resource with no path field yields "", not an error. SamePeerPath treats ""
// as "never a match", so the verifier reads this as "the peer did not honor the
// write" -- which is the correct, conservative reading.
func TestGetArtistPath_NoPathField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":42,"artistName":"Artist A"}`))
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "test-key", srv.Client(), testLogger())
	path, err := c.GetArtistPath(context.Background(), "42")
	if err != nil {
		t.Fatalf("GetArtistPath: %v", err)
	}
	if path != "" {
		t.Errorf("path = %q, want empty", path)
	}
}

// An empty id must be refused locally rather than sent as GET /api/v1/artist/,
// which Lidarr answers with the FULL artist list -- a 200 that would otherwise be
// parsed as a single artist with no path.
func TestGetArtistPath_EmptyIDIsRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("an empty artist id must never reach the peer")
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "test-key", srv.Client(), testLogger())
	if _, err := c.GetArtistPath(context.Background(), "  "); err == nil {
		t.Fatal("GetArtistPath accepted a blank artist id")
	}
}

// A null body is an ERROR, not "" -- "" would read as a legitimate pathless peer
// and silently pass the read-back off as a verified non-match.
func TestGetArtistPath_NullBodyIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`null`))
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "test-key", srv.Client(), testLogger())
	if _, err := c.GetArtistPath(context.Background(), "42"); err == nil {
		t.Fatal("an empty artist body was accepted as a valid read-back")
	}
}

// ListLibraryArtists must render Lidarr's INTEGER id as the string the relink
// compares against artist_platform_ids. An id that came back as "" or "0" would
// never match the link we hold.
func TestListLibraryArtists(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"id":7,"artistName":"Artist A","path":"/music/Artist A"},
			{"id":9,"artistName":"Artist B","path":"/classical/Artist B"}
		]`))
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "test-key", srv.Client(), testLogger())
	items, err := c.ListLibraryArtists(context.Background())
	if err != nil {
		t.Fatalf("ListLibraryArtists: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d artists, want 2: %+v", len(items), items)
	}
	if items[0].ID != "7" {
		t.Errorf("ID = %q, want %q -- Lidarr's integer id must render as the string the link stores",
			items[0].ID, "7")
	}
	if items[0].Name != "Artist A" || items[0].Path != "/music/Artist A" {
		t.Errorf("item[0] = %+v, want name=Artist A path=/music/Artist A", items[0])
	}
	if items[1].ID != "9" || items[1].Path != "/classical/Artist B" {
		t.Errorf("item[1] = %+v, want id=9 path=/classical/Artist B", items[1])
	}
}

func TestListLibraryArtists_PeerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "test-key", srv.Client(), testLogger())
	items, err := c.ListLibraryArtists(context.Background())
	if err == nil {
		t.Fatalf("ListLibraryArtists returned %+v and no error on a 500", items)
	}
	// The relink treats an enumeration failure as UNVERIFIED and keeps the link;
	// an empty-slice-and-nil-error here would instead read as "the peer knows no
	// artists", which proves nothing but looks like evidence.
	if items != nil {
		t.Errorf("items = %+v on error, want nil", items)
	}
}

// TrimSpace, not just len==0: a whitespace id would otherwise be URL-escaped into
// /api/v1/artist/%20%20 and 404.
func TestGetArtistPath_BlankIDMessage(t *testing.T) {
	c := NewWithHTTPClient("http://unused", "k", http.DefaultClient, testLogger())
	_, err := c.GetArtistPath(context.Background(), "")
	if err == nil {
		t.Fatal("empty id accepted")
	}
	if !strings.Contains(err.Error(), "required") {
		t.Errorf("error = %q, want it to say the id is required", err)
	}
}
