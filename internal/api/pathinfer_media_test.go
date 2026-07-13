package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/connection/emby"
	"github.com/sydlexius/stillwater/internal/connection/jellyfin"
	"github.com/sydlexius/stillwater/internal/connection/lidarr"
)

// --- media-server (Emby / Jellyfin) artist enumeration ------------------------
//
// #2380's root cause was asymmetry: Lidarr had a path-mapping inference seam and
// the two media servers did not, so an Emby or Jellyfin container mounting the
// library under its own prefix silently received raw HOST paths. The listers
// below are what closes that gap, so they are driven against real HTTP fixtures
// (httptest, no external dependency) rather than a fake, and every branch --
// paging, empty page, library error, artist error -- is asserted.

// mediaFixture is a scripted Emby/Jellyfin peer: it serves the music
// VirtualFolders list and pages of AlbumArtists. Both platforms speak the same
// raw JSON on these two endpoints, so one fixture drives both clients.
type mediaFixture struct {
	// libs is the /Library/VirtualFolders payload.
	libs []map[string]any
	// pages maps StartIndex -> the ItemsResponse payload returned for it.
	pages map[string]map[string]any
	// libStatus / artistStatus, when non-zero, make the respective endpoint fail
	// so the lister's error branches are exercised.
	libStatus    int
	artistStatus int
	// artistCalls counts AlbumArtists requests so a test can prove paging really
	// issued more than one round-trip.
	artistCalls int
}

func (f *mediaFixture) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/Library/VirtualFolders"):
			if f.libStatus != 0 {
				w.WriteHeader(f.libStatus)
				_, _ = w.Write([]byte(`{"error":"boom"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(f.libs)
		case strings.HasPrefix(r.URL.Path, "/Artists/AlbumArtists"):
			f.artistCalls++
			if f.artistStatus != 0 {
				w.WriteHeader(f.artistStatus)
				_, _ = w.Write([]byte(`{"error":"boom"}`))
				return
			}
			page, ok := f.pages[r.URL.Query().Get("StartIndex")]
			if !ok {
				page = map[string]any{"Items": []any{}, "TotalRecordCount": 0}
			}
			_ = json.NewEncoder(w).Encode(page)
		default:
			t.Errorf("unexpected request path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// artistPage builds one AlbumArtists page payload. total is the peer's reported
// TotalRecordCount, which is what drives the lister's paging decision.
func artistPage(total int, items ...[2]string) map[string]any {
	out := make([]any, 0, len(items))
	for _, it := range items {
		out = append(out, map[string]any{
			"Path":        it[0],
			"ProviderIds": map[string]any{"MusicBrainzArtist": it[1]},
		})
	}
	return map[string]any{"Items": out, "TotalRecordCount": total}
}

// pagedFixture builds a two-page media peer: page 1 holds Alpha, page 2 holds
// Beta, and TotalRecordCount exceeds page 1's item count so a correct lister
// MUST request the second page. A single-page implementation returns only Alpha.
func pagedFixture() *mediaFixture {
	const total = mediaArtistPageSize + 1
	return &mediaFixture{
		libs: []map[string]any{{"Name": "Music", "ItemId": "lib-1", "CollectionType": "music"}},
		pages: map[string]map[string]any{
			"0":   artistPage(total, [2]string{"/music/Alpha", "mbid-1"}),
			"200": artistPage(total, [2]string{"/music/Beta", "mbid-2"}),
		},
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestEmbyArtistLister_PagesAllArtists proves the Emby lister walks EVERY page
// (not just the first) and normalizes each item into (MBID, platform path).
// A single-page implementation would return only Alpha and fail here.
func TestEmbyArtistLister_PagesAllArtists(t *testing.T) {
	f := pagedFixture()
	srv := f.server(t)

	l := embyArtistLister{c: emby.New(srv.URL, "k", "", testLogger()), logger: testLogger()}
	got, err := l.ListArtistPaths(context.Background())
	if err != nil {
		t.Fatalf("ListArtistPaths: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %+v, want both pages' artists (paging must continue past page 1)", got)
	}
	if got[0] != (platformArtistPath{MBID: "mbid-1", Path: "/music/Alpha"}) ||
		got[1] != (platformArtistPath{MBID: "mbid-2", Path: "/music/Beta"}) {
		t.Fatalf("got %+v, want the two (MBID, platform path) records", got)
	}
	if f.artistCalls < 2 {
		t.Errorf("AlbumArtists called %d times, want >= 2 (paging)", f.artistCalls)
	}
}

// TestEmbyArtistLister_LibraryError: an unreachable/500 VirtualFolders endpoint
// must surface an error, never an empty (and therefore silently "no mappings")
// artist list.
func TestEmbyArtistLister_LibraryError(t *testing.T) {
	f := &mediaFixture{libStatus: http.StatusInternalServerError}
	srv := f.server(t)

	l := embyArtistLister{c: emby.New(srv.URL, "k", "", testLogger()), logger: testLogger()}
	got, err := l.ListArtistPaths(context.Background())
	if err == nil {
		t.Fatalf("got (%+v, nil), want an error when the libraries cannot be listed", got)
	}
	if got != nil {
		t.Errorf("got %+v alongside the error, want nil", got)
	}
}

// TestEmbyArtistLister_ArtistsError: the artist page failing mid-enumeration is
// an error too (a partial list would infer a mapping from a biased sample).
func TestEmbyArtistLister_ArtistsError(t *testing.T) {
	f := &mediaFixture{
		libs:         []map[string]any{{"Name": "Music", "ItemId": "lib-1", "CollectionType": "music"}},
		artistStatus: http.StatusBadGateway,
	}
	srv := f.server(t)

	l := embyArtistLister{c: emby.New(srv.URL, "k", "", testLogger()), logger: testLogger()}
	if _, err := l.ListArtistPaths(context.Background()); err == nil {
		t.Fatal("want an error when the artist page fails")
	}
}

// TestEmbyArtistLister_EmptyLibraryStopsPaging guards the empty-page break: a
// library with no artists must terminate rather than loop forever.
func TestEmbyArtistLister_EmptyLibraryStopsPaging(t *testing.T) {
	f := &mediaFixture{
		libs:  []map[string]any{{"Name": "Music", "ItemId": "lib-1", "CollectionType": "music"}},
		pages: map[string]map[string]any{}, // every StartIndex -> empty page
	}
	srv := f.server(t)

	l := embyArtistLister{c: emby.New(srv.URL, "k", "", testLogger()), logger: testLogger()}
	got, err := l.ListArtistPaths(context.Background())
	if err != nil {
		t.Fatalf("ListArtistPaths: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %+v, want none", got)
	}
	if f.artistCalls != 1 {
		t.Errorf("AlbumArtists called %d times, want exactly 1 (empty page must stop paging)", f.artistCalls)
	}
}

// TestJellyfinArtistLister_PagesAllArtists is the Jellyfin half of the
// asymmetry fix. Jellyfin is asserted DIRECTLY (not "same code as Emby"):
// #2380's whole defect class was one platform being wired and the other not.
func TestJellyfinArtistLister_PagesAllArtists(t *testing.T) {
	f := pagedFixture()
	srv := f.server(t)

	l := jellyfinArtistLister{c: jellyfin.New(srv.URL, "k", "", testLogger()), logger: testLogger()}
	got, err := l.ListArtistPaths(context.Background())
	if err != nil {
		t.Fatalf("ListArtistPaths: %v", err)
	}
	if len(got) != 2 ||
		got[0] != (platformArtistPath{MBID: "mbid-1", Path: "/music/Alpha"}) ||
		got[1] != (platformArtistPath{MBID: "mbid-2", Path: "/music/Beta"}) {
		t.Fatalf("got %+v, want both artists across both pages", got)
	}
}

func TestJellyfinArtistLister_LibraryError(t *testing.T) {
	f := &mediaFixture{libStatus: http.StatusInternalServerError}
	srv := f.server(t)

	l := jellyfinArtistLister{c: jellyfin.New(srv.URL, "k", "", testLogger()), logger: testLogger()}
	if _, err := l.ListArtistPaths(context.Background()); err == nil {
		t.Fatal("want an error when the libraries cannot be listed")
	}
}

func TestJellyfinArtistLister_ArtistsError(t *testing.T) {
	f := &mediaFixture{
		libs:         []map[string]any{{"Name": "Music", "ItemId": "lib-1", "CollectionType": "music"}},
		artistStatus: http.StatusBadGateway,
	}
	srv := f.server(t)

	l := jellyfinArtistLister{c: jellyfin.New(srv.URL, "k", "", testLogger()), logger: testLogger()}
	if _, err := l.ListArtistPaths(context.Background()); err == nil {
		t.Fatal("want an error when the artist page fails")
	}
}

// misreportingFixture serves an Emby/Jellyfin AlbumArtists endpoint that NEVER
// stops paging on its own: every page comes back full (mediaArtistPageSize
// items) and TotalRecordCount always claims there is at least one more page
// past whatever StartIndex was requested. A real peer that misreports its
// count this way (or simply never returns an empty page) would spin the
// lister forever without a page cap.
func misreportingFixture(t *testing.T) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/Library/VirtualFolders"):
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"Name": "Music", "ItemId": "lib-1", "CollectionType": "music"},
			})
		case strings.HasPrefix(r.URL.Path, "/Artists/AlbumArtists"):
			atomic.AddInt32(&calls, 1)
			items := make([]any, 0, mediaArtistPageSize)
			for i := 0; i < mediaArtistPageSize; i++ {
				items = append(items, map[string]any{
					"Path":        "/music/never-ending",
					"ProviderIds": map[string]any{"MusicBrainzArtist": "mbid"},
				})
			}
			// TotalRecordCount always claims one more artist exists past this
			// page, no matter how far StartIndex has already walked.
			start, _ := strconv.Atoi(r.URL.Query().Get("StartIndex"))
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Items":            items,
				"TotalRecordCount": start + mediaArtistPageSize + 1,
			})
		default:
			t.Errorf("unexpected request path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// TestEmbyArtistLister_PageCapTerminatesMisreportingPeer is #F1's proof: a
// peer whose AlbumArtists page never empties and whose TotalRecordCount never
// catches up must still TERMINATE (not hang) once mediaArtistPageCap pages
// have been walked, and the truncation must be logged so an operator can see
// the enumeration was cut short rather than silently believing it was
// complete.
//
// This test only passes with the page cap in place: reverting the cap check
// in ListArtistPaths makes this fixture page forever and the test times out.
func TestEmbyArtistLister_PageCapTerminatesMisreportingPeer(t *testing.T) {
	srv, calls := misreportingFixture(t)

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	l := embyArtistLister{c: emby.New(srv.URL, "k", "", logger), logger: logger}

	done := make(chan struct{})
	var got []platformArtistPath
	var err error
	go func() {
		got, err = l.ListArtistPaths(context.Background())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("ListArtistPaths did not terminate against a misreporting peer (called %d times) -- page cap did not bound the loop", atomic.LoadInt32(calls))
	}

	if err != nil {
		t.Fatalf("ListArtistPaths: %v, want a bounded (nil-error) result with a truncation log instead", err)
	}
	if got := len(got); got != mediaArtistPageCap*mediaArtistPageSize {
		t.Fatalf("got %d artists, want exactly mediaArtistPageCap*mediaArtistPageSize (%d) -- the cap must bound how much is returned",
			got, mediaArtistPageCap*mediaArtistPageSize)
	}
	if callCount := atomic.LoadInt32(calls); callCount != mediaArtistPageCap {
		t.Fatalf("AlbumArtists called %d times, want exactly mediaArtistPageCap (%d)", callCount, mediaArtistPageCap)
	}
	if !strings.Contains(logBuf.String(), "page cap") {
		t.Fatalf("log output = %q, want a truncation warning mentioning the page cap", logBuf.String())
	}
}

// TestJellyfinArtistLister_PageCapTerminatesMisreportingPeer is the Jellyfin
// half of #F1 -- asserted directly, not "same as Emby", per this package's
// existing convention of proving each platform's lister independently.
func TestJellyfinArtistLister_PageCapTerminatesMisreportingPeer(t *testing.T) {
	srv, calls := misreportingFixture(t)

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	l := jellyfinArtistLister{c: jellyfin.New(srv.URL, "k", "", logger), logger: logger}

	done := make(chan struct{})
	var err error
	go func() {
		_, err = l.ListArtistPaths(context.Background())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("ListArtistPaths did not terminate against a misreporting peer (called %d times) -- page cap did not bound the loop", atomic.LoadInt32(calls))
	}

	if err != nil {
		t.Fatalf("ListArtistPaths: %v, want a bounded (nil-error) result with a truncation log instead", err)
	}
	if callCount := atomic.LoadInt32(calls); callCount != mediaArtistPageCap {
		t.Fatalf("AlbumArtists called %d times, want exactly mediaArtistPageCap (%d)", callCount, mediaArtistPageCap)
	}
	if !strings.Contains(logBuf.String(), "page cap") {
		t.Fatalf("log output = %q, want a truncation warning mentioning the page cap", logBuf.String())
	}
}

// --- production factories -----------------------------------------------------

// TestMediaArtistListerFactory_Dispatch pins the PRODUCTION factory (the one the
// tests above bypass by constructing the listers directly): Emby and Jellyfin
// must each get a lister, and an unknown type must get none. A factory that
// silently returned nil for Jellyfin would reproduce #2380 exactly.
func TestMediaArtistListerFactory_Dispatch(t *testing.T) {
	log := testLogger()
	if l := mediaArtistListerFactory(&connection.Connection{Type: connection.TypeEmby, URL: "http://emby.invalid"}, log); l == nil {
		t.Error("emby: factory returned no lister")
	}
	if l := mediaArtistListerFactory(&connection.Connection{Type: connection.TypeJellyfin, URL: "http://jellyfin.invalid"}, log); l == nil {
		t.Error("jellyfin: factory returned no lister")
	}
	if l := mediaArtistListerFactory(&connection.Connection{Type: "unsupported", URL: "http://other.invalid"}, log); l != nil {
		t.Errorf("unsupported type: got %T, want no lister", l)
	}
}

// TestLidarrArtistListerFactory_Dispatch pins the production Lidarr factory.
func TestLidarrArtistListerFactory_Dispatch(t *testing.T) {
	if l := lidarrArtistListerFactory(&connection.Connection{Type: connection.TypeLidarr, URL: "http://lidarr.invalid"}, testLogger()); l == nil {
		t.Error("lidarr: factory returned no lister")
	}
}

// --- listPlatformArtistPaths dispatch guards ---------------------------------

// TestListPlatformArtistPaths_UnsupportedType: a type with no artist surface
// yields no artists and no error (inference is simply unavailable), and must not
// panic on the missing lister.
func TestListPlatformArtistPaths_UnsupportedType(t *testing.T) {
	r := newConnectionTestRouter(t)
	got, err := r.listPlatformArtistPaths(context.Background(),
		&connection.Connection{ID: "c1", Type: "unsupported"})
	if err != nil || got != nil {
		t.Fatalf("got (%+v, %v), want (nil, nil)", got, err)
	}
}

// TestListPlatformArtistPaths_NilListers covers the defensive nil-lister branch
// on both seams: a factory that cannot build a client must degrade to "no
// artists", not dereference nil.
func TestListPlatformArtistPaths_NilListers(t *testing.T) {
	r := newConnectionTestRouter(t)

	origL := lidarrArtistListerFactory
	lidarrArtistListerFactory = func(*connection.Connection, *slog.Logger) lidarrArtistLister { return nil }
	t.Cleanup(func() { lidarrArtistListerFactory = origL })

	origM := mediaArtistListerFactory
	mediaArtistListerFactory = func(*connection.Connection, *slog.Logger) mediaArtistLister { return nil }
	t.Cleanup(func() { mediaArtistListerFactory = origM })

	for _, typ := range []string{connection.TypeLidarr, connection.TypeEmby, connection.TypeJellyfin} {
		got, err := r.listPlatformArtistPaths(context.Background(),
			&connection.Connection{ID: "c1", Type: typ})
		if err != nil || got != nil {
			t.Errorf("%s: got (%+v, %v), want (nil, nil)", typ, got, err)
		}
	}
}

// TestInferPathMappings_JellyfinPeer is the per-peer-mapping assertion the fix
// turns on: a JELLYFIN peer (not Lidarr) whose artists live under /music while
// Stillwater sees them under /host/music must yield the host->container mapping.
func TestInferPathMappings_JellyfinPeer(t *testing.T) {
	r := newConnectionTestRouter(t)
	seedArtistMBIDPath(t, r, "a1", "mbid-1", "/host/music/Alpha")
	seedArtistMBIDPath(t, r, "a2", "mbid-2", "/host/music/Beta")
	withFakeMediaLister(t, fakeMediaLister{artists: []platformArtistPath{
		{MBID: "mbid-1", Path: "/music/Alpha"},
		{MBID: "mbid-2", Path: "/music/Beta"},
	}})

	mappings, matched, err := r.inferPathMappings(context.Background(),
		&connection.Connection{ID: "c-jf", Type: connection.TypeJellyfin, Enabled: true})
	if err != nil {
		t.Fatalf("infer: %v", err)
	}
	if matched != 2 {
		t.Errorf("matched = %d, want 2", matched)
	}
	if len(mappings) != 1 ||
		mappings[0].HostPrefix != "/host/music" ||
		mappings[0].PlatformPrefix != "/music" {
		t.Fatalf("mappings = %+v, want one /host/music -> /music", mappings)
	}
}

// TestInferPathMappings_MediaListerError surfaces the media-server enumeration
// failure as an error (callers treat it as "no inference available").
func TestInferPathMappings_MediaListerError(t *testing.T) {
	r := newConnectionTestRouter(t)
	seedArtistMBIDPath(t, r, "a1", "mbid-1", "/host/music/Alpha")
	withFakeMediaLister(t, fakeMediaLister{err: errFakeLidarr})

	if _, matched, err := r.inferPathMappings(context.Background(),
		&connection.Connection{ID: "c-emby", Type: connection.TypeEmby, Enabled: true}); err == nil {
		t.Fatalf("want the lister error to propagate (matched=%d)", matched)
	}
}

// TestInferPathMappings_NilConnection covers the nil guard.
func TestInferPathMappings_NilConnection(t *testing.T) {
	r := newConnectionTestRouter(t)
	mappings, matched, err := r.inferPathMappings(context.Background(), nil)
	if err != nil || matched != 0 || mappings != nil {
		t.Fatalf("got (%+v, %d, %v), want (nil, 0, nil)", mappings, matched, err)
	}
}

// --- auto-apply guard branches ------------------------------------------------

// TestApplyInferredPathMappingsIfEmpty_SkipsWhenAlreadyMapped covers the fast
// pre-check: a connection that already carries mappings must never enumerate the
// peer, so an operator's list cannot be re-derived (and re-written) on every save.
func TestApplyInferredPathMappingsIfEmpty_SkipsWhenAlreadyMapped(t *testing.T) {
	r := newConnectionTestRouter(t)
	id := seedLidarrConn(t, r)
	conn, err := r.connectionService.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	setPathMappings(conn, []connection.PathMapping{{HostPrefix: "/manual", PlatformPrefix: "/keep"}})

	seedArtistMBIDPath(t, r, "a1", "mbid-1", "/music/Alpha")
	seedArtistMBIDPath(t, r, "a2", "mbid-2", "/music/Beta")
	called := 0
	orig := lidarrArtistListerFactory
	lidarrArtistListerFactory = func(*connection.Connection, *slog.Logger) lidarrArtistLister {
		called++
		return fakeArtistLister{}
	}
	t.Cleanup(func() { lidarrArtistListerFactory = orig })

	r.applyInferredPathMappingsIfEmpty(context.Background(), conn)

	if called != 0 {
		t.Errorf("peer enumerated %d times for an already-mapped connection, want 0", called)
	}
	if m := conn.GetPathMappings(); len(m) != 1 || m[0].HostPrefix != "/manual" {
		t.Errorf("mappings = %+v, want the existing list untouched", m)
	}
}

// TestApplyInferredPathMappingsIfEmpty_ReloadFailureSkipsWrite covers the
// re-fetch error branch: if the canonical row cannot be re-read under the lock,
// auto-apply must NOT write (it has no verified empty-check to write against).
func TestApplyInferredPathMappingsIfEmpty_ReloadFailureSkipsWrite(t *testing.T) {
	r := newConnectionTestRouter(t)
	seedArtistMBIDPath(t, r, "a1", "mbid-1", "/music/Alpha")
	seedArtistMBIDPath(t, r, "a2", "mbid-2", "/music/Beta")
	withFakeLister(t, fakeArtistLister{artists: []lidarr.Artist{
		{ID: 1, ForeignArtistID: "mbid-1", Path: "/data/Alpha"},
		{ID: 2, ForeignArtistID: "mbid-2", Path: "/data/Beta"},
	}})

	// A connection that is NOT in the DB: inference succeeds, the re-fetch fails.
	ghost := &connection.Connection{
		ID:      "00000000-0000-0000-0000-0000000000ff",
		Type:    connection.TypeLidarr,
		Enabled: true,
	}
	r.applyInferredPathMappingsIfEmpty(context.Background(), ghost)

	if m := ghost.GetPathMappings(); len(m) != 0 {
		t.Errorf("mappings = %+v applied despite the reload failure, want none", m)
	}
}

// TestBuildInferencePairs_SkipsBlankRows covers the blank-key guards on both
// sides of the join: a Stillwater row with no MBID or no path, and a peer artist
// with no MBID or no path, contribute nothing. Without these guards an empty
// prefix pair would poison inference for every other artist.
func TestBuildInferencePairs_SkipsBlankRows(t *testing.T) {
	t.Parallel()

	pairs := buildInferencePairs(
		[]artist.MBIDPath{
			{MBID: "", Path: "/music/NoMBID"},
			{MBID: "mbid-nopath", Path: ""},
			{MBID: "mbid-ok", Path: "/music/Alpha"},
		},
		[]platformArtistPath{
			{MBID: "", Path: "/data/NoMBID"},
			{MBID: "mbid-ok", Path: ""},          // peer path blank -> skipped
			{MBID: "mbid-nopath", Path: "/data"}, // host path blank -> no join key
			{MBID: "mbid-ok", Path: "/data/Alpha"},
		},
	)
	if len(pairs) != 1 {
		t.Fatalf("pairs = %+v, want exactly the one fully-populated join", pairs)
	}
	if pairs[0].HostPath != "/music/Alpha" || pairs[0].PlatformPath != "/data/Alpha" {
		t.Fatalf("pair = %+v, want /music/Alpha -> /data/Alpha", pairs[0])
	}
}
