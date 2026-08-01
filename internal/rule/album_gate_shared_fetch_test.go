package rule

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
)

// These tests pin WHO OWNS THE DEADLINE on the album gate's release-group
// fetch.
//
// The gate routes through the per-artist EvaluationContext coalescer, whose
// entire purpose is to collapse this fetch with the discography checker's fetch
// for the same artist into ONE upstream call. That call is therefore SHARED:
// callers that lose the singleflight race wait on the winner's result and read
// the winner's cached error. Handing that shared fetch a context that carries
// one caller's deadline (or one caller's cancellation) means that caller can
//
//	(a) cancel an in-flight fetch other callers are blocked on, and
//	(b) poison the coalescer's cache with the resulting error, which is then
//	    served to every one of them.
//
// The fix derives the coalesced fetch's context with context.WithoutCancel plus
// a fresh discographyFetchTimeout, so the cap belongs to the coalescer. The
// DIRECT (no-EvaluationContext) path is caller-scoped by construction and keeps
// the caller's cancellation -- the second test below is the negative control
// that pins that half, so a future "just WithoutCancel everything" change is
// caught rather than silently detaching a genuinely per-caller fetch.
//
// Env-independent: an in-memory stub fetcher, no network, no library, no
// binary on PATH.

// blockingReleaseGroupFetcher stalls inside GetReleaseGroups until release is
// closed, then reports whether the context it was handed had been canceled by
// then. Returning that cancellation as an ERROR is what a real fetcher does, and
// it is what lets the coalescer cache the poison -- so the test can observe
// consequence (b) and not merely consequence (a).
type blockingReleaseGroupFetcher struct {
	groups  []provider.ReleaseGroupInfo
	started chan struct{} // closed once the fetch is actually running
	release chan struct{} // close to let the fetch finish

	mu       sync.Mutex
	calls    int
	ctxErrAt error // ctx.Err() observed at the moment the fetch completed
}

func newBlockingReleaseGroupFetcher(groups []provider.ReleaseGroupInfo) *blockingReleaseGroupFetcher {
	return &blockingReleaseGroupFetcher{
		groups:  groups,
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (f *blockingReleaseGroupFetcher) GetReleaseGroups(ctx context.Context, _ string) ([]provider.ReleaseGroupInfo, error) {
	f.mu.Lock()
	f.calls++
	first := f.calls == 1
	f.mu.Unlock()
	if first {
		close(f.started)
	}

	<-f.release

	err := ctx.Err()
	f.mu.Lock()
	f.ctxErrAt = err
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return f.groups, nil
}

func (f *blockingReleaseGroupFetcher) observed() (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, f.ctxErrAt
}

// TestAlbumGate_CoalescedFetchSurvivesOneCallersCancellation is the regression
// guard: with two callers sharing one EvaluationContext, canceling the FIRST
// caller's context mid-fetch must neither cancel the shared upstream call nor
// poison the cached result the SECOND caller reads.
//
// Ordering is deterministic, not timing-based: caller B is only started once
// the fetch is confirmed in flight, and the cancellation happens before the
// fetch is released. context cancellation propagates synchronously to derived
// contexts, so by the time the fetcher reads ctx.Err() the cancel has already
// landed -- under the pre-fix code it observes a canceled context every run.
func TestAlbumGate_CoalescedFetchSurvivesOneCallersCancellation(t *testing.T) {
	want := []provider.ReleaseGroupInfo{{Title: "OK Computer"}, {Title: "Kid A"}}
	fetcher := newBlockingReleaseGroupFetcher(want)
	g := ruleAlbumGate{
		albums:  artist.NewFilesystemAlbumSource(),
		fetcher: fetcher,
		logger:  testLogger(),
	}

	a := &artist.Artist{ID: "art-1", Name: gateArtistName}
	ec := NewEvaluationContext(a, &countingEvalProvider{}, testLogger())
	base := WithEvaluationContext(context.Background(), ec)

	ctxA, cancelA := context.WithCancel(base)
	defer cancelA()

	type outcome struct {
		titles []string
		known  bool
	}
	aDone := make(chan outcome, 1)
	go func() {
		titles, known := g.candidateTitles(ctxA, "nfo_has_mbid", a, mbidRadiohead)
		aDone <- outcome{titles, known}
	}()

	// PRECONDITION: the shared fetch is genuinely in flight before anything
	// else happens. Without this the test could cancel before the fetch even
	// started and prove nothing about an IN-FLIGHT shared call.
	<-fetcher.started

	bDone := make(chan outcome, 1)
	bReady := make(chan struct{})
	go func() {
		close(bReady)
		// Caller B's own context is never canceled. It arrives after the
		// placeholder is published, so it waits on the SAME upstream fetch.
		titles, known := g.candidateTitles(base, "discography_populated", a, mbidRadiohead)
		bDone <- outcome{titles, known}
	}()
	<-bReady

	// The caller-scoped cancellation under test. It must not reach the shared
	// fetch.
	cancelA()
	close(fetcher.release)

	<-aDone
	b := <-bDone

	calls, ctxErr := fetcher.observed()

	// PRECONDITION: exactly one upstream call, i.e. the two callers really did
	// coalesce. If they had each fetched, canceling A could not have harmed B
	// and every assertion below would pass vacuously.
	if calls != 1 {
		t.Fatalf("upstream release-group fetches = %d, want 1: the two callers must share one "+
			"coalesced fetch or this test proves nothing", calls)
	}
	if _, dedups := ec.Counters(); dedups != 1 {
		t.Fatalf("EvaluationContext dedups = %d, want 1: caller B must have been served by the "+
			"coalescer rather than issuing its own fetch", dedups)
	}

	// (a) the shared fetch must not have been canceled by caller A's deadline.
	if ctxErr != nil {
		t.Errorf("the shared coalesced fetch observed a canceled context (%v); one caller's "+
			"cancellation must not reach a fetch other callers are waiting on", ctxErr)
	}

	// (b) and the cached result served to caller B must not carry that poison.
	if !b.known {
		t.Errorf("caller B got known=false: caller A's cancellation poisoned the coalesced cache " +
			"entry B was waiting on")
	}
	if len(b.titles) != len(want) {
		t.Fatalf("caller B got %d titles, want %d", len(b.titles), len(want))
	}
	for i := range want {
		if b.titles[i] != want[i].Title {
			t.Errorf("caller B title[%d] = %q, want %q", i, b.titles[i], want[i].Title)
		}
	}
}

// TestAlbumGate_DirectFetchStillHonorsCallerCancellation is the negative
// control for the test above. With NO EvaluationContext on ctx (single-violation
// fixes and the bulk executor) the fetch is caller-scoped by construction --
// nobody else can be waiting on it -- so the caller's cancellation SHOULD reach
// it. This pins that the fix did not detach that path too.
func TestAlbumGate_DirectFetchStillHonorsCallerCancellation(t *testing.T) {
	fetcher := newBlockingReleaseGroupFetcher([]provider.ReleaseGroupInfo{{Title: "OK Computer"}})
	g := ruleAlbumGate{
		albums:  artist.NewFilesystemAlbumSource(),
		fetcher: fetcher,
		logger:  testLogger(),
	}

	a := &artist.Artist{ID: "art-1", Name: gateArtistName}

	// PRECONDITION: no coalescer attached, so this is genuinely the direct path.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if ec := EvaluationContextFromContext(ctx); ec != nil {
		t.Fatalf("fixture precondition: ctx must carry NO EvaluationContext, got %v", ec)
	}

	done := make(chan bool, 1)
	go func() {
		_, known := g.candidateTitles(ctx, "nfo_has_mbid", a, mbidRadiohead)
		done <- known
	}()

	<-fetcher.started
	cancel()
	close(fetcher.release)

	known := <-done

	calls, ctxErr := fetcher.observed()
	if calls != 1 {
		t.Fatalf("upstream fetches = %d, want 1", calls)
	}
	if !errors.Is(ctxErr, context.Canceled) {
		t.Errorf("direct-path fetch observed ctx.Err() = %v, want context.Canceled: an uncoalesced, "+
			"caller-scoped fetch must still be cancellable by its caller", ctxErr)
	}
	// And the failure still degrades to "not a determination", which ALLOWS --
	// the gate's fail-open policy is unchanged by any of this.
	if known {
		t.Errorf("candidateTitles known = true after a canceled fetch, want false (a failed fetch " +
			"must not read as a determination)")
	}
}
