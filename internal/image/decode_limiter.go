package image

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"golang.org/x/sync/semaphore"
)

// Process-wide bound on CONCURRENT full-pixel decodes (#2928).
//
// Process memory peak is `per-decode cost x concurrency`. maxDecodedBytes
// (#2929) bounds the first factor; this bounds the second. Before this, the
// request-reachable decoders -- POST .../images/logo/trim, the image
// upload/save path, and GeneratePlaceholder (reached from the scanner and from
// image-registry repair) -- decoded on their own goroutine with no listener
// limit and no semaphore, so N concurrent requests meant N concurrent decodes
// and the peak was a function of inbound request volume rather than of any
// configured number. SW_RULE_ENGINE_ARTIST_WORKERS bounds the rules-pass path
// only; it is not, and never was, a bound on the process.
//
// The bound is applied inside decodeWithLimit because that is the single funnel
// every decode in the package already passes through, which is what lets one
// change cover all eleven exported entry points without touching a signature.
const (
	// DefaultDecodeConcurrency is the default number of concurrent decodes.
	//
	// 2 matches SW_RULE_ENGINE_ARTIST_WORKERS (default 2) and the `cpus: "2.0"`
	// quota in docker-compose.yml, so the three numbers that shape the memory
	// derivation there agree by default instead of by coincidence. With
	// maxDecodedBytes at 400 MB the concurrent-decode term is bounded at
	// ~800 MB, and the trim path's second full-size buffer brings a worst-case
	// pair of concurrent trims to ~1.6 GB against the 3 GiB container ceiling.
	// Raising this raises that term proportionally -- mem_limit and GOMEMLIMIT
	// must move with it.
	DefaultDecodeConcurrency = 2

	// decodeAcquireTimeout bounds how long a caller waits for a decode slot.
	//
	// A plain blocking acquire (semaphore.Acquire with context.Background()) is
	// deliberately NOT used: decodeWithLimit has no context to carry a caller's
	// cancellation, so an unbounded block re-creates exactly the wedge that
	// #2689 spent three review rounds eliminating -- a saturated queue would
	// pin an HTTP handler goroutine indefinitely with no way for the client's
	// disconnect to release it. A bounded wait keeps ordinary contention
	// invisible (a queued decode simply waits its turn) while turning
	// sustained saturation into load shedding with a clear error instead of an
	// unbounded backlog.
	defaultDecodeAcquireTimeout = 30 * time.Second
)

// decodeAcquireTimeout is a var, not a const, solely so tests can shorten it;
// nothing in production ever writes it.
var decodeAcquireTimeout = defaultDecodeAcquireTimeout

// ErrDecodeBusy is returned when no decode slot became available within
// decodeAcquireTimeout. It is a load-shedding signal, not a corrupt-input
// signal: the same request may well succeed once the burst subsides.
var ErrDecodeBusy = errors.New("image decode capacity exhausted")

var (
	// decodeSem holds the live semaphore. It is swapped atomically so
	// SetMaxConcurrentDecodes can re-size the bound at boot (and would support
	// a live reload) without locking every decode; in-flight decodes drain
	// against the semaphore they acquired, new ones use the new bound.
	decodeSem atomic.Pointer[semaphore.Weighted]

	// decodeLimit mirrors the weight currently installed in decodeSem so
	// MaxConcurrentDecodes can report the bound in force.
	decodeLimit atomic.Int64
)

func init() {
	SetMaxConcurrentDecodes(DefaultDecodeConcurrency)
}

// SetMaxConcurrentDecodes installs a new process-wide bound on concurrent
// decodes. Non-positive values normalize to DefaultDecodeConcurrency, matching
// the convention SetArtistWorkers uses in internal/rule.
func SetMaxConcurrentDecodes(n int) {
	if n <= 0 {
		n = DefaultDecodeConcurrency
	}
	decodeSem.Store(semaphore.NewWeighted(int64(n)))
	decodeLimit.Store(int64(n))
}

// MaxConcurrentDecodes reports the bound currently in force.
func MaxConcurrentDecodes() int {
	return int(decodeLimit.Load())
}

// acquireDecodeSlot blocks until a decode slot is free or decodeAcquireTimeout
// elapses. The returned release function is always non-nil on success and must
// be called on the SAME semaphore that granted the slot -- hence the closure,
// which captures the pointer rather than re-loading it (a concurrent
// SetMaxConcurrentDecodes between acquire and release would otherwise release
// a permit into a semaphore that never issued it).
func acquireDecodeSlot() (release func(), err error) {
	sem := decodeSem.Load()
	if sem == nil {
		// Unreachable in practice (init installs one), but a nil dereference
		// here would be a far worse failure than an unbounded decode.
		return func() {}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), decodeAcquireTimeout)
	defer cancel()

	if err := sem.Acquire(ctx, 1); err != nil {
		return nil, fmt.Errorf("%w (limit %d, waited %s)", ErrDecodeBusy, MaxConcurrentDecodes(), decodeAcquireTimeout)
	}
	return func() { sem.Release(1) }, nil
}
