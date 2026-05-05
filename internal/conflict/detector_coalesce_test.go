package conflict

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/connection"
)

// waitFor spins until cond returns true or timeout elapses, yielding to the
// scheduler between checks. Used by the coalesce tests to wait for all
// stampede goroutines to reach refreshMu without resorting to time.Sleep.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", msg)
		}
		runtime.Gosched()
	}
}

// countingRepo wraps a connection list and counts how many times List is
// called. List is the first thing Refresh does, so the call count equals the
// number of Refresh body executions -- a clean signal that the cache-stampede
// guard is doing its job.
type countingRepo struct {
	conns []connection.Connection
	calls atomic.Int32
}

func (r *countingRepo) List(_ context.Context) ([]connection.Connection, error) {
	r.calls.Add(1)
	return r.conns, nil
}

// gatedClient blocks both peer checks on a release channel so a test can pile
// up concurrent Current() callers behind a single in-flight Refresh, then
// release them and assert the coalescing behavior.
type gatedClient struct {
	release  <-chan struct{}
	nfo      atomic.Int32
	image    atomic.Int32
	disables atomic.Int32
}

func (g *gatedClient) CheckNFOWriterEnabled(_ context.Context) (bool, string, error) {
	g.nfo.Add(1)
	if g.release != nil {
		<-g.release
	}
	return false, "", nil
}

func (g *gatedClient) CheckImageSaverEnabled(_ context.Context) (bool, string, error) {
	g.image.Add(1)
	if g.release != nil {
		<-g.release
	}
	return false, "", nil
}

func (g *gatedClient) DisableFileWriteBack(_ context.Context) error {
	g.disables.Add(1)
	return nil
}

// TestDetectorCurrentCoalescesConcurrentRefresh proves the cache-stampede
// guard in detector.go: when N goroutines hit Current() against a cold cache
// at the same instant, exactly one Refresh body executes and the rest receive
// the freshly-cached ledger.
//
// Without the refreshMu + double-check pattern, every concurrent caller would
// fall through to Refresh, which is the bug #1187 tracks.
func TestDetectorCurrentCoalescesConcurrentRefresh(t *testing.T) {
	const goroutines = 32

	release := make(chan struct{})
	repo := &countingRepo{conns: []connection.Connection{
		{ID: "a", Name: "a", Type: connection.TypeEmby, Enabled: true},
	}}
	client := &gatedClient{release: release}

	factory := func(c connection.Connection) (peerClient, pathProvider) {
		return client, nil
	}
	d := newDetectorWithClients(repo, nil, newLogger(), factory)
	// Long TTL: once the first Refresh lands, every other caller must hit
	// the fresh-cache fast path on its post-mutex re-check.
	d.ttl = time.Hour

	// Count how many goroutines have reached the pre-Lock site inside
	// Current(). This replaces a time.Sleep gate with a deterministic
	// barrier: once entered == goroutines, the leader holds refreshMu and
	// every follower has reached (or is about to park on) Lock().
	var entered atomic.Int32
	d.onBeforeRefreshLock = func() { entered.Add(1) }

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			d.Current(context.Background())
		}()
	}

	// Wait for every caller to reach the refreshMu contention point. If
	// the cache-stampede guard regressed, followers would still increment
	// `entered` here -- the assertion below on repo.calls is what catches
	// the regression.
	waitFor(t, 5*time.Second, func() bool {
		return entered.Load() == int32(goroutines)
	}, "all goroutines to reach refreshMu")

	close(release)
	wg.Wait()

	if got := repo.calls.Load(); got != 1 {
		t.Errorf("expected exactly 1 Refresh body execution, got %d (cache-stampede guard regressed)", got)
	}
	if got := client.nfo.Load(); got != 1 {
		t.Errorf("expected exactly 1 NFO peer check, got %d", got)
	}
	if got := client.image.Load(); got != 1 {
		t.Errorf("expected exactly 1 image peer check, got %d", got)
	}
}

// TestDetectorCurrentCoalescesAfterTTLExpiry covers the post-expiry path: a
// fresh cache populates, time moves past the TTL, then a stampede arrives.
// Same expectation as the cold-start case -- exactly one Refresh executes.
func TestDetectorCurrentCoalescesAfterTTLExpiry(t *testing.T) {
	const goroutines = 16

	repo := &countingRepo{conns: []connection.Connection{
		{ID: "a", Name: "a", Type: connection.TypeEmby, Enabled: true},
	}}
	client := &gatedClient{} // unblocked: first Refresh runs synchronously

	factory := func(c connection.Connection) (peerClient, pathProvider) {
		return client, nil
	}
	d := newDetectorWithClients(repo, nil, newLogger(), factory)
	d.ttl = 20 * time.Millisecond

	// Prime the cache.
	d.Current(context.Background())
	if got := repo.calls.Load(); got != 1 {
		t.Fatalf("expected priming Refresh to fire once, got %d", got)
	}

	// Expire the cache.
	time.Sleep(30 * time.Millisecond)

	// Now gate the next round of peer checks so the post-expiry stampede
	// piles up behind the leader.
	release := make(chan struct{})
	client.release = release

	// Count post-expiry stampede arrivals at the refreshMu contention
	// point so we can release the leader only after every caller has
	// reached the slow path. The priming Current() above ran before this
	// hook was installed, so it does not contribute to the count.
	var entered atomic.Int32
	d.onBeforeRefreshLock = func() { entered.Add(1) }

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			d.Current(context.Background())
		}()
	}
	waitFor(t, 5*time.Second, func() bool {
		return entered.Load() == int32(goroutines)
	}, "post-expiry goroutines to reach refreshMu")

	close(release)
	wg.Wait()

	// Priming Refresh + exactly one post-expiry Refresh.
	if got := repo.calls.Load(); got != 2 {
		t.Errorf("expected exactly 2 total Refresh executions (1 prime + 1 stampede), got %d", got)
	}
}
