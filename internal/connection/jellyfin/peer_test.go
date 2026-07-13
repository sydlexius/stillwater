package jellyfin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ListLibraryArtists is the enumeration the post-move relink resolves against, so
// what it returns decides whether Stillwater rewrites a peer link or keeps the one
// it holds. These tests pin the properties that decision depends on.

// The production topology has TWO music roots (e.g. /music and /classical), and
// Jellyfin's artist endpoint is queried per-ParentId. Both must be walked -- an
// implementation that stopped after the first would hide every artist living only
// in the second, and the relink would read that as "not resolved".
func TestListLibraryArtists_WalksEveryMusicLibrary(t *testing.T) {
	var parentIDs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/Library/VirtualFolders":
			_, _ = w.Write([]byte(`[
				{"Name":"Music","ItemId":"lib-1","CollectionType":"music","Locations":["/music"]},
				{"Name":"Classical","ItemId":"lib-2","CollectionType":"music","Locations":["/classical"]}
			]`))
		case strings.HasPrefix(r.URL.Path, "/Artists/AlbumArtists"):
			parent := r.URL.Query().Get("ParentId")
			parentIDs = append(parentIDs, parent)
			if parent == "lib-1" {
				_, _ = w.Write([]byte(`{"Items":[{"Id":"a1","Name":"Artist A","Path":"/music/Artist A"}],"TotalRecordCount":1}`))
				return
			}
			_, _ = w.Write([]byte(`{"Items":[{"Id":"b1","Name":"Artist B","Path":"/classical/Artist B"}],"TotalRecordCount":1}`))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "test-key", "u1", srv.Client(), testLogger())
	items, err := c.ListLibraryArtists(context.Background())
	if err != nil {
		t.Fatalf("ListLibraryArtists: %v", err)
	}

	if len(parentIDs) != 2 {
		t.Errorf("queried ParentIds %v, want both music libraries scoped separately", parentIDs)
	}
	if len(items) != 2 {
		t.Fatalf("got %d artists, want 2 (one per library): %+v", len(items), items)
	}
	if items[0].ID != "a1" || items[0].Path != "/music/Artist A" {
		t.Errorf("item[0] = %+v, want id=a1 path=/music/Artist A", items[0])
	}
	if items[1].ID != "b1" || items[1].Path != "/classical/Artist B" {
		t.Errorf("item[1] = %+v, want id=b1 path=/classical/Artist B", items[1])
	}
}

// Jellyfin is FOLDER-BACKED: a healthy artist always carries a real path, and that
// path is the relink's primary identity key (clause (a): the item AT the directory
// we moved to). Losing it would strand every Jellyfin relink on the weaker,
// Emby-only name rule -- which on a folder-backed peer refuses by design.
func TestListLibraryArtists_CarriesTheArtistPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/Library/VirtualFolders" {
			_, _ = w.Write([]byte(`[{"Name":"Music","ItemId":"lib-1","CollectionType":"music","Locations":["/music"]}]`))
			return
		}
		_, _ = w.Write([]byte(`{"Items":[{"Id":"jf-new","Name":"Artist A","Path":"/music/New Name"}],"TotalRecordCount":1}`))
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "test-key", "u1", srv.Client(), testLogger())
	items, err := c.ListLibraryArtists(context.Background())
	if err != nil {
		t.Fatalf("ListLibraryArtists: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d artists, want 1", len(items))
	}
	if items[0].Path != "/music/New Name" {
		t.Errorf("Path = %q, want /music/New Name -- the path is how the relink recognizes the moved item",
			items[0].Path)
	}
	if items[0].ID != "jf-new" {
		t.Errorf("ID = %q, want jf-new", items[0].ID)
	}
}

// A library with no ItemId cannot be scoped; skip it rather than issuing a
// ParentId-less request. That request would return an UNSCOPED artist list --
// including the metadata-only ghosts stranded outside every library root that the
// scoping exists to exclude, and re-linking to one of those is the #2380
// corruption itself.
func TestListLibraryArtists_SkipsLibraryWithNoItemID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/Library/VirtualFolders" {
			_, _ = w.Write([]byte(`[{"Name":"Broken","ItemId":"","CollectionType":"music","Locations":["/music"]}]`))
			return
		}
		t.Errorf("queried artists for a library with no ItemId (an UNSCOPED query surfaces ghosts): %s?%s",
			r.URL.Path, r.URL.RawQuery)
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "test-key", "u1", srv.Client(), testLogger())
	items, err := c.ListLibraryArtists(context.Background())
	if err != nil {
		t.Fatalf("ListLibraryArtists: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("got %+v, want no artists", items)
	}
}

// An enumeration failure must ERROR, never yield an empty slice. The relink treats
// an error as UNVERIFIED and keeps the link; an empty-and-nil would instead look
// like a peer that legitimately knows no artists -- which proves nothing but reads
// like evidence.
func TestListLibraryArtists_ErrorIsNotAnEmptyList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/Library/VirtualFolders" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"Name":"Music","ItemId":"lib-1","CollectionType":"music","Locations":["/music"]}]`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "test-key", "u1", srv.Client(), testLogger())
	items, err := c.ListLibraryArtists(context.Background())
	if err == nil {
		t.Fatalf("ListLibraryArtists returned %+v and no error on a 500", items)
	}
	if items != nil {
		t.Errorf("items = %+v on error, want nil", items)
	}
}
