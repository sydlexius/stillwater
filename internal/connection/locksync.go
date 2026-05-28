// Package connection provides the LockSync service in this file. LockSync
// pulls per-artist <lockdata> state from Emby/Jellyfin back into the
// Stillwater database so a user toggling the pin in the platform UI is
// reflected in artists.locked on the next scheduled sweep (issue #1726
// Part C).
//
// The Emby/Jellyfin clients already expose ArtistPlatformState.IsLocked --
// nothing else in the codebase reads it, so the wire-up here is the only
// consumer.
package connection

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"
)

// LockSyncLockSourcePlatform is the lock_source attribution written when
// LockSync flips artists.locked based on a platform pull. Defined as a
// constant so the value is grep-able and the tests assert against the
// same string.
const LockSyncLockSourcePlatform = "platform"

// lockSyncRecentPushWindow is the grace period during which a recent
// Stillwater-initiated lock change (artists.locked_at within the window)
// suppresses platform-overrides-DB updates. Without it, a user toggle
// in the Stillwater UI could race against a scheduled pull: the pull's
// snapshot was taken before PushLocks reached the platform, so it would
// see the pre-toggle value and immediately undo the user's change. Five
// minutes is well above any realistic platform push round-trip.
const lockSyncRecentPushWindow = 5 * time.Minute

// LockSyncArtistLocker is the slice of artist.Service LockSync needs to
// mutate the lock state. Declared as an interface so the connection
// package does not import internal/artist (which would create a cycle:
// artist already imports connection in some test paths).
type LockSyncArtistLocker interface {
	Lock(ctx context.Context, id, source string) error
	Unlock(ctx context.Context, id string) error
}

// LockSyncConnectionLister is the slice of connection.Service LockSync
// needs. Real callers pass *Service directly; the interface keeps the
// scheduler testable with a stub.
type LockSyncConnectionLister interface {
	GetByID(ctx context.Context, id string) (*Connection, error)
}

// LockSyncClientFactory builds an ArtistStateGetter for the given
// connection. Injected (never optional) so the connection package does
// not need to import the emby/jellyfin sub-packages -- that would form
// an import cycle since both clients import connection for shared
// types. The production factory lives in cmd/stillwater/main.go where
// the sub-packages can be referenced freely.
type LockSyncClientFactory func(conn *Connection, logger *slog.Logger) ArtistStateGetter

// LockSync is the pull-side counterpart to publish.Publisher.PushLocks:
// it walks every artist_platform_ids row, asks the owning platform for
// its current IsLocked value, and updates artists.locked when the
// platform value differs from the DB.
type LockSync struct {
	db      *sql.DB
	conns   LockSyncConnectionLister
	artists LockSyncArtistLocker
	factory LockSyncClientFactory
	logger  *slog.Logger
	syncWin time.Duration
}

// NewLockSync constructs a LockSync. factory must be non-nil; the
// production wiring in cmd/stillwater/main.go passes a closure that
// branches on conn.Type to emby/jellyfin clients.
func NewLockSync(db *sql.DB, conns LockSyncConnectionLister, artists LockSyncArtistLocker, factory LockSyncClientFactory, logger *slog.Logger) *LockSync {
	return &LockSync{
		db:      db,
		conns:   conns,
		artists: artists,
		factory: factory,
		logger:  logger.With(slog.String("component", "lock-sync")),
		syncWin: lockSyncRecentPushWindow,
	}
}

// SetRecentPushWindow overrides the grace period; intended for tests
// that need to drive the sync with sub-second windows.
func (s *LockSync) SetRecentPushWindow(d time.Duration) { s.syncWin = d }

// pullRow is one (artist, platform-mapping) tuple drained from the
// LockSync sweep query before any platform-side calls run.
type pullRow struct {
	artistID         string
	connectionID     string
	platformArtistID string
	dbLocked         bool
	lockedAt         sql.NullString
}

// Run performs one full LockSync sweep and returns the number of
// artists whose lock state was changed. Errors on individual rows are
// logged and skipped; the sweep continues so a single broken platform
// connection does not block sync for everyone else.
func (s *LockSync) Run(ctx context.Context) (int, error) {
	// Stream the (artist_id, connection_id, platform_artist_id,
	// locked, locked_at) tuples in one pass; LockSync ignores rows
	// where the connection is disabled (resolved later) and rows
	// whose locked_at falls inside the recent-push window.
	rows, err := s.db.QueryContext(ctx, `
		SELECT api.artist_id, api.connection_id, api.platform_artist_id,
		       a.locked, a.locked_at
		FROM artist_platform_ids api
		JOIN artists a ON a.id = api.artist_id`)
	if err != nil {
		return 0, fmt.Errorf("query lock-sync rows: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor

	var queue []pullRow
	for rows.Next() {
		var r pullRow
		var lockedInt int
		if err := rows.Scan(&r.artistID, &r.connectionID, &r.platformArtistID, &lockedInt, &r.lockedAt); err != nil {
			return 0, fmt.Errorf("scan lock-sync row: %w", err)
		}
		r.dbLocked = lockedInt != 0
		queue = append(queue, r)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate lock-sync rows: %w", err)
	}

	// Drain the cursor before issuing any writes -- modernc.org/sqlite
	// is single-writer and holding the SELECT cursor open during the
	// follow-up Lock/Unlock would serialize badly.
	changed := 0
	for _, r := range queue {
		if ctx.Err() != nil {
			return changed, ctx.Err()
		}
		if s.applyRow(ctx, r) {
			changed++
		}
	}
	return changed, nil
}

// applyRow handles one (artist, platform-mapping) tuple: skip-or-pull the
// platform state, decide whether the DB needs to flip, and apply the
// change. Returns true iff a real mutation landed. Pulled out of Run so
// the cognitive complexity stays under the lint threshold; the per-row
// logic is otherwise pure conditionals.
func (s *LockSync) applyRow(ctx context.Context, r pullRow) bool {
	if s.skipRecentPush(r.lockedAt) {
		return false
	}
	conn, err := s.conns.GetByID(ctx, r.connectionID)
	if err != nil {
		s.logger.Warn("lock-sync: connection lookup failed",
			slog.String("artist_id", r.artistID),
			slog.String("connection_id", r.connectionID),
			slog.String("error", err.Error()))
		return false
	}
	if conn == nil || !conn.Enabled {
		return false
	}
	client := s.factory(conn, s.logger)
	if client == nil {
		return false
	}
	state, err := client.GetArtistDetail(ctx, r.platformArtistID)
	if err != nil {
		s.logger.Warn("lock-sync: platform detail fetch failed",
			slog.String("artist_id", r.artistID),
			slog.String("connection", conn.Name),
			slog.String("error", err.Error()))
		return false
	}
	if state == nil || state.IsLocked == r.dbLocked {
		return false
	}
	if !s.mutateLock(ctx, r.artistID, state.IsLocked) {
		return false
	}
	s.logger.Info("lock-sync: applied platform lock change",
		slog.String("artist_id", r.artistID),
		slog.String("connection", conn.Name),
		slog.Bool("locked", state.IsLocked))
	return true
}

// mutateLock dispatches Lock or Unlock based on the desired final state,
// logging and swallowing any error. Returns true iff the mutation
// succeeded.
func (s *LockSync) mutateLock(ctx context.Context, artistID string, lock bool) bool {
	if lock {
		if err := s.artists.Lock(ctx, artistID, LockSyncLockSourcePlatform); err != nil {
			s.logger.Warn("lock-sync: Lock failed",
				slog.String("artist_id", artistID),
				slog.String("error", err.Error()))
			return false
		}
		return true
	}
	if err := s.artists.Unlock(ctx, artistID); err != nil {
		s.logger.Warn("lock-sync: Unlock failed",
			slog.String("artist_id", artistID),
			slog.String("error", err.Error()))
		return false
	}
	return true
}

// skipRecentPush reports whether the artist's locked_at falls within
// the recent-push grace window. A NULL locked_at (unlocked artist that
// has never been locked) cannot be a recent push, so it does not skip.
func (s *LockSync) skipRecentPush(lockedAt sql.NullString) bool {
	if !lockedAt.Valid || lockedAt.String == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, lockedAt.String)
	if err != nil {
		return false
	}
	return time.Since(t) < s.syncWin
}

// StartScheduler runs Run on a fixed interval until ctx is canceled.
// Mirrors the cadence-only loops used elsewhere in the codebase.
func (s *LockSync) StartScheduler(ctx context.Context, interval, startupDelay time.Duration) {
	s.logger.Info("lock-sync scheduler started",
		slog.String("interval", interval.String()),
		slog.String("startup_delay", startupDelay.String()))
	select {
	case <-ctx.Done():
		return
	case <-time.After(startupDelay):
	}
	if _, err := s.Run(ctx); err != nil {
		s.logger.Error("initial lock-sync failed", slog.String("error", err.Error()))
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.logger.Info("lock-sync scheduler stopped")
			return
		case <-ticker.C:
			if _, err := s.Run(ctx); err != nil {
				s.logger.Error("lock-sync failed", slog.String("error", err.Error()))
			}
		}
	}
}
