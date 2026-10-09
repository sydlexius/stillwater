// Package api -- extrafanart_preview_cache.go
//
// An in-memory cache for the extrafanart/ migration PREVIEW (#3434). Building a
// preview hashes every file in every extrafanart/ folder, which is slow on a
// network share, so the operator page reuses the last finished preview instead
// of re-reading the disk on every load.
//
// THE SAFETY RULE: a cached preview can never be acted on. Two things make that
// structural rather than a matter of care:
//
//   - the cache holds only the response DTO (extraFanartRunResult: base names and
//     fixed codes). It never holds an img.ExtraFanartPlan, the only thing
//     ApplyExtraFanartMigration accepts, so a cached value cannot be handed to the
//     mover even by mistake;
//   - the live run never reads the cache. It re-plans from disk on its own. The
//     only thing it does with the cache is Invalidate it (see
//     TestExtraFanartPreviewCacheIsOnlyTouchedByThePreviewPath, an AST test that
//     fails if any other function starts reading it).
//
// Files changed on disk behind the server's back (size, mtime, renames) are NOT
// detected: a cached preview can be stale. The operator's remedy is a refreshing
// POST dry run, which always reads the disk (and the page will say "as of" in a
// later change).
package api

import (
	"strings"
	"sync"
	"time"
)

// extraFanartPreviewMaxRows bounds what is cached. Past this the preview is still
// shown, just not kept, so a pathological library cannot pin unbounded memory.
const extraFanartPreviewMaxRows = 20000

// Reasons a finished preview was not stored. Used in logs and tests.
const (
	notCachedAborted       = "aborted"
	notCachedSkippedMissed = "skipped_missing_folders"
	notCachedTooLarge      = "over_row_bound"
	notCachedSuperseded    = "invalidated_during_walk"
)

// extraFanartPreviewSnapshot is the one kept preview.
type extraFanartPreviewSnapshot struct {
	key  string
	asOf time.Time
	res  extraFanartRunResult
}

// extraFanartPreviewCache holds at most ONE snapshot. The zero value is ready.
//
// Why a generation counter and not timestamps: an invalidation (a live run
// starting) and a preview's start can share the same clock reading, and a
// timestamp comparison cannot order two events at the same instant. A counter
// that only goes up always can.
type extraFanartPreviewCache struct {
	mu   sync.Mutex
	gen  uint64
	snap *extraFanartPreviewSnapshot
}

// extraFanartConventionKey is the cache key: the candidate fanart names plus the
// Kodi numbering flag. A different platform profile changes it, so a profile
// change misses the cache instead of showing a plan for the old naming.
func extraFanartConventionKey(names []string, kodi bool) string {
	k := strings.Join(names, "\x00")
	if kodi {
		return k + "\x00kodi"
	}
	return k
}

// begin returns the generation and the clock reading a preview must carry, both
// taken under the lock and BEFORE the preview reads anything from disk. Reading
// the generation first means any invalidation that lands during the walk makes
// store() refuse the result. The time is the "as of" stamp: it is when the data
// BEGAN to be read, never when the walk finished, so it can never claim to be
// fresher than the data it describes.
func (c *extraFanartPreviewCache) begin(now func() time.Time) (gen uint64, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gen, now()
}

// lookup returns a private copy of the snapshot when it matches key. A copy, so
// a reader that edits what it got cannot change what other readers will see.
func (c *extraFanartPreviewCache) lookup(key string) (*extraFanartRunResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.snap == nil || c.snap.key != key {
		return nil, false
	}
	res := copyExtraFanartResult(&c.snap.res)
	res.asOf = c.snap.asOf
	return res, true
}

// store keeps res as the snapshot unless it must not be. It returns "" when the
// result was stored, else the reason it was not. A result is refused when:
//   - the run aborted (it is partial);
//   - an artist's folder was missing (the notice tells the operator to remount and
//     reload, and a cached copy would make that advice a lie);
//   - it is over the row bound;
//   - gen no longer matches (an invalidation, such as a live run, happened while
//     this preview was walking, so its rows may describe files that have moved).
//
// The snapshot is a deep copy, so the caller's result (which it keeps using and
// may modify) shares no slice with what other readers will get.
func (c *extraFanartPreviewCache) store(gen uint64, key string, asOf time.Time, res *extraFanartRunResult) string {
	switch {
	case res.aborted:
		return notCachedAborted
	case res.ArtistsSkippedMissing > 0:
		return notCachedSkippedMissed
	case extraFanartRowCount(res) > extraFanartPreviewMaxRows:
		return notCachedTooLarge
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gen != gen {
		return notCachedSuperseded
	}
	snap := &extraFanartPreviewSnapshot{key: key, asOf: asOf, res: *copyExtraFanartResult(res)}
	c.snap = snap
	return ""
}

// Invalidate drops the snapshot and bumps the generation, so a preview that is
// mid-walk right now cannot store its (possibly pre-change) result afterwards.
func (c *extraFanartPreviewCache) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
	c.snap = nil
}

// extraFanartRowCount is the number of rows the page would build from res.
func extraFanartRowCount(res *extraFanartRunResult) int {
	n := len(res.Artists)
	for i := range res.Artists {
		n += len(res.Artists[i].Files)
	}
	return n
}

// copyExtraFanartResult deep-copies the slices of res (the struct's other fields
// are plain values), so two copies never share backing arrays.
func copyExtraFanartResult(res *extraFanartRunResult) *extraFanartRunResult {
	out := *res
	out.Artists = make([]extraFanartArtistResult, len(res.Artists))
	for i, a := range res.Artists {
		a.Files = append([]extraFanartFileResult{}, a.Files...)
		out.Artists[i] = a
	}
	return &out
}

// extraFanartClock is the router's time source; tests inject extraFanartNow.
func (r *Router) extraFanartClock() time.Time {
	if r.extraFanartNow != nil {
		return r.extraFanartNow()
	}
	return time.Now()
}
