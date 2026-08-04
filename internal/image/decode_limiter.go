package image

import (
	"errors"
	"fmt"
	"sync/atomic"
	"time"
)

// Process-wide bound on CONCURRENT LIVE DECODED IMAGES (#2928).
//
// Process memory peak is `per-image cost x concurrency`. maxDecodedBytes
// (#2929) bounds the first factor; this bounds the second.
//
// THE SECOND FACTOR IS LIVE IMAGES, NOT RUNNING DECODES, and the distinction
// is the whole bound. What occupies memory is the decoded buffer, which
// outlives the decode that produced it: a caller holds it for the rest of its
// work, and the trim paths allocate a second full-size buffer alongside it.
// An earlier revision released the slot when the decode call returned, which
// bounded the running decodes while leaving the live buffers unbounded --
// measured at limit 1, sixteen decoded images were live at once and the heap
// grew 256 MB. decodeWithLimit therefore hands its release closure to the
// caller, who defers it, so the slot covers the buffer's whole lifetime.
//
// What this does NOT bound is inbound request volume. Nothing in
// internal/server or internal/api caps in-flight requests, so callers can
// still queue up arbitrarily; they simply wait for a slot (up to
// decodeAcquireTimeout) rather than each holding a decoded image. Bounding
// concurrent requests is a separate, and separately-tracked, concern.
//
// Before this, the
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
// change cover all eleven exported entry points without changing any exported
// signature. Scoping the slot to the buffer's lifetime is only expressible
// because that buffer never escapes the package: no exported function in
// internal/image returns an image.Image, so each entry point can defer its own
// release.
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

// decodeAcquireTimeout is an atomic, not a const, solely so tests can shorten
// it. Nothing in production ever writes it. It is atomic rather than a plain
// var for the same reason decodeSem is: tests assign it from the goroutine
// running the test body while acquireDecodeSlot reads it from decoding
// goroutines that can outlive that assignment, and a plain var under that
// access pattern is a data race waiting on test shape rather than on the
// variable itself -- today's tests happen to join before restoring it, but
// nothing enforces that for a future test.
var decodeAcquireTimeout atomic.Int64

func init() {
	decodeAcquireTimeout.Store(int64(defaultDecodeAcquireTimeout))
}

// acquireTimeout returns the current decode-slot acquire timeout.
func acquireTimeout() time.Duration {
	return time.Duration(decodeAcquireTimeout.Load())
}

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

	timeout := acquireTimeout()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, nil
	case <-timer.C:
		return nil, fmt.Errorf("%w (limit %d, waited %s)", ErrDecodeBusy, MaxConcurrentDecodes(), timeout)
	}
}
