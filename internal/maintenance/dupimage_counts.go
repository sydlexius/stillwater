// Package maintenance -- dupimage_counts.go
//
// Periodic refresh of the cached duplicate-image offender counts that back the
// sidebar's "Duplicate Images" section (#2608).
//
// This is a scheduler ONLY. It owns cadence, not computation: the scans live
// behind dupimages.Cache's source functions, installed by the API router
// (which holds the rule.Pipeline and publish.Publisher handles). Following
// StartExistsFlagScanner / StartFanartHashBackfill, the loop fires once after
// a startup delay and then on a fixed interval until ctx is canceled.
//
// The cadence is deliberately modest. Both underlying scans are minutes-long
// (a from-disk re-hash of every artist's fanart; a query against every
// connected platform for every artist), and the value is inherently low-churn:
// the steady state is that an operator cleans the duplicates once and the
// section disappears for good. Freshness needs here are hours, not seconds.
package maintenance

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/sydlexius/stillwater/internal/dupimages"
)

// Default cadence for the duplicate-image count refresh. Applied when the
// caller passes a non-positive value.
const (
	defaultDupImageCountInterval     = 12 * time.Hour
	defaultDupImageCountStartupDelay = 2 * time.Minute
)

// StartDuplicateImageCountRefresh refreshes cache after startupDelay and then
// every interval until ctx is canceled. Blocking; run it in a goroutine.
//
// interval defaults to 12h and startupDelay to 2m when non-positive; both are
// parameters so tests can drive the loop in milliseconds.
//
// The startup delay is longer than the other scanners' because this refresh is
// the most expensive periodic task in the process and the least urgent: a
// cold cache renders as "no duplicate rows", which is also the steady-state
// correct answer for a clean library. (The Images section itself stays visible
// regardless -- it also carries the Unmatched item; only the duplicate rows
// hide. See the dupimages package doc.)
func (s *Service) StartDuplicateImageCountRefresh(ctx context.Context, cache *dupimages.Cache, interval, startupDelay time.Duration) {
	if cache == nil {
		// Fail loud: an unwired cache would leave the sidebar section dark
		// forever with no signal (this repo forbids silent-failure guards).
		s.logger.Error("duplicate-image count refresh not started: no cache provided")
		return
	}
	if interval <= 0 {
		interval = defaultDupImageCountInterval
	}
	if startupDelay <= 0 {
		startupDelay = defaultDupImageCountStartupDelay
	}
	s.logger.Info("duplicate-image count refresh started",
		slog.String("interval", interval.String()),
		slog.String("startup_delay", startupDelay.String()))

	select {
	case <-ctx.Done():
		s.logger.Info("duplicate-image count refresh stopped")
		return
	case <-time.After(startupDelay):
	}
	s.refreshDupImageCounts(ctx, cache, "initial")

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			s.logger.Info("duplicate-image count refresh stopped")
			return
		case <-ticker.C:
			s.refreshDupImageCounts(ctx, cache, "periodic")
		}
	}
}

// refreshDupImageCounts runs ONE scheduled refresh under a per-run deadline.
//
// The deadline is the point. This loop's ctx is process-lifetime, so an
// unbounded Refresh that stalls (a hung platform connection, a wedged disk
// read) blocks the loop forever: while it is blocked the ticker's subsequent
// ticks are DROPPED, so every later refresh is lost too, not merely the stalled
// one. Bounding each run means a stall costs one refresh and the loop resumes.
//
// The bound comes from dupimages.RefreshContext -- the same helper the lazy
// TriggerRefresh path uses -- so the two paths' deadlines cannot drift apart.
// The context is derived from ctx, so shutdown still cancels an in-flight run
// immediately, and cancel runs on every return, so nothing leaks per tick.
func (s *Service) refreshDupImageCounts(ctx context.Context, cache *dupimages.Cache, phase string) {
	runCtx, cancel := dupimages.RefreshContext(ctx)
	defer cancel()

	err := cache.Refresh(runCtx)
	switch {
	case err == nil:
		return
	case errors.Is(err, dupimages.ErrRefreshInFlight):
		// Not a failure: a lazy (sidebar-triggered) refresh is already
		// producing the very numbers this tick wanted. Info, not error.
		s.logger.Info("duplicate-image count refresh skipped: another refresh already running",
			slog.String("phase", phase))
	default:
		s.logger.Error("duplicate-image count refresh failed",
			slog.String("phase", phase), slog.Any("error", err))
	}
}
