package publish

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	img "github.com/sydlexius/stillwater/internal/image"
)

// uploadCall records one POST /Images/Backdrop/{index} the fake Jellyfin
// server received: the path, the DECODED body (the client base64-encodes
// per UploadImageAtIndexRaw, so this is the raw image bytes it actually
// sent, not the wire form), and the Authorization header it carried.
type uploadCall struct {
	path    string
	body    []byte
	authHdr string
}

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
//
// #3146 CR review: a PATH-ONLY assertion is vacuous on the property this
// test exists to prove. Jellyfin ignores the index in the URL -- that is
// the entire #3135 premise -- so the set of paths POSTed says nothing about
// which BYTES actually landed at which slot. A regression that uploaded
// the primary twice, or sent the two local slots in descending order,
// would still produce the path set {.../Backdrop/0, .../Backdrop/1} and
// pass a path-only check. This version captures each POST's DECODED body
// (UploadImageAtIndexRaw base64-encodes the wire payload, so the raw bytes
// this server sees are NOT what the client handed it -- decode before
// comparing) and asserts slot 0 receives bandJPEG(10) (fanart.jpg's exact
// content) and slot 1 receives bandJPEG(11) (fanart2.jpg's), IN ASCENDING
// UPLOAD ORDER, plus that the connection's API key rode along on the
// Authorization header every peer request carries it on
// (mediabrowser.JellyfinProfile.ApplyAuth: `MediaBrowser Token="..."`).
func TestUploadFanartForSync_JellyfinRoutesThroughFullResync(t *testing.T) {
	dir := t.TempDir()
	seedA, seedB := bandJPEG(t, 10), bandJPEG(t, 11)
	writeFile(t, filepath.Join(dir, "fanart.jpg"), seedA)
	writeFile(t, filepath.Join(dir, "fanart2.jpg"), seedB)

	const apiKey = "test-jellyfin-api-key-3146"

	var mu sync.Mutex
	var deletes []string // DELETE paths, in call order
	var uploads []uploadCall
	var authByMethod []authCall // EVERY request's auth header, in call order
	backdropCount := 2          // platform starts with 2 existing backdrops

	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		// #3146 CR review: record the Authorization header on EVERY request,
		// not only the uploads. A missing token on GetArtistDetail or
		// DeleteImageAtIndex fails a LIVE resync just as hard as one on the
		// POST, and asserting it only for uploads let that pass unnoticed.
		authByMethod = append(authByMethod, authCall{method: r.Method, path: r.URL.Path, authHdr: r.Header.Get("Authorization")})
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
			raw, _ := io.ReadAll(r.Body)
			decoded, decErr := base64.StdEncoding.DecodeString(string(raw))
			if decErr != nil {
				t.Errorf("POST %s body did not decode as base64: %v", r.URL.Path, decErr)
				decoded = raw // still record something so the call count stays accurate
			}
			uploads = append(uploads, uploadCall{path: r.URL.Path, body: decoded, authHdr: r.Header.Get("Authorization")})
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
			"c-jf": {ID: "c-jf", Name: "my-jellyfin", Type: connection.TypeJellyfin, URL: httpSrv.URL, APIKey: apiKey, Enabled: true, Status: "ok", Jellyfin: &connection.JellyfinConfig{PlatformUserID: "u1", FeatureImageWrite: true}},
		}},
	})

	warnings := p.SyncImageToPlatforms(context.Background(), &artist.Artist{ID: "a1", Name: "Test Artist", Path: dir}, "fanart")
	if len(warnings) != 0 {
		t.Fatalf("expected no warnings; got %v", warnings)
	}

	mu.Lock()
	gotDeletes := append([]string(nil), deletes...)
	gotUploads := append([]uploadCall(nil), uploads...)
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
		t.Fatalf("POST (indexed upload) calls = %d, want 2 (reupload the full local set); got paths %v", len(gotUploads), pathsOf(gotUploads))
	}

	// THE BODY+ORDER ASSERTION (#3146 CR fix): the resync must upload slot 0
	// FIRST with fanart.jpg's exact bytes, then slot 1 with fanart2.jpg's --
	// ascending order, correct content per slot. A path-only check cannot
	// tell "primary uploaded twice" or "slots reversed" apart from success,
	// because Jellyfin ignores the index either way; only the decoded body
	// at each ordinal position can.
	if gotUploads[0].path != "/Items/p1/Images/Backdrop/0" {
		t.Errorf("upload 1 path = %q, want /Items/p1/Images/Backdrop/0 (ascending order)", gotUploads[0].path)
	}
	if !bytesEqual(gotUploads[0].body, seedA) {
		t.Errorf("upload 1 (slot 0) body = %d bytes, want fanart.jpg's exact %d bytes -- wrong content landed in the first upload", len(gotUploads[0].body), len(seedA))
	}
	if gotUploads[1].path != "/Items/p1/Images/Backdrop/1" {
		t.Errorf("upload 2 path = %q, want /Items/p1/Images/Backdrop/1 (ascending order)", gotUploads[1].path)
	}
	if !bytesEqual(gotUploads[1].body, seedB) {
		t.Errorf("upload 2 (slot 1) body = %d bytes, want fanart2.jpg's exact %d bytes -- wrong content landed in the second upload", len(gotUploads[1].body), len(seedB))
	}

	// The API key must ride along on every peer request; Jellyfin carries it
	// on Authorization: MediaBrowser Token="...", not X-Emby-Token (Emby's
	// scheme -- see mediabrowser.JellyfinProfile vs EmbyProfile).
	wantAuth := fmt.Sprintf(`MediaBrowser Token="%s"`, apiKey)

	mu.Lock()
	gotAuth := append([]authCall(nil), authByMethod...)
	mu.Unlock()

	// Every request in the resync sequence, not just the uploads: the detail
	// GET that establishes how many slots to clear, each DELETE, and each
	// POST. An unauthenticated GET or DELETE breaks a live resync exactly as
	// completely as an unauthenticated upload.
	if len(gotAuth) == 0 {
		t.Fatal("no requests recorded; the resync issued nothing at all")
	}
	sawGet, sawDelete, sawPost := false, false, false
	for _, c := range gotAuth {
		if c.authHdr != wantAuth {
			t.Errorf("%s %s Authorization header = %q, want %q", c.method, c.path, c.authHdr, wantAuth)
		}
		switch c.method {
		case http.MethodGet:
			sawGet = true
		case http.MethodDelete:
			sawDelete = true
		case http.MethodPost:
			sawPost = true
		}
	}
	// Assert the PRECONDITION of the loop above: if the resync stopped issuing
	// one of these methods, the per-request check would pass vacuously over
	// whatever remained.
	if !sawGet || !sawDelete || !sawPost {
		t.Errorf("auth was not exercised across all three request kinds (GET=%v DELETE=%v POST=%v)", sawGet, sawDelete, sawPost)
	}
}

// authCall is one recorded request's method, path, and Authorization header,
// so the resync's auth can be asserted across every peer call rather than
// only the uploads (#3146 CR review).
type authCall struct {
	method  string
	path    string
	authHdr string
}

// pathsOf projects a slice of uploadCall down to its paths, for a
// count-mismatch failure message that still shows what was actually seen.
func pathsOf(calls []uploadCall) []string {
	out := make([]string, len(calls))
	for i, c := range calls {
		out[i] = c.path
	}
	return out
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

// No GetArtistBackdrop: fanartResyncClient (#3146 CR fix) narrowed to
// connection.ArtistStateGetter + IndexedImageDeleter + IndexedImageUploader,
// so this fake need not implement a method the resync path never calls.

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
	// fanart2.jpg: genuinely OVER img.MaxDecodeBytes (25 MB), so the READ
	// itself refuses it (img.ErrImageTooLarge) and snapshotFanart degrades
	// this slot to nil data -- not a fake/mocked degrade, the real read
	// bound firing on a real oversize file. #3017 moved WHICH check produces
	// a nil-data slot here: the per-file retention cap (12 MiB) no longer
	// refuses before the read, so a fixture merely over THAT cap is now read
	// and pushed successfully; only the read's own 25 MB bound still
	// degrades a slot the way this test's restorability-gate assertions need.
	// Sparse (a truncate, no bytes written): the cap check reads a stat, not
	// content, so sparseness is invisible to it.
	sparseFile(t, filepath.Join(dir, "fanart2.jpg"), img.MaxDecodeBytes+1)

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
	// Over img.MaxDecodeBytes (25 MB), not merely the retention cap; see the
	// #3017 note on the sibling test above for why.
	sparseFile(t, filepath.Join(dir, "fanart2.jpg"), img.MaxDecodeBytes+1)

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

// TestUploadFanartFullResyncForSync_RefusalReasonSurvivesTruncation is the
// #3146 CR fix: uploadFanartFullResyncForSync joins the restorability
// gate's refusal reason with snapshotFanart's own per-file warnings via
// strings.Join(warnings, "; "), and the RESULT of that join is truncated a
// SECOND time by the outer truncateWarning (each element was already
// truncated individually before the join). If the refusal reason -- the
// only actionable line, naming the slot that blocked the resync and why
// nothing was deleted -- is appended AFTER the snapshot noise rather than
// placed first, a long connection name or several snapshot warnings can
// push the join past maxWarningRunes and truncate the refusal reason away
// entirely, leaving the operator with snapshot noise and no explanation.
//
// This builds THREE genuinely oversize local files (each over
// img.MaxDecodeBytes, #3017: the retention cap alone no longer degrades a
// slot -- see the sibling tests above), so snapshotFanart's real read bound
// fires three times and produces three independent per-file warnings -- not
// a mocked warning list -- totaling well past maxWarningRunes (200) once
// joined with the refusal reason. It asserts the refusal reason's own
// distinguishing text ("could not be captured") and the specific slot index
// it names both survive INTACT in the final (possibly truncated) warning
// string, proving the ordering fix rather than merely asserting the
// function returns non-empty text.
func TestUploadFanartFullResyncForSync_RefusalReasonSurvivesTruncation(t *testing.T) {
	dir := t.TempDir()
	// fanart.jpg (index 0) is small and captures normally; fanart1/2/3.jpg
	// (indices 1-3) are each genuinely over img.MaxDecodeBytes, so
	// snapshotFanart's real read bound fires THREE times and produces three
	// independent per-file warnings -- not a mocked list. Sparse, so this
	// costs no real disk; the bound checks bytes actually read, which a
	// truncate delivers just as honestly as written content would.
	writeFile(t, filepath.Join(dir, "fanart.jpg"), bandJPEG(t, 90))
	for _, slot := range []int{1, 2, 3} {
		sparseFile(t, filepath.Join(dir, fmt.Sprintf("fanart%d.jpg", slot)), img.MaxDecodeBytes+1)
	}

	p := New(Deps{Logger: silentLogger()})
	// A deliberately long connection name: contributes to the truncation
	// pressure the same way a real operator's connection name would, on
	// top of the three snapshot warnings.
	conn := &connection.Connection{ID: "c-jf", Name: "my-jellyfin-connection-with-a-fairly-long-descriptive-name", Type: connection.TypeJellyfin}

	uploaded, warning := p.uploadFanartFullResyncForSync(context.Background(), &artist.Artist{ID: "a1", Path: dir}, artist.PlatformID{ConnectionID: "c-jf", PlatformArtistID: "p1"}, conn)
	if uploaded {
		t.Fatal("uploaded = true, want false -- three of the four local slots are oversize, so the restorability gate must refuse before any write")
	}

	// PRECONDITION: prove this scenario actually produces enough raw text to
	// reach (or exceed) maxWarningRunes, or the test would pass vacuously
	// regardless of ordering (nothing left for truncation to eat).
	if runeLen := len([]rune(warning)); runeLen < 190 {
		t.Fatalf("precondition failed: returned warning is only %d runes (want it near/at the %d-rune cap) -- this scenario does not generate enough text to prove the ordering fix; strengthen the fixture", runeLen, 200)
	}

	// THE #3146 ASSERTION: the refusal reason's distinguishing text and the
	// slot index it names must both survive, proving it was NOT the part
	// truncation ate. The gate returns on the FIRST nil-data slot it walks
	// (index 1, the lowest of the three oversize slots), so that is the
	// index the refusal names.
	if !strings.Contains(warning, "could not be captured") {
		t.Errorf("warning = %q, want the refusal reason's distinguishing text (\"could not be captured\") to survive truncation -- it did not, meaning the actionable explanation was lost", warning)
	}
	if !strings.Contains(warning, "fanart 1") {
		t.Errorf("warning = %q, want it to still name slot 1 (the first oversize file the gate walks) -- the slot identity was lost to truncation", warning)
	}
}
