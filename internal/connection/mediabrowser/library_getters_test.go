package mediabrowser

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
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
	gotRawPath string

	// doStatus/doBody/doErr configure Do's canned response; gotDoMethod/
	// gotDoPath/gotDoBody/gotDoContentType record what the caller passed so
	// image_writers_test.go can assert the request shape non-vacuously.
	doStatus         int
	doBody           string
	doErr            error
	gotDoMethod      string
	gotDoPath        string
	gotDoBody        string
	gotDoContentType string
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

func (r *rawTransport) GetRaw(_ context.Context, path string) ([]byte, string, error) {
	r.gotRawPath = path
	return r.rawBytes, r.rawType, r.rawErr
}

func (r *rawTransport) PostJSON(_ context.Context, _ string, _ io.Reader, _ any) error {
	return nil
}

// Do records the request shape and returns a canned response, matching the
// real BaseClient.Do primitive that the image_writers.go free functions
// call. Defaults to 200 OK with an empty body when doStatus is unset (zero
// value), so tests that don't care about the response can omit it.
func (r *rawTransport) Do(_ context.Context, method, path string, body io.Reader, contentType string) (*http.Response, error) {
	r.gotDoMethod = method
	r.gotDoPath = path
	r.gotDoContentType = contentType
	if body != nil {
		b, _ := io.ReadAll(body)
		r.gotDoBody = string(b)
	}
	if r.doErr != nil {
		return nil, r.doErr
	}
	status := r.doStatus
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(bytes.NewReader([]byte(r.doBody))),
	}, nil
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
	case *[]scheduledTask:
		src, ok := v.([]scheduledTask)
		if !ok {
			return errors.New("assignInto: type mismatch for []scheduledTask")
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
	if want := "/Items/artist1/Images/Backdrop/2"; tr.gotRawPath != want {
		t.Errorf("got path=%q want=%q", tr.gotRawPath, want)
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
	if want := "/Items/artist1/Images/Primary"; tr.gotRawPath != want {
		t.Errorf("got path=%q want=%q", tr.gotRawPath, want)
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

// --- #2426 evidence primitives: listing completeness ---

// TestListLibraryArtistsComplete_ShortPageIsComplete pins the normal case: the
// peer signals end-of-data with a short page, so the enumeration saw
// everything and absences from it are meaningful.
func TestListLibraryArtistsComplete_ShortPageIsComplete(t *testing.T) {
	fetch := func(_ context.Context, _ string, startIndex, limit int) ([]connection.PeerArtist, int, error) {
		if startIndex > 0 {
			return nil, 0, nil
		}
		// Fewer than limit: the peer's own "that is all I have".
		return make([]connection.PeerArtist, 3), 3, nil
	}

	items, complete, err := ListLibraryArtistsComplete(context.Background(), []string{"lib1"}, fetch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !complete {
		t.Error("complete = false after the peer returned a short page")
	}
	if len(items) != 3 {
		t.Errorf("got %d items, want 3", len(items))
	}
}

// TestListLibraryArtistsComplete_PageCapIsTruncated is the guard the brief
// asked for explicitly. A peer that keeps returning full pages until the cap
// yields a listing that LOOKS finished, and a caller reasoning from absence
// would silently conclude that everything past the cap does not exist.
//
// Latent today (it needs a 100k-artist library), which is exactly why it is
// pinned now rather than left for that library to discover: the bug is
// invisible at the call site and only reachable at a scale nobody tests at.
func TestListLibraryArtistsComplete_PageCapIsTruncated(t *testing.T) {
	calls := 0
	fetch := func(_ context.Context, _ string, _, limit int) ([]connection.PeerArtist, int, error) {
		calls++
		// Always a full page: the peer never signals the end.
		return make([]connection.PeerArtist, limit), limit, nil
	}

	items, complete, err := ListLibraryArtistsComplete(context.Background(), []string{"lib1"}, fetch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if complete {
		t.Error("complete = true after stopping at the page cap; absences in a truncated " +
			"listing are not meaningful and a destructive caller would act on them")
	}
	if calls != listArtistsPageCap {
		t.Errorf("made %d fetch calls, want exactly the cap (%d)", calls, listArtistsPageCap)
	}
	// The items ARE valid; only the absences are unreliable. A caller
	// enumerating what exists must still get them.
	if len(items) != listArtistsPageCap*listArtistsPageLimit {
		t.Errorf("got %d items, want %d: truncation must not discard what was read",
			len(items), listArtistsPageCap*listArtistsPageLimit)
	}
}

// TestListLibraryArtistsComplete_OneTruncatedLibraryTaintsTheWholeResult is the
// multi-library case. Completeness is an ALL, not an ANY: a caller asking "is
// this artist absent?" is asking about the whole enumeration, so one truncated
// library makes the entire answer unreliable even if every other library
// finished cleanly.
//
// The fixture puts the clean library FIRST so a per-library flag that is
// overwritten rather than accumulated would pass. Ordering matters here.
func TestListLibraryArtistsComplete_OneTruncatedLibraryTaintsTheWholeResult(t *testing.T) {
	fetch := func(_ context.Context, libID string, startIndex, limit int) ([]connection.PeerArtist, int, error) {
		if libID == "clean" {
			if startIndex > 0 {
				return nil, 0, nil
			}
			return make([]connection.PeerArtist, 2), 2, nil
		}
		// "endless" never returns a short page.
		return make([]connection.PeerArtist, limit), limit, nil
	}

	_, complete, err := ListLibraryArtistsComplete(context.Background(),
		[]string{"clean", "endless"}, fetch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if complete {
		t.Error("complete = true when one of two libraries truncated; completeness is an ALL")
	}
}

// TestListLibraryArtistsComplete_ErrorIsNeverComplete: a failed enumeration
// saw nothing conclusive, so it must not report completeness. The bool is
// checked independently of the error because a caller that reads it without
// checking err first must still get the safe answer.
func TestListLibraryArtistsComplete_ErrorIsNeverComplete(t *testing.T) {
	fetch := func(_ context.Context, _ string, _, _ int) ([]connection.PeerArtist, int, error) {
		return nil, 0, errors.New("peer exploded")
	}

	items, complete, err := ListLibraryArtistsComplete(context.Background(), []string{"lib1"}, fetch)
	if err == nil {
		t.Fatal("expected the fetch error to propagate")
	}
	if complete {
		t.Error("complete = true on a failed enumeration")
	}
	if items != nil {
		t.Errorf("items = %v on error, want nil: a partial read must not look like a result", items)
	}
}

// TestListLibraryArtistsComplete_NoLibrariesIsVacuouslyComplete documents a
// deliberate edge: zero libraries means nothing was left unread, so the
// enumeration is complete and empty.
//
// This is safe in the drop context and worth stating: a caller must still
// treat "the artist is absent from an empty listing" as absence only if it
// independently expected the artist's library to be in the list at all. That
// judgment belongs to the caller; this function reports only what it saw.
func TestListLibraryArtistsComplete_NoLibrariesIsVacuouslyComplete(t *testing.T) {
	fetch := func(_ context.Context, _ string, _, _ int) ([]connection.PeerArtist, int, error) {
		t.Fatal("fetch must not be called when there are no libraries")
		return nil, 0, nil
	}

	items, complete, err := ListLibraryArtistsComplete(context.Background(), nil, fetch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !complete {
		t.Error("complete = false for an empty library list; nothing went unread")
	}
	if len(items) != 0 {
		t.Errorf("got %d items from no libraries", len(items))
	}
}

// TestListLibraryArtistsRaw_StillDelegatesAndDropsTheFlag proves the old
// signature keeps working for its existing callers (emby/jellyfin
// ListLibraryArtists), so this PR adds a capability without changing any
// current behavior.
func TestListLibraryArtistsRaw_StillDelegatesAndDropsTheFlag(t *testing.T) {
	fetch := func(_ context.Context, _ string, startIndex, _ int) ([]connection.PeerArtist, int, error) {
		if startIndex > 0 {
			return nil, 0, nil
		}
		return make([]connection.PeerArtist, 2), 2, nil
	}

	items, err := ListLibraryArtistsRaw(context.Background(), []string{"lib1"}, fetch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(items) != 2 {
		t.Errorf("got %d items, want 2", len(items))
	}
}
