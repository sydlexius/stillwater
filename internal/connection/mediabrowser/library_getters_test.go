package mediabrowser

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/sydlexius/stillwater/internal/connection"
)

// rawTransport is a minimal Transport test double for the getter functions
// in this file. Distinct from fakeTransport (library_options_test.go),
// which is shaped around the raw-JSON snapshot/restore flow; these getters
// need a GetRaw stub and per-path canned Get results instead.
type rawTransport struct {
	getResults map[string]any
	getErr     error
	rawBytes   []byte
	rawType    string
	rawErr     error
}

func (r *rawTransport) Get(_ context.Context, path string, result any) error {
	if r.getErr != nil {
		return r.getErr
	}
	v, ok := r.getResults[path]
	if !ok {
		return nil
	}
	return assignInto(result, v)
}

func (r *rawTransport) GetRaw(_ context.Context, _ string) ([]byte, string, error) {
	return r.rawBytes, r.rawType, r.rawErr
}

func (r *rawTransport) PostJSON(_ context.Context, _ string, _ io.Reader, _ any) error {
	return nil
}

// assignInto copies v (a pointer-shaped value the test constructs) into
// result via a type assertion on the pointer types this file's tests
// actually use, avoiding a JSON round-trip.
func assignInto(result any, v any) error {
	switch dst := result.(type) {
	case *[]testVirtualFolder:
		src, ok := v.([]testVirtualFolder)
		if !ok {
			return errors.New("assignInto: type mismatch for []testVirtualFolder")
		}
		*dst = src
	case *testArtistDetailItem:
		src, ok := v.(testArtistDetailItem)
		if !ok {
			return errors.New("assignInto: type mismatch for testArtistDetailItem")
		}
		*dst = src
	case *testItemsResponse:
		src, ok := v.(testItemsResponse)
		if !ok {
			return errors.New("assignInto: type mismatch for testItemsResponse")
		}
		*dst = src
	default:
		return errors.New("assignInto: unsupported result type")
	}
	return nil
}

type testVirtualFolder struct {
	Name           string
	CollectionType string
	ItemID         string
}

type testArtistDetailItem struct {
	Name string
}

type testItemsResponse struct {
	Items []testArtistItem
}

type testArtistItem struct {
	ID, Name, Path string
}

func TestGetArtistBackdropRaw(t *testing.T) {
	tr := &rawTransport{rawBytes: []byte("abc"), rawType: "image/jpeg"}
	data, ct, err := GetArtistBackdropRaw(context.Background(), tr, "artist1", 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(data) != "abc" || ct != "image/jpeg" {
		t.Errorf("got data=%q ct=%q", data, ct)
	}
}

func TestGetArtistBackdropRaw_Error(t *testing.T) {
	wantErr := errors.New("boom")
	tr := &rawTransport{rawErr: wantErr}
	_, _, err := GetArtistBackdropRaw(context.Background(), tr, "artist1", 0)
	if !errors.Is(err, wantErr) {
		t.Errorf("expected wrapped error, got %v", err)
	}
}

func TestGetArtistImageRaw_Success(t *testing.T) {
	tr := &rawTransport{rawBytes: []byte("xyz"), rawType: "image/png"}
	data, ct, err := GetArtistImageRaw(context.Background(), tr, "artist1", "Primary", "thumb")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(data) != "xyz" || ct != "image/png" {
		t.Errorf("got data=%q ct=%q", data, ct)
	}
}

func TestGetArtistImageRaw_UnsupportedType(t *testing.T) {
	tr := &rawTransport{}
	_, _, err := GetArtistImageRaw(context.Background(), tr, "artist1", "", "bogus")
	if err == nil {
		t.Fatal("expected error for unsupported image type")
	}
}

func TestFilterMusicLibraries(t *testing.T) {
	folders := []testVirtualFolder{
		{Name: "Music", CollectionType: "music"},
		{Name: "Blank", CollectionType: ""},
		{Name: "Movies", CollectionType: "movies"},
		{Name: "MUSIC-caps", CollectionType: "MUSIC"},
	}
	got := FilterMusicLibraries(folders, testLogger(), "emby",
		func(f testVirtualFolder) string { return f.CollectionType },
		func(f testVirtualFolder) string { return f.Name },
	)
	if len(got) != 3 {
		t.Fatalf("expected 3 included libraries, got %d: %+v", len(got), got)
	}
	for _, f := range got {
		if f.Name == "Movies" {
			t.Errorf("movies library should have been excluded: %+v", got)
		}
	}
}

func TestFilterMusicLibraries_NilLogger(t *testing.T) {
	folders := []testVirtualFolder{{Name: "Music", CollectionType: "music"}}
	got := FilterMusicLibraries(folders, nil, "jellyfin",
		func(f testVirtualFolder) string { return f.CollectionType },
		func(f testVirtualFolder) string { return f.Name },
	)
	if len(got) != 1 {
		t.Fatalf("expected 1 library, got %d", len(got))
	}
}

func TestGetMusicLibrariesRaw2(t *testing.T) {
	tr := &rawTransport{getResults: map[string]any{
		"/Library/VirtualFolders": []testVirtualFolder{{Name: "Music", CollectionType: "music", ItemID: "1"}},
	}}
	var folders []testVirtualFolder
	if err := GetMusicLibrariesRaw2(context.Background(), tr, &folders); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(folders) != 1 || folders[0].ItemID != "1" {
		t.Errorf("unexpected folders: %+v", folders)
	}
}

func TestGetMusicLibrariesRaw2_Error(t *testing.T) {
	wantErr := errors.New("boom")
	tr := &rawTransport{getErr: wantErr}
	var folders []testVirtualFolder
	err := GetMusicLibrariesRaw2(context.Background(), tr, &folders)
	if !errors.Is(err, wantErr) {
		t.Errorf("expected wrapped error, got %v", err)
	}
}

func TestBuildArtistPlatformState(t *testing.T) {
	got := BuildArtistPlatformState(ArtistDetailFields{
		Name:              "Artist",
		SortName:          "Artist, The",
		Overview:          "bio",
		Genres:            []string{"rock"},
		Tags:              []string{"tag"},
		PremiereDate:      "2020",
		EndDate:           "",
		MusicBrainzID:     "mbid",
		ImageTags:         map[string]string{"Primary": "abc", "Logo": "", "Banner": "def"},
		BackdropImageTags: []string{"bd1", "bd2"},
		Locked:            true,
		LockedFields:      []string{"Name"},
	})
	if got.Name != "Artist" || got.Biography != "bio" || got.MusicBrainzID != "mbid" {
		t.Errorf("unexpected base fields: %+v", got)
	}
	if !got.HasThumb {
		t.Error("expected HasThumb true (Primary tag present)")
	}
	if got.HasLogo {
		t.Error("expected HasLogo false (empty Logo tag)")
	}
	if !got.HasBanner {
		t.Error("expected HasBanner true")
	}
	if !got.HasFanart || got.BackdropCount != 2 {
		t.Errorf("expected HasFanart true and BackdropCount 2, got %+v", got)
	}
	if !got.IsLocked {
		t.Error("expected IsLocked true")
	}
}

func TestGetArtistDetailRaw_NoUserID(t *testing.T) {
	tr := &rawTransport{}
	var item testArtistDetailItem
	err := GetArtistDetailRaw(context.Background(), tr, "", "artist1", &item)
	if err == nil {
		t.Fatal("expected error when userID is empty")
	}
}

func TestGetArtistDetailRaw_Success(t *testing.T) {
	tr := &rawTransport{getResults: map[string]any{
		"/Users/user1/Items/artist1?Fields=Overview,Genres,Tags,SortName,ProviderIds,ImageTags,BackdropImageTags,PremiereDate,EndDate,LockedFields": testArtistDetailItem{Name: "Artist"},
	}}
	var item testArtistDetailItem
	if err := GetArtistDetailRaw(context.Background(), tr, "user1", "artist1", &item); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if item.Name != "Artist" {
		t.Errorf("unexpected item: %+v", item)
	}
}

func TestGetArtistDetailRaw_Error(t *testing.T) {
	wantErr := errors.New("boom")
	tr := &rawTransport{getErr: wantErr}
	var item testArtistDetailItem
	err := GetArtistDetailRaw(context.Background(), tr, "user1", "artist1", &item)
	if !errors.Is(err, wantErr) {
		t.Errorf("expected wrapped error, got %v", err)
	}
}

func TestGetArtistsRaw(t *testing.T) {
	tr := &rawTransport{getResults: map[string]any{
		"/Artists/AlbumArtists?ParentId=lib1&StartIndex=0&Limit=10&Recursive=true&Fields=Path,ProviderIds,ImageTags,BackdropImageTags,Overview,Genres,Tags,SortName,PremiereDate,EndDate": testItemsResponse{
			Items: []testArtistItem{{ID: "a1", Name: "Artist"}},
		},
	}}
	var resp testItemsResponse
	if err := GetArtistsRaw(context.Background(), tr, "lib1", 0, 10, &resp); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Items) != 1 || resp.Items[0].ID != "a1" {
		t.Errorf("unexpected items: %+v", resp.Items)
	}
}

func TestGetArtistsRaw_Error(t *testing.T) {
	wantErr := errors.New("boom")
	tr := &rawTransport{getErr: wantErr}
	var resp testItemsResponse
	err := GetArtistsRaw(context.Background(), tr, "lib1", 0, 10, &resp)
	if !errors.Is(err, wantErr) {
		t.Errorf("expected wrapped error, got %v", err)
	}
}

func TestListLibraryArtistsRaw(t *testing.T) {
	calls := map[string]int{}
	fetch := func(_ context.Context, libraryID string, startIndex, limit int) ([]connection.PeerArtist, int, error) {
		calls[libraryID]++
		if libraryID == "lib1" && startIndex == 0 {
			// A full page: the loop must issue a second page for this library.
			items := make([]connection.PeerArtist, limit)
			for i := range items {
				items[i] = connection.PeerArtist{ID: libraryID, Name: "a"}
			}
			return items, limit, nil
		}
		return []connection.PeerArtist{{ID: libraryID + "-last", Name: "b"}}, 1, nil
	}
	out, err := ListLibraryArtistsRaw(context.Background(), []string{"lib1", "", "lib2"}, fetch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// lib1: one full page (500) + one short page (1) = 501; lib2: one short page (1).
	if len(out) != 502 {
		t.Fatalf("expected 502 artists total, got %d", len(out))
	}
	if calls["lib1"] != 2 {
		t.Errorf("expected lib1 to be paged twice, got %d calls", calls["lib1"])
	}
	if calls["lib2"] != 1 {
		t.Errorf("expected lib2 to be paged once, got %d calls", calls["lib2"])
	}
	if _, ok := calls[""]; ok {
		t.Error("empty library ID should have been skipped entirely")
	}
}

func TestListLibraryArtistsRaw_FetchError(t *testing.T) {
	wantErr := errors.New("boom")
	fetch := func(_ context.Context, _ string, _, _ int) ([]connection.PeerArtist, int, error) {
		return nil, 0, wantErr
	}
	_, err := ListLibraryArtistsRaw(context.Background(), []string{"lib1"}, fetch)
	if !errors.Is(err, wantErr) {
		t.Errorf("expected wrapped error, got %v", err)
	}
}

func TestListLibraryArtistsRaw_PageCap(t *testing.T) {
	// A misbehaving peer that always returns a full page must not spin forever;
	// the loop must stop at listArtistsPageCap pages.
	calls := 0
	fetch := func(_ context.Context, _ string, _, limit int) ([]connection.PeerArtist, int, error) {
		calls++
		return make([]connection.PeerArtist, limit), limit, nil
	}
	_, err := ListLibraryArtistsRaw(context.Background(), []string{"lib1"}, fetch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != listArtistsPageCap {
		t.Errorf("expected exactly %d calls (page cap), got %d", listArtistsPageCap, calls)
	}
}
