package mediabrowser

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
)

// fakeTransport is the test double used by every helper test in this file.
// getResponses keeps a per-path queue of what subsequent Get() calls should
// fill into result; posts records every PostJSON body in arrival order so
// assertions can pin both order and shape. postErrs lets a test inject a
// failure on the Nth POST to drive partial-failure behavior.
type fakeTransport struct {
	getResponses map[string][]any
	getErrs      map[string]error
	posts        []postCall
	postErrs     []error
}

type postCall struct {
	path string
	body []byte
}

func newFakeTransport() *fakeTransport {
	return &fakeTransport{
		getResponses: map[string][]any{},
		getErrs:      map[string]error{},
	}
}

func (f *fakeTransport) Get(_ context.Context, path string, result any) error {
	if err, ok := f.getErrs[path]; ok {
		return err
	}
	queue, ok := f.getResponses[path]
	if !ok || len(queue) == 0 {
		return nil
	}
	resp := queue[0]
	f.getResponses[path] = queue[1:]
	// Round-trip the canned response through JSON so the test fixture's
	// exact shape (e.g. []map[string]any) lands in result without the
	// caller having to know the destination type.
	buf, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	return json.Unmarshal(buf, result)
}

func (f *fakeTransport) PostJSON(_ context.Context, path string, body io.Reader, _ any) error {
	buf, _ := io.ReadAll(body)
	idx := len(f.posts)
	f.posts = append(f.posts, postCall{path: path, body: buf})
	if idx < len(f.postErrs) {
		return f.postErrs[idx]
	}
	return nil
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestSanitizeLibraryOptions_DropsNilValues(t *testing.T) {
	in := map[string]any{
		"keepString": "x",
		"keepBool":   true,
		"dropNil":    nil,
		"keepSlice":  []any{"a"},
	}
	got := SanitizeLibraryOptions(in)
	if _, ok := got["dropNil"]; ok {
		t.Errorf("dropNil should have been removed: %+v", got)
	}
	if got["keepString"] != "x" || got["keepBool"] != true {
		t.Errorf("non-nil values should be preserved: %+v", got)
	}
	if !reflect.DeepEqual(got["keepSlice"], []any{"a"}) {
		t.Errorf("slice not preserved: %+v", got["keepSlice"])
	}
}

func TestSanitizeLibraryOptions_EmptyInput(t *testing.T) {
	got := SanitizeLibraryOptions(map[string]any{})
	if len(got) != 0 {
		t.Errorf("empty in should produce empty out, got %+v", got)
	}
}

func TestBuildSnapshot_VersionAndRoundTrip(t *testing.T) {
	entries := []LibrarySaverSnapshotEntry{
		{LibraryID: "m1", LibraryName: "Music", SaveLocalMetadata: true, MetadataSavers: []string{"Nfo"}},
	}
	snapJSON, err := BuildSnapshot(entries)
	if err != nil {
		t.Fatalf("BuildSnapshot err = %v", err)
	}
	var snap LibraryWriteBackSnapshot
	if err := json.Unmarshal([]byte(snapJSON), &snap); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if snap.Version != 1 {
		t.Errorf("version = %d, want 1", snap.Version)
	}
	if !reflect.DeepEqual(snap.Libraries, entries) {
		t.Errorf("libraries round-trip mismatch: got %+v, want %+v", snap.Libraries, entries)
	}
	if snap.SnapshottedAt.IsZero() {
		t.Errorf("SnapshottedAt should be populated")
	}
}

func TestGetMusicLibrariesRaw_FiltersOnCollectionType(t *testing.T) {
	tr := newFakeTransport()
	tr.getResponses["/Library/VirtualFolders"] = []any{[]map[string]any{
		// Explicit music.
		{"ItemId": "m1", "Name": "Music", "CollectionType": "music",
			"LibraryOptions": map[string]any{"SaveLocalMetadata": true}},
		// Empty CollectionType -- treated as music (mixed/legacy libs).
		{"ItemId": "mixed", "Name": "Mixed", "CollectionType": "",
			"LibraryOptions": map[string]any{"SaveLocalMetadata": false}},
		// Movies -- excluded.
		{"ItemId": "movies", "Name": "Movies", "CollectionType": "movies"},
	}}

	libs, err := GetMusicLibrariesRaw(context.Background(), tr, testLogger(), "emby")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(libs) != 2 {
		t.Fatalf("want 2 libraries (music + mixed), got %d: %+v", len(libs), libs)
	}
	for _, lib := range libs {
		if lib.ID == "movies" {
			t.Errorf("movies library should have been excluded")
		}
	}
}

func TestGetMusicLibrariesRaw_NilOptionsBecomeEmptyMap(t *testing.T) {
	tr := newFakeTransport()
	tr.getResponses["/Library/VirtualFolders"] = []any{[]map[string]any{
		{"ItemId": "m1", "Name": "Music", "CollectionType": "music"},
	}}
	libs, err := GetMusicLibrariesRaw(context.Background(), tr, nil, "jellyfin")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(libs) != 1 {
		t.Fatalf("want 1 library, got %d", len(libs))
	}
	if libs[0].Options == nil {
		t.Errorf("Options should be non-nil empty map even when missing in source")
	}
}

func TestGetMusicLibrariesRaw_TransportErrorIsWrapped(t *testing.T) {
	tr := newFakeTransport()
	tr.getErrs["/Library/VirtualFolders"] = errors.New("network down")
	if _, err := GetMusicLibrariesRaw(context.Background(), tr, testLogger(), "emby"); err == nil {
		t.Fatal("expected error")
	} else if !strings.Contains(err.Error(), "virtual folders") {
		t.Errorf("error = %q, should mention 'virtual folders'", err.Error())
	}
}

func TestPostLibraryOptionsRaw_WrapsInLibraryOptionsInfoEnvelope(t *testing.T) {
	tr := newFakeTransport()
	opts := map[string]any{"SaveLocalMetadata": false}
	if err := PostLibraryOptionsRaw(context.Background(), tr, testLogger(), "emby", "lib1", opts); err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(tr.posts) != 1 {
		t.Fatalf("post count = %d, want 1", len(tr.posts))
	}
	if !strings.Contains(tr.posts[0].path, "Id=lib1") {
		t.Errorf("path %q should contain Id=lib1", tr.posts[0].path)
	}
	var wrapper struct {
		ID             string         `json:"Id"`
		LibraryOptions map[string]any `json:"LibraryOptions"`
	}
	if err := json.Unmarshal(tr.posts[0].body, &wrapper); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if wrapper.ID != "lib1" {
		t.Errorf("Id = %q, want %q", wrapper.ID, "lib1")
	}
	if wrapper.LibraryOptions["SaveLocalMetadata"] != false {
		t.Errorf("LibraryOptions.SaveLocalMetadata = %v, want false", wrapper.LibraryOptions["SaveLocalMetadata"])
	}
}

func TestDisableFileWriteBack_PostsSaveLocalMetadataFalsePerLibrary(t *testing.T) {
	tr := newFakeTransport()
	tr.getResponses["/Library/VirtualFolders"] = []any{[]map[string]any{
		{"ItemId": "m1", "Name": "Music", "CollectionType": "music",
			"LibraryOptions": map[string]any{"SaveLocalMetadata": true, "MetadataSavers": []any{"Nfo"}}},
		{"ItemId": "m2", "Name": "Audiobooks", "CollectionType": "music",
			"LibraryOptions": map[string]any{"SaveLocalMetadata": true}},
	}}

	if err := DisableFileWriteBack(context.Background(), tr, testLogger(), "emby"); err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(tr.posts) != 2 {
		t.Fatalf("want 2 POSTs, got %d", len(tr.posts))
	}
	for i, p := range tr.posts {
		var wrapper struct {
			LibraryOptions map[string]any `json:"LibraryOptions"`
		}
		if err := json.Unmarshal(p.body, &wrapper); err != nil {
			t.Fatalf("post %d body decode: %v", i, err)
		}
		if wrapper.LibraryOptions["SaveLocalMetadata"] != false {
			t.Errorf("post %d: SaveLocalMetadata = %v, want false", i, wrapper.LibraryOptions["SaveLocalMetadata"])
		}
		// MetadataSavers must be left alone -- mutating it alongside the
		// kill switch crashes some peer builds with NRE.
		if i == 0 {
			savers, _ := wrapper.LibraryOptions["MetadataSavers"].([]any)
			if len(savers) != 1 || savers[0] != "Nfo" {
				t.Errorf("post %d: MetadataSavers should be preserved as [Nfo], got %v", i, savers)
			}
		}
	}
}

func TestDisableFileWriteBack_ReturnsFirstErrorButContinues(t *testing.T) {
	tr := newFakeTransport()
	tr.getResponses["/Library/VirtualFolders"] = []any{[]map[string]any{
		{"ItemId": "m1", "Name": "A", "CollectionType": "music"},
		{"ItemId": "m2", "Name": "B", "CollectionType": "music"},
	}}
	tr.postErrs = []error{errors.New("first POST broken"), nil}

	err := DisableFileWriteBack(context.Background(), tr, testLogger(), "emby")
	if err == nil || !strings.Contains(err.Error(), "first POST broken") {
		t.Errorf("expected first POST error to surface, got %v", err)
	}
	// Both POSTs should still have been attempted -- one library's
	// failure should not abandon siblings.
	if len(tr.posts) != 2 {
		t.Errorf("want 2 POST attempts despite first failing, got %d", len(tr.posts))
	}
}

func TestRestoreLibraryOptions_RejectsUnknownVersion(t *testing.T) {
	tr := newFakeTransport()
	err := RestoreLibraryOptions(context.Background(), tr, testLogger(), "emby", `{"version":2,"libraries":[]}`)
	if err == nil || !strings.Contains(err.Error(), "unsupported snapshot version") {
		t.Errorf("expected version error, got %v", err)
	}
	if len(tr.posts) != 0 {
		t.Errorf("rejected snapshot should not POST, got %d posts", len(tr.posts))
	}
}

func TestRestoreLibraryOptions_RejectsMalformedJSON(t *testing.T) {
	tr := newFakeTransport()
	err := RestoreLibraryOptions(context.Background(), tr, testLogger(), "emby", `{not json`)
	if err == nil || !strings.Contains(err.Error(), "decoding snapshot") {
		t.Errorf("expected decode error, got %v", err)
	}
}

func TestRestoreLibraryOptions_ReplaysSnapshotShape(t *testing.T) {
	tr := newFakeTransport()
	tr.getResponses["/Library/VirtualFolders"] = []any{[]map[string]any{
		{"ItemId": "m1", "Name": "Music", "CollectionType": "music",
			"LibraryOptions": map[string]any{"SaveLocalMetadata": false}},
	}}
	snap := LibraryWriteBackSnapshot{
		Version: 1,
		Libraries: []LibrarySaverSnapshotEntry{{
			LibraryID: "m1", LibraryName: "Music",
			SaveLocalMetadata: true, MetadataSavers: []string{"Nfo", "TagLib"},
		}},
	}
	buf, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := RestoreLibraryOptions(context.Background(), tr, testLogger(), "emby", string(buf)); err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(tr.posts) != 1 {
		t.Fatalf("want 1 POST, got %d", len(tr.posts))
	}
	var wrapper struct {
		LibraryOptions map[string]any `json:"LibraryOptions"`
	}
	if err := json.Unmarshal(tr.posts[0].body, &wrapper); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if wrapper.LibraryOptions["SaveLocalMetadata"] != true {
		t.Errorf("SaveLocalMetadata should be replayed as true, got %v", wrapper.LibraryOptions["SaveLocalMetadata"])
	}
	gotSavers, _ := wrapper.LibraryOptions["MetadataSavers"].([]any)
	if len(gotSavers) != 2 || gotSavers[0] != "Nfo" || gotSavers[1] != "TagLib" {
		t.Errorf("MetadataSavers should be [Nfo, TagLib], got %v", gotSavers)
	}
}

func TestRestoreLibraryOptions_SkipsMissingLibraries(t *testing.T) {
	tr := newFakeTransport()
	// Peer has m1 but snapshot references ghost.
	tr.getResponses["/Library/VirtualFolders"] = []any{[]map[string]any{
		{"ItemId": "m1", "Name": "Music", "CollectionType": "music"},
	}}
	snap := LibraryWriteBackSnapshot{
		Version: 1,
		Libraries: []LibrarySaverSnapshotEntry{{
			LibraryID: "ghost", SaveLocalMetadata: true, MetadataSavers: []string{"Nfo"},
		}},
	}
	buf, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := RestoreLibraryOptions(context.Background(), tr, testLogger(), "emby", string(buf)); err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(tr.posts) != 0 {
		t.Errorf("missing-on-peer libraries should not be POSTed, got %d posts", len(tr.posts))
	}
}

func TestRestoreLibraryOptions_NilSaversBecomeEmptySlice(t *testing.T) {
	// Pinning the contract: a snapshot entry with MetadataSavers=nil must
	// still POST an explicit empty array, not a JSON null. The peer treats
	// null as "leave existing savers in place" which would defeat the
	// restore intent on a server that mutated savers in the meantime.
	tr := newFakeTransport()
	tr.getResponses["/Library/VirtualFolders"] = []any{[]map[string]any{
		{"ItemId": "m1", "Name": "Music", "CollectionType": "music"},
	}}
	snap := LibraryWriteBackSnapshot{
		Version: 1,
		Libraries: []LibrarySaverSnapshotEntry{{
			LibraryID: "m1", SaveLocalMetadata: false, MetadataSavers: nil,
		}},
	}
	buf, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := RestoreLibraryOptions(context.Background(), tr, testLogger(), "emby", string(buf)); err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(tr.posts) != 1 {
		t.Fatalf("want 1 POST, got %d", len(tr.posts))
	}
	var wrapper struct {
		LibraryOptions map[string]any `json:"LibraryOptions"`
	}
	if err := json.Unmarshal(tr.posts[0].body, &wrapper); err != nil {
		t.Fatalf("decode: %v", err)
	}
	gotSavers, ok := wrapper.LibraryOptions["MetadataSavers"].([]any)
	if !ok {
		t.Fatalf("MetadataSavers should be a JSON array, got %T (%v)", wrapper.LibraryOptions["MetadataSavers"], wrapper.LibraryOptions["MetadataSavers"])
	}
	if len(gotSavers) != 0 {
		t.Errorf("nil savers should serialize as empty array, got %v", gotSavers)
	}
}
