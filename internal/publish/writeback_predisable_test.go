package publish

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/library"
)

// fixedLibraryResolver returns a library with a fixed shared-FS status for the
// pre-push write-back gate (#2533).
type fixedLibraryResolver struct{ status string }

func (r fixedLibraryResolver) FindForArtistPath(_ context.Context, path string) (*library.Library, error) {
	return &library.Library{Path: path, SharedFSStatus: r.status}, nil
}

// eventLog records an ordered, concurrency-safe sequence of named events so a
// test can assert that the managed-peer write-back disable happens BEFORE the
// image upload (#2533).
type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (e *eventLog) add(name string) {
	e.mu.Lock()
	e.events = append(e.events, name)
	e.mu.Unlock()
}

func (e *eventLog) snapshot() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.events))
	copy(out, e.events)
	return out
}

func indexOf(seq []string, name string) int {
	for i, s := range seq {
		if s == name {
			return i
		}
	}
	return -1
}

// stubWritebackCtrl is a recording writebackController: it logs a "check" and a
// "disable" event into the shared eventLog and returns scripted results.
type stubWritebackCtrl struct {
	log        *eventLog
	saverOn    bool
	checkErr   error
	disableErr error
	checks     *int
	disables   *int
}

func (s *stubWritebackCtrl) CheckImageSaverEnabled(_ context.Context) (bool, string, error) {
	s.log.add("check")
	*s.checks++
	return s.saverOn, "MusicLib", s.checkErr
}

func (s *stubWritebackCtrl) DisableFileWriteBack(_ context.Context) error {
	s.log.add("disable")
	*s.disables++
	return s.disableErr
}

// predisableHarness wires a Publisher whose image upload lands on an httptest
// server (recording an "upload" event) and whose write-back controller factory
// is swapped for a recording stub. It returns the publisher, the event log, and
// pointers to the check/disable call counters. newWritebackControllerFn is
// restored on cleanup.
func predisableHarness(t *testing.T, conn *connection.Connection, sharedFSStatus string, stub *stubWritebackCtrl, log *eventLog, factoryCalls *int) (*Publisher, string) {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		log.add("upload")
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	conn.URL = srv.URL

	dir := t.TempDir()
	// folder.jpg is the primary (thumb) source; fanart.jpg is the primary
	// backdrop source so both the primary and fanart operator-push paths have a
	// file to read and actually attempt an upload.
	for _, name := range []string{"folder.jpg", "fanart.jpg"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("JFIF"), 0o600); err != nil {
			t.Fatalf("writing test image %s: %v", name, err)
		}
	}

	orig := newWritebackControllerFn
	newWritebackControllerFn = func(_ *connection.Connection, _ *slog.Logger) writebackController {
		*factoryCalls++
		return stub
	}
	t.Cleanup(func() { newWritebackControllerFn = orig })

	p := New(Deps{
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: conn.ID, PlatformArtistID: "p1"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			conn.ID: conn,
		}},
		LibraryService: fixedLibraryResolver{status: sharedFSStatus},
		Logger:         silentLogger(),
	})
	return p, dir
}

// TestSyncImage_ManagedConnection_DisablesWritebackBeforeUpload proves the core
// #2533 fix: on the operator push path, a MANAGED connection has its peer
// write-back turned OFF (check + disable) BEFORE the image is uploaded, so a
// shared-volume peer cannot clobber the operator's file after the push.
func TestSyncImage_ManagedConnection_DisablesWritebackBeforeUpload(t *testing.T) {
	log := &eventLog{}
	var checks, disables, factoryCalls int
	stub := &stubWritebackCtrl{log: log, saverOn: true, checks: &checks, disables: &disables}
	conn := &connection.Connection{
		ID: "c-emby", Name: "emby", Type: connection.TypeEmby, Enabled: true, Status: "ok",
		FeatureManageServerFiles: true,
		Emby:                     &connection.EmbyConfig{PlatformUserID: "u1"},
	}
	p, dir := predisableHarness(t, conn, library.SharedFSConfirmed, stub, log, &factoryCalls)

	p.SyncImageToPlatforms(context.Background(), &artist.Artist{ID: "a1", Name: "A", Path: dir}, "thumb")

	seq := log.snapshot()
	di, ui := indexOf(seq, "disable"), indexOf(seq, "upload")
	if di == -1 {
		t.Fatalf("no disable event recorded; managed write-back was not turned off. seq=%v", seq)
	}
	if ui == -1 {
		t.Fatalf("no upload event recorded; the image was never pushed. seq=%v", seq)
	}
	if di >= ui {
		t.Errorf("disable must happen BEFORE upload; got seq=%v (disable@%d, upload@%d)", seq, di, ui)
	}
	if checks != 1 || disables != 1 {
		t.Errorf("checks=%d disables=%d, want 1 and 1", checks, disables)
	}
}

// TestSyncImage_UnmanagedConnection_NoWritebackControl proves unmanaged
// connections are UNCHANGED: no saver check, no disable, no peer setting flip;
// the upload still happens exactly as before.
func TestSyncImage_UnmanagedConnection_NoWritebackControl(t *testing.T) {
	log := &eventLog{}
	var checks, disables, factoryCalls int
	stub := &stubWritebackCtrl{log: log, saverOn: true, checks: &checks, disables: &disables}
	conn := &connection.Connection{
		ID: "c-emby", Name: "emby", Type: connection.TypeEmby, Enabled: true, Status: "ok",
		FeatureManageServerFiles: false, // unmanaged
		Emby:                     &connection.EmbyConfig{PlatformUserID: "u1"},
	}
	p, dir := predisableHarness(t, conn, library.SharedFSConfirmed, stub, log, &factoryCalls)

	p.SyncImageToPlatforms(context.Background(), &artist.Artist{ID: "a1", Name: "A", Path: dir}, "thumb")

	seq := log.snapshot()
	if factoryCalls != 0 {
		t.Errorf("write-back controller was constructed for an UNMANAGED connection (factoryCalls=%d); must be 0", factoryCalls)
	}
	if checks != 0 || disables != 0 {
		t.Errorf("unmanaged connection triggered saver control: checks=%d disables=%d, want 0/0. seq=%v", checks, disables, seq)
	}
	if indexOf(seq, "upload") == -1 {
		t.Errorf("upload did not happen for the unmanaged connection; behavior changed. seq=%v", seq)
	}
}

// TestSyncImage_ManagedConnection_SaverAlreadyOff_Idempotent proves the check
// short-circuits a redundant disable round-trip when the peer saver is already
// off.
func TestSyncImage_ManagedConnection_SaverAlreadyOff_Idempotent(t *testing.T) {
	log := &eventLog{}
	var checks, disables, factoryCalls int
	stub := &stubWritebackCtrl{log: log, saverOn: false, checks: &checks, disables: &disables}
	conn := &connection.Connection{
		ID: "c-jf", Name: "jf", Type: connection.TypeJellyfin, Enabled: true, Status: "ok",
		FeatureManageServerFiles: true,
		Jellyfin:                 &connection.JellyfinConfig{PlatformUserID: "u1"},
	}
	p, dir := predisableHarness(t, conn, library.SharedFSConfirmed, stub, log, &factoryCalls)

	p.SyncImageToPlatforms(context.Background(), &artist.Artist{ID: "a1", Name: "A", Path: dir}, "thumb")

	seq := log.snapshot()
	if checks != 1 {
		t.Errorf("checks=%d, want 1 (the saver state must be read)", checks)
	}
	if disables != 0 {
		t.Errorf("disables=%d, want 0 (already-off must not trigger a redundant disable)", disables)
	}
	if indexOf(seq, "upload") == -1 {
		t.Errorf("upload did not happen. seq=%v", seq)
	}
}

// TestSyncImage_ManagedConnection_CheckError_DisablesDefensively proves that
// when the saver-state check fails, we still disable (fail toward preserving
// the operator's file) before uploading.
func TestSyncImage_ManagedConnection_CheckError_DisablesDefensively(t *testing.T) {
	log := &eventLog{}
	var checks, disables, factoryCalls int
	stub := &stubWritebackCtrl{log: log, saverOn: false, checkErr: context.DeadlineExceeded, checks: &checks, disables: &disables}
	conn := &connection.Connection{
		ID: "c-emby", Name: "emby", Type: connection.TypeEmby, Enabled: true, Status: "ok",
		FeatureManageServerFiles: true,
		Emby:                     &connection.EmbyConfig{PlatformUserID: "u1"},
	}
	p, dir := predisableHarness(t, conn, library.SharedFSConfirmed, stub, log, &factoryCalls)

	p.SyncImageToPlatforms(context.Background(), &artist.Artist{ID: "a1", Name: "A", Path: dir}, "thumb")

	seq := log.snapshot()
	if disables != 1 {
		t.Errorf("disables=%d, want 1 (a failed check must disable defensively)", disables)
	}
	di, ui := indexOf(seq, "disable"), indexOf(seq, "upload")
	if di == -1 || ui == -1 || di >= ui {
		t.Errorf("defensive disable must precede upload; seq=%v", seq)
	}
}

// TestSyncImage_ManagedConnection_NotSharedFS_NoDisable proves the shared-FS
// gate: a MANAGED connection whose library is NOT on a shared filesystem
// (dedicated volume) pushes UNCHANGED -- the peer cannot clobber a file it does
// not share, so no saver check and no disable fire. This is the maintainer's
// zero-behavior-change requirement for non-shared setups (#2533).
func TestSyncImage_ManagedConnection_NotSharedFS_NoDisable(t *testing.T) {
	log := &eventLog{}
	var checks, disables, factoryCalls int
	stub := &stubWritebackCtrl{log: log, saverOn: true, checks: &checks, disables: &disables}
	conn := &connection.Connection{
		ID: "c-emby", Name: "emby", Type: connection.TypeEmby, Enabled: true, Status: "ok",
		FeatureManageServerFiles: true, // managed, but...
		Emby:                     &connection.EmbyConfig{PlatformUserID: "u1"},
	}
	// ...the library is on a dedicated (non-shared) volume.
	p, dir := predisableHarness(t, conn, library.SharedFSNone, stub, log, &factoryCalls)

	p.SyncImageToPlatforms(context.Background(), &artist.Artist{ID: "a1", Name: "A", Path: dir}, "thumb")

	seq := log.snapshot()
	if factoryCalls != 0 {
		t.Errorf("write-back controller constructed for a NON-shared-FS library (factoryCalls=%d); must be 0", factoryCalls)
	}
	if checks != 0 || disables != 0 {
		t.Errorf("non-shared-FS managed connection triggered saver control: checks=%d disables=%d, want 0/0. seq=%v", checks, disables, seq)
	}
	if indexOf(seq, "upload") == -1 {
		t.Errorf("upload did not happen for the non-shared-FS connection; behavior changed. seq=%v", seq)
	}
}

// TestSyncImage_ManagedConnection_UnevaluatedSharedFS_Disables proves the
// fail-closed gap fix: a MANAGED connection whose library has NOT yet been
// evaluated by the shared-FS conflict detector (SharedFSStatus == "", the
// library's zero value before the detector ever runs) is treated as shared
// -- the pre-disable still fires -- rather than silently skipping protection
// during that unknown window (#2533 fail-closed gap).
func TestSyncImage_ManagedConnection_UnevaluatedSharedFS_Disables(t *testing.T) {
	log := &eventLog{}
	var checks, disables, factoryCalls int
	stub := &stubWritebackCtrl{log: log, saverOn: true, checks: &checks, disables: &disables}
	conn := &connection.Connection{
		ID: "c-emby", Name: "emby", Type: connection.TypeEmby, Enabled: true, Status: "ok",
		FeatureManageServerFiles: true,
		Emby:                     &connection.EmbyConfig{PlatformUserID: "u1"},
	}
	// Unevaluated: the conflict detector has never run for this library.
	p, dir := predisableHarness(t, conn, "", stub, log, &factoryCalls)

	p.SyncImageToPlatforms(context.Background(), &artist.Artist{ID: "a1", Name: "A", Path: dir}, "thumb")

	seq := log.snapshot()
	di, ui := indexOf(seq, "disable"), indexOf(seq, "upload")
	if di == -1 {
		t.Fatalf("no disable event for an unevaluated (SharedFSStatus=\"\") library; must fail closed and disable. seq=%v", seq)
	}
	if ui == -1 {
		t.Fatalf("no upload event recorded; the image was never pushed. seq=%v", seq)
	}
	if di >= ui {
		t.Errorf("disable must happen BEFORE upload; got seq=%v (disable@%d, upload@%d)", seq, di, ui)
	}
	if checks != 1 || disables != 1 {
		t.Errorf("checks=%d disables=%d, want 1 and 1", checks, disables)
	}
}

// TestSyncAllFanart_ManagedConnection_DisablesWritebackBeforeUpload proves the
// operator-initiated FANART push (SyncAllFanartToPlatforms) gets the identical
// pre-push write-back disable: an operator who crops/sets/reorders a backdrop
// on a shared volume is protected the same way as a primary crop (#2533).
func TestSyncAllFanart_ManagedConnection_DisablesWritebackBeforeUpload(t *testing.T) {
	log := &eventLog{}
	var checks, disables, factoryCalls int
	stub := &stubWritebackCtrl{log: log, saverOn: true, checks: &checks, disables: &disables}
	conn := &connection.Connection{
		ID: "c-emby", Name: "emby", Type: connection.TypeEmby, Enabled: true, Status: "ok",
		FeatureManageServerFiles: true,
		Emby:                     &connection.EmbyConfig{PlatformUserID: "u1"},
	}
	p, dir := predisableHarness(t, conn, library.SharedFSConfirmed, stub, log, &factoryCalls)

	p.SyncAllFanartToPlatforms(context.Background(), &artist.Artist{ID: "a1", Name: "A", Path: dir})

	seq := log.snapshot()
	di, ui := indexOf(seq, "disable"), indexOf(seq, "upload")
	if di == -1 {
		t.Fatalf("no disable event on the fanart operator push; managed write-back not turned off. seq=%v", seq)
	}
	if ui == -1 {
		t.Fatalf("no upload event; the fanart was never pushed. seq=%v", seq)
	}
	if di >= ui {
		t.Errorf("disable must precede the fanart upload; seq=%v (disable@%d, upload@%d)", seq, di, ui)
	}
}

// errLibraryResolver simulates a shared-FS lookup failure so
// artistOnSharedFS's fail-closed error branch can be exercised directly.
type errLibraryResolver struct{ err error }

func (r errLibraryResolver) FindForArtistPath(_ context.Context, _ string) (*library.Library, error) {
	return nil, r.err
}

// nilLibraryResolver returns a nil library with no error, exercising
// artistOnSharedFS's fail-closed nil-library branch.
type nilLibraryResolver struct{}

func (nilLibraryResolver) FindForArtistPath(_ context.Context, _ string) (*library.Library, error) {
	return nil, nil
}

// TestArtistOnSharedFS_GuardClauses_FailClosed proves the three preconditions
// guarding the shared-FS lookup (no library service wired, a nil artist, or
// an artist with no configured image path) all fail closed -- treated as
// shared rather than skipping protection -- instead of panicking.
func TestArtistOnSharedFS_GuardClauses_FailClosed(t *testing.T) {
	tests := []struct {
		name string
		p    *Publisher
		a    *artist.Artist
	}{
		{
			name: "nil library service",
			p:    New(Deps{Logger: silentLogger()}),
			a:    &artist.Artist{ID: "a1", Path: "/music/a"},
		},
		{
			name: "nil artist",
			p:    New(Deps{Logger: silentLogger(), LibraryService: fixedLibraryResolver{status: library.SharedFSConfirmed}}),
			a:    nil,
		},
		{
			name: "empty artist path",
			p:    New(Deps{Logger: silentLogger(), LibraryService: fixedLibraryResolver{status: library.SharedFSConfirmed}}),
			a:    &artist.Artist{ID: "a1", Path: ""},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.p.artistOnSharedFS(context.Background(), tc.a) {
				t.Errorf("%s must fail closed (return true)", tc.name)
			}
		})
	}
}

// TestArtistOnSharedFS_LookupError_FailsClosed proves a FindForArtistPath
// error is treated as shared (fail closed) so the pre-disable still fires
// rather than silently skipping protection on a transient lookup failure.
func TestArtistOnSharedFS_LookupError_FailsClosed(t *testing.T) {
	p := New(Deps{Logger: silentLogger(), LibraryService: errLibraryResolver{err: context.DeadlineExceeded}})
	if !p.artistOnSharedFS(context.Background(), &artist.Artist{ID: "a1", Path: "/music/a"}) {
		t.Error("a shared-FS lookup error must fail closed (return true)")
	}
}

// TestArtistOnSharedFS_NilLibrary_FailsClosed proves a nil (not-found)
// library result fails closed.
func TestArtistOnSharedFS_NilLibrary_FailsClosed(t *testing.T) {
	p := New(Deps{Logger: silentLogger(), LibraryService: nilLibraryResolver{}})
	if !p.artistOnSharedFS(context.Background(), &artist.Artist{ID: "a1", Path: "/music/a"}) {
		t.Error("a nil library lookup result must fail closed (return true)")
	}
}

// TestEnsurePeerWritebackDisabled_UnsupportedConnectionType_NoOp proves that a
// connection type without a write-back controller (e.g. Lidarr) is a no-op:
// the real (unmocked) newWritebackControllerFn factory returns nil for it,
// so ensurePeerWritebackDisabled must return immediately without panicking.
func TestEnsurePeerWritebackDisabled_UnsupportedConnectionType_NoOp(t *testing.T) {
	p := New(Deps{Logger: silentLogger()})
	conn := &connection.Connection{ID: "c-lidarr", Name: "lidarr", Type: connection.TypeLidarr, Enabled: true, Status: "ok"}

	// Must not panic on a nil controller, and must not touch the connection.
	p.ensurePeerWritebackDisabled(context.Background(), conn)
}

// TestEnsurePeerWritebackDisabled_DisableError_LogsAndReturns proves that when
// the peer's disable call itself fails, ensurePeerWritebackDisabled logs and
// returns without panicking -- the push proceeds best-effort (#2533: the
// local file is already the source of truth).
func TestEnsurePeerWritebackDisabled_DisableError_LogsAndReturns(t *testing.T) {
	log := &eventLog{}
	var checks, disables int
	stub := &stubWritebackCtrl{log: log, saverOn: true, disableErr: context.DeadlineExceeded, checks: &checks, disables: &disables}

	orig := newWritebackControllerFn
	newWritebackControllerFn = func(_ *connection.Connection, _ *slog.Logger) writebackController { return stub }
	t.Cleanup(func() { newWritebackControllerFn = orig })

	p := New(Deps{Logger: silentLogger()})
	conn := &connection.Connection{ID: "c-emby", Name: "emby", Type: connection.TypeEmby, Enabled: true, Status: "ok"}

	p.ensurePeerWritebackDisabled(context.Background(), conn)

	if checks != 1 {
		t.Errorf("checks=%d, want 1", checks)
	}
	if disables != 1 {
		t.Errorf("disables=%d, want 1 (the disable must still be attempted)", disables)
	}
	if indexOf(log.snapshot(), "disable") == -1 {
		t.Error("no disable event recorded despite the saver being on")
	}
}

// TestSyncAllFanart_UnmanagedConnection_NoWritebackControl proves the fanart
// path leaves unmanaged connections unchanged, mirroring the primary path.
func TestSyncAllFanart_UnmanagedConnection_NoWritebackControl(t *testing.T) {
	log := &eventLog{}
	var checks, disables, factoryCalls int
	stub := &stubWritebackCtrl{log: log, saverOn: true, checks: &checks, disables: &disables}
	conn := &connection.Connection{
		ID: "c-emby", Name: "emby", Type: connection.TypeEmby, Enabled: true, Status: "ok",
		FeatureManageServerFiles: false, // unmanaged
		Emby:                     &connection.EmbyConfig{PlatformUserID: "u1"},
	}
	p, dir := predisableHarness(t, conn, library.SharedFSConfirmed, stub, log, &factoryCalls)

	p.SyncAllFanartToPlatforms(context.Background(), &artist.Artist{ID: "a1", Name: "A", Path: dir})

	seq := log.snapshot()
	if factoryCalls != 0 || checks != 0 || disables != 0 {
		t.Errorf("unmanaged fanart push triggered saver control: factoryCalls=%d checks=%d disables=%d, want 0/0/0. seq=%v", factoryCalls, checks, disables, seq)
	}
	if indexOf(seq, "upload") == -1 {
		t.Errorf("fanart upload did not happen for the unmanaged connection. seq=%v", seq)
	}
}
