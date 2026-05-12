package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/auth"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/database"
	"github.com/sydlexius/stillwater/internal/encryption"
	"github.com/sydlexius/stillwater/internal/library"
	"github.com/sydlexius/stillwater/internal/nfo"
)

// newNFOTestServer constructs a Router wired with the four services
// handleNFOConflictCheck reads: artist, NFO snapshot, connection, library.
// Returns the router plus handles to seed fixtures.
//
// Accepts testing.TB so both tests and the fuzz target can call it.
func newNFOTestServer(t testing.TB) (*Router, *artist.Service, *connection.Service, *nfo.SnapshotService) {
	t.Helper()

	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	if err := database.Migrate(db); err != nil {
		t.Fatalf("running migrations: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	enc, _, err := encryption.NewEncryptor("")
	if err != nil {
		t.Fatalf("encryptor: %v", err)
	}

	authSvc := auth.NewService(db)
	artistSvc := artist.NewService(db)
	libSvc := library.NewService(db)
	connSvc := connection.NewService(db, enc)
	nfoSnapSvc := nfo.NewSnapshotService(db)

	r := NewRouter(RouterDeps{
		AuthService:        authSvc,
		ArtistService:      artistSvc,
		LibraryService:     libSvc,
		ConnectionService:  connSvc,
		NFOSnapshotService: nfoSnapSvc,
		DB:                 db,
		Logger:             logger,
		StaticFS:           os.DirFS("../../web/static"),
	})

	return r, artistSvc, connSvc, nfoSnapSvc
}

// writeNFO writes a minimal NFO file into the given directory with the
// supplied modification time.
func writeNFO(t *testing.T, dir string, modTime time.Time) {
	t.Helper()
	path := filepath.Join(dir, "artist.nfo")
	body := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<artist>
  <name>Test Artist</name>
  <sortname>Test Artist</sortname>
  <type>group</type>
  <musicbrainzartistid>11111111-1111-1111-1111-111111111111</musicbrainzartistid>
  <genre>Rock</genre>
  <biography>A biography long enough to exercise the parser.</biography>
</artist>`)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("writing NFO: %v", err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
}

// TestHandleNFOConflictCheck_NoConflict covers the path where:
//   - the artist has a filesystem path,
//   - an NFO snapshot exists,
//   - the on-disk NFO has NOT been modified since the snapshot,
//   - no connection-side NFO writer is configured.
//
// Expected: HTTP 200 with HasConflict=false and ExternalWriter empty.
func TestHandleNFOConflictCheck_NoConflict(t *testing.T) {
	t.Parallel()
	r, artistSvc, _, nfoSnapSvc := newNFOTestServer(t)
	ctx := context.Background()

	dir := t.TempDir()
	a := &artist.Artist{
		Name:     "No Conflict",
		SortName: "No Conflict",
		Type:     "group",
		Path:     dir,
	}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// Save snapshot at the latest "known good" point, then write the file
	// with a modtime in the past so the snapshot timestamp is AFTER the file.
	if _, err := nfoSnapSvc.Save(ctx, a.ID, "<artist><name>No Conflict</name></artist>"); err != nil {
		t.Fatalf("saving snapshot: %v", err)
	}
	writeNFO(t, dir, time.Now().Add(-1*time.Hour))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/"+a.ID+"/nfo/conflict", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleNFOConflictCheck(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	var got nfo.ConflictCheck
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.HasConflict {
		t.Errorf("HasConflict = true, want false (file older than snapshot)")
	}
	if got.ExternalWriter != "" {
		t.Errorf("ExternalWriter = %q, want empty", got.ExternalWriter)
	}
	// The on-disk NFO file exists (writeNFO was called), so the handler
	// must surface its mtime via LastModified to lock the API contract.
	if got.LastModified == nil || got.LastModified.IsZero() {
		t.Errorf("LastModified = %v, want populated (file is on disk)", got.LastModified)
	}
}

// TestHandleNFOConflictCheck_DetectedConflict covers the path where:
//   - the artist has a filesystem path,
//   - an NFO snapshot exists,
//   - the on-disk NFO has been modified AFTER the snapshot.
//
// Expected: HTTP 200 with HasConflict=true and Reason populated.
func TestHandleNFOConflictCheck_DetectedConflict(t *testing.T) {
	t.Parallel()
	r, artistSvc, _, nfoSnapSvc := newNFOTestServer(t)
	ctx := context.Background()

	dir := t.TempDir()
	a := &artist.Artist{
		Name:     "Has Conflict",
		SortName: "Has Conflict",
		Type:     "group",
		Path:     dir,
	}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// Snapshot taken at a known point in the past.
	snap, err := nfoSnapSvc.Save(ctx, a.ID, "<artist><name>Has Conflict</name></artist>")
	if err != nil {
		t.Fatalf("saving snapshot: %v", err)
	}
	// File modified AFTER the snapshot timestamp.
	writeNFO(t, dir, snap.CreatedAt.Add(2*time.Hour))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/"+a.ID+"/nfo/conflict", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleNFOConflictCheck(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	var got nfo.ConflictCheck
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.HasConflict {
		t.Errorf("HasConflict = false, want true (file newer than snapshot)")
	}
	if got.Reason == "" {
		t.Errorf("Reason empty, want populated for detected conflict")
	}
	// File exists and is the source of the conflict; LastModified must
	// carry the file's mtime so the UI can show "modified at X".
	if got.LastModified == nil || got.LastModified.IsZero() {
		t.Errorf("LastModified = %v, want populated for file-driven conflict", got.LastModified)
	}
}

// TestHandleNFOConflictCheck_NoSnapshotFallback exercises the 24h-fallback
// branch: the artist has a path and a file but no snapshot rows exist. The
// handler should fall back to "since = now - 24h" and not crash.
func TestHandleNFOConflictCheck_NoSnapshotFallback(t *testing.T) {
	t.Parallel()
	r, artistSvc, _, _ := newNFOTestServer(t)
	ctx := context.Background()

	dir := t.TempDir()
	a := &artist.Artist{Name: "No Snapshot", SortName: "No Snapshot", Type: "group", Path: dir}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	// File modified well within the 24h window => conflict expected.
	writeNFO(t, dir, time.Now().Add(-30*time.Minute))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/"+a.ID+"/nfo/conflict", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleNFOConflictCheck(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	var got nfo.ConflictCheck
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// The file is younger than the 24h cutoff => HasConflict=true.
	if !got.HasConflict {
		t.Errorf("HasConflict = false, want true (no snapshot, file inside 24h window)")
	}
	// File exists on disk; LastModified must echo its mtime so the UI
	// can render the "modified at" timestamp on the conflict banner.
	if got.LastModified == nil || got.LastModified.IsZero() {
		t.Errorf("LastModified = %v, want populated", got.LastModified)
	}
}

// TestHandleNFOConflictCheck_NotFound covers the early-return on
// artistService.GetByID failure.
func TestHandleNFOConflictCheck_NotFound(t *testing.T) {
	t.Parallel()
	r, _, _, _ := newNFOTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/missing/nfo/conflict", nil)
	req.SetPathValue("id", "missing")
	w := httptest.NewRecorder()

	r.handleNFOConflictCheck(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

// TestHandleNFOConflictCheck_LidarrWriterFlagged stands up a fake Lidarr
// server whose /api/v1/metadata response declares an enabled NFO consumer.
// The handler's connection loop must populate ExternalWriter with the
// "lidarr:<name>" format and -- because no actual file conflict exists --
// fill Reason with the nfoWriterWarning copy.
func TestHandleNFOConflictCheck_LidarrWriterFlagged(t *testing.T) {
	t.Parallel()
	r, artistSvc, connSvc, _ := newNFOTestServer(t)
	ctx := context.Background()

	// Fake Lidarr: /api/v1/metadata returns one enabled consumer with
	// artistMetadata=true. The path-prefix check is sufficient because
	// the Lidarr client appends "/api/v1/metadata" verbatim.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// Guard the inbound request contract so a regression that drops
		// the API key or sends the wrong verb fails the test directly
		// instead of silently returning success. Mirrors the
		// "Mock servers/handlers check request bodies and headers"
		// guideline.
		if req.Method != http.MethodGet {
			t.Errorf("Lidarr mock: method = %s, want GET", req.Method)
		}
		if req.Header.Get("X-Api-Key") == "" && req.URL.Query().Get("apikey") == "" {
			t.Errorf("Lidarr mock: expected X-Api-Key header or apikey query param")
		}
		if strings.HasSuffix(req.URL.Path, "/api/v1/metadata") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[
				{"id":1,"name":"Kodi","enable":true,"fields":[
					{"name":"artistMetadata","value":true}
				]}
			]`))
			return
		}
		http.NotFound(w, req)
	}))
	t.Cleanup(srv.Close)

	conn := &connection.Connection{
		Name:    "Lidarr-1",
		Type:    connection.TypeLidarr,
		URL:     srv.URL,
		APIKey:  "test-key",
		Enabled: true,
	}
	if err := connSvc.Create(ctx, conn); err != nil {
		t.Fatalf("creating connection: %v", err)
	}

	// Artist with no file conflict (no on-disk file at all is fine -- the
	// file-check returns HasConflict=false, leaving the writer check to
	// populate Reason via nfoWriterWarning).
	dir := t.TempDir()
	a := &artist.Artist{Name: "Lidarr Watched", SortName: "Lidarr Watched", Type: "group", Path: dir}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/"+a.ID+"/nfo/conflict", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleNFOConflictCheck(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	var got nfo.ConflictCheck
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.HasConflict {
		t.Errorf("HasConflict = true, want false (no file conflict, only writer flagged)")
	}
	// No on-disk NFO was written in this test; LastModified must remain
	// nil (omitted from the JSON via the `,omitempty` tag) so the API
	// contract for "writer-only conflict" stays distinct from "file
	// conflict".
	if got.LastModified != nil {
		t.Errorf("LastModified = %v, want nil (no file on disk)", got.LastModified)
	}
	wantPrefix := connection.TypeLidarr + ":"
	if !strings.HasPrefix(got.ExternalWriter, wantPrefix) {
		t.Errorf("ExternalWriter = %q, want prefix %q", got.ExternalWriter, wantPrefix)
	}
	if got.Reason == "" {
		t.Errorf("Reason = empty, want nfoWriterWarning copy when writer flagged")
	}
	if !strings.Contains(got.Reason, "Lidarr") {
		t.Errorf("Reason = %q, want it to mention Lidarr", got.Reason)
	}
}

// TestHandleNFOConflictCheck_DisabledConnectionIgnored confirms the
// `if !conn.Enabled { continue }` short-circuit inside the connection loop.
func TestHandleNFOConflictCheck_DisabledConnectionIgnored(t *testing.T) {
	t.Parallel()
	r, artistSvc, connSvc, _ := newNFOTestServer(t)
	ctx := context.Background()

	// Fake Lidarr that would normally flag a writer -- but the connection
	// is created with Enabled=false, so the loop must skip it. The handler
	// t.Errorfs on any hit, turning "loop skipped disabled connection
	// correctly" into a directly-observable assertion: a regression that
	// dispatches outbound calls to disabled connections fails this test.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("disabled connection should not make outbound metadata requests")
		http.Error(w, "unexpected outbound call to disabled connection", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	conn := &connection.Connection{
		Name:    "Lidarr-Off",
		Type:    connection.TypeLidarr,
		URL:     srv.URL,
		APIKey:  "test-key",
		Enabled: false,
	}
	if err := connSvc.Create(ctx, conn); err != nil {
		t.Fatalf("creating connection: %v", err)
	}

	dir := t.TempDir()
	a := &artist.Artist{Name: "Disabled", SortName: "Disabled", Type: "group", Path: dir}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/"+a.ID+"/nfo/conflict", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleNFOConflictCheck(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	var got nfo.ConflictCheck
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ExternalWriter != "" {
		t.Errorf("ExternalWriter = %q, want empty (disabled connection)", got.ExternalWriter)
	}
}

// FuzzHandleNFOConflictCheck wraps the handler in an HTTP request with a
// fuzzed JSON body. handleNFOConflictCheck itself does not decode JSON --
// it consumes the path param and queries services -- but a body is still
// read by the test request, and we want any future refactor that DOES
// decode a body to remain panic-free. This is the E10 panic-safety target;
// it asserts the handler can never panic regardless of input.
//
// The fuzz target also exercises the path-param handling by feeding the
// fuzzed bytes as the artist ID, which routes through the GetByID
// not-found path on most inputs and through real lookup when the input
// happens to match the seed artist's UUID.
func FuzzHandleNFOConflictCheck(f *testing.F) {
	// Build a test server once; reused across fuzz iterations. We do NOT
	// use t.Parallel-style isolation here because fuzz workers re-enter
	// the same Fuzz function and a new router per iteration would
	// dominate runtime.
	r, artistSvc, _, _ := newNFOTestServer(f)
	ctx := context.Background()

	// Seed: one artist with a path so the file-conflict branch runs.
	dir, err := os.MkdirTemp("", "fuzz-nfo-")
	if err != nil {
		f.Fatalf("mkdtemp: %v", err)
	}
	f.Cleanup(func() { _ = os.RemoveAll(dir) })

	a := &artist.Artist{Name: "Fuzz", SortName: "Fuzz", Type: "group", Path: dir}
	if err := artistSvc.Create(ctx, a); err != nil {
		f.Fatalf("seed artist: %v", err)
	}

	// Seed corpus from the happy-path test body shapes. The handler does
	// not decode JSON today, but seeding with valid-shaped JSON keeps the
	// corpus useful if a future patch starts decoding the body. The seed
	// containing the real artist UUID drives the GetByID success path; all
	// other inputs route through the not-found / lookup-error branches.
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"force":true}`))
	f.Add([]byte(`{"unknown_field":"value"}`))
	f.Add([]byte(`{"id":"` + a.ID + `"}`))
	f.Add([]byte("not-json-at-all"))
	f.Add([]byte(""))
	f.Add([]byte("\x00\x01\x02"))
	f.Add([]byte(a.ID))

	f.Fuzz(func(t *testing.T, body []byte) {
		// Derive the path-param id from the fuzz input so adversarial
		// bytes also exercise artistSvc.GetByID's parsing/lookup path,
		// not only the request-body branch. Empty bodies fall back to
		// the seed artist's UUID so the lookup-success branch still
		// runs on the empty seed.
		id := string(body)
		if id == "" {
			id = a.ID
		}
		req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/"+a.ID+"/nfo/conflict", bytes.NewReader(body))
		req.SetPathValue("id", id)
		w := httptest.NewRecorder()

		// The contract under fuzz is: must not panic.
		r.handleNFOConflictCheck(w, req)

		// Drain the response body so the recorder does not retain a leak
		// across many iterations.
		_, _ = io.Copy(io.Discard, w.Body)
	})
}
