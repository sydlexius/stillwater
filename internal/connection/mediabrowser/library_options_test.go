package mediabrowser

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"sort"
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

func (f *fakeTransport) GetRaw(_ context.Context, path string) ([]byte, string, error) {
	return nil, "", fmt.Errorf("GetRaw not stubbed for path %s", path)
}

func (f *fakeTransport) PostJSON(_ context.Context, path string, body io.Reader, _ any) error {
	buf, err := io.ReadAll(body)
	if err != nil {
		return fmt.Errorf("read post body: %w", err)
	}
	idx := len(f.posts)
	f.posts = append(f.posts, postCall{path: path, body: buf})
	if idx < len(f.postErrs) {
		return f.postErrs[idx]
	}
	return nil
}

// Do is not exercised by this file's library-options tests (none of them
// touch the image write/delete paths), so it returns a fixed 200 OK with an
// empty body -- satisfying the Transport interface without pretending to
// model behavior nothing here asserts on.
func (f *fakeTransport) Do(_ context.Context, _, _ string, _ io.Reader, _ string) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(nil))}, nil
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
		// MetadataSavers must be CLEARED, not preserved. This assertion used
		// to demand the opposite ("preserved as [Nfo]"), which is what let
		// #2420 ship: SaveLocalMetadata=false does not stop the peer's NFO
		// saver, so leaving the saver armed left the peer writing into a
		// library Stillwater claims to manage. Verified against live Emby
		// 4.9.5.0 and Jellyfin 10.11.10.
		savers, ok := wrapper.LibraryOptions["MetadataSavers"].([]any)
		if !ok {
			t.Errorf("post %d: MetadataSavers = %#v, want an empty JSON array (a null list is a shape the peer rejects)",
				i, wrapper.LibraryOptions["MetadataSavers"])
			continue
		}
		if len(savers) != 0 {
			t.Errorf("post %d: MetadataSavers = %v, want empty -- an armed saver still writes to disk", i, savers)
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

// statefulPeer models the one behavior that matters for #2420: the peer STORES
// its library options, applies a POST as a full REPLACE of LibraryOptions (which
// is what the real endpoint does), and -- crucially -- decides whether it would
// write an NFO to disk from the SAVER LIST, not from SaveLocalMetadata.
//
// That last part is the whole defect. The shipped code set
// SaveLocalMetadata=false and left MetadataSavers=["Nfo"], and both live peers
// (Emby 4.9.5.0, Jellyfin 10.11.10) went on writing artist.nfo into the library.
// A fake that only records POST bodies cannot express that, which is exactly why
// the old test passed while the peer kept writing: it asserted what Stillwater
// SENT, never what the peer would DO.
type statefulPeer struct {
	libs map[string]map[string]any
}

func newStatefulPeer(libs map[string]map[string]any) *statefulPeer {
	return &statefulPeer{libs: libs}
}

func (p *statefulPeer) Get(_ context.Context, path string, result any) error {
	if path != "/Library/VirtualFolders" {
		return fmt.Errorf("unexpected GET %s", path)
	}
	folders := make([]map[string]any, 0, len(p.libs))
	for id, opts := range p.libs {
		folders = append(folders, map[string]any{
			"ItemId": id, "Name": id, "CollectionType": "music", "LibraryOptions": opts,
		})
	}
	sort.Slice(folders, func(i, j int) bool {
		return folders[i]["ItemId"].(string) < folders[j]["ItemId"].(string)
	})
	buf, err := json.Marshal(folders)
	if err != nil {
		return err
	}
	return json.Unmarshal(buf, result)
}

func (p *statefulPeer) GetRaw(_ context.Context, path string) ([]byte, string, error) {
	return nil, "", fmt.Errorf("GetRaw not stubbed for path %s", path)
}

func (p *statefulPeer) PostJSON(_ context.Context, _ string, body io.Reader, _ any) error {
	var wrapper struct {
		ID             string         `json:"Id"`
		LibraryOptions map[string]any `json:"LibraryOptions"`
	}
	buf, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(buf, &wrapper); err != nil {
		return err
	}
	if _, ok := p.libs[wrapper.ID]; !ok {
		return fmt.Errorf("unknown library %q", wrapper.ID)
	}
	// The real endpoint REPLACES LibraryOptions wholesale.
	p.libs[wrapper.ID] = wrapper.LibraryOptions
	return nil
}

// Do is not exercised by the #2420 save-back scenario this double models
// (it only drives the library-options snapshot/disable/restore flow via
// Get/PostJSON), so it returns a fixed 200 OK -- satisfying the Transport
// interface without pretending to model behavior nothing here asserts on.
func (p *statefulPeer) Do(_ context.Context, _, _ string, _ io.Reader, _ string) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(nil))}, nil
}

// wouldWriteNFO reports whether this peer would still write an NFO into the
// library. Keyed on the saver list, because that is what the real servers do.
func (p *statefulPeer) wouldWriteNFO(libID string) bool {
	savers, _ := p.libs[libID]["MetadataSavers"].([]any)
	return len(savers) > 0
}

// TestDisableFileWriteBack_PeerCanNoLongerWriteNFO is the #2420 guard. It asks
// the only question that matters: after Stillwater says it manages this
// library, would the peer still write to it?
// EVERY music library must be disarmed, not just the first: a peer commonly has
// several (Music, Audiobooks, a classical root), and one still-armed library is
// enough to re-create a directory and resurrect a duplicate.
func TestDisableFileWriteBack_PeerCanNoLongerWriteNFO(t *testing.T) {
	peer := newStatefulPeer(map[string]map[string]any{
		"m1": {"SaveLocalMetadata": true, "MetadataSavers": []any{"Nfo"}},
		"m2": {"SaveLocalMetadata": true, "MetadataSavers": []any{"Nfo"}},
	})

	// Precondition: assert the fake actually models a peer that writes. Without
	// this the test could pass vacuously against a peer that never wrote at all.
	for _, id := range []string{"m1", "m2"} {
		if !peer.wouldWriteNFO(id) {
			t.Fatalf("precondition: library %q must start out able to write NFOs, or this test proves nothing", id)
		}
	}

	if err := DisableFileWriteBack(context.Background(), peer, testLogger(), "emby"); err != nil {
		t.Fatalf("DisableFileWriteBack: %v", err)
	}

	// THE ASSERTION THE OLD TEST COULD NOT MAKE. Not "did we send the flag?"
	// but "can the peer still write?"
	for _, id := range []string{"m1", "m2"} {
		if peer.wouldWriteNFO(id) {
			t.Errorf("library %q would STILL write an NFO after DisableFileWriteBack: MetadataSavers=%v. "+
				"SaveLocalMetadata=false does not disarm the saver; leaving it armed is #2420, and it is "+
				"what re-creates a renamed-away directory and resurrects a duplicate artist (#2380)",
				id, peer.libs[id]["MetadataSavers"])
		}
		if peer.libs[id]["SaveLocalMetadata"] != false {
			t.Errorf("library %q: SaveLocalMetadata = %v, want false", id, peer.libs[id]["SaveLocalMetadata"])
		}
	}
}

// TestDisableFileWriteBack_PreservesUnmodeledFields guards the round-trip: the
// peer's options carry fields Stillwater does not model, and dropping them makes
// the real server crash. Only the two write-back keys may change.
func TestDisableFileWriteBack_PreservesUnmodeledFields(t *testing.T) {
	peer := newStatefulPeer(map[string]map[string]any{
		"m1": {
			"SaveLocalMetadata":            true,
			"MetadataSavers":               []any{"Nfo"},
			"EnableRealtimeMonitor":        true,
			"PathInfos":                    []any{map[string]any{"Path": "/music"}},
			"TypeOptions":                  []any{map[string]any{"Type": "MusicArtist"}},
			"AutomaticRefreshIntervalDays": float64(30),
		},
	})

	if err := DisableFileWriteBack(context.Background(), peer, testLogger(), "jellyfin"); err != nil {
		t.Fatalf("DisableFileWriteBack: %v", err)
	}

	got := peer.libs["m1"]
	for _, k := range []string{"EnableRealtimeMonitor", "PathInfos", "TypeOptions", "AutomaticRefreshIntervalDays"} {
		if _, ok := got[k]; !ok {
			t.Errorf("unmodeled field %q was DROPPED; the peer 500s when options come back incomplete", k)
		}
	}
}

// TestDisableThenRestore_ReArmsTheSaver proves the clear is reversible: turning
// the toggle off must give the user their saver list back, or we have silently
// reconfigured their server.
func TestDisableThenRestore_ReArmsTheSaver(t *testing.T) {
	peer := newStatefulPeer(map[string]map[string]any{
		"m1": {"SaveLocalMetadata": true, "MetadataSavers": []any{"Nfo"}},
	})

	// Build the snapshot the way the caller does (Router.snapshotLibraryOptions).
	libs, err := GetMusicLibrariesRaw(context.Background(), peer, testLogger(), "emby")
	if err != nil {
		t.Fatalf("GetMusicLibrariesRaw: %v", err)
	}
	entries := make([]LibrarySaverSnapshotEntry, 0, len(libs))
	for _, lib := range libs {
		savers := []string{}
		if raw, ok := lib.Options["MetadataSavers"].([]any); ok {
			for _, s := range raw {
				if str, ok := s.(string); ok {
					savers = append(savers, str)
				}
			}
		}
		slm, _ := lib.Options["SaveLocalMetadata"].(bool)
		entries = append(entries, LibrarySaverSnapshotEntry{
			LibraryID: lib.ID, LibraryName: lib.Name, SaveLocalMetadata: slm, MetadataSavers: savers,
		})
	}
	snapshot, err := BuildSnapshot(entries)
	if err != nil {
		t.Fatalf("BuildSnapshot: %v", err)
	}

	if err := DisableFileWriteBack(context.Background(), peer, testLogger(), "emby"); err != nil {
		t.Fatalf("DisableFileWriteBack: %v", err)
	}
	if peer.wouldWriteNFO("m1") {
		t.Fatal("saver still armed after disable")
	}

	if err := RestoreLibraryOptions(context.Background(), peer, testLogger(), "emby", snapshot); err != nil {
		t.Fatalf("RestoreLibraryOptions: %v", err)
	}
	if !peer.wouldWriteNFO("m1") {
		t.Errorf("restore did NOT re-arm the user's saver list: MetadataSavers=%v, want [Nfo]. "+
			"Disabling the toggle must hand the server back as we found it",
			peer.libs["m1"]["MetadataSavers"])
	}
	if peer.libs["m1"]["SaveLocalMetadata"] != true {
		t.Errorf("SaveLocalMetadata = %v, want true restored", peer.libs["m1"]["SaveLocalMetadata"])
	}
}
