package publish

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/nfo"
)

// setupNFODB creates a SQLite test DB with the minimum schema needed by
// NFOSettingsService and SnapshotService. Mirrors the helpers in
// internal/nfo/*_test.go but kept local to honor the M49 W5 file-ownership
// boundary (no shared test helpers across packages).
//
// The DB lives in t.TempDir(), NOT ":memory:". sql.DB is a connection
// pool, and modernc.org/sqlite makes each ":memory:" connection a private
// in-memory database; a query on a fresh pooled connection would not see
// the schema we just created and would fail with `no such table`. The
// temp-file pattern (used by internal/backup/backup_test.go) shares one
// on-disk DB across the whole pool. t.TempDir cleans up automatically.
func setupNFODB(t *testing.T) *sql.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "nfo.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	_, err = db.ExecContext(context.Background(), `
		CREATE TABLE settings (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL DEFAULT '',
			updated_at TEXT NOT NULL DEFAULT (datetime('now'))
		);
		CREATE TABLE nfo_snapshots (
			id TEXT PRIMARY KEY,
			artist_id TEXT NOT NULL,
			content TEXT NOT NULL,
			created_at TEXT NOT NULL DEFAULT (datetime('now'))
		);
		CREATE INDEX idx_nfo_snapshots_artist_id ON nfo_snapshots(artist_id);
	`)
	if err != nil {
		t.Fatalf("creating tables: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// recordingExpectedWrites is a minimal expectedWritesTracker that records the
// add/remove pair, so tests can assert the watcher integration ran.
type recordingExpectedWrites struct {
	mu       sync.Mutex
	added    []string
	removed  []string
	wasAdded map[string]bool
}

func newRecordingExpectedWrites() *recordingExpectedWrites {
	return &recordingExpectedWrites{wasAdded: map[string]bool{}}
}

func (r *recordingExpectedWrites) Add(path string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.added = append(r.added, path)
	r.wasAdded[path] = true
}

func (r *recordingExpectedWrites) Remove(path string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.removed = append(r.removed, path)
}

func (r *recordingExpectedWrites) snapshot() (added, removed []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.added...), append([]string(nil), r.removed...)
}

// errPlatformLister returns an error from GetPlatformIDs so the
// PushMetadataAsync error path can be exercised.
type errPlatformLister struct{}

func (errPlatformLister) GetPlatformIDs(_ context.Context, _ string) ([]artist.PlatformID, error) {
	return nil, errors.New("listing platform ids failed")
}

// --- BuildArtistPushData ---

// TestBuildArtistPushData exercises every branch of the type switch and
// verifies common fields are propagated.
func TestBuildArtistPushData(t *testing.T) {
	common := &artist.Artist{
		ID:             "a1",
		Name:           "Test",
		SortName:       "Test, The",
		Biography:      "bio",
		Genres:         []string{"rock"},
		Styles:         []string{"prog"},
		Moods:          []string{"melancholic"},
		Disambiguation: "the band",
		YearsActive:    "1970-1980",
		MusicBrainzID:  "mbid-1",
		Born:           "1950-01-01",
		Died:           "2010-01-01",
		Formed:         "1970-01-01",
		Disbanded:      "1980-01-01",
	}

	t.Run("group propagates Formed/Disbanded only", func(t *testing.T) {
		a := *common
		a.Type = "group"
		got := BuildArtistPushData(&a)
		if got.Name != "Test" || got.SortName != "Test, The" {
			t.Errorf("common field propagation broken: %+v", got)
		}
		if got.Formed != a.Formed || got.Disbanded != a.Disbanded {
			t.Errorf("group must propagate Formed/Disbanded, got %+v", got)
		}
		if got.Born != "" || got.Died != "" {
			t.Errorf("group must NOT set Born/Died, got Born=%q Died=%q", got.Born, got.Died)
		}
	})

	t.Run("orchestra/choir share group semantics", func(t *testing.T) {
		for _, ty := range []string{"orchestra", "choir"} {
			a := *common
			a.Type = ty
			got := BuildArtistPushData(&a)
			if got.Born != "" || got.Died != "" {
				t.Errorf("%s must not set Born/Died", ty)
			}
			if got.Formed == "" || got.Disbanded == "" {
				t.Errorf("%s must set Formed/Disbanded", ty)
			}
		}
	})

	t.Run("solo propagates Born/Died only", func(t *testing.T) {
		a := *common
		a.Type = "solo"
		got := BuildArtistPushData(&a)
		if got.Born != a.Born || got.Died != a.Died {
			t.Errorf("solo must propagate Born/Died, got %+v", got)
		}
		if got.Formed != "" || got.Disbanded != "" {
			t.Errorf("solo must NOT set Formed/Disbanded, got Formed=%q Disbanded=%q",
				got.Formed, got.Disbanded)
		}
	})

	t.Run("unknown type propagates all four dates (fallback chain)", func(t *testing.T) {
		a := *common
		a.Type = "" // empty -> default case
		got := BuildArtistPushData(&a)
		if got.Born != a.Born || got.Died != a.Died {
			t.Errorf("default must set Born/Died, got %+v", got)
		}
		if got.Formed != a.Formed || got.Disbanded != a.Disbanded {
			t.Errorf("default must set Formed/Disbanded, got %+v", got)
		}
	})

	t.Run("slices and identifiers propagate", func(t *testing.T) {
		a := *common
		a.Type = "solo"
		got := BuildArtistPushData(&a)
		if len(got.Genres) != 1 || got.Genres[0] != "rock" {
			t.Errorf("Genres not propagated: %v", got.Genres)
		}
		if got.MusicBrainzID != "mbid-1" {
			t.Errorf("MusicBrainzID not propagated: %q", got.MusicBrainzID)
		}
		if got.Disambiguation != "the band" {
			t.Errorf("Disambiguation not propagated: %q", got.Disambiguation)
		}
	})
}

// --- WriteBackNFO ---

// writeArtistDir creates a temp artist directory with the given pre-existing
// NFO content (skipping write when content is empty). Returns the dir path.
func writeArtistDir(t *testing.T, existing string) string {
	t.Helper()
	dir := t.TempDir()
	if existing != "" {
		if err := os.WriteFile(filepath.Join(dir, "artist.nfo"), []byte(existing), 0o644); err != nil {
			t.Fatalf("seeding artist.nfo: %v", err)
		}
	}
	return dir
}

// TestWriteBackNFO_NilReceiver verifies the guard against a nil Publisher
// (called by callers that haven't wired the publisher yet).
func TestWriteBackNFO_NilReceiver(t *testing.T) {
	var p *Publisher
	// Must not panic.
	p.WriteBackNFO(context.Background(), &artist.Artist{ID: "a", Path: "/tmp"})
}

// TestWriteBackNFO_NoPath verifies the early return when the artist has no
// filesystem path (e.g. virtual / unlinked artists).
func TestWriteBackNFO_NoPath(t *testing.T) {
	p := New(Deps{Logger: silentLogger()})
	p.WriteBackNFO(context.Background(), &artist.Artist{ID: "a"})
	// Nothing observable; the assertion is "did not panic".
}

// TestWriteBackNFO_NoExistingNFOSkipped verifies that an artist directory
// without a pre-existing artist.nfo is left alone; creating new NFOs is the
// rule engine's job, per the package doc.
func TestWriteBackNFO_NoExistingNFOSkipped(t *testing.T) {
	dir := writeArtistDir(t, "") // no artist.nfo
	ew := newRecordingExpectedWrites()
	p := New(Deps{Logger: silentLogger(), ExpectedWrites: ew})
	p.WriteBackNFO(context.Background(), &artist.Artist{ID: "a", Path: dir, Name: "X"})

	if _, err := os.Stat(filepath.Join(dir, "artist.nfo")); !os.IsNotExist(err) {
		t.Errorf("expected NFO to remain absent, stat err=%v", err)
	}
	added, removed := ew.snapshot()
	if len(added) != 0 || len(removed) != 0 {
		t.Errorf("ExpectedWrites should not be touched when no NFO exists: added=%v removed=%v",
			added, removed)
	}
}

// TestWriteBackNFO_HappyPath verifies that an existing NFO is rewritten with
// the current metadata and that the expectedWrites tracker is properly
// add/remove balanced.
func TestWriteBackNFO_HappyPath(t *testing.T) {
	const existing = `<?xml version="1.0"?>
<artist>
  <name>OldName</name>
</artist>
`
	dir := writeArtistDir(t, existing)
	db := setupNFODB(t)
	logger := silentLogger()
	ss := nfo.NewSnapshotService(db)
	settings := nfo.NewNFOSettingsService(db, logger)
	ew := newRecordingExpectedWrites()

	p := New(Deps{
		Logger:             logger,
		NFOSnapshotService: ss,
		NFOSettingsService: settings,
		ExpectedWrites:     ew,
	})

	a := &artist.Artist{
		ID:        "artist-1",
		Name:      "NewName",
		Path:      dir,
		Genres:    []string{"rock"},
		Biography: "A bio.",
	}
	p.WriteBackNFO(context.Background(), a)

	// NFO file rewritten with new name.
	got, err := os.ReadFile(filepath.Join(dir, "artist.nfo"))
	if err != nil {
		t.Fatalf("reading rewritten NFO: %v", err)
	}
	if !strings.Contains(string(got), "<name>NewName</name>") {
		t.Errorf("rewritten NFO missing new name. Got:\n%s", got)
	}
	if strings.Contains(string(got), "OldName") {
		t.Errorf("rewritten NFO still contains OldName. Got:\n%s", got)
	}
	// Old content captured in a snapshot (best-effort).
	snaps, err := ss.List(context.Background(), "artist-1")
	if err != nil {
		t.Fatalf("listing snapshots: %v", err)
	}
	if len(snaps) != 1 {
		t.Errorf("expected 1 snapshot of pre-write content, got %d", len(snaps))
	}

	added, removed := ew.snapshot()
	want := filepath.Join(dir, "artist.nfo")
	if len(added) != 1 || added[0] != want {
		t.Errorf("ExpectedWrites.Add mismatch: %v", added)
	}
	if len(removed) != 1 || removed[0] != want {
		t.Errorf("ExpectedWrites.Remove mismatch: %v", removed)
	}
}

// TestWriteBackNFO_FieldMapServiceErrorFallsBackToDefault verifies the
// best-effort branch where reading the field map fails (closed DB) and the
// writer falls back to the default map.
func TestWriteBackNFO_FieldMapServiceErrorFallsBackToDefault(t *testing.T) {
	dir := writeArtistDir(t, "<artist><name>Old</name></artist>")
	db := setupNFODB(t)
	logger := silentLogger()
	settings := nfo.NewNFOSettingsService(db, logger)
	// Close the DB so GetFieldMap returns an error.
	_ = db.Close()

	p := New(Deps{Logger: logger, NFOSettingsService: settings})
	a := &artist.Artist{ID: "artist-1", Name: "Survivor", Path: dir}
	p.WriteBackNFO(context.Background(), a)

	got, err := os.ReadFile(filepath.Join(dir, "artist.nfo"))
	if err != nil {
		t.Fatalf("reading NFO: %v", err)
	}
	if !strings.Contains(string(got), "<name>Survivor</name>") {
		t.Errorf("write should still succeed using default field map: %s", got)
	}
}

// TestWriteBackNFO_FilesystemErrorLogsAndReturns verifies the partial-write
// recovery branch: when stat returns a non-IsNotExist error (e.g. the NFO
// path is a directory, not a file), WriteBackNFO logs and returns without
// crashing or writing.
func TestWriteBackNFO_StatNonNotExistError(t *testing.T) {
	dir := t.TempDir()
	// Make "artist.nfo" a directory so os.Stat succeeds but the writer cannot
	// overwrite it. Stat does not return an error here so this exercises the
	// success branch of Stat followed by a failure in the writer.
	if err := os.MkdirAll(filepath.Join(dir, "artist.nfo"), 0o755); err != nil {
		t.Fatalf("seeding artist.nfo as a directory: %v", err)
	}

	p := New(Deps{Logger: silentLogger()})
	a := &artist.Artist{ID: "a1", Name: "X", Path: dir}
	// Must not panic; underlying atomic write will fail (logged at Error).
	p.WriteBackNFO(context.Background(), a)
}

// TestWriteBackNFO_RespectsLibraryLockSetting verifies that the lockNFO
// resolution path is taken when a libraryService is wired and the resolved
// library has NFOLockData=true.
func TestWriteBackNFO_RespectsLibraryLockSetting(t *testing.T) {
	dir := writeArtistDir(t, "<artist><name>Old</name></artist>")
	logger := silentLogger()

	p := New(Deps{
		Logger:         logger,
		LibraryService: &alwaysOnResolver{},
	})

	a := &artist.Artist{ID: "a1", Name: "Locked", Path: dir}
	p.WriteBackNFO(context.Background(), a)

	got, err := os.ReadFile(filepath.Join(dir, "artist.nfo"))
	if err != nil {
		t.Fatalf("reading NFO: %v", err)
	}
	if !strings.Contains(strings.ToLower(string(got)), "<lockdata>true</lockdata>") {
		t.Errorf("NFO should carry <lockdata>true</lockdata> when library opts in. Got:\n%s", got)
	}
}

// --- PushMetadataAsync ---

// embyServer returns a test server that responds to both the GET item fetch
// and the POST item update. POST bodies are buffered under a mutex so tests
// can assert that required metadata fields actually round-trip into the
// pushed payload (a happy-path test could otherwise pass even if the
// serializer dropped Name or the date fields).
type pushHits struct {
	posts      atomic.Int32
	gets       atomic.Int32
	mu         sync.Mutex
	postBodies [][]byte
}

// findPostBody returns a snapshot of POST bodies that contain ALL of the
// required substrings. The publisher dispatches both a metadata-update POST
// (carrying the marshaled artist payload) and a follow-up lock POST whose
// body is empty; asserting against "the last body" would race between them.
// Searching by substring lets the caller assert on the meaningful payload
// regardless of arrival order.
func (h *pushHits) findPostBody(required ...string) []byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, b := range h.postBodies {
		s := string(b)
		matched := true
		for _, want := range required {
			if !strings.Contains(s, want) {
				matched = false
				break
			}
		}
		if matched {
			return append([]byte(nil), b...)
		}
	}
	return nil
}

func newEmbyTestServer(hits *pushHits) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			hits.gets.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Id":"p1","LockData":false}`))
		case http.MethodPost:
			// Capture body before incrementing the counter so any
			// concurrent reader observing posts > N is guaranteed to
			// see the corresponding body snapshot.
			body, _ := io.ReadAll(r.Body)
			hits.mu.Lock()
			hits.postBodies = append(hits.postBodies, body)
			hits.mu.Unlock()
			hits.posts.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
}

// TestPushMetadataAsync_NilReceiver verifies the nil receiver guard.
func TestPushMetadataAsync_NilReceiver(t *testing.T) {
	var p *Publisher
	p.PushMetadataAsync(context.Background(), &artist.Artist{ID: "a"})
}

// TestPushMetadataAsync_HappyPath spins a fake Emby server, wires it up, and
// verifies a POST arrives carrying the artist's metadata.
func TestPushMetadataAsync_HappyPath(t *testing.T) {
	hits := &pushHits{}
	srv := newEmbyTestServer(hits)
	defer srv.Close()

	p := New(Deps{
		Logger: silentLogger(),
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c-emby", PlatformArtistID: "p1"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c-emby": {ID: "c-emby", Name: "emby", Type: connection.TypeEmby, URL: srv.URL, Enabled: true, PlatformUserID: "u1"},
		}},
	})

	a := &artist.Artist{ID: "a1", Name: "PushMe", Type: "solo", Born: "1970-01-01"}
	p.PushMetadataAsync(context.Background(), a)

	waitForPosts(t, &hits.posts, 1)

	// Verify the POST payload actually carries the artist's metadata.
	// Without this assertion, the happy-path test would pass even if the
	// serializer silently dropped Name or the date fields. findPostBody
	// searches all captured bodies for one that contains every required
	// substring, so we don't race against the empty follow-up POST that
	// the publisher dispatches after the metadata one.
	if body := hits.findPostBody(`"Name":"PushMe"`, `"1970-01-01"`); body == nil {
		// Surface every captured body to make the failure diagnosable.
		hits.mu.Lock()
		all := append([][]byte(nil), hits.postBodies...)
		hits.mu.Unlock()
		t.Errorf("no POST body carried the artist payload; captured bodies:")
		for i, b := range all {
			t.Errorf("  [%d] %s", i, string(b))
		}
	}
}

// TestPushMetadataAsync_ListerErrorReturnsEarly verifies that an error from
// GetPlatformIDs short-circuits before any goroutine is spawned.
func TestPushMetadataAsync_ListerErrorReturnsEarly(t *testing.T) {
	hits := &pushHits{}
	srv := newEmbyTestServer(hits)
	defer srv.Close()

	p := New(Deps{
		Logger:        silentLogger(),
		ArtistService: errPlatformLister{},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c": {ID: "c", URL: srv.URL, Enabled: true, Type: connection.TypeEmby},
		}},
	})
	p.PushMetadataAsync(context.Background(), &artist.Artist{ID: "a1"})
	time.Sleep(150 * time.Millisecond)
	if got := hits.posts.Load(); got != 0 {
		t.Errorf("no POST should arrive when lister errors; got %d", got)
	}
}

// TestPushMetadataAsync_NoPlatformIDs verifies early-return when the artist
// has no platform mappings.
func TestPushMetadataAsync_NoPlatformIDs(t *testing.T) {
	hits := &pushHits{}
	srv := newEmbyTestServer(hits)
	defer srv.Close()

	p := New(Deps{
		Logger:        silentLogger(),
		ArtistService: &fakePlatformLister{ids: nil},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c": {ID: "c", URL: srv.URL, Enabled: true, Type: connection.TypeEmby},
		}},
	})
	p.PushMetadataAsync(context.Background(), &artist.Artist{ID: "a1"})
	time.Sleep(100 * time.Millisecond)
	if got := hits.posts.Load(); got != 0 {
		t.Errorf("no POST should arrive for an artist with no platform IDs; got %d", got)
	}
}

// TestPushMetadataAsync_ConnectionLookupErrorSkipped verifies that an error
// from GetByID inside the goroutine does not abort other goroutines and does
// not hit the server.
func TestPushMetadataAsync_ConnectionLookupErrorSkipped(t *testing.T) {
	hits := &pushHits{}
	srv := newEmbyTestServer(hits)
	defer srv.Close()

	// fakeConnectionGetter returns an error for unknown ids (mismatched key).
	p := New(Deps{
		Logger: silentLogger(),
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "missing", PlatformArtistID: "p1"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"unrelated": {ID: "unrelated"},
		}},
	})
	p.PushMetadataAsync(context.Background(), &artist.Artist{ID: "a1"})
	time.Sleep(200 * time.Millisecond)
	if got := hits.posts.Load(); got != 0 {
		t.Errorf("no POST should arrive when connection lookup fails; got %d", got)
	}
}

// TestPushMetadataAsync_DisabledConnectionSkipped verifies that a connection
// with Enabled=false is not contacted.
func TestPushMetadataAsync_DisabledConnectionSkipped(t *testing.T) {
	hits := &pushHits{}
	srv := newEmbyTestServer(hits)
	defer srv.Close()

	p := New(Deps{
		Logger: silentLogger(),
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c-off", PlatformArtistID: "p1"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c-off": {ID: "c-off", Type: connection.TypeEmby, URL: srv.URL, Enabled: false},
		}},
	})
	p.PushMetadataAsync(context.Background(), &artist.Artist{ID: "a1"})
	time.Sleep(200 * time.Millisecond)
	if got := hits.posts.Load(); got != 0 {
		t.Errorf("disabled connection should not be contacted; got %d POSTs", got)
	}
}

// TestPushMetadataAsync_UnsupportedConnectionTypeSkipped verifies that
// connection types without a MetadataPusher (e.g. Lidarr) are skipped.
func TestPushMetadataAsync_UnsupportedConnectionTypeSkipped(t *testing.T) {
	hits := &pushHits{}
	srv := newEmbyTestServer(hits)
	defer srv.Close()

	p := New(Deps{
		Logger: silentLogger(),
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c-lid", PlatformArtistID: "p1"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c-lid": {ID: "c-lid", Type: connection.TypeLidarr, URL: srv.URL, Enabled: true},
		}},
	})
	p.PushMetadataAsync(context.Background(), &artist.Artist{ID: "a1"})
	time.Sleep(200 * time.Millisecond)
	if got := hits.posts.Load(); got != 0 {
		t.Errorf("Lidarr connection should be skipped; got %d POSTs", got)
	}
}

// TestPushMetadataAsync_PlatformUnreachableLogsError verifies that a push to
// an unreachable URL does not panic and does not block. The error is logged
// inside the goroutine; we only assert "no posts succeeded" by pointing the
// test server at a closed server.
func TestPushMetadataAsync_PlatformUnreachable(t *testing.T) {
	hits := &pushHits{}
	srv := newEmbyTestServer(hits)
	closedURL := srv.URL
	srv.Close() // close immediately so requests fail

	p := New(Deps{
		Logger: silentLogger(),
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c-emby", PlatformArtistID: "p1"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c-emby": {ID: "c-emby", Type: connection.TypeEmby, URL: closedURL, Enabled: true, PlatformUserID: "u1"},
		}},
	})
	p.PushMetadataAsync(context.Background(), &artist.Artist{ID: "a1", Name: "X"})

	// Give the goroutine time to error out.
	time.Sleep(300 * time.Millisecond)
	if got := hits.posts.Load(); got != 0 {
		t.Errorf("closed server cannot receive POSTs; got %d", got)
	}
}

// TestPushMetadataAsync_ContextWithoutCancel verifies that the per-goroutine
// timeout is derived from a canceled parent context (context.WithoutCancel
// path). The push should still run to completion despite cancellation.
func TestPushMetadataAsync_OutlivesParentCancel(t *testing.T) {
	hits := &pushHits{}
	srv := newEmbyTestServer(hits)
	defer srv.Close()

	p := New(Deps{
		Logger: silentLogger(),
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c-emby", PlatformArtistID: "p1"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c-emby": {ID: "c-emby", Type: connection.TypeEmby, URL: srv.URL, Enabled: true, PlatformUserID: "u1"},
		}},
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel BEFORE dispatch

	p.PushMetadataAsync(ctx, &artist.Artist{ID: "a1", Name: "OutlivesCancel"})

	// The goroutine uses context.WithoutCancel so the push should still happen.
	waitForPosts(t, &hits.posts, 1)
}

// --- PublishMetadata (orchestrator) ---

// TestPublishMetadata_NilReceiver verifies the nil guard.
func TestPublishMetadata_NilReceiver(t *testing.T) {
	var p *Publisher
	p.PublishMetadata(context.Background(), &artist.Artist{ID: "a"})
}

// TestPublishMetadata_WritesNFOAndPushes is the end-to-end smoke test: an
// artist with an existing NFO file and one platform mapping should both
// see its NFO rewritten and trigger an outbound POST.
func TestPublishMetadata_WritesNFOAndPushes(t *testing.T) {
	const existing = "<artist><name>Old</name></artist>"
	dir := writeArtistDir(t, existing)
	db := setupNFODB(t)
	logger := silentLogger()

	hits := &pushHits{}
	srv := newEmbyTestServer(hits)
	defer srv.Close()

	ew := newRecordingExpectedWrites()
	p := New(Deps{
		Logger:             logger,
		NFOSnapshotService: nfo.NewSnapshotService(db),
		NFOSettingsService: nfo.NewNFOSettingsService(db, logger),
		ExpectedWrites:     ew,
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c-emby", PlatformArtistID: "p1"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c-emby": {ID: "c-emby", Type: connection.TypeEmby, URL: srv.URL, Enabled: true, PlatformUserID: "u1"},
		}},
	})

	a := &artist.Artist{ID: "a1", Name: "PublishedName", Path: dir, Type: "group", Formed: "1970-01-01"}
	p.PublishMetadata(context.Background(), a)

	// NFO write was synchronous.
	got, err := os.ReadFile(filepath.Join(dir, "artist.nfo"))
	if err != nil {
		t.Fatalf("reading rewritten NFO: %v", err)
	}
	if !strings.Contains(string(got), "<name>PublishedName</name>") {
		t.Errorf("NFO should reflect new name. Got:\n%s", got)
	}

	// Async push should still arrive.
	waitForPosts(t, &hits.posts, 1)
}

// TestPublishMetadata_NoPathSkipsNFOButStillPushes verifies that an artist
// without a filesystem path skips the NFO write but still triggers the
// platform push (the two are independent).
func TestPublishMetadata_NoPathSkipsNFOButStillPushes(t *testing.T) {
	hits := &pushHits{}
	srv := newEmbyTestServer(hits)
	defer srv.Close()

	p := New(Deps{
		Logger: silentLogger(),
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c-emby", PlatformArtistID: "p1"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c-emby": {ID: "c-emby", Type: connection.TypeEmby, URL: srv.URL, Enabled: true, PlatformUserID: "u1"},
		}},
	})

	a := &artist.Artist{ID: "a1", Name: "Pathless"} // Path == ""
	p.PublishMetadata(context.Background(), a)

	waitForPosts(t, &hits.posts, 1)
}
