package publish

import (
	"sync"
	"time"
)

// Per-artist cache of platform near-duplicate backdrops (#3138 S3a). A
// background sweep (a later slice) writes it; the "No duplicate images" rule's
// checker reads it, so the checker does no platform I/O.
//
// In memory only, like the report snapshot (#3092): a restart costs one sweep.

// PlatformDupState is what the cache knows about one artist.
type PlatformDupState int

const (
	// PlatformDupUnknown is deliberately the ZERO value: an artist that was
	// never swept, whose entry was invalidated, or whose entry was computed at
	// another tolerance must never read as clean, and a missing map entry or a
	// forgotten field yields exactly this.
	PlatformDupUnknown PlatformDupState = iota
	// PlatformDupClean means every prune target was read and nothing is redundant.
	PlatformDupClean
	// PlatformDupFound means redundant backdrops exist; see Findings.
	PlatformDupFound
	// PlatformDupUndetermined means the sweep ran but could not decide; see Reasons.
	// Neither clean nor a finding.
	PlatformDupUndetermined
)

// Reasons the sweep adds to the publisher's PruneSkip* constants.
const (
	PlatformDupReasonError          = "sweep_error"
	PlatformDupReasonReadFailed     = "platform_read_failed"
	PlatformDupReasonUnhealthy      = "connection_unhealthy"
	PlatformDupReasonNoTarget       = "no_platform_target"
	PlatformDupReasonStaleTolerance = "stale_tolerance"
)

// PlatformDupFinding is one connection's redundant backdrops for an artist.
type PlatformDupFinding struct {
	ConnectionID string
	Connection   string // display name
	Backdrops    int    // total backdrops read from the platform
	Redundant    int    // copies the fixer's platform phase would delete
}

// PlatformDupEntry is one artist's cached result. Treat it as read-only: the
// slices are shared with the cache.
type PlatformDupEntry struct {
	State      PlatformDupState
	Findings   []PlatformDupFinding // PlatformDupFound only
	Reasons    []string             // why the state is Unknown or Undetermined
	ComputedAt time.Time            // zero when the artist was never swept
	Tolerance  float64              // similarity tolerance the entry was computed at
}

// Redundant is the total across connections.
func (e PlatformDupEntry) Redundant() int {
	n := 0
	for _, f := range e.Findings {
		n += f.Redundant
	}
	return n
}

type platformDupCached struct {
	entry   PlatformDupEntry
	targets []string // backdropTargetKey of every target the entry rests on
}

// PlatformDupCache is safe for concurrent use. Its mutex guards map access
// only and is never held across platform or database I/O.
//
// ONE SWEEPER. begin/store share a single cache-wide window (written), so the
// cache supports one serial writer: begin, read the platform, store, repeat.
// A store with no window open is refused.
//
// WHAT A READER MUST NOT ASSUME. Lookup enforces no age bound; ComputedAt is
// there for the reader to judge. Invalidation covers platform backdrop WRITES
// that take the per-target lock, and nothing else: not a platform mapping
// added or removed, an artist lock, protected fanart, a connection toggled, or
// an artist deleted. The checker that reads this cache must handle those.
type PlatformDupCache struct {
	mu       sync.RWMutex
	entries  map[string]platformDupCached // by artist ID
	byTarget map[string]string            // target key -> artist ID
	// written collects the targets a writer locked while a sweep read is in
	// flight (nil when none is), so a result that raced a write is discarded.
	written map[string]struct{}
}

func newPlatformDupCache() *PlatformDupCache {
	return &PlatformDupCache{entries: map[string]platformDupCached{}, byTarget: map[string]string{}}
}

// Lookup returns the artist's entry for a reader working at tolerance (the
// rule's normalized tolerance). Never swept reads as the zero entry. An entry
// computed at a different tolerance also reads as Unknown, with
// PlatformDupReasonStaleTolerance and its original ComputedAt and Tolerance,
// because "same picture" meant something else when it was computed.
func (c *PlatformDupCache) Lookup(artistID string, tolerance float64) PlatformDupEntry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.entries[artistID]
	if !ok {
		return PlatformDupEntry{}
	}
	if e.entry.Tolerance != tolerance {
		return PlatformDupEntry{
			Reasons:    []string{PlatformDupReasonStaleTolerance},
			ComputedAt: e.entry.ComputedAt, Tolerance: e.entry.Tolerance,
		}
	}
	return e.entry
}

// Invalidate drops one artist's entry.
func (c *PlatformDupCache) Invalidate(artistID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.drop(artistID)
}

// Clear drops every entry (the rule option was turned off).
func (c *PlatformDupCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries, c.byTarget = map[string]platformDupCached{}, map[string]string{}
	c.written = nil // a read still in flight predates the clear: refuse its store
}

// Len is the number of artists with an entry.
func (c *PlatformDupCache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}

func (c *PlatformDupCache) drop(artistID string) {
	for _, k := range c.entries[artistID].targets {
		// Only this artist's own index keys: a target that has since been
		// stored under another artist must keep pointing at that artist.
		if c.byTarget[k] == artistID {
			delete(c.byTarget, k)
		}
	}
	delete(c.entries, artistID)
}

// invalidateTarget is the Publisher's backdrop-write observer: a Stillwater
// writer has locked this target, so whatever was cached about it is stale.
func (c *PlatformDupCache) invalidateTarget(connectionID, platformArtistID string) {
	key := backdropTargetKey(connectionID, platformArtistID)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.written != nil {
		c.written[key] = struct{}{}
	}
	if id, ok := c.byTarget[key]; ok {
		c.drop(id)
	}
}

// begin starts tracking writes for the sweep's next platform read.
func (c *PlatformDupCache) begin() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.written = map[string]struct{}{}
}

// store records the entry unless a writer locked one of its targets since
// begin: that result may describe the platform as it was BEFORE the write, and
// storing it would resurrect what the write just invalidated. It closes the
// window, and refuses when none is open (no begin, or a Clear since).
func (c *PlatformDupCache) store(artistID string, e PlatformDupEntry, targets []string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	written := c.written
	c.written = nil
	if written == nil {
		return false
	}
	for _, k := range targets {
		if _, raced := written[k]; raced {
			return false
		}
	}
	c.drop(artistID)
	for _, k := range targets {
		// A target has one owner. If the mapping moved here from another
		// artist, that artist's entry describes a target it no longer has.
		if owner, ok := c.byTarget[k]; ok && owner != artistID {
			c.drop(owner)
		}
		c.byTarget[k] = artistID
	}
	c.entries[artistID] = platformDupCached{entry: e, targets: targets}
	return true
}

// fresh reports whether the sweep can skip the artist: a decided entry is good
// for ttl, an undecided one is retried after retry.
func (c *PlatformDupCache) fresh(artistID string, tolerance float64, now time.Time, ttl, retry time.Duration) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.entries[artistID]
	if !ok || e.entry.Tolerance != tolerance {
		return false
	}
	if e.entry.State == PlatformDupClean || e.entry.State == PlatformDupFound {
		return now.Sub(e.entry.ComputedAt) < ttl
	}
	return now.Sub(e.entry.ComputedAt) < retry
}
