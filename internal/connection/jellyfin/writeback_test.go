package jellyfin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/sydlexius/stillwater/internal/connection/mediabrowser"
)

// fakeJellyfinServer mirrors emby/writeback_test.go but targets the
// Jellyfin-shaped endpoints and type layout.
type fakeJellyfinServer struct {
	mu           sync.Mutex
	libs         []VirtualFolder
	receivedOpts map[string]LibraryOptions
}

func newFakeJellyfinServer(libs []VirtualFolder) (*httptest.Server, *fakeJellyfinServer) {
	f := &fakeJellyfinServer{libs: libs, receivedOpts: map[string]LibraryOptions{}}
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
				http.Error(w, "read body error", http.StatusBadRequest)
				return
			}
			// Jellyfin's endpoint expects the LibraryOptionsInfo wrapper
			// {"Id":"...","LibraryOptions":{...}}. Unwrap before decoding
			// into the typed LibraryOptions so the test's mock matches
			// the real peer's contract.
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

// received reports whether the fake server observed a POST for libID and
// returns the recorded LibraryOptions when it did. The ok flag is the only
// reliable way to tell "client never posted" from "client posted a
// zero-value" -- without it, a regression that stops posting altogether
// would silently pass assertions that only inspect the returned fields.
func (f *fakeJellyfinServer) received(libID string) (LibraryOptions, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	opts, ok := f.receivedOpts[libID]
	return opts, ok
}

func TestJellyfinCheckImageSaverEnabled(t *testing.T) {
	srv, _ := newFakeJellyfinServer([]VirtualFolder{
		{Name: "Music", ItemID: "m1", CollectionType: "music",
			LibraryOptions: LibraryOptions{SaveLocalMetadata: true}},
	})
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", "", srv.Client(), testLogger())
	on, lib, err := c.CheckImageSaverEnabled(context.Background())
	if err != nil || !on || lib != "Music" {
		t.Errorf("got (%v,%q,%v), want (true,Music,nil)", on, lib, err)
	}
}

// TestJellyfinCheckImageSaverDisabled exercises the complementary branch where
// SaveLocalMetadata=false. The check must return (on=false, lib="") so the
// gate doesn't flip closed for a peer that already has its saver off. Without
// this case a regression that always reports "enabled" would still pass
// TestJellyfinCheckImageSaverEnabled, since both report on=true.
func TestJellyfinCheckImageSaverDisabled(t *testing.T) {
	srv, _ := newFakeJellyfinServer([]VirtualFolder{
		{Name: "Music", ItemID: "m1", CollectionType: "music",
			LibraryOptions: LibraryOptions{SaveLocalMetadata: false}},
	})
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", "", srv.Client(), testLogger())
	on, lib, err := c.CheckImageSaverEnabled(context.Background())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if on || lib != "" {
		t.Errorf("got (on=%v,lib=%q), want (false,\"\")", on, lib)
	}
}

func TestJellyfinSnapshotAndDisable(t *testing.T) {
	srv, fake := newFakeJellyfinServer([]VirtualFolder{
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
		t.Fatalf("unmarshal snapshot err = %v", err)
	}
	if len(snap.Libraries) != 1 {
		t.Fatalf("snapshot library count = %d, want 1: %+v", len(snap.Libraries), snap)
	}
	if !snap.Libraries[0].SaveLocalMetadata {
		t.Fatalf("snapshot SaveLocalMetadata = false, want true: %+v", snap)
	}
	wantSavers := []string{"Nfo"}
	if !reflect.DeepEqual(snap.Libraries[0].MetadataSavers, wantSavers) {
		t.Fatalf("snapshot MetadataSavers = %v, want %v", snap.Libraries[0].MetadataSavers, wantSavers)
	}

	if err := c.DisableFileWriteBack(context.Background()); err != nil {
		t.Fatalf("disable err = %v", err)
	}
	got, ok := fake.received("m1")
	if !ok {
		t.Fatalf("DisableFileWriteBack did not POST LibraryOptions for m1")
	}
	// SaveLocalMetadata=false is the master kill switch; MetadataSavers is
	// intentionally left alone (see client for rationale). Pin both halves
	// of that contract so a regression that "tidies up" by clearing the
	// saver list trips this test.
	if got.SaveLocalMetadata {
		t.Errorf("SaveLocalMetadata not cleared: %+v", got)
	}
	// wantSavers is declared above for the snapshot assertion; reuse it
	// here to pin the post-disable preservation contract.
	if !reflect.DeepEqual(got.MetadataSavers, wantSavers) {
		t.Errorf("MetadataSavers should be preserved unchanged, got %v want %v", got.MetadataSavers, wantSavers)
	}
}

func TestJellyfinRestoreAppliesSnapshot(t *testing.T) {
	srv, fake := newFakeJellyfinServer([]VirtualFolder{
		{Name: "Music", ItemID: "m1", CollectionType: "music",
			LibraryOptions: LibraryOptions{SaveLocalMetadata: false, MetadataSavers: []string{}}},
	})
	defer srv.Close()

	snap := mediabrowser.LibraryWriteBackSnapshot{
		Version: 1,
		Libraries: []mediabrowser.LibrarySaverSnapshotEntry{{
			LibraryID:         "m1",
			SaveLocalMetadata: true,
			MetadataSavers:    []string{"Nfo"},
		}},
	}
	buf, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal snapshot err = %v", err)
	}

	c := NewWithHTTPClient(srv.URL, "key", "", srv.Client(), testLogger())
	if err := c.RestoreLibraryOptions(context.Background(), string(buf)); err != nil {
		t.Fatalf("restore err = %v", err)
	}
	got, ok := fake.received("m1")
	if !ok {
		t.Fatalf("RestoreLibraryOptions did not POST LibraryOptions for m1")
	}
	if !got.SaveLocalMetadata {
		t.Errorf("restored SaveLocalMetadata = false, want true: %+v", got)
	}
	wantSavers := []string{"Nfo"}
	if !reflect.DeepEqual(got.MetadataSavers, wantSavers) {
		t.Errorf("restored MetadataSavers = %v, want %v", got.MetadataSavers, wantSavers)
	}
}

func TestJellyfinRestoreRejectsUnknownVersion(t *testing.T) {
	srv, _ := newFakeJellyfinServer(nil)
	defer srv.Close()
	c := NewWithHTTPClient(srv.URL, "key", "", srv.Client(), testLogger())
	err := c.RestoreLibraryOptions(context.Background(), `{"version":2,"libraries":[]}`)
	if err == nil || !strings.Contains(err.Error(), "unsupported snapshot version") {
		t.Errorf("want version error, got %v", err)
	}
}
