package publish

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/platform"
)

// artistRefreshRecorder captures the RefreshArtist calls a swapped
// artistRefresherFactory routes to it, keyed by connection ID. It lets the
// gated-dispatch tests assert which connections were refreshed (and with which
// platform artist ID) without standing up real Emby/Jellyfin peers; the
// production factory type-switch is covered separately by
// TestArtistRefresherFactory.
type artistRefreshRecorder struct {
	mu    sync.Mutex
	calls map[string]string // connID -> platformArtistID passed to RefreshArtist
}

func (rec *artistRefreshRecorder) forConn(conn *connection.Connection) artistRefresher {
	return &recordingArtistRefresher{rec: rec, connID: conn.ID}
}

func (rec *artistRefreshRecorder) count() int {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return len(rec.calls)
}

func (rec *artistRefreshRecorder) assertCalled(t *testing.T, connID, wantID string) {
	t.Helper()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	got, ok := rec.calls[connID]
	if !ok {
		t.Errorf("connection %s: expected a refresh call, got none", connID)
		return
	}
	if got != wantID {
		t.Errorf("connection %s: refreshed with platform id %q, want %q", connID, got, wantID)
	}
}

func (rec *artistRefreshRecorder) assertNoCall(t *testing.T, connID string) {
	t.Helper()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if _, ok := rec.calls[connID]; ok {
		t.Errorf("connection %s: expected no refresh call, but one was recorded", connID)
	}
}

type recordingArtistRefresher struct {
	rec    *artistRefreshRecorder
	connID string
}

func (r *recordingArtistRefresher) RefreshArtist(_ context.Context, platformArtistID string) error {
	r.rec.mu.Lock()
	defer r.rec.mu.Unlock()
	if r.rec.calls == nil {
		r.rec.calls = map[string]string{}
	}
	r.rec.calls[r.connID] = platformArtistID
	return nil
}

// swapArtistRefresherFactory overrides the package-level factory for the
// duration of the test, restoring it on cleanup. Mirrors
// swapMergeRefresherFactory. Not parallel-safe (the var is global).
func swapArtistRefresherFactory(t *testing.T, f func(*connection.Connection, *slog.Logger) (artistRefresher, bool)) {
	t.Helper()
	orig := artistRefresherFactory
	artistRefresherFactory = f
	t.Cleanup(func() { artistRefresherFactory = orig })
}

// waitForRefreshCount polls up to 2s for the recorder to reach want calls.
// RefreshArtistOnPlatforms dispatches fire-and-forget goroutines, so the test
// waits on the observable side effect rather than sleeping a fixed interval.
func waitForRefreshCount(t *testing.T, rec *artistRefreshRecorder, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if rec.count() >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected %d refresh calls, got %d", want, rec.count())
}

// TestArtistRefresherFactory locks in the production factory type-switch: Emby
// and Jellyfin get a non-nil per-artist refresher; Lidarr and unknown types do
// not (out of scope for #2336, no NFO re-import primitive).
func TestArtistRefresherFactory(t *testing.T) {
	logger := silentLogger()
	cases := []struct {
		name   string
		conn   *connection.Connection
		wantOK bool
	}{
		{"emby", &connection.Connection{Type: connection.TypeEmby}, true},
		{"jellyfin", &connection.Connection{Type: connection.TypeJellyfin}, true},
		{"lidarr", &connection.Connection{Type: connection.TypeLidarr}, false},
		{"unknown", &connection.Connection{Type: "kodi"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, ok := artistRefresherFactory(c.conn, logger)
			if ok != c.wantOK {
				t.Errorf("ok = %v, want %v", ok, c.wantOK)
			}
			if ok && r == nil {
				t.Error("factory returned ok=true with a nil refresher")
			}
			if !ok && r != nil {
				t.Error("factory returned ok=false with a non-nil refresher")
			}
		})
	}
}

// TestRefreshArtistOnPlatforms_GatedDispatch is the core #2336 regression: the
// feature-gated NFO re-import fires ONLY for enabled, healthy, opted-in
// Emby/Jellyfin connections. It covers, in a single fan-out so the two positive
// dispatches act as a barrier for the negative assertions:
//   - opted-in Emby + Jellyfin      -> dispatched (with the platform artist ID)
//   - FeatureTriggerRefresh == false -> skipped (the toggle gate)
//   - Enabled == false               -> skipped
//   - Status != "ok"                 -> skipped
//   - unsupported type (Lidarr)      -> skipped
func TestRefreshArtistOnPlatforms_GatedDispatch(t *testing.T) {
	rec := &artistRefreshRecorder{}
	swapArtistRefresherFactory(t, func(conn *connection.Connection, _ *slog.Logger) (artistRefresher, bool) {
		// Defer the unsupported decision to the production-shaped rule so the
		// Lidarr connection exercises the factory-miss branch even though the
		// gate would also skip it (Lidarr has no trigger-refresh toggle).
		switch conn.Type {
		case connection.TypeEmby, connection.TypeJellyfin:
			return rec.forConn(conn), true
		default:
			return nil, false
		}
	})

	embyOn := &connection.Connection{
		ID: "c-emby-on", Type: connection.TypeEmby, Enabled: true, Status: "ok", Name: "Emby On",
		Emby: &connection.EmbyConfig{FeatureTriggerRefresh: true},
	}
	jfOn := &connection.Connection{
		ID: "c-jf-on", Type: connection.TypeJellyfin, Enabled: true, Status: "ok", Name: "JF On",
		Jellyfin: &connection.JellyfinConfig{FeatureTriggerRefresh: true},
	}
	// Opted-out via the toggle: enabled + ok but FeatureTriggerRefresh=false.
	gateOff := &connection.Connection{
		ID: "c-gate-off", Type: connection.TypeEmby, Enabled: true, Status: "ok", Name: "Gate Off",
		Emby: &connection.EmbyConfig{FeatureTriggerRefresh: false},
	}
	// Disabled: the destructive re-import must not fire even when opted in.
	disabled := &connection.Connection{
		ID: "c-disabled", Type: connection.TypeEmby, Enabled: false, Status: "ok", Name: "Disabled",
		Emby: &connection.EmbyConfig{FeatureTriggerRefresh: true},
	}
	// Unhealthy: Status != "ok" is skipped even when opted in.
	badStatus := &connection.Connection{
		ID: "c-bad", Type: connection.TypeEmby, Enabled: true, Status: "error", Name: "Bad",
		Emby: &connection.EmbyConfig{FeatureTriggerRefresh: true},
	}
	// Lidarr: no NFO re-import primitive (unsupported), skipped.
	lidarr := &connection.Connection{
		ID: "c-lidarr", Type: connection.TypeLidarr, Enabled: true, Status: "ok", Name: "Lidarr",
	}

	p := New(Deps{
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c-emby-on", PlatformArtistID: "emby-pid"},
			{ArtistID: "a1", ConnectionID: "c-jf-on", PlatformArtistID: "jf-pid"},
			{ArtistID: "a1", ConnectionID: "c-gate-off", PlatformArtistID: "off-pid"},
			{ArtistID: "a1", ConnectionID: "c-disabled", PlatformArtistID: "dis-pid"},
			{ArtistID: "a1", ConnectionID: "c-bad", PlatformArtistID: "bad-pid"},
			{ArtistID: "a1", ConnectionID: "c-lidarr", PlatformArtistID: "42"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c-emby-on":  embyOn,
			"c-jf-on":    jfOn,
			"c-gate-off": gateOff,
			"c-disabled": disabled,
			"c-bad":      badStatus,
			"c-lidarr":   lidarr,
		}},
		Logger: silentLogger(),
	})

	p.RefreshArtistOnPlatforms(context.Background(), &artist.Artist{ID: "a1", Name: "X"})

	// Two opted-in connections must dispatch; wait on that observable outcome.
	waitForRefreshCount(t, rec, 2)
	rec.assertCalled(t, "c-emby-on", "emby-pid")
	rec.assertCalled(t, "c-jf-on", "jf-pid")

	// Grace so any (incorrect) negative-case goroutine that was going to fire
	// has time to record before we assert its absence. The negatives do less
	// work than the positives, so once the two positives are recorded the
	// negatives have almost certainly returned; the grace covers scheduling skew.
	time.Sleep(100 * time.Millisecond)
	rec.assertNoCall(t, "c-gate-off")
	rec.assertNoCall(t, "c-disabled")
	rec.assertNoCall(t, "c-bad")
	rec.assertNoCall(t, "c-lidarr")
	if got := rec.count(); got != 2 {
		t.Errorf("total refresh calls = %d, want exactly 2 (only opted-in connections)", got)
	}
}

// TestPublishMetadata_TriggersGatedRefresh is the #2336 item-(b) wiring proof:
// PublishMetadata must invoke the gated NFO re-import (after WriteBackNFO) so an
// opted-in connection actually re-reads the freshly-written NFO. Removing the
// RefreshArtistOnPlatforms call from PublishMetadata makes this RED. The artist
// has a real path with no artist.nfo, so WriteBackNFO CREATES one (nfoWritten
// == true) and the destructive re-import is allowed to fire.
func TestPublishMetadata_TriggersGatedRefresh(t *testing.T) {
	rec := &artistRefreshRecorder{}
	swapArtistRefresherFactory(t, func(conn *connection.Connection, _ *slog.Logger) (artistRefresher, bool) {
		return rec.forConn(conn), true
	})

	p := New(Deps{
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c-emby", PlatformArtistID: "emby-pid"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c-emby": {
				ID: "c-emby", Type: connection.TypeEmby, Enabled: true, Status: "ok", Name: "Emby",
				Emby: &connection.EmbyConfig{FeatureTriggerRefresh: true},
			},
		}},
		Logger: silentLogger(),
	})

	// A real path with no artist.nfo -> WriteBackNFO creates one (nfoWritten),
	// so the gated re-import must fire. PushMetadataAsync's dead-URL push fails
	// harmlessly; only the refresh dispatch is asserted.
	p.PublishMetadata(context.Background(), &artist.Artist{ID: "a1", Name: "X", Path: t.TempDir()})

	waitForRefreshCount(t, rec, 1)
	rec.assertCalled(t, "c-emby", "emby-pid")
}

// TestPublishMetadata_SkipsRefreshWhenNoNFOWritten is the #2336 review-P3
// regression: when WriteBackNFO writes NO NFO this publish (the artist has no
// library path), the destructive FullRefresh re-import MUST be skipped -- with
// no fresh local NFO an opted-in Emby could re-scrape from online fetchers and
// clobber the platform metadata. Removing the nfoWritten gate from
// PublishMetadata makes this RED (the refresh fires with count 1).
func TestPublishMetadata_SkipsRefreshWhenNoNFOWritten(t *testing.T) {
	rec := &artistRefreshRecorder{}
	swapArtistRefresherFactory(t, func(conn *connection.Connection, _ *slog.Logger) (artistRefresher, bool) {
		return rec.forConn(conn), true
	})

	p := New(Deps{
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c-emby", PlatformArtistID: "emby-pid"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c-emby": {
				ID: "c-emby", Type: connection.TypeEmby, Enabled: true, Status: "ok", Name: "Emby",
				Emby: &connection.EmbyConfig{FeatureTriggerRefresh: true},
			},
		}},
		Logger: silentLogger(),
	})

	// No Path -> WriteBackNFO returns false (nothing written) -> re-import gated off.
	p.PublishMetadata(context.Background(), &artist.Artist{ID: "a1", Name: "X"})

	// Grace so a (buggy) ungated dispatch would have time to record before we
	// assert its absence. The push runs first (dead URL, fails fast) and the
	// coordinator only then decides whether to fire the refresh.
	time.Sleep(200 * time.Millisecond)
	if got := rec.count(); got != 0 {
		t.Errorf("refresh calls = %d, want 0 when no NFO was written this publish", got)
	}
}

// TestPublishMetadata_SkipsRefreshWhenNFODisabledByProfile covers the
// WriteBackNFO profile-gate false-return (NFOWriteAllowed == false): even with
// a valid artist Path, an active platform profile with NFOEnabled=false (e.g.
// Plex) must skip the NFO write, so PublishMetadata must not fire the gated
// destructive re-import either -- there is no fresh local NFO for the platform
// to re-read.
func TestPublishMetadata_SkipsRefreshWhenNFODisabledByProfile(t *testing.T) {
	rec := &artistRefreshRecorder{}
	swapArtistRefresherFactory(t, func(conn *connection.Connection, _ *slog.Logger) (artistRefresher, bool) {
		return rec.forConn(conn), true
	})

	p := New(Deps{
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c-emby", PlatformArtistID: "emby-pid"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c-emby": {
				ID: "c-emby", Type: connection.TypeEmby, Enabled: true, Status: "ok", Name: "Emby",
				Emby: &connection.EmbyConfig{FeatureTriggerRefresh: true},
			},
		}},
		PlatformService: &fakePlatformProvider{profile: &platform.Profile{Name: "Plex", NFOEnabled: false}},
		Logger:          silentLogger(),
	})

	// Valid Path, but the active profile disables NFO writing -> WriteBackNFO
	// returns false via the profile gate (not the a.Path=="" branch), so the
	// re-import must stay gated off.
	p.PublishMetadata(context.Background(), &artist.Artist{ID: "a1", Name: "X", Path: t.TempDir()})

	time.Sleep(200 * time.Millisecond)
	if got := rec.count(); got != 0 {
		t.Errorf("refresh calls = %d, want 0 when the active profile disables NFO writing", got)
	}
}

// TestPublishMetadata_SkipsRefreshWhenNFOWriteFails covers the WriteBackNFO
// atomic-write-failure false-return: when the artist directory is unwritable,
// filesystem.WriteFileAtomic fails, WriteBackNFO returns false, and
// PublishMetadata must not fire the gated destructive re-import.
func TestPublishMetadata_SkipsRefreshWhenNFOWriteFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; chmod 0o555 does not block writes and the write-failure branch cannot be exercised on this runner")
	}
	rec := &artistRefreshRecorder{}
	swapArtistRefresherFactory(t, func(conn *connection.Connection, _ *slog.Logger) (artistRefresher, bool) {
		return rec.forConn(conn), true
	})

	artistDir := t.TempDir()
	if err := os.Chmod(artistDir, 0o555); err != nil {
		t.Fatalf("chmod 555 artist dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(artistDir, 0o755) })

	p := New(Deps{
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c-emby", PlatformArtistID: "emby-pid"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c-emby": {
				ID: "c-emby", Type: connection.TypeEmby, Enabled: true, Status: "ok", Name: "Emby",
				Emby: &connection.EmbyConfig{FeatureTriggerRefresh: true},
			},
		}},
		Logger: silentLogger(),
	})

	// Unwritable artist dir -> the atomic create write fails, WriteBackNFO
	// returns false, and the re-import must stay gated off.
	p.PublishMetadata(context.Background(), &artist.Artist{ID: "a1", Name: "X", Path: artistDir})

	time.Sleep(200 * time.Millisecond)
	if got := rec.count(); got != 0 {
		t.Errorf("refresh calls = %d, want 0 when the NFO write fails", got)
	}
}

// TestRefreshArtistOnPlatforms_NilReceiver covers the nil-receiver guard.
func TestRefreshArtistOnPlatforms_NilReceiver(t *testing.T) {
	var p *Publisher
	// Must not panic.
	p.RefreshArtistOnPlatforms(context.Background(), &artist.Artist{ID: "a1"})
}

// TestRefreshArtistOnPlatforms_NoPlatformIDs covers the early return when the
// artist is not mapped to any connection: no factory call, no panic.
func TestRefreshArtistOnPlatforms_NoPlatformIDs(t *testing.T) {
	rec := &artistRefreshRecorder{}
	swapArtistRefresherFactory(t, func(conn *connection.Connection, _ *slog.Logger) (artistRefresher, bool) {
		return rec.forConn(conn), true
	})
	p := New(Deps{
		ArtistService:     &fakePlatformLister{ids: nil},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{}},
		Logger:            silentLogger(),
	})
	p.RefreshArtistOnPlatforms(context.Background(), &artist.Artist{ID: "a1"})
	time.Sleep(50 * time.Millisecond)
	if got := rec.count(); got != 0 {
		t.Errorf("refresh calls = %d, want 0 when artist has no platform mappings", got)
	}
}

// TestRefreshArtistOnPlatforms_ListerErrorReturns covers the GetPlatformIDs
// error branch: it logs and returns without dispatching.
func TestRefreshArtistOnPlatforms_ListerErrorReturns(t *testing.T) {
	rec := &artistRefreshRecorder{}
	swapArtistRefresherFactory(t, func(conn *connection.Connection, _ *slog.Logger) (artistRefresher, bool) {
		return rec.forConn(conn), true
	})
	p := New(Deps{
		ArtistService:     errPlatformLister{},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{}},
		Logger:            silentLogger(),
	})
	p.RefreshArtistOnPlatforms(context.Background(), &artist.Artist{ID: "a1"})
	time.Sleep(50 * time.Millisecond)
	if got := rec.count(); got != 0 {
		t.Errorf("refresh calls = %d, want 0 when GetPlatformIDs errors", got)
	}
}
