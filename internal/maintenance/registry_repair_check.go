// Package maintenance -- registry_repair_check.go
//
// Cached background detector for "does the image registry need repair?"
// (#2678, slice 2a). The only honest detector is the repair's own dry run, but
// a dry run walks the library and fully decodes every candidate image, so it
// can never be polled on a UI cadence. This file runs it on a slow schedule
// and caches the answer; the banner endpoint reads the cache and never scans.
//
// Cadence mirrors StartDuplicateImageCountRefresh: a 2-minute startup delay,
// then every 12 hours.
package maintenance

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"
)

const (
	defaultRegistryRepairCheckInterval     = 12 * time.Hour
	defaultRegistryRepairCheckStartupDelay = 2 * time.Minute
	// registryRepairCheckTimeout bounds one scheduled dry run, matching the
	// 30-minute work limit the repair endpoint applies to a real run.
	registryRepairCheckTimeout = 30 * time.Minute
)

// RegistryRepairCache holds the last known "rows needing repair" count. The
// zero value is usable: never checked (ok=false).
type RegistryRepairCache struct {
	mu        sync.Mutex
	count     int
	checkedAt time.Time
	ok        bool
	running   bool   // single-flight latch: one detector scan at a time
	gen       uint64 // bumped by SetFromRepair so an older in-flight scan cannot overwrite it
}

// Get returns the cached count, when it was recorded, and ok=false when the
// registry was never checked or the last check failed (count is then 0).
func (c *RegistryRepairCache) Get() (count int, checkedAt time.Time, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.count, c.checkedAt, c.ok
}

// SetFromRepair records the outcome of a committed library-wide repair without
// re-scanning: count is 0 for a clean run, else the number of failed writes.
// It also invalidates any detector scan that started before this call, since
// that scan may have read the pre-repair state.
func (c *RegistryRepairCache) SetFromRepair(count int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.count, c.checkedAt, c.ok = count, time.Now().UTC(), true
	c.gen++
}

// begin claims the single-flight latch; false means a scan is already running.
func (c *RegistryRepairCache) begin() (gen uint64, claimed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.running {
		return 0, false
	}
	c.running = true
	return c.gen, true
}

// finish releases the latch and stores the scan result unless a committed
// repair updated the cache since begin (gen moved), in which case the repair's
// fresher answer wins. A failed scan (err != nil) marks the cache not-ok.
func (c *RegistryRepairCache) finish(gen uint64, count int, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.running = false
	if gen != c.gen {
		return
	}
	if err != nil {
		c.count, c.ok = 0, false
		return
	}
	c.count, c.checkedAt, c.ok = count, time.Now().UTC(), true
}

// scanRegistryRepair is the real detector: both repair passes as a dry run.
// It reuses the same service methods the repair endpoint composes, so the
// count is exactly what a preview would report as Rebuilt + Restored (a dry
// run reports Rebuilt as rows planned, not inserted, hence RowsPlanned).
func (s *Service) scanRegistryRepair(ctx context.Context) (int, error) {
	rebuild, err := s.RepairImageRegistry(ctx, ImageRepairOpts{Commit: false})
	if err != nil {
		return 0, err
	}
	restore, err := s.RestoreExistsFlags(ctx, ExistsFlagRestoreOpts{Commit: false})
	if err != nil {
		return 0, err
	}
	return rebuild.RowsPlanned + restore.Restored, nil
}

// RegistryRepairClaim atomically claims the repair/detector exclusion shared
// with the user-started repair: ok=false means a repair is running (the caller
// must not scan); ok=true means the repair endpoint now refuses to start until
// release is called. Check-then-act on a bool snapshot is not enough, since a
// repair could start between the check and the scan.
type RegistryRepairClaim func() (release func(), ok bool)

// checkRegistryRepair runs ONE detector pass: skipped while claim refuses (a
// user-started repair is running), a no-op while another scan holds the latch,
// and bounded by timeout. The claim is held for the whole scan.
func (s *Service) checkRegistryRepair(ctx context.Context, cache *RegistryRepairCache, claim RegistryRepairClaim, timeout time.Duration) {
	release, ok := claim()
	if !ok {
		s.logger.Info("registry repair check skipped: a repair is running")
		return
	}
	defer release()
	gen, claimed := cache.begin()
	if !claimed {
		s.logger.Info("registry repair check skipped: a check is already running")
		return
	}
	// A panic in the scan must not leave the single-flight latch held (every
	// later tick would then skip forever) or take the process down; the repair
	// endpoint wraps the same two passes in recover for the same reason.
	defer func() {
		if rv := recover(); rv != nil {
			s.logger.Error("panic in registry repair check", "recover", rv, "stack", string(debug.Stack()))
			cache.finish(gen, 0, fmt.Errorf("registry repair check panicked: %v", rv))
		}
	}()
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	scan := s.registryScan
	if scan == nil {
		scan = s.scanRegistryRepair
	}
	count, err := scan(runCtx)
	if err != nil {
		s.logger.Error("registry repair check failed", slog.Any("error", err))
	}
	cache.finish(gen, count, err)
}

// StartRegistryRepairCheck refreshes cache after startupDelay and then every
// interval until ctx is canceled. Blocking; run it in a goroutine. interval
// and startupDelay default to 12h and 2m when non-positive.
//
// claim is how the detector excludes a user-started repair: the API router owns
// that singleton (registryRepairRunning under registryRepairMu) and hands out
// an atomic claim that also blocks a repair from starting while a scan is in
// flight. A dry run racing a real run would read half-written state, so a tick
// that finds a repair running is skipped; the next tick (or the repair's own
// SetFromRepair) supplies the answer.
func (s *Service) StartRegistryRepairCheck(ctx context.Context, cache *RegistryRepairCache, claim RegistryRepairClaim, interval, startupDelay time.Duration) {
	if cache == nil || claim == nil {
		s.logger.Error("registry repair check not started: cache or claim not provided")
		return
	}
	if interval <= 0 {
		interval = defaultRegistryRepairCheckInterval
	}
	if startupDelay <= 0 {
		startupDelay = defaultRegistryRepairCheckStartupDelay
	}
	select {
	case <-ctx.Done():
		return
	case <-time.After(startupDelay):
	}
	s.checkRegistryRepair(ctx, cache, claim, registryRepairCheckTimeout)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.checkRegistryRepair(ctx, cache, claim, registryRepairCheckTimeout)
		}
	}
}
