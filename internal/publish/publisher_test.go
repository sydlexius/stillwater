package publish

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/library"
)

// --- test doubles ---

type fakePlatformLister struct {
	ids        []artist.PlatformID
	members    []artist.BandMember
	membersErr error
}

func (f *fakePlatformLister) GetPlatformIDs(_ context.Context, _ string) ([]artist.PlatformID, error) {
	return f.ids, nil
}

func (f *fakePlatformLister) ListMembersByArtistID(_ context.Context, _ string) ([]artist.BandMember, error) {
	if f.membersErr != nil {
		return nil, f.membersErr
	}
	return f.members, nil
}

type fakeConnectionGetter struct {
	conns map[string]*connection.Connection
	mu    sync.Mutex
	calls int
}

func (f *fakeConnectionGetter) GetByID(_ context.Context, id string) (*connection.Connection, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	c, ok := f.conns[id]
	if !ok {
		return nil, fmt.Errorf("no connection %s", id)
	}
	return c, nil
}

// waitForPosts spins up to 2s for the given number of POSTs to arrive at the
// test server. Lock-push dispatches goroutines, so we cannot assert
// synchronously after PushLocks returns.
func waitForPosts(t *testing.T, got *atomic.Int32, want int32) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got.Load() >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected %d POSTs, got %d", want, got.Load())
}

func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// --- tests ---

// TestPushLocks_RoutesToEachConnection verifies that a multi-platform artist
// results in one UpdateArtistLocks POST per enabled connection carrying the
// artist's current lock state.
func TestPushLocks_RoutesToEachConnection(t *testing.T) {
	var posts atomic.Int32
	type received struct {
		LockData     bool     `json:"LockData"`
		LockedFields []string `json:"LockedFields"`
	}
	bodies := make(chan received, 4)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			// Any GET (fetch current item) returns a minimal body.
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path == "/Items" {
				// Jellyfin fetch shape
				_, _ = w.Write([]byte(`{"Items":[{"Id":"p1","LockData":false,"LockedFields":[]}]}`))
			} else {
				// Emby fetch shape
				_, _ = w.Write([]byte(`{"Id":"p1","LockData":false,"LockedFields":[]}`))
			}
			return
		}
		var body received
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies <- body
		posts.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	conns := &fakeConnectionGetter{conns: map[string]*connection.Connection{
		"c-emby": {ID: "c-emby", Name: "emby", Type: connection.TypeEmby, URL: srv.URL, Enabled: true, PlatformUserID: "u1"},
		"c-jf":   {ID: "c-jf", Name: "jf", Type: connection.TypeJellyfin, URL: srv.URL, Enabled: true, PlatformUserID: "u1"},
	}}
	p := New(Deps{
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c-emby", PlatformArtistID: "p1"},
			{ArtistID: "a1", ConnectionID: "c-jf", PlatformArtistID: "p1"},
		}},
		ConnectionService: conns,
		Logger:            silentLogger(),
	})

	a := &artist.Artist{ID: "a1", Locked: true, LockedFields: []string{"biography", "genres"}}
	p.PushLocks(context.Background(), a)

	waitForPosts(t, &posts, 2)
	close(bodies)

	// Emby body should carry LockData=true and canonicalized LockedFields
	// (Overview+Genres). Jellyfin body carries LockData=true only; its
	// LockedFields come from the fetched server value (empty).
	seenEmbyLocked := false
	seenJellyfinLocked := false
	for body := range bodies {
		if !body.LockData {
			t.Errorf("LockData = false on a POST body, want true")
		}
		if len(body.LockedFields) == 2 {
			seenEmbyLocked = true
		}
		if len(body.LockedFields) == 0 {
			seenJellyfinLocked = true
		}
	}
	if !seenEmbyLocked {
		t.Error("did not observe Emby POST with 2 canonicalized LockedFields (Overview, Genres)")
	}
	if !seenJellyfinLocked {
		t.Error("did not observe Jellyfin POST preserving server-side empty LockedFields")
	}
}

// TestPushLocks_DisabledConnectionSkipped verifies that a connection with
// Enabled=false is not contacted.
func TestPushLocks_DisabledConnectionSkipped(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := New(Deps{
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c-off", PlatformArtistID: "p1"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c-off": {ID: "c-off", Name: "off", Type: connection.TypeEmby, URL: srv.URL, Enabled: false, PlatformUserID: "u1"},
		}},
		Logger: silentLogger(),
	})
	p.PushLocks(context.Background(), &artist.Artist{ID: "a1"})

	// Give goroutine a chance to hit the server if the guard is broken.
	time.Sleep(200 * time.Millisecond)
	if got := hits.Load(); got != 0 {
		t.Errorf("disabled connection was contacted %d times, want 0", got)
	}
}

// TestPushLocks_UnsupportedConnectionTypeSkipped verifies that connection
// types without a LockSyncer (e.g. Lidarr) are skipped silently.
func TestPushLocks_UnsupportedConnectionTypeSkipped(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := New(Deps{
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c-lid", PlatformArtistID: "p1"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c-lid": {ID: "c-lid", Name: "lid", Type: connection.TypeLidarr, URL: srv.URL, Enabled: true},
		}},
		Logger: silentLogger(),
	})
	p.PushLocks(context.Background(), &artist.Artist{ID: "a1"})

	time.Sleep(200 * time.Millisecond)
	if got := hits.Load(); got != 0 {
		t.Errorf("Lidarr connection was contacted %d times, want 0 (no LockSyncer)", got)
	}
}

// TestPushLocks_NoPlatformIDs verifies the early-return when the artist has
// no connection mappings (no goroutines spawned, no panic).
func TestPushLocks_NoPlatformIDs(t *testing.T) {
	p := New(Deps{
		ArtistService:     &fakePlatformLister{ids: nil},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{}},
		Logger:            silentLogger(),
	})
	p.PushLocks(context.Background(), &artist.Artist{ID: "a1"})
}

// alwaysOnResolver / alwaysOffResolver / nilResolver / errorResolver
// satisfy the publisher's libraryResolver interface for resolveLockNFO
// branch coverage. Returning *library.Library directly keeps the test
// surface narrow without instantiating a real Service.

type alwaysOnResolver struct{}

func (alwaysOnResolver) FindForArtistPath(_ context.Context, _ string) (*library.Library, error) {
	return &library.Library{ID: "lib-on", NFOLockData: true}, nil
}

type alwaysOffResolver struct{}

func (alwaysOffResolver) FindForArtistPath(_ context.Context, _ string) (*library.Library, error) {
	return &library.Library{ID: "lib-off", NFOLockData: false}, nil
}

type nilResolver struct{}

func (nilResolver) FindForArtistPath(_ context.Context, _ string) (*library.Library, error) {
	return nil, nil
}

type errorResolver struct{}

func (errorResolver) FindForArtistPath(_ context.Context, _ string) (*library.Library, error) {
	return nil, fmt.Errorf("resolver boom")
}

// TestLockSyncClientFactory verifies the factory wired into
// cmd/stillwater/main.go: Emby -> emby client, Jellyfin -> jellyfin
// client, anything else (including nil) -> nil. The branches matter
// because LockSync uses the nil return as the "this connection has no
// lock concept" signal.
func TestLockSyncClientFactory(t *testing.T) {
	logger := silentLogger()
	factory := LockSyncClientFactory()

	if got := factory(nil, logger); got != nil {
		t.Errorf("nil connection -> %T, want nil", got)
	}
	if got := factory(&connection.Connection{Type: connection.TypeEmby, URL: "http://e", PlatformUserID: "u"}, logger); got == nil {
		t.Error("TypeEmby -> nil; want non-nil emby client")
	}
	if got := factory(&connection.Connection{Type: connection.TypeJellyfin, URL: "http://j", PlatformUserID: "u"}, logger); got == nil {
		t.Error("TypeJellyfin -> nil; want non-nil jellyfin client")
	}
	if got := factory(&connection.Connection{Type: connection.TypeLidarr}, logger); got != nil {
		t.Errorf("TypeLidarr -> %T, want nil (no lock concept)", got)
	}
}

// TestResolveLockNFO covers every branch of Publisher.ResolveLockNFO:
// nil libraryService, nil artist, empty path, lookup error, no match, and
// matched library with NFOLockData on or off. Issue #1264 set the safe
// default to false; issue #1726 OR-ed in artist.Locked, which is covered
// by a separate test below.
func TestResolveLockNFO(t *testing.T) {
	logger := silentLogger()

	a := &artist.Artist{ID: "a1", Path: "/music/jazz/Coltrane"}

	t.Run("nil libraryService -> false", func(t *testing.T) {
		p := New(Deps{Logger: logger})
		if got := p.ResolveLockNFO(context.Background(), a); got {
			t.Error("nil libraryService must default to false")
		}
	})

	t.Run("nil artist -> false", func(t *testing.T) {
		p := New(Deps{Logger: logger, LibraryService: &alwaysOnResolver{}})
		if got := p.ResolveLockNFO(context.Background(), nil); got {
			t.Error("nil artist must default to false")
		}
	})

	t.Run("empty artist path -> false", func(t *testing.T) {
		p := New(Deps{Logger: logger, LibraryService: &alwaysOnResolver{}})
		if got := p.ResolveLockNFO(context.Background(), &artist.Artist{ID: "x"}); got {
			t.Error("empty artist path must default to false")
		}
	})

	t.Run("resolver error -> false (best-effort)", func(t *testing.T) {
		p := New(Deps{Logger: logger, LibraryService: &errorResolver{}})
		if got := p.ResolveLockNFO(context.Background(), a); got {
			t.Error("resolver error must default to false (best-effort)")
		}
	})

	t.Run("no matching library -> false", func(t *testing.T) {
		p := New(Deps{Logger: logger, LibraryService: &nilResolver{}})
		if got := p.ResolveLockNFO(context.Background(), a); got {
			t.Error("no matching library must default to false")
		}
	})

	t.Run("library with NFOLockData=true -> true", func(t *testing.T) {
		p := New(Deps{Logger: logger, LibraryService: &alwaysOnResolver{}})
		if got := p.ResolveLockNFO(context.Background(), a); !got {
			t.Error("matched library with NFOLockData=true must return true")
		}
	})

	t.Run("library with NFOLockData=false -> false", func(t *testing.T) {
		p := New(Deps{Logger: logger, LibraryService: &alwaysOffResolver{}})
		if got := p.ResolveLockNFO(context.Background(), a); got {
			t.Error("matched library with NFOLockData=false must return false")
		}
	})

	// Issue #1726 OR-of-knobs: per-artist Locked alone is sufficient to
	// flip the resolver true, regardless of the library setting (and
	// without even consulting the library lookup).
	t.Run("artist.Locked=true with NFOLockData=false -> true (#1726)", func(t *testing.T) {
		p := New(Deps{Logger: logger, LibraryService: &alwaysOffResolver{}})
		locked := &artist.Artist{ID: "a1", Path: "/music/jazz/Coltrane", Locked: true}
		if got := p.ResolveLockNFO(context.Background(), locked); !got {
			t.Error("artist.Locked=true must return true even when library NFOLockData=false")
		}
	})
}

// recordingNotifier captures every NotifyConnectionPushFailed call so
// PushLocks / PushMetadataAsync tests can assert on the (connection,
// error_class) pair the publisher hands the SSE bridge. Concurrent calls
// from per-connection goroutines are serialized by mu.
//
// done is an optional non-buffered signal: every call sends a struct{} on
// it after appending to calls, so a test can block on receive instead of
// polling for the slice length to grow. Tests that do not care about
// synchronization (most pre-channel-conversion ones) leave done nil and
// the send is skipped.
type recordingNotifier struct {
	mu    sync.Mutex
	calls []notifyCall
	done  chan struct{}
}

type notifyCall struct {
	connection string
	errorClass string
	artistID   string
	artistName string
	operation  string
	err        error
}

func (r *recordingNotifier) NotifyConnectionPushFailed(connectionName, errorClass, artistID, artistName, operation string, err error) {
	r.mu.Lock()
	r.calls = append(r.calls, notifyCall{
		connection: connectionName,
		errorClass: errorClass,
		artistID:   artistID,
		artistName: artistName,
		operation:  operation,
		err:        err,
	})
	ch := r.done
	r.mu.Unlock()
	if ch != nil {
		// Buffered channel so a producer never blocks; tests that wire it
		// up are responsible for draining when they expect more than one
		// notify within a single test.
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (r *recordingNotifier) snapshot() []notifyCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]notifyCall, len(r.calls))
	copy(out, r.calls)
	return out
}

// TestPushLocks_NotifierFiresOnSyncFailure verifies the goroutine path
// invokes the Notifier exactly once with the connection name and a
// non-empty error class when the platform PUT returns a non-2xx
// response. The toast pipeline depends on this for the per-connection
// failure surface (#1088): without this hook the user gets a green
// success path on the originating lock toggle while the platform
// silently rejected the write.
func TestPushLocks_NotifierFiresOnSyncFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			// Emby fetch shape; LockSyncer needs the current item before issuing the PUT.
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Id":"p1","LockData":false,"LockedFields":[]}`))
			return
		}
		// Reject every PUT so UpdateArtistLocks returns an error.
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	notifier := &recordingNotifier{}
	p := New(Deps{
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c-emby", PlatformArtistID: "p1"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c-emby": {ID: "c-emby", Name: "my-emby", Type: connection.TypeEmby, URL: srv.URL, Enabled: true, PlatformUserID: "u1"},
		}},
		Logger:   silentLogger(),
		Notifier: notifier,
	})

	a := &artist.Artist{ID: "a1", Name: "Test Artist", Locked: true, LockedFields: []string{"biography"}}
	p.PushLocks(context.Background(), a)

	// PushLocks dispatches its work in a goroutine; spin briefly until
	// the notifier observes the failure. The 401 path is synchronous
	// inside UpdateArtistLocks so a short deadline is sufficient.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(notifier.snapshot()) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	got := notifier.snapshot()
	if len(got) != 1 {
		t.Fatalf("notifier calls = %d, want 1; calls=%+v", len(got), got)
	}
	if got[0].connection != "my-emby" {
		t.Errorf("connection = %q, want %q", got[0].connection, "my-emby")
	}
	// classifyPushErr should map the 401 response to "auth_failed" so
	// the toast tells the operator the specific intervention needed
	// instead of the generic "lock sync failed" pre-fix string.
	if got[0].errorClass != "auth_failed" {
		t.Errorf("errorClass = %q, want %q (401 should classify as auth_failed)", got[0].errorClass, "auth_failed")
	}
	if got[0].artistID != "a1" {
		t.Errorf("artistID = %q, want %q", got[0].artistID, "a1")
	}
	if got[0].artistName != "Test Artist" {
		t.Errorf("artistName = %q, want %q", got[0].artistName, "Test Artist")
	}
	if got[0].operation != "lock_toggle" {
		t.Errorf("operation = %q, want %q", got[0].operation, "lock_toggle")
	}
	if got[0].err == nil {
		t.Error("err = nil, want a non-nil error so logs can correlate")
	}
}

// TestPushLocks_NotifierNotCalledOnSuccess verifies the success path
// does not invoke the notifier; only failed pushes should surface to
// the operator via toast.
func TestPushLocks_NotifierNotCalledOnSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Id":"p1","LockData":false,"LockedFields":[]}`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	notifier := &recordingNotifier{}
	p := New(Deps{
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c-emby", PlatformArtistID: "p1"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c-emby": {ID: "c-emby", Name: "my-emby", Type: connection.TypeEmby, URL: srv.URL, Enabled: true, PlatformUserID: "u1"},
		}},
		Logger:   silentLogger(),
		Notifier: notifier,
	})

	a := &artist.Artist{ID: "a1", Locked: true}
	p.PushLocks(context.Background(), a)

	// Give the goroutine time to complete a successful PUT.
	time.Sleep(200 * time.Millisecond)
	if got := notifier.snapshot(); len(got) != 0 {
		t.Errorf("notifier calls on success = %d, want 0; calls=%+v", len(got), got)
	}
}

// TestPushLocks_NotifierUsesStableClassOnConnectionLookupFailure verifies
// that when GetByID itself fails (connection deleted between platform-ID
// resolution and lock push), the notifier receives a value from the
// classifyPushErr taxonomy rather than a free-form "connection lookup
// failed" string. The toast bridge maps error_class to localized copy, so
// any string outside the enum falls through to a generic "push failed"
// rendering and the operator loses the actionable hint.
func TestPushLocks_NotifierUsesStableClassOnConnectionLookupFailure(t *testing.T) {
	notifier := &recordingNotifier{}
	p := New(Deps{
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c-gone", PlatformArtistID: "p1"},
		}},
		// Empty map -> fakeConnectionGetter.GetByID returns an error;
		// classifyPushErr does not recognize the substring so the fallback
		// "rejected" class is what the notifier should observe.
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{}},
		Logger:            silentLogger(),
		Notifier:          notifier,
	})

	a := &artist.Artist{ID: "a1", Name: "Test Artist", Locked: true}
	p.PushLocks(context.Background(), a)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(notifier.snapshot()) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	got := notifier.snapshot()
	if len(got) != 1 {
		t.Fatalf("notifier calls = %d, want 1; calls=%+v", len(got), got)
	}
	// The fake's "no connection <id>" error has no substring classifyPushErr
	// matches, so the fallback "rejected" applies. The critical contract is
	// that errorClass is one of the taxonomy values, not the pre-fix
	// free-form "connection lookup failed" string.
	if got[0].errorClass != "rejected" {
		t.Errorf("errorClass = %q, want %q (classifyPushErr fallback)", got[0].errorClass, "rejected")
	}
	if got[0].errorClass == "connection lookup failed" {
		t.Error("errorClass leaked the pre-fix free-form string; should use classifyPushErr taxonomy")
	}
	// The connection name should fall back to the short-UUID label since
	// GetByID never returned a usable name.
	if got[0].connection == "" {
		t.Error("connection label is empty; expected shortConnLabel fallback")
	}
	if got[0].err == nil {
		t.Error("err = nil, want the wrapped GetByID error so logs can correlate")
	}
}

// TestClassifyPushErr pins the error-class taxonomy that drives the
// per-connection failure toast. The categories are intentionally small
// (each maps to a distinct operator response); adding a new one is fine
// but renaming an existing one breaks i18n keys in en.json once the
// front-end maps them, so every category gets a representative test.
func TestClassifyPushErr(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"context deadline", context.DeadlineExceeded, "timeout"},
		{"wrapped deadline", fmt.Errorf("pushing locks: %w", context.DeadlineExceeded), "timeout"},
		{"connection refused", errors.New("Post \"http://emby/\": dial tcp 1.2.3.4:8096: connect: connection refused"), "unreachable"},
		{"no such host", errors.New("Post \"http://emby/\": dial tcp: lookup emby.lan: no such host"), "unreachable"},
		{"401 status", errors.New("update locks: status 401"), "auth_failed"},
		{"403 status", errors.New("update locks: status 403"), "auth_failed"},
		{"HTTP 401", errors.New("authentication failed: HTTP 401"), "auth_failed"},
		{"404 status", errors.New("update locks: status 404"), "not_found"},
		{"HTTP 404", errors.New("item missing: HTTP 404"), "not_found"},
		{"503 status", errors.New("update locks: status 503"), "server_error"},
		{"HTTP 500", errors.New("plain: HTTP 500"), "server_error"},
		{"unknown", errors.New("something weird happened"), "rejected"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyPushErr(tc.err)
			if got != tc.want {
				t.Errorf("classifyPushErr(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// TestShortConnLabel covers the GetByID-error fallback path: when the
// publisher cannot resolve a connection name, the operator still needs
// something correlatable. Eight hex chars is the minimum that
// disambiguates the typical 2-4 connections an install has.
func TestShortConnLabel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", "unknown connection"},
		{"short", "abc", "unknown connection (id=abc)"},
		{"uuid", "01234567-89ab-cdef-0123-456789abcdef", "unknown connection (id=01234567)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := shortConnLabel(tc.in)
			if got != tc.want {
				t.Errorf("shortConnLabel(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestPushMetadataAsync_NotifierFiresOnPushFailure mirrors
// TestPushLocks_NotifierFiresOnSyncFailure for the PushMetadataAsync
// surface (#1642). When the platform write returns 401, the per-connection
// goroutine must invoke the Notifier exactly once with the operation slug
// "metadata_push" and a non-empty error class from the classifyPushErr
// taxonomy (auth_failed for 401). Without this hook the operator gets
// nothing in the UI while the platform silently rejected the write.
func TestPushMetadataAsync_NotifierFiresOnPushFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Emby PushMetadata posts directly to /Items/{id}; reject every
		// write with 401 so the push goroutine surfaces an auth_failed
		// classification through classifyPushErr.
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	notifier := &recordingNotifier{done: make(chan struct{}, 1)}
	p := New(Deps{
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c-emby", PlatformArtistID: "p1"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c-emby": {ID: "c-emby", Name: "my-emby", Type: connection.TypeEmby, URL: srv.URL, Enabled: true, PlatformUserID: "u1"},
		}},
		Logger:   silentLogger(),
		Notifier: notifier,
	})

	a := &artist.Artist{ID: "a1", Name: "Test Artist"}
	p.PushMetadataAsync(context.Background(), a)

	// Block on the channel-backed notify signal instead of polling. The
	// 401 path is synchronous inside PushMetadata, so the goroutine
	// completes well within the 2s safety timeout.
	select {
	case <-notifier.done:
	case <-time.After(2 * time.Second):
		t.Fatal("notify did not fire within 2s")
	}

	got := notifier.snapshot()
	if len(got) != 1 {
		t.Fatalf("notifier calls = %d, want 1; calls=%+v", len(got), got)
	}
	if got[0].connection != "my-emby" {
		t.Errorf("connection = %q, want %q", got[0].connection, "my-emby")
	}
	if got[0].errorClass != "auth_failed" {
		t.Errorf("errorClass = %q, want %q (401 should classify as auth_failed)", got[0].errorClass, "auth_failed")
	}
	if got[0].artistID != "a1" {
		t.Errorf("artistID = %q, want %q", got[0].artistID, "a1")
	}
	if got[0].artistName != "Test Artist" {
		t.Errorf("artistName = %q, want %q", got[0].artistName, "Test Artist")
	}
	if got[0].operation != "metadata_push" {
		t.Errorf("operation = %q, want %q (PushMetadataAsync must emit pushOpMetadataPush)", got[0].operation, "metadata_push")
	}
	if got[0].err == nil {
		t.Error("err = nil, want a non-nil error so logs can correlate")
	}
}

// TestPushMetadataAsync_NotifierFiresOnConnectionLookupFailure verifies
// the GetByID lookup-failure branch. When a connection is deleted between
// platform-ID resolution and metadata push, the goroutine must still
// surface a toast (operator otherwise sees a green success path while the
// underlying write was never attempted). Connection name falls back to
// shortConnLabel since GetByID failed before we could read the real name.
func TestPushMetadataAsync_NotifierFiresOnConnectionLookupFailure(t *testing.T) {
	notifier := &recordingNotifier{done: make(chan struct{}, 1)}
	p := New(Deps{
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c-gone", PlatformArtistID: "p1"},
		}},
		// Empty conns map -> fakeConnectionGetter.GetByID returns an error.
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{}},
		Logger:            silentLogger(),
		Notifier:          notifier,
	})

	a := &artist.Artist{ID: "a1", Name: "Test Artist"}
	p.PushMetadataAsync(context.Background(), a)

	select {
	case <-notifier.done:
	case <-time.After(2 * time.Second):
		t.Fatal("notify did not fire within 2s")
	}

	got := notifier.snapshot()
	if len(got) != 1 {
		t.Fatalf("notifier calls = %d, want 1; calls=%+v", len(got), got)
	}
	if got[0].operation != "metadata_push" {
		t.Errorf("operation = %q, want %q", got[0].operation, "metadata_push")
	}
	// Pin the exact shortConnLabel("c-gone") fallback so a future regression
	// that changes the label format or drops the prefix-truncation logic
	// fails here. Asserting non-empty would let an unrelated format change
	// pass silently.
	if want := shortConnLabel("c-gone"); got[0].connection != want {
		t.Errorf("connection = %q, want %q", got[0].connection, want)
	}
	// Pin the exact "rejected" class so the PushMetadataAsync lookup-failure
	// path stays in parity with PushLocks: classifyPushErr's default
	// taxonomy bucket for a non-network/non-status error is "rejected", and
	// this is the contract the surrounding toast surface relies on.
	if got[0].errorClass != "rejected" {
		t.Errorf("errorClass = %q, want %q (lookup failure must classify as rejected)", got[0].errorClass, "rejected")
	}
	if got[0].err == nil {
		t.Error("err = nil, want a non-nil error so logs can correlate")
	}
}

// TestPushMetadataAsync_NotifierNotCalledOnSuccess covers the happy path:
// no notify, no toast. Symmetric with TestPushLocks_NotifierNotCalledOnSuccess
// so a regression on either surface fails its own test, not the other's.
func TestPushMetadataAsync_NotifierNotCalledOnSuccess(t *testing.T) {
	// pushDone fires when the test server has finished handling the
	// metadata POST (the one PushMetadataAsync's goroutine awaits).
	// Refresh requests fire after the POST and are not waited on. The
	// channel-backed barrier replaces a sleep timer so the test does not
	// race on a slow CI box.
	pushDone := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/Refresh") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusOK)
		select {
		case pushDone <- struct{}{}:
		default:
		}
	}))
	defer srv.Close()

	notifier := &recordingNotifier{done: make(chan struct{}, 1)}
	p := New(Deps{
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c-emby", PlatformArtistID: "p1"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c-emby": {ID: "c-emby", Name: "my-emby", Type: connection.TypeEmby, URL: srv.URL, Enabled: true, PlatformUserID: "u1"},
		}},
		Logger:   silentLogger(),
		Notifier: notifier,
	})

	a := &artist.Artist{ID: "a1", Name: "Test Artist"}
	p.PushMetadataAsync(context.Background(), a)

	// Wait for the metadata POST to complete server-side, then assert
	// the notifier never fired. A spurious notify (regression) would
	// already have raced into r.calls by the time the POST returned.
	select {
	case <-pushDone:
	case <-time.After(2 * time.Second):
		t.Fatal("metadata POST did not complete within 2s")
	}
	// Tiny yield so any spurious notify enqueued from inside the publisher
	// goroutine has a chance to land before we read the snapshot. This is
	// not a polling loop; the success path is synchronous post-POST.
	select {
	case <-notifier.done:
		t.Errorf("notifier fired on success path")
	case <-time.After(50 * time.Millisecond):
	}
	if got := notifier.snapshot(); len(got) != 0 {
		t.Errorf("notifier calls on success = %d, want 0; calls=%+v", len(got), got)
	}
}
