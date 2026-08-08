// Package image -- readio.go
//
// Cancellable filesystem-read primitives (#2689).
//
// THE PROBLEM. Go cannot interrupt an in-flight read(2) on a regular file.
// A network-mounted library that stops responding (#2680) leaves the read
// blocked in the kernel with no timeout and no signal that reaches it, so a
// `context.WithTimeout` wrapped around the CALL is decoration: nothing ever
// consults the context, because control never returns to Go code that could.
// That is why three successive attempts to bound this from the outside were
// rejected in review (#2684) -- each bounded something ADJACENT to the
// blocking call rather than the call itself -- and why a write deadline
// (#2685) cannot help: the hang happens before any write is attempted.
//
// The consequence is not merely a slow request. Four remediation endpoints
// claim a singleton flag released by a deferred unlock, so a handler that
// never returns never releases its slot, and every later invocation gets HTTP
// 409 for the life of the process.
//
// THE MECHANISM. The only construct that returns control to the caller while
// a regular-file read is wedged is a goroutine race: run the blocking call in
// its own goroutine, report its result on a BUFFERED channel, and select that
// against ctx.Done(). On cancellation the caller returns immediately and the
// stuck goroutine is ABANDONED -- it finishes if the mount ever recovers, and
// its buffered send never blocks, so it exits cleanly whenever that happens.
//
// THE TRADEOFF, AND ITS BOUND. An abandoned read holds one open file
// descriptor and whatever io.ReadAll has buffered so far. Within a single
// operation the leak is self-limiting: the enclosing pass checks its context
// and stops issuing new reads once canceled, so one canceled operation
// abandons at most one read.
//
// ACROSS operations it is not self-limiting, and that is the case this file
// actually guards. An operator whose mount is permanently dead retries the
// remediation endpoint; each retry wedges on the first file, times out, and
// abandons one more read. Nothing about "the operation stops issuing new
// reads" bounds a SEQUENCE of operations. So the count of in-flight abandoned
// reads is tracked process-wide and capped: past the cap these helpers refuse
// to start a new read at all and return ErrTooManyStalledReads.
//
// Refusing is the SAFE direction, and that is load-bearing rather than
// convenient. Every consumer of these primitives treats an error as "could
// not look" -- skip the artist, skip the slot, leave the registry row alone --
// and never as "nothing is there". (ResolveFanart's own doc comment states
// that invariant explicitly: an empty result licenses deleting stale rows,
// while an error must leave everything untouched.) So the cap can only ever
// cause work to be SKIPPED and reported, never destructive action taken on a
// wrong premise.
//
// The counter self-heals: a mount that comes back lets every abandoned read
// complete, each decrementing on its way out, and the cap stops applying.
//
// WHAT THIS FILE DOES NOT COVER (#2930). READS are bounded. WRITES, RENAMES
// and STATS are not, and that is deliberate rather than an omission: a mount
// that blocks on rename(2) still wedges a handler exactly as it did before
// #2689. Abandoning a half-committed rename is worse than blocking on one --
// a rollback that can be canceled mid-sequence is a data-integrity bug, not a
// fix -- so the singleton-holding chains remain interruptible on their reads
// only. Do not read "reads are bounded" as "the singleton is safe from a
// stalled mount"; a wedge whose stack sits in a write or a rename is exactly
// the shape this file cannot help with, and is still worth suspecting first.
package image

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
)

// maxStalledReads caps how many abandoned reads may be in flight process-wide
// before these helpers refuse to start another.
//
// Sized against the accumulation shape, not against throughput. The passes
// that reach these primitives read one file at a time, and a canceled pass
// abandons at most one read, so the count grows by roughly ONE PER OPERATOR
// RETRY against a dead mount. Sixteen retries is far more than an operator
// will issue before investigating, while capping the descriptor and buffer
// growth hard enough that a wedged mount cannot exhaust the process.
//
// The realized memory cost is much smaller than the nominal bound suggests:
// io.ReadAll grows its buffer as bytes ARRIVE, so a read stalled at byte zero
// -- the shape a dead mount actually produces -- holds one descriptor and a
// kilobyte-scale initial buffer, not MaxDecodeBytes.
const maxStalledReads = 16

// stalledReads counts reads abandoned by a canceled caller and still running.
// Incremented when a caller walks away from a read, decremented when that
// read finally completes (which it does whenever the mount recovers, or never,
// which is exactly the condition the cap exists to bound).
var stalledReads atomic.Int64

// StalledReadCount reports how many abandoned filesystem reads are currently
// in flight. It is a live gauge, not a cumulative total.
//
// Exported for tests. No operator-facing surface consumes this today -- it is
// currently internal-only, despite the value it reports being exactly what an
// operator debugging a wedged mount would want to see. Wiring it to a real
// diagnostics surface (a metrics endpoint, an admin page) is future work, not
// something this function claims to already provide.
func StalledReadCount() int64 { return stalledReads.Load() }

// ErrTooManyStalledReads reports that too many previously-abandoned reads are
// still in flight for a new one to be started safely. It is a sentinel so
// callers CAN distinguish it from an ordinary I/O fault, but they are not
// required to: it is an error precisely so that every existing "could not
// look -- skip and report" path handles it correctly without changes.
//
// The message names the mount because that is what an operator seeing this
// has to go fix; the condition is never caused by the library's contents.
//
// THE CAP IS PROCESS-GLOBAL, NOT PER-MOUNT. Once 16 abandoned reads are
// outstanding against ONE wedged mount, this error is returned for every
// read anywhere in the process, including a perfectly healthy file on a
// different filesystem. That is a deliberate, safe direction to fail in: no
// caller in this codebase treats this error as a positive claim that a file
// is absent (the non-strict discovery paths degrade to "could not look, skip
// and report" rather than "not found", and the one caller that could
// otherwise overwrite -- BackupSingleSlot -- aborts on the same gauge rather
// than proceeding past it). A destructive path never mistakes a refused read
// for a genuine absence. Narrowing the cap to be per-mount would need a way
// to key an abandoned read by its filesystem, which the read itself has no
// way to determine once the kernel has swallowed it; global-and-safe is the
// tradeoff made here rather than precise-and-unbuildable.
var ErrTooManyStalledReads = errStalledReads{}

type errStalledReads struct{}

func (errStalledReads) Error() string {
	return "too many stalled filesystem reads are still in flight; the library mount appears to be unresponsive"
}

// runCancellable runs fn on its own goroutine and returns either its result or
// the context's error, whichever comes first.
//
// The channel is buffered so an abandoned fn can always complete its send and
// exit rather than blocking forever on a receiver that has gone away -- which
// would convert a temporary leak into a permanent one.
func runCancellable[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	var zero T
	// Cheap short-circuit: an already-canceled caller should not spend a
	// goroutine or touch the filesystem at all.
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	// APPROXIMATE, not atomic: this Load and the Add(1) below (on the
	// ctx.Done() path) are two separate operations, so several callers can
	// each observe a count under the cap and all proceed, briefly pushing the
	// true in-flight count above maxStalledReads. That is DELIBERATE and
	// safe in the only direction that matters: the race can only let MORE
	// reads through than the nominal cap, never fewer, and this cap exists to
	// bound unbounded growth against a wedged mount, not to enforce an exact
	// ceiling. Making it exact would need a lock on every call on the hot
	// path for a race whose worst case is "the cap is soft by a handful of
	// reads for one instant." Not worth it.
	if stalledReads.Load() >= maxStalledReads {
		return zero, ErrTooManyStalledReads
	}

	type result struct {
		val T
		err error
	}
	ch := make(chan result, 1)
	// abandoned carries the accounting for exactly one increment, and BOTH
	// sides give it back with the same compare-and-swap. That symmetry is the
	// point: the parent sets it and then re-checks the channel, while the
	// child finishes and checks it, so without a single-winner primitive an
	// interleaving where the child completes right as the parent gives up
	// would decrement TWICE and drive the gauge negative -- permanently
	// under-counting every later leak and defeating the cap. A CAS true->false
	// can succeed only once, so whichever side observes the completion first
	// pays it back and the other does nothing.
	var abandoned atomic.Bool
	go func() {
		val, err := fn()
		ch <- result{val, err}
		if abandoned.CompareAndSwap(true, false) {
			stalledReads.Add(-1)
		}
	}()

	select {
	case r := <-ch:
		return r.val, r.err
	case <-ctx.Done():
		// The read is still running inside the kernel and cannot be stopped.
		// Count it as abandoned BEFORE returning, so a caller that immediately
		// retries sees the elevated count.
		stalledReads.Add(1)
		abandoned.Store(true)
		// Re-check the channel: fn may have completed between the two select
		// cases being evaluated, in which case nothing actually leaked and the
		// count must be given back. Without this a busy-but-healthy mount
		// would ratchet the gauge up on every near-miss and eventually refuse
		// reads it has no reason to refuse.
		select {
		case r := <-ch:
			if abandoned.CompareAndSwap(true, false) {
				stalledReads.Add(-1)
			}
			return r.val, r.err
		default:
		}
		return zero, ctx.Err()
	}
}

// readFileBounded reads path under context control and returns its bytes,
// bounded at MaxDecodeBytes.
//
// The bound is MaxDecodeBytes rather than a caller-supplied limit ON PURPOSE.
// The helper reads ONE BYTE PAST it and returns ErrImageTooLarge when the file
// is larger, so the "exactly at the limit is fine, one byte over is not"
// boundary lives in exactly one place and cannot be restated wrongly. A
// parameterised limit would invite a caller to pass the read length directly
// and silently move that boundary by a byte the first time someone forgot the
// +1 -- and nothing else in the suite would notice. Reusing the same constant
// decodeWithLimit enforces also keeps the read bound and the decode bound from
// drifting apart.
//
// io.LimitReader, not os.Stat: a Stat-then-read has a TOCTOU window in which
// the file can grow between the size check and the read, so the check bounds a
// NUMBER while the read stays unbounded. The LimitReader bounds the
// ALLOCATION itself, which is the thing that has to be bounded. Go has no
// allocation-failure path, so an over-budget allocation is a fatal runtime
// error rather than an error value -- under a container memory limit that is a
// SIGKILL and a restart loop, not something a caller can recover from.
//
// The security boundary on path is enforced at the CALL SITES, exactly as it
// was for the os.Open this replaces: callers construct paths from trusted
// sources (database rows, filesystem discovery, fixed naming patterns), never
// from request input.
func readFileBounded(ctx context.Context, path string) ([]byte, error) {
	data, err := runCancellable(ctx, func() ([]byte, error) {
		f, openErr := os.Open(filepath.Clean(path))
		if openErr != nil {
			return nil, openErr
		}
		defer func() { _ = f.Close() }()
		return io.ReadAll(io.LimitReader(f, MaxDecodeBytes+1))
	})
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > MaxDecodeBytes {
		return nil, ErrImageTooLarge
	}
	return data, nil
}

// ReadImageFileBounded reads an image file under context control, bounded at
// MaxDecodeBytes exactly as HashFile is.
//
// It is the exported entry point for callers OUTSIDE this package that need
// the same read-then-decode-from-memory split HashFile performs internally --
// today the registry-repair verification gate, which decodes a file twice
// (placeholder, then dimensions) and used to do it by rewinding one os.File
// handle held open across both. Handing those decoders a bytes.Reader each
// keeps the only blocking step bounded by ctx while the decodes stay pure CPU
// work that cannot hang on a mount.
//
// Over-size files return ErrImageTooLarge, matching HashFile so the read bound
// and the decode bound cannot drift apart.
func ReadImageFileBounded(ctx context.Context, path string) ([]byte, error) {
	return readFileBounded(ctx, path)
}

// readDirCtx lists dir under context control.
//
// Same shape and same tradeoff as readFileBounded. It returns os.ReadDir's own
// []os.DirEntry so the fanart matchers -- which already take pre-read entries
// and do no I/O of their own -- need no change at all.
func readDirCtx(ctx context.Context, dir string) ([]os.DirEntry, error) {
	return runCancellable(ctx, func() ([]os.DirEntry, error) {
		return os.ReadDir(dir)
	})
}

// statCtx stats path under context control.
//
// A stat is a single metadata round trip rather than a byte stream, so it is
// the least likely of the three to stall for long -- but on a hard-mounted
// export that has stopped answering it hangs exactly as completely as a read,
// and one of these sits on the registry-repair RESTORE path (see
// confirmSlotOnDisk), which is inside a singleton-holding handler. Wrapping it
// is what keeps that handler's deadline honest.
func statCtx(ctx context.Context, path string) (os.FileInfo, error) {
	return runCancellable(ctx, func() (os.FileInfo, error) {
		return os.Stat(path)
	})
}

// LstatBounded stats path under context control WITHOUT following a symlink.
//
// Exported for the phash-quarantine restore's occupancy check (#2930), which
// ran a raw os.Lstat while every read around it was already ctx-bound. That
// check sits inside a singleton-holding handler, so a stalled mount there
// wedges the handler and every later invocation gets HTTP 409 for the life of
// the process -- the exact failure mode this file exists to prevent, reached
// through the one call the original pass did not cover.
//
// Lstat, not Stat, and that distinction is load-bearing: the caller is refusing
// to CLOBBER whatever occupies a computed slot name, so a dangling symlink must
// read as OCCUPIED. os.Stat would follow it, fail with ENOENT, and report the
// path free -- licensing a write that replaces the operator's link.
func LstatBounded(ctx context.Context, path string) (os.FileInfo, error) {
	return runCancellable(ctx, func() (os.FileInfo, error) {
		return os.Lstat(path)
	})
}

// ReadFailureDistrustsLoop reports the error a failed read inside a
// MULTI-CANDIDATE LOOP should abort with, or nil when the failure is
// per-candidate and the loop may skip it and keep going.
//
// The distinction is whether the failure is a fact about THIS FILE or about the
// MOUNT (#2933):
//
//   - Per-candidate, keep going: a vanished file, an over-size file, a
//     permissions error. Each says something about one path and nothing about
//     the rest of the set.
//   - Distrusts the whole loop: a canceled/timed-out ctx, and the process-wide
//     abandoned-read cap (ErrTooManyStalledReads). Both mean the read DID NOT
//     HAPPEN for a reason that applies equally to every later candidate -- the
//     same unresponsive mount that wedged this read will wedge the next one.
//
// Why the cap belongs here, which is the #2933 fix. Every loop that consulted
// only ctx.Err() let a cap refusal take the skip branch. But the cap saturates
// precisely BECAUSE reads are already wedged against an unresponsive mount, so
// a refusal carries the same information a deadline does, arriving by a
// different route. The reasoning those call sites had already written down --
// "no further candidate in this loop can be trusted either" -- covers both
// causes exactly; only their code enumerated one.
//
// It lives in this package, beside the sentinel, because THREE loops in three
// packages need the same answer and had each spelled out only half of it. The
// consequence of getting it wrong differs sharply per caller, which is an
// argument for one shared predicate rather than three local ones: see the call
// sites in internal/rule (a redundant restored slot), internal/publish (an
// unrecoverable file -- the snapshot is the only copy that can undo a peer's
// delete), and internal/api (a push reported as succeeded).
//
// ctx is consulted BEFORE the error value so a cancellation still aborts the
// loop when the failing read reports something else on its way out. A read
// abandoned mid-flight can surface any error, so keying only on the error would
// let a cancellation be swallowed as an ordinary skip.
func ReadFailureDistrustsLoop(ctx context.Context, readErr error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if errors.Is(readErr, ErrTooManyStalledReads) {
		return readErr
	}
	return nil
}
