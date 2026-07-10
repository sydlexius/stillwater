package publish

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
)

// erroringArtistRefresher records the call (so the test can prove the attempt
// happened) and then returns an error, exercising RefreshArtistOnPlatforms'
// per-connection error branch.
type erroringArtistRefresher struct {
	rec    *artistRefreshRecorder
	connID string
}

func (r *erroringArtistRefresher) RefreshArtist(_ context.Context, platformArtistID string) error {
	r.rec.mu.Lock()
	if r.rec.calls == nil {
		r.rec.calls = map[string]string{}
	}
	r.rec.calls[r.connID] = platformArtistID
	r.rec.mu.Unlock()
	return fmt.Errorf("simulated refresh failure")
}

// panickingArtistRefresher panics from RefreshArtist, exercising the goroutine's
// recover guard. It records nothing (the panic aborts before any record).
type panickingArtistRefresher struct{}

func (panickingArtistRefresher) RefreshArtist(context.Context, string) error {
	panic("simulated refresher panic")
}

// TestRefreshArtistOnPlatforms_RefreshErrorIsNonFatal proves a refresh failure on
// one connection is logged, non-fatal, and does NOT prevent the other opted-in
// connections from being refreshed. Isolation is the point: if the error were
// propagated (or aborted the loop) instead of contained per goroutine, c-ok
// would never dispatch and the wait for 2 calls would time out.
func TestRefreshArtistOnPlatforms_RefreshErrorIsNonFatal(t *testing.T) {
	rec := &artistRefreshRecorder{}
	swapArtistRefresherFactory(t, func(conn *connection.Connection, _ *slog.Logger) (artistRefresher, bool) {
		if conn.ID == "c-err" {
			return &erroringArtistRefresher{rec: rec, connID: conn.ID}, true
		}
		return rec.forConn(conn), true
	})

	embyErr := &connection.Connection{
		ID: "c-err", Type: connection.TypeEmby, Enabled: true, Status: "ok", Name: "Err",
		Emby: &connection.EmbyConfig{FeatureTriggerRefresh: true},
	}
	embyOK := &connection.Connection{
		ID: "c-ok", Type: connection.TypeEmby, Enabled: true, Status: "ok", Name: "OK",
		Emby: &connection.EmbyConfig{FeatureTriggerRefresh: true},
	}
	p := New(Deps{
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c-err", PlatformArtistID: "err-pid"},
			{ArtistID: "a1", ConnectionID: "c-ok", PlatformArtistID: "ok-pid"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c-err": embyErr,
			"c-ok":  embyOK,
		}},
		Logger: silentLogger(),
	})

	p.RefreshArtistOnPlatforms(context.Background(), &artist.Artist{ID: "a1", Name: "X"})

	// Both must be attempted: c-err records then returns its error, c-ok records
	// normally. Reaching 2 proves the error on c-err did not block c-ok.
	waitForRefreshCount(t, rec, 2)
	rec.assertCalled(t, "c-err", "err-pid") // the error branch actually ran
	rec.assertCalled(t, "c-ok", "ok-pid")   // sibling connection unaffected
}

// TestRefreshArtistOnPlatforms_PanicRecovered proves a panic inside one
// connection's refresh goroutine is recovered (the recover guard) and does NOT
// take down the process or prevent a sibling connection from dispatching. Remove
// the recover in refresh_trigger.go and this test crashes the binary instead of
// passing.
func TestRefreshArtistOnPlatforms_PanicRecovered(t *testing.T) {
	rec := &artistRefreshRecorder{}
	swapArtistRefresherFactory(t, func(conn *connection.Connection, _ *slog.Logger) (artistRefresher, bool) {
		if conn.ID == "c-panic" {
			return panickingArtistRefresher{}, true
		}
		return rec.forConn(conn), true
	})

	embyPanic := &connection.Connection{
		ID: "c-panic", Type: connection.TypeEmby, Enabled: true, Status: "ok", Name: "Panic",
		Emby: &connection.EmbyConfig{FeatureTriggerRefresh: true},
	}
	embyOK := &connection.Connection{
		ID: "c-ok", Type: connection.TypeEmby, Enabled: true, Status: "ok", Name: "OK",
		Emby: &connection.EmbyConfig{FeatureTriggerRefresh: true},
	}
	p := New(Deps{
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c-panic", PlatformArtistID: "panic-pid"},
			{ArtistID: "a1", ConnectionID: "c-ok", PlatformArtistID: "ok-pid"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c-panic": embyPanic,
			"c-ok":    embyOK,
		}},
		Logger: silentLogger(),
	})

	p.RefreshArtistOnPlatforms(context.Background(), &artist.Artist{ID: "a1", Name: "X"})

	// The healthy sibling must still dispatch despite the panic in c-panic.
	waitForRefreshCount(t, rec, 1)
	rec.assertCalled(t, "c-ok", "ok-pid")
	// c-panic aborted before recording anything.
	time.Sleep(100 * time.Millisecond)
	rec.assertNoCall(t, "c-panic")
}

// TestRefreshArtistOnPlatforms_ConnectionFetchErrorSkips proves that when the
// connection for a platform mapping cannot be fetched (GetByID errors), that
// mapping is skipped (logged, no refresh) while the other, resolvable
// connections still dispatch.
func TestRefreshArtistOnPlatforms_ConnectionFetchErrorSkips(t *testing.T) {
	rec := &artistRefreshRecorder{}
	swapArtistRefresherFactory(t, func(conn *connection.Connection, _ *slog.Logger) (artistRefresher, bool) {
		return rec.forConn(conn), true
	})

	embyOK := &connection.Connection{
		ID: "c-ok", Type: connection.TypeEmby, Enabled: true, Status: "ok", Name: "OK",
		Emby: &connection.EmbyConfig{FeatureTriggerRefresh: true},
	}
	p := New(Deps{
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			// c-missing is referenced by a mapping but absent from the getter,
			// so GetByID returns an error for it.
			{ArtistID: "a1", ConnectionID: "c-missing", PlatformArtistID: "gone-pid"},
			{ArtistID: "a1", ConnectionID: "c-ok", PlatformArtistID: "ok-pid"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c-ok": embyOK, // no c-missing entry
		}},
		Logger: silentLogger(),
	})

	p.RefreshArtistOnPlatforms(context.Background(), &artist.Artist{ID: "a1", Name: "X"})

	waitForRefreshCount(t, rec, 1)
	rec.assertCalled(t, "c-ok", "ok-pid")
	time.Sleep(100 * time.Millisecond)
	rec.assertNoCall(t, "c-missing")
	if got := rec.count(); got != 1 {
		t.Errorf("total refresh calls = %d, want exactly 1 (the unresolvable connection is skipped)", got)
	}
}
