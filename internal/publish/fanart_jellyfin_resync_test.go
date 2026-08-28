package publish

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
)

// TestUploadFanartForSync_JellyfinRoutesThroughFullResync is the #3135
// regression: it asserts the REQUEST SEQUENCE a Jellyfin-typed connection
// receives for a fanart replace is delete-every-backdrop-then-reupload-the-
// full-set, never a single indexed POST -- because a real Jellyfin 10.11.10
// ignores the index on that single POST and appends instead (measured in
// #3135, pinned by TestLiveJellyfin_IndexedUploadStillIgnoresIndex_WorkedAroundNotFixed
// in live_backdrop_replace_integration_test.go).
//
// The server starts with TWO backdrops already present (proving the resync
// clears an EXISTING set, not just the empty-platform degenerate case), and
// the local directory holds two local files (fanart.jpg index 0, fanart2.jpg
// index 1). A pre-fix build (routing Jellyfin through UploadImageAtIndex(...,
// 0, ...) the same way Emby does) would issue exactly one POST to
// /Images/Backdrop/0 and zero DELETE calls -- this test is shown to fail against
// that shape in the PR report.
func TestUploadFanartForSync_JellyfinRoutesThroughFullResync(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "fanart.jpg"), bandJPEG(t, 10))
	writeFile(t, filepath.Join(dir, "fanart2.jpg"), bandJPEG(t, 11))

	var mu sync.Mutex
	var deletes []string // DELETE paths, in call order
	var uploads []string // POST paths, in call order
	backdropCount := 2   // platform starts with 2 existing backdrops

	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/Items/"):
			// GetArtistDetail: report the CURRENT backdrop count so the
			// resync's delete loop knows how many slots to clear.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			tags := make([]string, backdropCount)
			for i := range tags {
				tags[i] = "tag" + strconv.Itoa(i)
			}
			body, _ := json.Marshal(map[string]any{"BackdropImageTags": tags})
			_, _ = w.Write(body)
		case r.Method == http.MethodDelete:
			deletes = append(deletes, r.URL.Path)
			if backdropCount > 0 {
				backdropCount--
			}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost:
			uploads = append(uploads, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer httpSrv.Close()

	p := New(Deps{
		Logger: silentLogger(),
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c-jf", PlatformArtistID: "p1"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c-jf": {ID: "c-jf", Name: "my-jellyfin", Type: connection.TypeJellyfin, URL: httpSrv.URL, Enabled: true, Status: "ok", Jellyfin: &connection.JellyfinConfig{PlatformUserID: "u1", FeatureImageWrite: true}},
		}},
	})

	warnings := p.SyncImageToPlatforms(context.Background(), &artist.Artist{ID: "a1", Name: "Test Artist", Path: dir}, "fanart")
	if len(warnings) != 0 {
		t.Fatalf("expected no warnings; got %v", warnings)
	}

	mu.Lock()
	gotDeletes := append([]string(nil), deletes...)
	gotUploads := append([]string(nil), uploads...)
	mu.Unlock()

	// PRECONDITION-STYLE COUNT ASSERTIONS (not a label): the whole point of
	// #3135 is that the fix issues MORE than one request -- a single
	// indexed POST is exactly what the pre-fix code did and exactly what
	// does not work on Jellyfin.
	if len(gotDeletes) != 2 {
		t.Fatalf("DELETE calls = %d, want 2 (clear both existing backdrops); got paths %v", len(gotDeletes), gotDeletes)
	}
	// High-index-first: DeleteImageAtIndexRaw's own doc comment says the
	// peer re-indexes after every delete, so index 1 must be deleted before
	// index 0 or the second delete targets the wrong (re-indexed) slot.
	if gotDeletes[0] != "/Items/p1/Images/Backdrop/1" || gotDeletes[1] != "/Items/p1/Images/Backdrop/0" {
		t.Errorf("delete order = %v, want [.../Backdrop/1, .../Backdrop/0] (high-index-first)", gotDeletes)
	}
	if len(gotUploads) != 2 {
		t.Fatalf("POST (indexed upload) calls = %d, want 2 (reupload the full local set); got paths %v", len(gotUploads), gotUploads)
	}
	// Order-independent set check: both local slots must be re-sent to
	// their own index.
	wantUploads := map[string]bool{"/Items/p1/Images/Backdrop/0": true, "/Items/p1/Images/Backdrop/1": true}
	for _, p := range gotUploads {
		if !wantUploads[p] {
			t.Errorf("unexpected upload path %q", p)
		}
		delete(wantUploads, p)
	}
	if len(wantUploads) != 0 {
		t.Errorf("missing expected upload path(s): %v", wantUploads)
	}
}

// TestUploadFanartForSync_JellyfinNoopSkipsResyncEntirely is the fanartTargetNoop
// preservation guard (#3135 explicit hazard): a sync whose bytes already
// match the platform's slot-0 content must decide noop and must NEVER run
// the destructive delete-all-and-reupload sequence. If this regressed, an
// operator re-saving the SAME fanart (a retry after a lost response, or an
// idle background reconcile pass) would blow away and rebuild every
// Jellyfin backdrop for no reason -- and during the rebuild window the
// platform genuinely has zero backdrops.
//
// The server reports ONE existing backdrop whose bytes (via GetArtistBackdrop)
// are byte-identical to the local primary being synced, so
// resolveFanartReplaceTarget must resolve fanartTargetNoop. Zero DELETE calls and
// zero POSTs proves the noop short-circuit fired before the platform-
// capability branch, not merely that SOME request count happened to match.
func TestUploadFanartForSync_JellyfinNoopSkipsResyncEntirely(t *testing.T) {
	dir := t.TempDir()
	primary := bandJPEG(t, 20)
	writeFile(t, filepath.Join(dir, "fanart.jpg"), primary)

	var mu sync.Mutex
	var deletes, uploads []string

	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/Items/p1"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			body, _ := json.Marshal(map[string]any{"BackdropImageTags": []string{"tag0"}})
			_, _ = w.Write(body)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/Images/Backdrop/0"):
			// GetArtistBackdrop(index 0): return the SAME bytes as the local
			// primary, so the resolver's exact-byte comparison finds a match
			// and decides noop.
			w.Header().Set("Content-Type", "image/jpeg")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(primary)
		case r.Method == http.MethodDelete:
			deletes = append(deletes, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost:
			uploads = append(uploads, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer httpSrv.Close()

	p := New(Deps{
		Logger: silentLogger(),
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c-jf", PlatformArtistID: "p1"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c-jf": {ID: "c-jf", Name: "my-jellyfin", Type: connection.TypeJellyfin, URL: httpSrv.URL, Enabled: true, Status: "ok", Jellyfin: &connection.JellyfinConfig{PlatformUserID: "u1", FeatureImageWrite: true}},
		}},
	})

	warnings := p.SyncImageToPlatforms(context.Background(), &artist.Artist{ID: "a1", Name: "Test Artist", Path: dir}, "fanart")
	if len(warnings) != 0 {
		t.Fatalf("expected no warnings; got %v", warnings)
	}

	mu.Lock()
	gotDeletes := append([]string(nil), deletes...)
	gotUploads := append([]string(nil), uploads...)
	mu.Unlock()

	if len(gotDeletes) != 0 {
		t.Errorf("DELETE calls = %d, want 0 -- a noop decision must never run the destructive resync; got %v", len(gotDeletes), gotDeletes)
	}
	if len(gotUploads) != 0 {
		t.Errorf("POST calls = %d, want 0 -- a noop decision must never upload; got %v", len(gotUploads), gotUploads)
	}
}

// TestUploadFanartForSync_EmbyStillUsesIndexedReplace_NotResync is the
// contrast case: an Emby-typed connection must NOT be rerouted through the
// resync path #3135 adds. Only one POST (the indexed in-place replace) and
// zero DELETE calls -- exactly TestSyncImageToPlatforms_FanartUsesIndexedReplacePath's
// existing assertion, restated here to prove the new platform-capability
// branch in uploadFanartForSync does not change Emby's behavior at all.
func TestUploadFanartForSync_EmbyStillUsesIndexedReplace_NotResync(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "fanart.jpg"), bandJPEG(t, 30))

	var mu sync.Mutex
	var deletes, uploads []string

	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("{}")) // empty platform: BackdropCount 0
		case http.MethodDelete:
			deletes = append(deletes, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		case http.MethodPost:
			uploads = append(uploads, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer httpSrv.Close()

	p := New(Deps{
		Logger: silentLogger(),
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c-emby", PlatformArtistID: "p1"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c-emby": {ID: "c-emby", Name: "my-emby", Type: connection.TypeEmby, URL: httpSrv.URL, Enabled: true, Status: "ok", Emby: &connection.EmbyConfig{PlatformUserID: "u1", FeatureImageWrite: true}},
		}},
	})

	warnings := p.SyncImageToPlatforms(context.Background(), &artist.Artist{ID: "a1", Name: "Test Artist", Path: dir}, "fanart")
	if len(warnings) != 0 {
		t.Fatalf("expected no warnings; got %v", warnings)
	}

	mu.Lock()
	gotDeletes := append([]string(nil), deletes...)
	gotUploads := append([]string(nil), uploads...)
	mu.Unlock()

	if len(gotDeletes) != 0 {
		t.Errorf("DELETE calls = %d, want 0 (Emby must never take the resync path)", len(gotDeletes))
	}
	if len(gotUploads) != 1 || gotUploads[0] != "/Items/p1/Images/Backdrop/0" {
		t.Errorf("upload paths = %v, want exactly [/Items/p1/Images/Backdrop/0] (Emby's ordinary indexed replace)", gotUploads)
	}
}

// TestUploadFanartFullResyncForSync_PartialDeleteFailureStillReuploadsAll
// covers the "continue and report" contract uploadFanartFullResyncForSync's
// doc comment states: one DELETE failing must not abort the clear loop or
// skip the reupload loop, because stopping partway would leave the artist
// with SOME backdrops deleted and NONE restored -- strictly worse than
// continuing. Forces index 1's delete to fail (index 0 still deleted fine)
// and asserts BOTH local files are still reuploaded, plus a warning names
// the failure.
func TestUploadFanartFullResyncForSync_PartialDeleteFailureStillReuploadsAll(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "fanart.jpg"), bandJPEG(t, 40))
	writeFile(t, filepath.Join(dir, "fanart2.jpg"), bandJPEG(t, 41))

	var mu sync.Mutex
	var deletes, uploads []string
	backdropCount := 2

	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			tags := make([]string, backdropCount)
			for i := range tags {
				tags[i] = "tag" + strconv.Itoa(i)
			}
			body, _ := json.Marshal(map[string]any{"BackdropImageTags": tags})
			_, _ = w.Write(body)
		case http.MethodDelete:
			deletes = append(deletes, r.URL.Path)
			if strings.HasSuffix(r.URL.Path, "/1") {
				// The high-index delete (issued FIRST) fails.
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			backdropCount--
			w.WriteHeader(http.StatusNoContent)
		case http.MethodPost:
			uploads = append(uploads, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer httpSrv.Close()

	p := New(Deps{
		Logger: silentLogger(),
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c-jf", PlatformArtistID: "p1"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c-jf": {ID: "c-jf", Name: "my-jellyfin", Type: connection.TypeJellyfin, URL: httpSrv.URL, Enabled: true, Status: "ok", Jellyfin: &connection.JellyfinConfig{PlatformUserID: "u1", FeatureImageWrite: true}},
		}},
	})

	warnings := p.SyncImageToPlatforms(context.Background(), &artist.Artist{ID: "a1", Name: "Test Artist", Path: dir}, "fanart")

	mu.Lock()
	gotDeletes := append([]string(nil), deletes...)
	gotUploads := append([]string(nil), uploads...)
	mu.Unlock()

	if len(gotDeletes) != 2 {
		t.Fatalf("DELETE calls = %d, want 2 (both attempted despite the first failing); got %v", len(gotDeletes), gotDeletes)
	}
	if len(gotUploads) != 2 {
		t.Fatalf("POST (reupload) calls = %d, want 2 (both local slots reuploaded despite the partial delete failure); got %v", len(gotUploads), gotUploads)
	}
	foundDeleteWarning := false
	for _, w := range warnings {
		if strings.Contains(w, "could not clear backdrop") {
			foundDeleteWarning = true
		}
	}
	if !foundDeleteWarning {
		t.Errorf("warnings = %v, want one mentioning the failed clear", warnings)
	}
}

// fakeResyncClient is a minimal fanartResyncClient double for the
// uploadFanartFullResyncForSync unit tests below that need to control
// GetArtistDetail/DeleteImageAtIndex/UploadImageAtIndex errors directly,
// rather than going through an httptest server. Each *Err field, when set,
// makes the corresponding call fail; otherwise GetArtistDetail reports
// backdropCount and the delete/upload calls succeed.
type fakeResyncClient struct {
	backdropCount int
	detailErr     error
	deleteErr     error
	uploadErr     error
}

func (f *fakeResyncClient) GetArtistDetail(_ context.Context, _ string) (*connection.ArtistPlatformState, error) {
	if f.detailErr != nil {
		return nil, f.detailErr
	}
	return &connection.ArtistPlatformState{BackdropCount: f.backdropCount}, nil
}

func (f *fakeResyncClient) GetArtistBackdrop(_ context.Context, _ string, _ int) ([]byte, string, error) {
	return nil, "", errors.New("fakeResyncClient: GetArtistBackdrop should never be called by the resync path")
}

func (f *fakeResyncClient) DeleteImageAtIndex(_ context.Context, _ string, _ string, _ int) error {
	return f.deleteErr
}

func (f *fakeResyncClient) UploadImageAtIndex(_ context.Context, _ string, _ string, _ int, _ []byte, _ string) error {
	return f.uploadErr
}

// withFakeFanartResyncClient swaps newFanartResyncClient to hand every
// construction the given fake, restoring the real one on cleanup. Mirrors
// withFakePhashClient's pattern in phash_platform_test.go.
func withFakeFanartResyncClient(t *testing.T, fake fanartResyncClient) {
	t.Helper()
	prev := newFanartResyncClient
	newFanartResyncClient = func(_ *connection.Connection, _ *slog.Logger) fanartResyncClient { return fake }
	t.Cleanup(func() { newFanartResyncClient = prev })
}

// TestUploadFanartFullResyncForSync_UnsupportedConnectionTypeWarnsLoudly
// covers the nil-client branch: a connection type newFanartResyncClient does
// not support (Lidarr) must warn and report uploaded=false, never panic or
// silently proceed.
func TestUploadFanartFullResyncForSync_UnsupportedConnectionTypeWarnsLoudly(t *testing.T) {
	p := New(Deps{Logger: silentLogger()})
	conn := &connection.Connection{ID: "c-lid", Name: "my-lidarr", Type: connection.TypeLidarr}
	uploaded, warning := p.uploadFanartFullResyncForSync(context.Background(), &artist.Artist{ID: "a1", Path: t.TempDir()}, artist.PlatformID{ConnectionID: "c-lid", PlatformArtistID: "p1"}, conn)
	if uploaded {
		t.Error("uploaded = true, want false for an unsupported connection type")
	}
	if !strings.Contains(warning, "unsupported connection type") {
		t.Errorf("warning = %q, want it to mention unsupported connection type", warning)
	}
}

// TestUploadFanartFullResyncForSync_NoImageDirWarns covers ImageDir
// returning "" (no artist path, no image cache dir configured).
func TestUploadFanartFullResyncForSync_NoImageDirWarns(t *testing.T) {
	p := New(Deps{Logger: silentLogger()})
	conn := &connection.Connection{ID: "c-jf", Name: "my-jellyfin", Type: connection.TypeJellyfin}
	uploaded, warning := p.uploadFanartFullResyncForSync(context.Background(), &artist.Artist{ID: "a1"}, artist.PlatformID{ConnectionID: "c-jf", PlatformArtistID: "p1"}, conn)
	if uploaded {
		t.Error("uploaded = true, want false when no image directory is configured")
	}
	if !strings.Contains(warning, "no image directory") {
		t.Errorf("warning = %q, want it to mention the missing image directory", warning)
	}
}

// TestUploadFanartFullResyncForSync_EmptyLocalSetWarns covers the
// zero-local-fanart branch: an artist directory that exists but holds no
// fanart files at all (resolveFanartReplaceTarget already ruled out noop
// against the primary before this function is called, so reaching here with
// nothing on disk means a race, not the ordinary path).
func TestUploadFanartFullResyncForSync_EmptyLocalSetWarns(t *testing.T) {
	p := New(Deps{Logger: silentLogger()})
	conn := &connection.Connection{ID: "c-jf", Name: "my-jellyfin", Type: connection.TypeJellyfin}
	uploaded, warning := p.uploadFanartFullResyncForSync(context.Background(), &artist.Artist{ID: "a1", Path: t.TempDir()}, artist.PlatformID{ConnectionID: "c-jf", PlatformArtistID: "p1"}, conn)
	if uploaded {
		t.Error("uploaded = true, want false when the local fanart set is empty")
	}
	if !strings.Contains(warning, "no local fanart found") {
		t.Errorf("warning = %q, want it to mention no local fanart found", warning)
	}
}

// TestUploadFanartFullResyncForSync_DetailErrorWarnsAndSkips covers the
// GetArtistDetail failure branch: the resync must not attempt any delete or
// upload when it cannot even read the platform's current backdrop count.
func TestUploadFanartFullResyncForSync_DetailErrorWarnsAndSkips(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "fanart.jpg"), bandJPEG(t, 50))

	fake := &fakeResyncClient{detailErr: errors.New("platform unreachable")}
	withFakeFanartResyncClient(t, fake)

	p := New(Deps{Logger: silentLogger()})
	conn := &connection.Connection{ID: "c-jf", Name: "my-jellyfin", Type: connection.TypeJellyfin}
	uploaded, warning := p.uploadFanartFullResyncForSync(context.Background(), &artist.Artist{ID: "a1", Path: dir}, artist.PlatformID{ConnectionID: "c-jf", PlatformArtistID: "p1"}, conn)
	if uploaded {
		t.Error("uploaded = true, want false when GetArtistDetail fails")
	}
	if !strings.Contains(warning, "could not read platform backdrop state") {
		t.Errorf("warning = %q, want it to mention the backdrop-state read failure", warning)
	}
}

// TestUploadFanartFullResyncForSync_UploadErrorIsReportedNotSwallowed covers
// the reupload-failure branch: a failing UploadImageAtIndex must appear in
// the returned warning, and (since the delete loop ran against a platform
// that reported zero backdrops) uploaded must still be false because no
// upload actually succeeded.
func TestUploadFanartFullResyncForSync_UploadErrorIsReportedNotSwallowed(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "fanart.jpg"), bandJPEG(t, 51))

	fake := &fakeResyncClient{backdropCount: 0, uploadErr: errors.New("upload rejected")}
	withFakeFanartResyncClient(t, fake)

	p := New(Deps{Logger: silentLogger()})
	conn := &connection.Connection{ID: "c-jf", Name: "my-jellyfin", Type: connection.TypeJellyfin}
	uploaded, warning := p.uploadFanartFullResyncForSync(context.Background(), &artist.Artist{ID: "a1", Path: dir}, artist.PlatformID{ConnectionID: "c-jf", PlatformArtistID: "p1"}, conn)
	if uploaded {
		t.Error("uploaded = true, want false when every upload in the resync fails")
	}
	if !strings.Contains(warning, "failed to upload backdrop") {
		t.Errorf("warning = %q, want it to mention the upload failure", warning)
	}
}

// TestUploadFanartForSync_JellyfinDegradedSlotRefusesResync_NoDataLoss is the
// #3145 hostile-review regression: it wires the exact misbehavior the
// reviewer reproduced -- a local fanart set with one file over
// maxFanartSnapshotFileBytes, synced against a Jellyfin connection whose
// platform already holds backdrops -- and proves the platform's existing
// backdrops survive UNTOUCHED (byte-for-byte), rather than asserting a
// label like "no error" or "a warning was returned".
//
// Before the #3145 fix, uploadFanartFullResyncForSync's delete loop cleared
// every platform backdrop based on the PLATFORM's own count, with no
// awareness that one local slot had degraded to nil data during
// snapshotFanart. The upload loop then skipped the nil-data slot, so this
// scenario (2 local files, one over-cap, platform starting with 2
// backdrops) previously ended with BackdropCount 0 on the platform:
// deletes=2, uploads=1 -- a captured file replaced, but the DEGRADED file's
// platform copy destroyed with nothing to restore it. That is
// SIZE-CAP DESTRUCTION, strictly worse than the pre-#3135 state where a
// Jellyfin sync never deleted anything at all.
//
// This test runs the REAL production path end to end
// (SyncImageToPlatforms -> uploadOneImageForSync -> uploadFanartForSync ->
// uploadFanartFullResyncForSync), against a stateful fake Jellyfin server
// that tracks actual backdrop bytes per index (so DELETE/POST genuinely
// mutate platform state exactly like a real peer would), and asserts:
//  1. Zero DELETE requests reached the server.
//  2. Zero POST (upload) requests reached the server.
//  3. The platform's BackdropCount is still exactly 2 afterward.
//  4. Both original backdrops' CONTENT is byte-identical to what was
//     seeded -- not merely that the count coincidentally matches.
//  5. The returned warning names the degraded slot, so the operator is not
//     left guessing why nothing happened.
func TestUploadFanartForSync_JellyfinDegradedSlotRefusesResync_NoDataLoss(t *testing.T) {
	dir := t.TempDir()
	// A small, distinct primary -- this is `data` in uploadFanartForSync's
	// signature, the file resolveFanartReplaceTarget resolves against.
	primary := bandJPEG(t, 60)
	writeFile(t, filepath.Join(dir, "fanart.jpg"), primary)
	// fanart2.jpg: genuinely OVER maxFanartSnapshotFileBytes (12 MiB), so
	// snapshotFanart's pre-read stat refuses it and degrades this slot to
	// nil data -- not a fake/mocked degrade, the real budget check firing
	// on a real oversize file.
	oversize := make([]byte, (12<<20)+1)
	for i := range oversize {
		oversize[i] = byte(i) // non-zero content; irrelevant to the cap, just avoids a suspiciously-uniform fixture
	}
	writeFile(t, filepath.Join(dir, "fanart2.jpg"), oversize)

	// Platform starts with TWO existing, byte-distinct backdrops -- the
	// state the delete loop would have cleared pre-fix.
	seedA, seedB := bandJPEG(t, 61), bandJPEG(t, 62)

	var mu sync.Mutex
	backdrops := [][]byte{seedA, seedB}
	var deletes []string
	var uploads []string

	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/Images/Backdrop/"):
			// GetArtistBackdrop: serve the CURRENT bytes at this index, so
			// resolveFanartReplaceTarget's content-hash comparisons see
			// real, distinct backdrop content rather than a canned stub.
			parts := strings.Split(r.URL.Path, "/")
			idx, convErr := strconv.Atoi(parts[len(parts)-1])
			if convErr != nil || idx < 0 || idx >= len(backdrops) {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "image/jpeg")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(backdrops[idx])
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/Items/"):
			// GetArtistDetail: report the CURRENT backdrop count.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			tags := make([]string, len(backdrops))
			for i := range tags {
				tags[i] = "tag" + strconv.Itoa(i)
			}
			body, _ := json.Marshal(map[string]any{"BackdropImageTags": tags})
			_, _ = w.Write(body)
		case r.Method == http.MethodDelete:
			deletes = append(deletes, r.URL.Path)
			parts := strings.Split(r.URL.Path, "/")
			idx, convErr := strconv.Atoi(parts[len(parts)-1])
			if convErr == nil && idx >= 0 && idx < len(backdrops) {
				backdrops = append(backdrops[:idx], backdrops[idx+1:]...)
			}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost:
			uploads = append(uploads, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer httpSrv.Close()

	p := New(Deps{
		Logger: silentLogger(),
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c-jf", PlatformArtistID: "p1"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c-jf": {ID: "c-jf", Name: "my-jellyfin", Type: connection.TypeJellyfin, URL: httpSrv.URL, Enabled: true, Status: "ok", Jellyfin: &connection.JellyfinConfig{PlatformUserID: "u1", FeatureImageWrite: true}},
		}},
	})

	warnings := p.SyncImageToPlatforms(context.Background(), &artist.Artist{ID: "a1", Name: "Test Artist", Path: dir}, "fanart")

	mu.Lock()
	gotDeletes := append([]string(nil), deletes...)
	gotUploads := append([]string(nil), uploads...)
	gotBackdrops := append([][]byte(nil), backdrops...)
	mu.Unlock()

	// THE #3145 ASSERTIONS: count AND content, not a label.
	if len(gotDeletes) != 0 {
		t.Errorf("DELETE calls = %d, want 0 -- a degraded local slot must refuse the WHOLE resync before any destructive call, got paths %v", len(gotDeletes), gotDeletes)
	}
	if len(gotUploads) != 0 {
		t.Errorf("POST calls = %d, want 0 -- no partial resync may proceed either, got paths %v", len(gotUploads), gotUploads)
	}
	if len(gotBackdrops) != 2 {
		t.Fatalf("platform BackdropCount after sync = %d, want 2 (unchanged) -- the platform's existing backdrops must survive a refused resync", len(gotBackdrops))
	}
	if !bytesEqual(gotBackdrops[0], seedA) {
		t.Error("platform backdrop 0 content changed -- it must be byte-identical to what was seeded")
	}
	if !bytesEqual(gotBackdrops[1], seedB) {
		t.Error("platform backdrop 1 content changed -- it must be byte-identical to what was seeded")
	}

	foundWarning := false
	for _, w := range warnings {
		if strings.Contains(w, "could not be captured") {
			foundWarning = true
		}
	}
	if !foundWarning {
		t.Errorf("warnings = %v, want one naming the degraded slot", warnings)
	}
}

// TestUploadFanartForSync_JellyfinDegradedSlot_PlatformOutnumbersLocal is the
// #3145 review's "harder case": the platform holds MORE backdrops than the
// local set has captured slots at all (5 platform backdrops, 2 local files,
// one of those two degraded). Proves the refusal leaves every one of the
// FIVE platform backdrops untouched -- not just the two the local set would
// have addressed -- so an operator's platform-side backdrops the local sync
// was never going to touch cannot be stranded or destroyed by a refused
// resync either.
func TestUploadFanartForSync_JellyfinDegradedSlot_PlatformOutnumbersLocal(t *testing.T) {
	dir := t.TempDir()
	primary := bandJPEG(t, 70)
	writeFile(t, filepath.Join(dir, "fanart.jpg"), primary)
	oversize := make([]byte, (12<<20)+1)
	for i := range oversize {
		oversize[i] = byte(i * 7)
	}
	writeFile(t, filepath.Join(dir, "fanart2.jpg"), oversize)

	// FIVE existing, byte-distinct platform backdrops -- outnumbering the
	// local set (2 files) entirely.
	seeds := [][]byte{bandJPEG(t, 71), bandJPEG(t, 72), bandJPEG(t, 73), bandJPEG(t, 74), bandJPEG(t, 75)}

	var mu sync.Mutex
	backdrops := append([][]byte(nil), seeds...)
	var deletes, uploads []string

	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/Images/Backdrop/"):
			parts := strings.Split(r.URL.Path, "/")
			idx, convErr := strconv.Atoi(parts[len(parts)-1])
			if convErr != nil || idx < 0 || idx >= len(backdrops) {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "image/jpeg")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(backdrops[idx])
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/Items/"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			tags := make([]string, len(backdrops))
			for i := range tags {
				tags[i] = "tag" + strconv.Itoa(i)
			}
			body, _ := json.Marshal(map[string]any{"BackdropImageTags": tags})
			_, _ = w.Write(body)
		case r.Method == http.MethodDelete:
			deletes = append(deletes, r.URL.Path)
			parts := strings.Split(r.URL.Path, "/")
			idx, convErr := strconv.Atoi(parts[len(parts)-1])
			if convErr == nil && idx >= 0 && idx < len(backdrops) {
				backdrops = append(backdrops[:idx], backdrops[idx+1:]...)
			}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost:
			uploads = append(uploads, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer httpSrv.Close()

	p := New(Deps{
		Logger: silentLogger(),
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c-jf", PlatformArtistID: "p1"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c-jf": {ID: "c-jf", Name: "my-jellyfin", Type: connection.TypeJellyfin, URL: httpSrv.URL, Enabled: true, Status: "ok", Jellyfin: &connection.JellyfinConfig{PlatformUserID: "u1", FeatureImageWrite: true}},
		}},
	})

	_ = p.SyncImageToPlatforms(context.Background(), &artist.Artist{ID: "a1", Name: "Test Artist", Path: dir}, "fanart")

	mu.Lock()
	gotDeletes := append([]string(nil), deletes...)
	gotUploads := append([]string(nil), uploads...)
	gotBackdrops := append([][]byte(nil), backdrops...)
	mu.Unlock()

	if len(gotDeletes) != 0 {
		t.Errorf("DELETE calls = %d, want 0", len(gotDeletes))
	}
	if len(gotUploads) != 0 {
		t.Errorf("POST calls = %d, want 0", len(gotUploads))
	}
	if len(gotBackdrops) != 5 {
		t.Fatalf("platform BackdropCount after sync = %d, want 5 (all untouched, including the 3 the local set never addressed)", len(gotBackdrops))
	}
	for i, want := range seeds {
		if !bytesEqual(gotBackdrops[i], want) {
			t.Errorf("platform backdrop %d content changed -- every one of the 5 must survive a refused resync untouched", i)
		}
	}
}
