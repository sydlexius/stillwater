package connection

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sydlexius/stillwater/internal/database"
	"github.com/sydlexius/stillwater/internal/encryption"
)

// stubLocker records Lock/Unlock calls for assertion.
type stubLocker struct {
	mu        sync.Mutex
	locks     []string
	unlocks   []string
	sources   []string
	lockErr   error
	unlockErr error
}

func (s *stubLocker) Lock(_ context.Context, id, source string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.locks = append(s.locks, id)
	s.sources = append(s.sources, source)
	return s.lockErr
}

func (s *stubLocker) Unlock(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unlocks = append(s.unlocks, id)
	return s.unlockErr
}

// stubStateGetter returns a fixed ArtistPlatformState per platform ID.
// errs lets a test route an error to a specific platform-id while letting
// other ids succeed -- needed to prove the sweep continues past an
// individual failure rather than aborting at the first error.
type stubStateGetter struct {
	states map[string]*ArtistPlatformState
	errs   map[string]error
	err    error
}

func (s *stubStateGetter) GetArtistDetail(_ context.Context, platformArtistID string) (*ArtistPlatformState, error) {
	if e, ok := s.errs[platformArtistID]; ok {
		return nil, e
	}
	if s.err != nil {
		return nil, s.err
	}
	return s.states[platformArtistID], nil
}

// setupLockSyncDB returns a fresh in-memory DB with migrations applied,
// the connection service for inserting test connection rows, and a
// helper to insert an artist + platform mapping ready for LockSync to
// consume.
func setupLockSyncDB(t *testing.T) (*LockSync, *Service, *stubLocker, *stubStateGetter, func(artistID, name string, locked bool, platformConnID, platformArtistID string)) {
	t.Helper()
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := database.Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	enc, _, err := encryption.NewEncryptor("")
	if err != nil {
		t.Fatalf("encryptor: %v", err)
	}
	connSvc := NewService(db, enc)
	locker := &stubLocker{}
	getter := &stubStateGetter{states: make(map[string]*ArtistPlatformState)}

	factory := func(_ *Connection, _ *slog.Logger) ArtistStateGetter { return getter }
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	lockSync := NewLockSync(db, connSvc, locker, factory, logger)

	insert := func(artistID, name string, locked bool, connID, platformArtistID string) {
		t.Helper()
		lockedInt := 0
		var lockedAt any
		if locked {
			lockedInt = 1
			lockedAt = "2020-01-01T00:00:00Z"
		}
		// path NOT NULL: supply a placeholder that is unique per artist.
		if _, err := db.Exec(`INSERT INTO artists (id, name, path, locked, locked_at) VALUES (?, ?, ?, ?, ?)`,
			artistID, name, "/music/"+artistID, lockedInt, lockedAt); err != nil {
			t.Fatalf("insert artist: %v", err)
		}
		if _, err := db.Exec(`INSERT INTO artist_platform_ids (artist_id, connection_id, platform_artist_id) VALUES (?, ?, ?)`,
			artistID, connID, platformArtistID); err != nil {
			t.Fatalf("insert platform id: %v", err)
		}
	}
	return lockSync, connSvc, locker, getter, insert
}

// newEnabledEmby creates and persists an enabled emby connection,
// returning its id.
func newEnabledEmby(t *testing.T, svc *Service) string {
	t.Helper()
	c := &Connection{Name: "Emby", Type: TypeEmby, URL: "http://emby", APIKey: "k", Enabled: true}
	if err := svc.Create(context.Background(), c); err != nil {
		t.Fatalf("Create connection: %v", err)
	}
	return c.ID
}

// TestLockSync_PullsLockFromPlatform asserts the canonical case: the
// platform reports IsLocked=true, the DB has locked=0, LockSync flips
// it on with lock_source="platform".
func TestLockSync_PullsLockFromPlatform(t *testing.T) {
	t.Parallel()
	lockSync, connSvc, locker, getter, insert := setupLockSyncDB(t)
	lockSync.SetRecentPushWindow(time.Nanosecond) // disable the grace gate

	connID := newEnabledEmby(t, connSvc)
	insert("a1", "Aretha", false, connID, "emby-a1")
	getter.states["emby-a1"] = &ArtistPlatformState{IsLocked: true}

	changed, err := lockSync.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if changed != 1 {
		t.Errorf("changed = %d, want 1", changed)
	}
	if len(locker.locks) != 1 || locker.locks[0] != "a1" {
		t.Errorf("locks = %v, want [a1]", locker.locks)
	}
	if len(locker.sources) != 1 || locker.sources[0] != LockSyncLockSourcePlatform {
		t.Errorf("sources = %v, want [%q]", locker.sources, LockSyncLockSourcePlatform)
	}
}

// TestLockSync_PullsUnlockFromPlatform mirrors the previous test in the
// reverse direction: DB locked=1, platform IsLocked=false.
func TestLockSync_PullsUnlockFromPlatform(t *testing.T) {
	t.Parallel()
	lockSync, connSvc, locker, getter, insert := setupLockSyncDB(t)
	lockSync.SetRecentPushWindow(time.Nanosecond)

	connID := newEnabledEmby(t, connSvc)
	insert("a2", "Beyonce", true, connID, "emby-a2")
	getter.states["emby-a2"] = &ArtistPlatformState{IsLocked: false}

	changed, err := lockSync.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if changed != 1 {
		t.Errorf("changed = %d, want 1", changed)
	}
	if len(locker.unlocks) != 1 || locker.unlocks[0] != "a2" {
		t.Errorf("unlocks = %v, want [a2]", locker.unlocks)
	}
}

// TestLockSync_NoOpWhenInSync verifies the sweep skips rows whose
// platform state already matches the DB. Most rows on most sweeps
// hit this branch in production.
func TestLockSync_NoOpWhenInSync(t *testing.T) {
	t.Parallel()
	lockSync, connSvc, locker, getter, insert := setupLockSyncDB(t)
	lockSync.SetRecentPushWindow(time.Nanosecond)

	connID := newEnabledEmby(t, connSvc)
	insert("a3", "Carole", false, connID, "emby-a3")
	getter.states["emby-a3"] = &ArtistPlatformState{IsLocked: false}

	changed, err := lockSync.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if changed != 0 {
		t.Errorf("changed = %d, want 0", changed)
	}
	if len(locker.locks)+len(locker.unlocks) != 0 {
		t.Errorf("expected no mutations, got locks=%v unlocks=%v", locker.locks, locker.unlocks)
	}
}

// TestLockSync_SkipsRecentPush is the conflict-policy fixture: when a
// user just toggled the lock via the Stillwater UI, the artist's
// locked_at is recent and LockSync must NOT overwrite it from the
// platform's still-stale snapshot. Without this gate, the next sweep
// would silently undo every user toggle in the same way the original
// re-lock loop did.
func TestLockSync_SkipsRecentPush(t *testing.T) {
	t.Parallel()
	lockSync, connSvc, locker, getter, insert := setupLockSyncDB(t)
	// Leave the default 5-minute window in place. The seeded locked_at
	// is now-ish (insert uses 2020 but we override below).

	connID := newEnabledEmby(t, connSvc)
	insert("a4", "Dolly", true, connID, "emby-a4")
	// Recent-unlock companion: a4b is unlocked but locked_at is fresh
	// (e.g. user just toggled OFF in the UI). Platform still reports
	// IsLocked=true (stale snapshot). Without the gate the sweep would
	// re-lock the artist and silently undo the user's unlock -- exactly
	// the loop this PR is fixing, but via the LockSync surface.
	insert("a4b", "Emmylou", false, connID, "emby-a4b")
	// Bump locked_at into the window so the gate triggers on both rows.
	if _, err := lockSync.db.Exec(`UPDATE artists SET locked_at = ? WHERE id IN ('a4', 'a4b')`,
		time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("update locked_at: %v", err)
	}
	// Platform says unlocked for a4 (would call Unlock without the gate)
	// and locked for a4b (would call Lock without the gate).
	getter.states["emby-a4"] = &ArtistPlatformState{IsLocked: false}
	getter.states["emby-a4b"] = &ArtistPlatformState{IsLocked: true}

	changed, err := lockSync.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if changed != 0 {
		t.Errorf("changed = %d, want 0 (recent push must suppress both directions)", changed)
	}
	if len(locker.unlocks) != 0 {
		t.Errorf("unlocks = %v, want empty (recent push gate must block re-unlock from stale platform)", locker.unlocks)
	}
	if len(locker.locks) != 0 {
		t.Errorf("locks = %v, want empty (recent push gate must block re-lock from stale platform)", locker.locks)
	}
}

// TestLockSync_SkipsDisabledConnection guards against a disabled
// Emby/Jellyfin entry triggering platform requests during a sweep.
func TestLockSync_SkipsDisabledConnection(t *testing.T) {
	t.Parallel()
	lockSync, connSvc, locker, getter, insert := setupLockSyncDB(t)
	lockSync.SetRecentPushWindow(time.Nanosecond)

	// Create disabled connection.
	c := &Connection{Name: "EmbyOff", Type: TypeEmby, URL: "http://emby", APIKey: "k", Enabled: false}
	if err := connSvc.Create(context.Background(), c); err != nil {
		t.Fatalf("Create: %v", err)
	}
	insert("a5", "Etta", false, c.ID, "emby-a5")
	getter.states["emby-a5"] = &ArtistPlatformState{IsLocked: true}

	changed, err := lockSync.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if changed != 0 || len(locker.locks) != 0 {
		t.Errorf("expected no work for disabled connection; changed=%d locks=%v", changed, locker.locks)
	}
}

// TestLockSync_SurvivesPlatformError checks that a single failing
// GetArtistDetail does not abort the sweep for the rest of the queue:
// the failing row is skipped, a healthy row in the same sweep is still
// mutated. Routes the error per platform-id so only the failing row
// returns an error and we can assert the healthy row was processed.
func TestLockSync_SurvivesPlatformError(t *testing.T) {
	t.Parallel()
	lockSync, connSvc, locker, getter, insert := setupLockSyncDB(t)
	lockSync.SetRecentPushWindow(time.Nanosecond)
	getter.errs = map[string]error{"emby-a6": errors.New("HTTP 500")}

	connID := newEnabledEmby(t, connSvc)
	insert("a6", "Florence", false, connID, "emby-a6")
	getter.states["emby-a6"] = &ArtistPlatformState{IsLocked: true}
	// Healthy second row: must still mutate after the first row errors.
	insert("a6b", "Gladys", false, connID, "emby-a6b")
	getter.states["emby-a6b"] = &ArtistPlatformState{IsLocked: true}

	changed, err := lockSync.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if changed != 1 {
		t.Errorf("changed = %d, want 1 (healthy row must process despite failing row)", changed)
	}
	if len(locker.locks) != 1 || locker.locks[0] != "a6b" {
		t.Errorf("locks = %v, want [a6b] (healthy row only; failing row skipped)", locker.locks)
	}
}

// TestLockSync_LockErrorIsLoggedAndSkipped exercises the mutateLock
// error path: the underlying Lock call fails, the row is skipped, and
// the sweep continues. Pairs with the Lock-success path covered above
// so both branches of mutateLock are exercised.
func TestLockSync_LockErrorIsLoggedAndSkipped(t *testing.T) {
	t.Parallel()
	lockSync, connSvc, locker, getter, insert := setupLockSyncDB(t)
	lockSync.SetRecentPushWindow(time.Nanosecond)
	locker.lockErr = errors.New("DB locked")

	connID := newEnabledEmby(t, connSvc)
	insert("a7", "Grace", false, connID, "emby-a7")
	getter.states["emby-a7"] = &ArtistPlatformState{IsLocked: true}

	changed, err := lockSync.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if changed != 0 {
		t.Errorf("changed = %d, want 0 (Lock failed)", changed)
	}
}

// TestLockSync_UnlockErrorIsLoggedAndSkipped mirrors the Lock-error
// test for the Unlock branch of mutateLock: when the underlying Unlock
// fails, the row is skipped, the sweep continues, and the changed
// counter does not advance.
func TestLockSync_UnlockErrorIsLoggedAndSkipped(t *testing.T) {
	t.Parallel()
	lockSync, connSvc, locker, getter, insert := setupLockSyncDB(t)
	lockSync.SetRecentPushWindow(time.Nanosecond)
	locker.unlockErr = errors.New("DB locked")

	connID := newEnabledEmby(t, connSvc)
	insert("a8", "Helena", true, connID, "emby-a8")
	getter.states["emby-a8"] = &ArtistPlatformState{IsLocked: false}

	changed, err := lockSync.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if changed != 0 {
		t.Errorf("changed = %d, want 0 (Unlock failed)", changed)
	}
}

// TestLockSync_StartSchedulerStopsOnCancel verifies the scheduler loop exits
// cleanly when the context is canceled after the main ticker loop has started.
//
// The test runs inside a testing/synctest bubble so the fake clock provides
// deterministic synchronization without time.Sleep or test-support hooks in
// production code. Inside the bubble, time.After and time.NewTicker use the
// fake clock, which only advances when every goroutine in the bubble is
// durably blocked:
//
//  1. time.Sleep(2ms) blocks the test goroutine on a fake 2ms timer.
//  2. The scheduler goroutine is blocked on time.After(1ms) - the startup delay.
//  3. All goroutines are durably blocked, so the fake clock advances to 1ms.
//  4. Scheduler wakes, runs its initial Run() (fast in-memory SQLite), then
//     blocks on time.NewTicker(1h) in the main loop.
//  5. All goroutines are again durably blocked, so the clock advances to 2ms.
//  6. Test goroutine wakes. The scheduler is GUARANTEED to be in its ticker
//     select at this point (the fake clock could not have reached 2ms otherwise).
//  7. cancel() fires ctx.Done(), scheduler logs "stopped" and returns.
func TestLockSync_StartSchedulerStopsOnCancel(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		lockSync, _, _, _, _ := setupLockSyncDB(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		done := make(chan struct{})
		go func() {
			lockSync.StartScheduler(ctx, time.Hour, time.Millisecond)
			close(done)
		}()

		// The return from time.Sleep guarantees the scheduler is in its main
		// ticker loop (see function comment for the full reasoning).
		time.Sleep(2 * time.Millisecond)
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("scheduler did not exit within 5s of cancel")
		}
	})
}

// TestLockSync_StartSchedulerExitsOnPreCanceledCtx covers the early-exit
// branch of StartScheduler's startup-delay select: when the context is already
// canceled before the startup delay fires, the scheduler must return without
// running the initial sweep. This pins the case <-ctx.Done(): return branch
// that the sibling test above never reaches (the sibling always lets the
// startup delay fire first).
func TestLockSync_StartSchedulerExitsOnPreCanceledCtx(t *testing.T) {
	t.Parallel()
	lockSync, _, _, _, _ := setupLockSyncDB(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel: startup-delay select must fire ctx.Done immediately

	done := make(chan struct{})
	go func() {
		// Long startup delay ensures only ctx.Done() can win the select.
		lockSync.StartScheduler(ctx, time.Hour, time.Hour)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler did not exit with a pre-canceled context within 2s")
	}
}

// TestLockSync_LockSourcePlatformConstant is a tiny invariance test:
// the lock_source string is part of the user-facing contract (the UI
// displays it) and the artist service's validation accepts it. Pin
// the constant so a typo in either side is caught.
func TestLockSync_LockSourcePlatformConstant(t *testing.T) {
	t.Parallel()
	if LockSyncLockSourcePlatform != "platform" {
		t.Errorf("LockSyncLockSourcePlatform = %q, want %q", LockSyncLockSourcePlatform, "platform")
	}
}
