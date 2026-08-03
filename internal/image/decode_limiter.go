package image

import (
	"errors"
	"fmt"
	"sync/atomic"
	"time"
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

	// defaultDecodeAcquireTimeout bounds how long a caller waits for a slot.
	//
	// An UNBOUNDED wait is deliberately not used. decodeWithLimit takes no
	// context (none of the eleven exported entry points thread one, and adding
	// them would be a signature-breaking change across four packages), so a
	// caller blocked here would have no cancellation path at all: a saturated
	// queue would pin an HTTP handler goroutine indefinitely with no way for
	// the client's disconnect to release it. That is precisely the wedge #2689
	// spent three review rounds eliminating, and re-creating it here would
	// trade an unbounded memory peak for an unbounded goroutine backlog.
	//
	// A bounded wait gets both halves: ordinary contention stays invisible (a
	// queued decode simply waits its turn, and a decode is fast relative to
	// 30 s), while sustained saturation sheds load with a clear error instead
	// of accumulating a queue. 30 s sits above any plausible single decode and
	// below any sane HTTP client timeout, so the caller sees the error rather
	// than a dead connection.
	defaultDecodeAcquireTimeout = 30 * time.Second
)

// decodeAcquireTimeout is a var, not a const, solely so tests can shorten it.
// Nothing in production ever writes it.
var decodeAcquireTimeout = defaultDecodeAcquireTimeout

// ErrDecodeBusy is returned when no decode slot became available within
// decodeAcquireTimeout. It is a load-shedding signal, not a corrupt-input
// signal: the same request may well succeed once the burst subsides.
var ErrDecodeBusy = errors.New("image decode capacity exhausted")

var (
	// decodeSem is a counting semaphore: capacity is the concurrency bound, a
	// send takes a slot and a receive returns it. A buffered channel is used
	// rather than golang.org/x/sync/semaphore because the channel form
	// expresses a bounded wait through a plain select, with no
	// context.Context anywhere -- and fabricating a context here purely to
	// carry a timeout is exactly the "ignores its caller's context" shape the
	// contextcheck linter flags, on a call chain where several callers really
	// do hold a context they cannot pass down.
	//
	// The pointer is swapped atomically so SetMaxConcurrentDecodes can re-size
	// the bound without locking every decode; in-flight decodes release into
	// the channel they acquired from, new ones use the new channel.
	decodeSem atomic.Pointer[chan struct{}]

	// decodeLimit mirrors the capacity currently installed in decodeSem so
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
	sem := make(chan struct{}, n)
	decodeSem.Store(&sem)
	decodeLimit.Store(int64(n))
}

// MaxConcurrentDecodes reports the bound currently in force.
func MaxConcurrentDecodes() int {
	return int(decodeLimit.Load())
}

// acquireDecodeSlot waits for a decode slot, up to decodeAcquireTimeout, and
// returns the function that releases it. The release closure captures the
// channel that granted the slot rather than re-reading decodeSem, so a
// concurrent SetMaxConcurrentDecodes between acquire and release cannot return
// a permit to a semaphore that never issued one.
func acquireDecodeSlot() (release func(), err error) {
	semPtr := decodeSem.Load()
	if semPtr == nil {
		// Unreachable in practice (init installs one). Failing open is
		// deliberate: one unbounded decode is a far smaller problem than a nil
		// dereference on every image request.
		return func() {}, nil
	}
	sem := *semPtr

	// Fast path: a slot is free, so no timer is allocated.
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, nil
	default:
	}

	timer := time.NewTimer(decodeAcquireTimeout)
	defer timer.Stop()
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, nil
	case <-timer.C:
		return nil, fmt.Errorf("%w (limit %d, waited %s)", ErrDecodeBusy, MaxConcurrentDecodes(), decodeAcquireTimeout)
	}
}
