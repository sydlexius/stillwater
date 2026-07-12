package emby

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sydlexius/stillwater/internal/connection/mediabrowser"
)

// fakeEmbyServer returns a test server that serves GetMusicLibraries and
// records every POST to /Library/VirtualFolders/LibraryOptions so tests can
// assert what Stillwater actually sent to the peer.
type fakeEmbyServer struct {
	mu           sync.Mutex
	libs         []VirtualFolder
	receivedOpts map[string]LibraryOptions // libraryID -> last posted options
}

func newFakeEmbyServer(libs []VirtualFolder) (*httptest.Server, *fakeEmbyServer) {
	f := &fakeEmbyServer{libs: libs, receivedOpts: map[string]LibraryOptions{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/Library/VirtualFolders":
			f.mu.Lock()
			defer f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(f.libs)
		case r.Method == http.MethodPost && r.URL.Path == "/Library/VirtualFolders/LibraryOptions":
			libID := r.URL.Query().Get("Id")
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "read body err: "+err.Error(), http.StatusBadRequest)
				return
			}
			// Emby's endpoint requires the LibraryOptionsInfo wrapper
			// {"Id":"...","LibraryOptions":{...}}. Unwrap in the mock so
			// the test exercises the same code path as real peers.
			var wrapper struct {
				ID             string          `json:"Id"`
				LibraryOptions json.RawMessage `json:"LibraryOptions"`
			}
			if err := json.Unmarshal(body, &wrapper); err != nil {
				http.Error(w, "bad json", http.StatusBadRequest)
				return
			}
			var opts LibraryOptions
			if err := json.Unmarshal(wrapper.LibraryOptions, &opts); err != nil {
				http.Error(w, "bad json", http.StatusBadRequest)
				return
			}
			if wrapper.ID != "" {
				libID = wrapper.ID
			}
			f.mu.Lock()
			f.receivedOpts[libID] = opts
			for i := range f.libs {
				if f.libs[i].ItemID == libID {
					f.libs[i].LibraryOptions = opts
				}
			}
			f.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	return srv, f
}

func (f *fakeEmbyServer) received(libID string) (LibraryOptions, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	opts, ok := f.receivedOpts[libID]
	return opts, ok
}

func TestCheckImageSaverEnabled_True(t *testing.T) {
	srv, _ := newFakeEmbyServer([]VirtualFolder{
		{Name: "Music", ItemID: "m1", CollectionType: "music",
			LibraryOptions: LibraryOptions{SaveLocalMetadata: true}},
	})
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", "", srv.Client(), testLogger())
	on, lib, err := c.CheckImageSaverEnabled(context.Background())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !on || lib != "Music" {
		t.Errorf("got (%v,%q), want (true,Music)", on, lib)
	}
}

func TestCheckImageSaverEnabled_False(t *testing.T) {
	srv, _ := newFakeEmbyServer([]VirtualFolder{
		{Name: "Music", ItemID: "m1", CollectionType: "music",
			LibraryOptions: LibraryOptions{SaveLocalMetadata: false}},
	})
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", "", srv.Client(), testLogger())
	on, _, err := c.CheckImageSaverEnabled(context.Background())
	if err != nil || on {
		t.Errorf("want (false,nil) got (%v,%v)", on, err)
	}
}

func TestSnapshotLibraryOptions_CapturesSaverState(t *testing.T) {
	srv, _ := newFakeEmbyServer([]VirtualFolder{
		{Name: "Music", ItemID: "m1", CollectionType: "music",
			LibraryOptions: LibraryOptions{SaveLocalMetadata: true, MetadataSavers: []string{"Nfo"}}},
	})
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", "", srv.Client(), testLogger())
	snapJSON, err := c.SnapshotLibraryOptions(context.Background())
	if err != nil {
		t.Fatalf("snapshot err = %v", err)
	}
	var snap mediabrowser.LibraryWriteBackSnapshot
	if err := json.Unmarshal([]byte(snapJSON), &snap); err != nil {
		t.Fatalf("bad snapshot json: %v", err)
	}
	if snap.Version != 1 {
		t.Errorf("want version 1, got %d", snap.Version)
	}
	if len(snap.Libraries) != 1 {
		t.Fatalf("want 1 lib, got %d", len(snap.Libraries))
	}
	e := snap.Libraries[0]
	if e.LibraryID != "m1" || !e.SaveLocalMetadata || len(e.MetadataSavers) != 1 || e.MetadataSavers[0] != "Nfo" {
		t.Errorf("unexpected snapshot entry: %+v", e)
	}
}

func TestDisableFileWriteBack_ClearsSaverAndMetadata(t *testing.T) {
	srv, fake := newFakeEmbyServer([]VirtualFolder{
		{Name: "Music", ItemID: "m1", CollectionType: "music",
			LibraryOptions: LibraryOptions{SaveLocalMetadata: true, MetadataSavers: []string{"Nfo"}}},
	})
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", "", srv.Client(), testLogger())
	if err := c.DisableFileWriteBack(context.Background()); err != nil {
		t.Fatalf("disable err = %v", err)
	}
	got, ok := fake.received("m1")
	if !ok {
		t.Fatal("no POST recorded")
	}
	// BOTH SaveLocalMetadata and MetadataSavers must be cleared.
	//
	// This test used to pin the opposite contract -- "MetadataSavers should be
	// preserved unchanged" -- on the belief that SaveLocalMetadata was a master
	// switch and that clearing the saver list NullReferenceException'd real Emby
	// builds. Both halves were false, and pinning them is how #2420 shipped:
	// live Emby 4.9.5.0 with SaveLocalMetadata=false and an armed Nfo saver goes
	// right on writing NFO files into the library. Clearing the savers stops it,
	// returns 204, reads back empty, and leaves the server healthy.
	if got.SaveLocalMetadata {
		t.Errorf("want SaveLocalMetadata cleared, got %+v", got)
	}
	if len(got.MetadataSavers) != 0 {
		t.Errorf("MetadataSavers = %v, want empty -- an armed saver still writes to disk (#2420)", got.MetadataSavers)
	}
}

func TestRestoreLibraryOptions_ReplaysSnapshot(t *testing.T) {
	// Start with a peer whose savers are off (after disable).
	srv, fake := newFakeEmbyServer([]VirtualFolder{
		{Name: "Music", ItemID: "m1", CollectionType: "music",
			LibraryOptions: LibraryOptions{SaveLocalMetadata: false, MetadataSavers: []string{}}},
	})
	defer srv.Close()

	// Snapshot from before disable: user had savers on.
	snap := mediabrowser.LibraryWriteBackSnapshot{
		Version: 1,
		Libraries: []mediabrowser.LibrarySaverSnapshotEntry{{
			LibraryID:         "m1",
			LibraryName:       "Music",
			SaveLocalMetadata: true,
			MetadataSavers:    []string{"Nfo"},
		}},
	}
	buf, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}

	c := NewWithHTTPClient(srv.URL, "key", "", srv.Client(), testLogger())
	if err := c.RestoreLibraryOptions(context.Background(), string(buf)); err != nil {
		t.Fatalf("restore err = %v", err)
	}
	got, ok := fake.received("m1")
	if !ok {
		t.Fatal("no POST recorded")
	}
	if !got.SaveLocalMetadata || len(got.MetadataSavers) != 1 || got.MetadataSavers[0] != "Nfo" {
		t.Errorf("want saver+nfo restored, got %+v", got)
	}
}

func TestRestoreLibraryOptions_SkipsMissingLibrary(t *testing.T) {
	srv, fake := newFakeEmbyServer([]VirtualFolder{
		{Name: "Music", ItemID: "m1", CollectionType: "music"},
	})
	defer srv.Close()

	// Snapshot references a library ID that no longer exists on the peer.
	snap := mediabrowser.LibraryWriteBackSnapshot{
		Version: 1,
		Libraries: []mediabrowser.LibrarySaverSnapshotEntry{{
			LibraryID: "ghost", SaveLocalMetadata: true, MetadataSavers: []string{"Nfo"},
		}},
	}
	buf, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}

	c := NewWithHTTPClient(srv.URL, "key", "", srv.Client(), testLogger())
	if err := c.RestoreLibraryOptions(context.Background(), string(buf)); err != nil {
		t.Fatalf("restore err = %v", err)
	}
	if _, ok := fake.received("ghost"); ok {
		t.Error("should not have POSTed to a missing library")
	}
}

func TestRestoreLibraryOptions_RejectsUnknownVersion(t *testing.T) {
	srv, _ := newFakeEmbyServer(nil)
	defer srv.Close()
	c := NewWithHTTPClient(srv.URL, "key", "", srv.Client(), testLogger())
	err := c.RestoreLibraryOptions(context.Background(), `{"version":999,"libraries":[]}`)
	if err == nil || !strings.Contains(err.Error(), "unsupported snapshot version") {
		t.Errorf("want version error, got %v", err)
	}
}
