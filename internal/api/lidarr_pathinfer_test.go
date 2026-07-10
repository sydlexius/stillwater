package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/connection/lidarr"
)

// errFakeLidarr is the canned GetArtists failure used to drive best-effort
// error branches in the collector and its callers.
var errFakeLidarr = errors.New("fake lidarr GetArtists failure")

// fakeArtistLister is a test double for the lidarrArtistLister seam. It returns
// a fixed artist list (or a fixed error) so inference can be exercised without a
// real Lidarr HTTP fixture.
type fakeArtistLister struct {
	artists []lidarr.Artist
	err     error
}

func (f fakeArtistLister) GetArtists(context.Context) ([]lidarr.Artist, error) {
	return f.artists, f.err
}

// withFakeLister swaps the package-level lidarrArtistListerFactory for the run
// and restores it on cleanup. Tests using it must NOT call t.Parallel() because
// the factory is process-global.
func withFakeLister(t *testing.T, lister lidarrArtistLister) {
	t.Helper()
	orig := lidarrArtistListerFactory
	t.Cleanup(func() { lidarrArtistListerFactory = orig })
	lidarrArtistListerFactory = func(*connection.Connection, *slog.Logger) lidarrArtistLister {
		return lister
	}
}

// seedArtistMBIDPath inserts an artist row with a MusicBrainz provider ID and a
// path so ListMBIDPaths returns it.
func seedArtistMBIDPath(t *testing.T, r *Router, id, mbid, path string) {
	t.Helper()
	mustExec(t, r.db, `INSERT INTO artists (id, name, path) VALUES (?, ?, ?)`, id, id, path)
	mustExec(t, r.db,
		`INSERT INTO artist_provider_ids (artist_id, provider, provider_id) VALUES (?, 'musicbrainz', ?)`,
		id, mbid)
}

func postInferPathMappings(t *testing.T, r *Router, id string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/connections/"+id+"/path-mappings/infer", nil)
	req.SetPathValue("id", id)
	w := httptest.NewRecorder()
	r.handleInferPathMappings(w, req)
	return w
}

// TestHandleInferPathMappings_AppliesWhenEmpty is the core acceptance test: two
// artists matched by MBID with a consistent /music -> /data prefix difference
// yield an applied mapping and an info line reporting the outcome.
func TestHandleInferPathMappings_AppliesWhenEmpty(t *testing.T) {
	r := newConnectionTestRouter(t)
	id := seedLidarrConn(t, r)

	seedArtistMBIDPath(t, r, "a1", "mbid-1", "/music/Alpha")
	seedArtistMBIDPath(t, r, "a2", "mbid-2", "/music/Beta")
	withFakeLister(t, fakeArtistLister{artists: []lidarr.Artist{
		{ID: 1, ForeignArtistID: "mbid-1", Path: "/data/Alpha"},
		{ID: 2, ForeignArtistID: "mbid-2", Path: "/data/Beta"},
	}})

	w := postInferPathMappings(t, r, id)
	if w.Code != http.StatusOK {
		t.Fatalf("infer: status %d, body %s", w.Code, w.Body.String())
	}
	if body := w.Body.String(); !strings.Contains(body, "path_mapping_inferred") {
		t.Errorf("fragment missing inferred info line; body:\n%s", body)
	}

	got, err := r.connectionService.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	mappings := got.GetPathMappings()
	if len(mappings) != 1 || mappings[0].HostPrefix != "/music" || mappings[0].PlatformPrefix != "/data" {
		t.Fatalf("applied mappings = %+v, want one /music->/data", mappings)
	}
}

// TestHandleInferPathMappings_DoesNotOverwrite confirms B3 precedence: an
// existing (operator-entered) mapping list is never clobbered by a re-infer.
func TestHandleInferPathMappings_DoesNotOverwrite(t *testing.T) {
	r := newConnectionTestRouter(t)
	id := seedLidarrConn(t, r)

	// Pre-set a manual mapping.
	existing, err := r.connectionService.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	setPathMappings(existing, []connection.PathMapping{{HostPrefix: "/manual", PlatformPrefix: "/keep"}})
	if err := r.connectionService.Update(context.Background(), existing); err != nil {
		t.Fatalf("seed manual mapping: %v", err)
	}

	seedArtistMBIDPath(t, r, "a1", "mbid-1", "/music/Alpha")
	seedArtistMBIDPath(t, r, "a2", "mbid-2", "/music/Beta")
	withFakeLister(t, fakeArtistLister{artists: []lidarr.Artist{
		{ID: 1, ForeignArtistID: "mbid-1", Path: "/data/Alpha"},
		{ID: 2, ForeignArtistID: "mbid-2", Path: "/data/Beta"},
	}})

	w := postInferPathMappings(t, r, id)
	if w.Code != http.StatusOK {
		t.Fatalf("infer: status %d, body %s", w.Code, w.Body.String())
	}
	// P3-1: the info line must signal the derived mappings were NOT applied
	// (existing kept), not the plain "Inferred N" applied message.
	if body := w.Body.String(); !strings.Contains(body, "path_mapping_inferred_kept") {
		t.Errorf("fragment missing 'kept existing' info line; body:\n%s", body)
	}

	got, err := r.connectionService.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	mappings := got.GetPathMappings()
	if len(mappings) != 1 || mappings[0].HostPrefix != "/manual" || mappings[0].PlatformPrefix != "/keep" {
		t.Fatalf("mappings = %+v, want manual mapping preserved", mappings)
	}
}

// TestHandleInferPathMappings_NoMatchesInfoLine confirms the zero-match surface:
// when no Lidarr artist matches, the fragment shows the "no mappings inferred"
// line and applies nothing.
func TestHandleInferPathMappings_NoMatchesInfoLine(t *testing.T) {
	r := newConnectionTestRouter(t)
	id := seedLidarrConn(t, r)

	seedArtistMBIDPath(t, r, "a1", "mbid-1", "/music/Alpha")
	withFakeLister(t, fakeArtistLister{artists: []lidarr.Artist{
		{ID: 9, ForeignArtistID: "mbid-unmatched", Path: "/data/Other"},
	}})

	w := postInferPathMappings(t, r, id)
	if w.Code != http.StatusOK {
		t.Fatalf("infer: status %d, body %s", w.Code, w.Body.String())
	}
	if body := w.Body.String(); !strings.Contains(body, "path_mapping_none_inferred") {
		t.Errorf("fragment missing no-match info line; body:\n%s", body)
	}
	got, err := r.connectionService.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(got.GetPathMappings()) != 0 {
		t.Errorf("mappings = %+v, want none applied", got.GetPathMappings())
	}
}

// TestApplyInferredPathMappingsIfEmpty_ReCheckUnderLock is the P2-1 regression
// guard: auto-apply must NOT clobber a mapping list that was saved during its
// (up-to-10s) Lidarr enumeration. It reproduces the TOCTOU by handing auto-apply
// a STALE in-memory connection whose PathMappings are empty (the create-time
// snapshot) while the canonical DB row already carries an operator-saved list
// (the concurrent manual save that landed mid-enumeration). The re-fetch +
// re-check inside pathMappingsMu must see the persisted list and back off,
// leaving it untouched.
func TestApplyInferredPathMappingsIfEmpty_ReCheckUnderLock(t *testing.T) {
	r := newConnectionTestRouter(t)
	id := seedLidarrConn(t, r)

	// The stale in-memory snapshot the create/update handler would pass: empty
	// mappings, enabled, Lidarr.
	stale, err := r.connectionService.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("load stale: %v", err)
	}
	if len(stale.GetPathMappings()) != 0 {
		t.Fatalf("precondition: stale snapshot should have no mappings, got %+v", stale.GetPathMappings())
	}

	// Inference would derive /music -> /data from these.
	seedArtistMBIDPath(t, r, "a1", "mbid-1", "/music/Alpha")
	seedArtistMBIDPath(t, r, "a2", "mbid-2", "/music/Beta")
	withFakeLister(t, fakeArtistLister{artists: []lidarr.Artist{
		{ID: 1, ForeignArtistID: "mbid-1", Path: "/data/Alpha"},
		{ID: 2, ForeignArtistID: "mbid-2", Path: "/data/Beta"},
	}})

	// Simulate the concurrent manual save that committed while the (stale)
	// snapshot was still being enumerated: the canonical row now has /manual.
	canonical, err := r.connectionService.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("load canonical: %v", err)
	}
	setPathMappings(canonical, []connection.PathMapping{{HostPrefix: "/manual", PlatformPrefix: "/keep"}})
	if err := r.connectionService.Update(context.Background(), canonical); err != nil {
		t.Fatalf("seed manual save: %v", err)
	}

	// Auto-apply runs with the stale (empty) snapshot: the fast pre-check passes,
	// inference derives /music->/data, then the re-check under lock sees /manual
	// and must back off.
	r.applyInferredPathMappingsIfEmpty(context.Background(), stale)

	got, err := r.connectionService.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	mappings := got.GetPathMappings()
	if len(mappings) != 1 || mappings[0].HostPrefix != "/manual" || mappings[0].PlatformPrefix != "/keep" {
		t.Fatalf("mappings = %+v, want /manual preserved (auto-apply must back off)", mappings)
	}
}

// TestApplyInferredPathMappingsIfEmpty_AppliesWhenStillEmpty confirms the
// non-racy path still applies: when nothing lands during the enumeration, the
// re-check under lock sees an empty list and persists the inferred set.
func TestApplyInferredPathMappingsIfEmpty_AppliesWhenStillEmpty(t *testing.T) {
	r := newConnectionTestRouter(t)
	id := seedLidarrConn(t, r)

	conn, err := r.connectionService.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	seedArtistMBIDPath(t, r, "a1", "mbid-1", "/music/Alpha")
	seedArtistMBIDPath(t, r, "a2", "mbid-2", "/music/Beta")
	withFakeLister(t, fakeArtistLister{artists: []lidarr.Artist{
		{ID: 1, ForeignArtistID: "mbid-1", Path: "/data/Alpha"},
		{ID: 2, ForeignArtistID: "mbid-2", Path: "/data/Beta"},
	}})

	r.applyInferredPathMappingsIfEmpty(context.Background(), conn)

	// In-memory connection reflects the applied set (used by JSON responses).
	if m := conn.GetPathMappings(); len(m) != 1 || m[0].HostPrefix != "/music" || m[0].PlatformPrefix != "/data" {
		t.Fatalf("in-memory mappings = %+v, want /music->/data", m)
	}
	got, err := r.connectionService.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if m := got.GetPathMappings(); len(m) != 1 || m[0].HostPrefix != "/music" || m[0].PlatformPrefix != "/data" {
		t.Fatalf("persisted mappings = %+v, want /music->/data", m)
	}
}

// --- collector (inferLidarrPathMappings) direct branch coverage ---------------

// TestInferLidarrPathMappings_MatchedPairs covers the happy path: two artists
// matched by MBID produce a consistent mapping and matched==2.
func TestInferLidarrPathMappings_MatchedPairs(t *testing.T) {
	r := newConnectionTestRouter(t)
	id := seedLidarrConn(t, r)
	conn, err := r.connectionService.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	seedArtistMBIDPath(t, r, "a1", "mbid-1", "/music/Alpha")
	seedArtistMBIDPath(t, r, "a2", "mbid-2", "/music/Beta")
	withFakeLister(t, fakeArtistLister{artists: []lidarr.Artist{
		{ID: 1, ForeignArtistID: "mbid-1", Path: "/data/Alpha"},
		{ID: 2, ForeignArtistID: "MBID-2", Path: "/data/Beta"}, // case-insensitive match
	}})

	mappings, matched, err := r.inferLidarrPathMappings(context.Background(), conn)
	if err != nil {
		t.Fatalf("infer: %v", err)
	}
	if matched != 2 {
		t.Errorf("matched = %d, want 2", matched)
	}
	if len(mappings) != 1 || mappings[0].HostPrefix != "/music" || mappings[0].PlatformPrefix != "/data" {
		t.Fatalf("mappings = %+v, want one /music->/data", mappings)
	}
}

// TestInferLidarrPathMappings_NoStillwaterArtists covers the early return when
// ListMBIDPaths yields nothing (no artist has both an MBID and a path).
func TestInferLidarrPathMappings_NoStillwaterArtists(t *testing.T) {
	r := newConnectionTestRouter(t)
	id := seedLidarrConn(t, r)
	conn, _ := r.connectionService.GetByID(context.Background(), id)
	// No artists seeded. The fake lister would return one, but the collector
	// must short-circuit before calling it.
	withFakeLister(t, fakeArtistLister{artists: []lidarr.Artist{
		{ID: 1, ForeignArtistID: "mbid-1", Path: "/data/Alpha"},
	}})

	mappings, matched, err := r.inferLidarrPathMappings(context.Background(), conn)
	if err != nil || matched != 0 || len(mappings) != 0 {
		t.Fatalf("got (%+v, %d, %v); want (nil, 0, nil)", mappings, matched, err)
	}
}

// TestInferLidarrPathMappings_GetArtistsError covers the best-effort GetArtists
// failure branch: the error is surfaced (callers treat it as "no inference").
func TestInferLidarrPathMappings_GetArtistsError(t *testing.T) {
	r := newConnectionTestRouter(t)
	id := seedLidarrConn(t, r)
	conn, _ := r.connectionService.GetByID(context.Background(), id)
	seedArtistMBIDPath(t, r, "a1", "mbid-1", "/music/Alpha")
	withFakeLister(t, fakeArtistLister{err: errFakeLidarr})

	_, matched, err := r.inferLidarrPathMappings(context.Background(), conn)
	if err == nil {
		t.Fatal("expected GetArtists error to propagate")
	}
	if matched != 0 {
		t.Errorf("matched = %d, want 0 on error", matched)
	}
}

// TestInferLidarrPathMappings_NoMatches covers the path where Lidarr has
// artists but none match a Stillwater MBID: zero pairs, empty mappings.
func TestInferLidarrPathMappings_NoMatches(t *testing.T) {
	r := newConnectionTestRouter(t)
	id := seedLidarrConn(t, r)
	conn, _ := r.connectionService.GetByID(context.Background(), id)
	seedArtistMBIDPath(t, r, "a1", "mbid-1", "/music/Alpha")
	withFakeLister(t, fakeArtistLister{artists: []lidarr.Artist{
		{ID: 9, ForeignArtistID: "mbid-unmatched", Path: "/data/Other"},
		{ID: 10, ForeignArtistID: "", Path: "/data/Blank"}, // blank MBID guard
	}})

	mappings, matched, err := r.inferLidarrPathMappings(context.Background(), conn)
	if err != nil {
		t.Fatalf("infer: %v", err)
	}
	if matched != 0 || len(mappings) != 0 {
		t.Fatalf("got (%+v, %d); want (nil, 0)", mappings, matched)
	}
}

// TestInferLidarrPathMappings_ListMBIDPathsError covers the best-effort branch
// where the Stillwater-side query fails: the error is surfaced so callers treat
// it as "no inference available". Forced by closing the DB before the call.
func TestInferLidarrPathMappings_ListMBIDPathsError(t *testing.T) {
	r := newConnectionTestRouter(t)
	id := seedLidarrConn(t, r)
	conn, err := r.connectionService.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// Close the underlying DB so ListMBIDPaths errors on QueryContext. The
	// test-DB cleanup closes idempotently, so this is safe.
	if cerr := r.db.Close(); cerr != nil {
		t.Fatalf("close db: %v", cerr)
	}

	_, matched, err := r.inferLidarrPathMappings(context.Background(), conn)
	if err == nil {
		t.Fatal("expected ListMBIDPaths error to propagate")
	}
	if matched != 0 {
		t.Errorf("matched = %d, want 0 on error", matched)
	}
}

// TestInferLidarrPathMappings_NonLidarr covers the type guard: a non-Lidarr
// connection yields no inference without touching the lister.
func TestInferLidarrPathMappings_NonLidarr(t *testing.T) {
	r := newConnectionTestRouter(t)
	id := seedEmbyConn(t, r)
	conn, _ := r.connectionService.GetByID(context.Background(), id)

	mappings, matched, err := r.inferLidarrPathMappings(context.Background(), conn)
	if err != nil || matched != 0 || len(mappings) != 0 {
		t.Fatalf("got (%+v, %d, %v); want (nil, 0, nil)", mappings, matched, err)
	}
}

// --- auto-apply guard branches ------------------------------------------------

// TestApplyInferredPathMappingsIfEmpty_SkipsDisabled and _SkipsNonLidarr cover
// the two fast-return guards (disabled / wrong type) before any enumeration.
func TestApplyInferredPathMappingsIfEmpty_SkipsDisabled(t *testing.T) {
	r := newConnectionTestRouter(t)
	id := seedLidarrConn(t, r)
	conn, _ := r.connectionService.GetByID(context.Background(), id)
	conn.Enabled = false
	// Fake lister would infer, but a disabled connection must never enumerate.
	withFakeLister(t, fakeArtistLister{artists: []lidarr.Artist{
		{ID: 1, ForeignArtistID: "mbid-1", Path: "/data/Alpha"},
	}})
	seedArtistMBIDPath(t, r, "a1", "mbid-1", "/music/Alpha")

	r.applyInferredPathMappingsIfEmpty(context.Background(), conn)
	if len(conn.GetPathMappings()) != 0 {
		t.Errorf("disabled connection got mappings applied: %+v", conn.GetPathMappings())
	}
}

func TestApplyInferredPathMappingsIfEmpty_SkipsNonLidarr(t *testing.T) {
	r := newConnectionTestRouter(t)
	id := seedEmbyConn(t, r)
	conn, _ := r.connectionService.GetByID(context.Background(), id)
	r.applyInferredPathMappingsIfEmpty(context.Background(), conn) // must be a no-op
}

// TestApplyInferredPathMappingsIfEmpty_ErrorSwallowed covers the best-effort
// path: a GetArtists error must not panic or write anything.
func TestApplyInferredPathMappingsIfEmpty_ErrorSwallowed(t *testing.T) {
	r := newConnectionTestRouter(t)
	id := seedLidarrConn(t, r)
	conn, _ := r.connectionService.GetByID(context.Background(), id)
	seedArtistMBIDPath(t, r, "a1", "mbid-1", "/music/Alpha")
	withFakeLister(t, fakeArtistLister{err: errFakeLidarr})

	r.applyInferredPathMappingsIfEmpty(context.Background(), conn)
	got, _ := r.connectionService.GetByID(context.Background(), id)
	if len(got.GetPathMappings()) != 0 {
		t.Errorf("mappings applied despite GetArtists error: %+v", got.GetPathMappings())
	}
}

// TestHandleInferPathMappings_InferErrorInfoLine covers the handler branch where
// inference errors: it renders the "no mappings inferred" line (matched reset to
// 0) and applies nothing, still returning 200.
func TestHandleInferPathMappings_InferErrorInfoLine(t *testing.T) {
	r := newConnectionTestRouter(t)
	id := seedLidarrConn(t, r)
	seedArtistMBIDPath(t, r, "a1", "mbid-1", "/music/Alpha")
	withFakeLister(t, fakeArtistLister{err: errFakeLidarr})

	w := postInferPathMappings(t, r, id)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200; body %s", w.Code, w.Body.String())
	}
	if body := w.Body.String(); !strings.Contains(body, "path_mapping_none_inferred") {
		t.Errorf("expected none-inferred info line on inference error; body:\n%s", body)
	}
	got, _ := r.connectionService.GetByID(context.Background(), id)
	if len(got.GetPathMappings()) != 0 {
		t.Errorf("mappings applied despite inference error: %+v", got.GetPathMappings())
	}
}

// TestHandleInferPathMappings_NotFound and _NonLidarr cover the gate branches.
func TestHandleInferPathMappings_NotFound(t *testing.T) {
	r := newConnectionTestRouter(t)
	w := postInferPathMappings(t, r, "00000000-0000-0000-0000-000000000000")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404; body %s", w.Code, w.Body.String())
	}
}

func TestHandleInferPathMappings_NonLidarr(t *testing.T) {
	r := newConnectionTestRouter(t)
	id := seedEmbyConn(t, r)
	w := postInferPathMappings(t, r, id)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400; body %s", w.Code, w.Body.String())
	}
}

// TestHandleInferPathMappings_MissingID covers the empty-path-value 400 branch.
func TestHandleInferPathMappings_MissingID(t *testing.T) {
	r := newConnectionTestRouter(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/connections//path-mappings/infer", nil)
	req.SetPathValue("id", "") // no id
	w := httptest.NewRecorder()
	r.handleInferPathMappings(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400; body %s", w.Code, w.Body.String())
	}
}

// TestHandleCreateConnection_AutoAppliesInferredMappings covers the create-flow
// auto-apply call site end to end: creating an enabled Lidarr connection (with
// skip_test so no live call) runs inference and, because the connection starts
// with no mappings, persists the derived set.
func TestHandleCreateConnection_AutoAppliesInferredMappings(t *testing.T) {
	r := newConnectionTestRouter(t)

	seedArtistMBIDPath(t, r, "a1", "mbid-1", "/music/Alpha")
	seedArtistMBIDPath(t, r, "a2", "mbid-2", "/music/Beta")
	withFakeLister(t, fakeArtistLister{artists: []lidarr.Artist{
		{ID: 1, ForeignArtistID: "mbid-1", Path: "/data/Alpha"},
		{ID: 2, ForeignArtistID: "mbid-2", Path: "/data/Beta"},
	}})

	body := strings.NewReader(`{"name":"Lidarr1","type":"lidarr","url":"http://lidarr.local:8686","api_key":"k","enabled":true,"skip_test":true}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/connections", body)
	req.Header.Set("Content-Type", "application/json")
	w := serveValidated(t, http.HandlerFunc(r.handleCreateConnection), req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create status %d, want 201; body %s", w.Code, w.Body.String())
	}

	conns, err := r.connectionService.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(conns) != 1 {
		t.Fatalf("len(conns) = %d, want 1", len(conns))
	}
	m := conns[0].GetPathMappings()
	if len(m) != 1 || m[0].HostPrefix != "/music" || m[0].PlatformPrefix != "/data" {
		t.Fatalf("auto-applied mappings = %+v, want one /music->/data", m)
	}
}
