package rule

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
)

// Issue #2880. These tests pin WHO OWNS THE CONTEXT of a COALESCED provider
// fetch, for the whole EvaluationContext surface rather than the single call
// site #2878 fixed.
//
// The coalescer's entire purpose is to collapse concurrent requests for the
// same key into ONE upstream call. That call is therefore SHARED: whoever
// loses the singleflight race blocks on the winner's result and reads the
// winner's cached error. Running it under any one caller's context means that
// caller can
//
//	(a) cancel an in-flight fetch other callers are blocked on, and
//	(b) poison the coalescer's CACHE with the resulting error, which is then
//	    served to all of them as though their own fetch had failed.
//
// (b) is the reason a caller's own fallback does not make this benign, and it
// is what these tests assert -- observing only (a) would let a fix that
// detaches the context but still caches the poison pass.
//
// The fix makes dispatch hand every fetch closure a context derived with
// context.WithoutCancel, so no caller's cancellation reaches shared work. The
// last test is the negative control pinning that DIRECT (non-coalesced) paths
// stay caller-scoped, so a later "just detach everything" change is caught
// rather than silently making a genuinely per-caller fetch uncancellable.
//
// Env-independent: in-memory stubs, no network, no library, no binary on PATH.

// blockingEvalProvider stalls inside every EvalProvider method until release
// is closed, then reports whether the context it was handed had been canceled
// by then. Returning that cancellation as an ERROR is what a real provider
// does, and it is what lets the coalescer cache the poison -- so the tests can
// observe consequence (b), not merely (a).
type blockingEvalProvider struct {
	started chan struct{} // closed once the first fetch is actually running
	release chan struct{} // close to let the fetch finish

	mu       sync.Mutex
	calls    int
	ctxErrAt error // ctx.Err() observed at the moment the fetch completed
}

func newBlockingEvalProvider() *blockingEvalProvider {
	return &blockingEvalProvider{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
}

// block is the shared body of every stub method: announce that the fetch is in
// flight, wait to be released, then report the context state at completion.
func (p *blockingEvalProvider) block(ctx context.Context) error {
	p.mu.Lock()
	p.calls++
	first := p.calls == 1
	p.mu.Unlock()
	if first {
		close(p.started)
	}

	<-p.release

	err := ctx.Err()
	p.mu.Lock()
	p.ctxErrAt = err
	p.mu.Unlock()
	return err
}

func (p *blockingEvalProvider) observed() (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls, p.ctxErrAt
}

func (p *blockingEvalProvider) FetchImages(ctx context.Context, _ string, _ map[provider.ProviderName]string) (*provider.FetchResult, error) {
	if err := p.block(ctx); err != nil {
		return nil, err
	}
	return &provider.FetchResult{}, nil
}

func (p *blockingEvalProvider) FetchMetadata(ctx context.Context, _, _ string, _ map[provider.ProviderName]string) (*provider.FetchResult, error) {
	if err := p.block(ctx); err != nil {
		return nil, err
	}
	return &provider.FetchResult{}, nil
}

func (p *blockingEvalProvider) FetchFieldFromProviders(ctx context.Context, _, _, _ string, _ map[provider.ProviderName]string) ([]provider.FieldProviderResult, error) {
	if err := p.block(ctx); err != nil {
		return nil, err
	}
	return []provider.FieldProviderResult{{Provider: provider.ProviderName("stub")}}, nil
}

func (p *blockingEvalProvider) Search(ctx context.Context, _ string) ([]provider.ArtistSearchResult, error) {
	if err := p.block(ctx); err != nil {
		return nil, err
	}
	return []provider.ArtistSearchResult{{Name: "found"}}, nil
}

// TestEvaluationContext_CoalescedFetchSurvivesOneCallersCancellation is the
// #2880 regression guard across every coalesced method. Two callers share one
// EvaluationContext; canceling the FIRST caller's context mid-fetch must
// neither cancel the shared upstream call nor poison the cached result the
// SECOND caller reads.
//
// Ordering is deterministic rather than timing-based: caller B starts only
// once the fetch is confirmed in flight, and the cancellation lands before the
// fetch is released. Context cancellation propagates synchronously to derived
// contexts, so by the time the stub reads ctx.Err() the cancel has already
// landed -- under the pre-fix code it observes a canceled context every run.
func TestEvaluationContext_CoalescedFetchSurvivesOneCallersCancellation(t *testing.T) {
	// One subtest per coalesced method, since each one captures its own
	// closure and a fix applied to only some of them must fail here.
	cases := []struct {
		name string
		// call issues the coalesced request and returns a non-nil error when
		// the caller did not get a usable result.
		call func(ctx context.Context, ec *EvaluationContext) error
	}{
		{
			name: "FetchImages",
			call: func(ctx context.Context, ec *EvaluationContext) error {
				_, err := ec.FetchImages(ctx, "mbid-1", nil)
				return err
			},
		},
		{
			name: "FetchMetadata",
			call: func(ctx context.Context, ec *EvaluationContext) error {
				_, err := ec.FetchMetadata(ctx, "mbid-1", "Artist", nil)
				return err
			},
		},
		{
			name: "FetchFieldFromProviders",
			call: func(ctx context.Context, ec *EvaluationContext) error {
				_, err := ec.FetchFieldFromProviders(ctx, "mbid-1", "Artist", "genre", nil)
				return err
			},
		},
		{
			name: "Search",
			call: func(ctx context.Context, ec *EvaluationContext) error {
				_, err := ec.Search(ctx, "Artist")
				return err
			},
		},
		{
			name: "GetReleaseGroups",
			call: func(ctx context.Context, ec *EvaluationContext) error {
				_, err := ec.GetReleaseGroups(ctx, "mbid-1", func(fetchCtx context.Context) ([]provider.ReleaseGroupInfo, error) {
					if err := fetchCtx.Err(); err != nil {
						return nil, err
					}
					return []provider.ReleaseGroupInfo{{Title: "OK Computer"}}, nil
				})
				return err
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prov := newBlockingEvalProvider()
			ec := NewEvaluationContext(&artist.Artist{ID: "artist-1", Name: "Artist"}, prov, testLogger())

			// GetReleaseGroups does not route through EvalProvider -- its
			// closure is supplied by the caller -- so it needs its own
			// in-flight gate rather than the stub provider's.
			releaseGroups := tc.name == "GetReleaseGroups"
			started := prov.started
			release := prov.release
			if releaseGroups {
				started = make(chan struct{})
				release = make(chan struct{})
			}

			ctxA, cancelA := context.WithCancel(context.Background())
			defer cancelA()

			errA := make(chan error, 1)
			go func() {
				if releaseGroups {
					_, err := ec.GetReleaseGroups(ctxA, "mbid-1", func(fetchCtx context.Context) ([]provider.ReleaseGroupInfo, error) {
						close(started)
						<-release
						if cerr := fetchCtx.Err(); cerr != nil {
							return nil, cerr
						}
						return []provider.ReleaseGroupInfo{{Title: "OK Computer"}}, nil
					})
					errA <- err
					return
				}
				errA <- tc.call(ctxA, ec)
			}()

			// Only once the shared fetch is genuinely in flight does a second
			// caller arriving mean anything -- otherwise B could win the race
			// and the test would assert nothing about sharing.
			<-started

			errB := make(chan error, 1)
			go func() { errB <- tc.call(context.Background(), ec) }()

			// Caller A walks away while the shared fetch is still running.
			cancelA()
			close(release)

			<-errA // A's own outcome is not the subject; B's is.
			if err := <-errB; err != nil {
				t.Errorf("the SECOND caller got an error from a fetch it never canceled: %v\n"+
					"caller A's cancellation reached the shared coalesced fetch (or its cached error).", err)
			}

			calls, ctxErr := prov.observed()
			if releaseGroups {
				// The stub provider is untouched on this path (the closure is
				// caller-supplied), so coalescing is asserted via the
				// EvaluationContext's own counters instead. Without this the
				// subtest would pass with coalescing entirely DISABLED: two
				// independent fetches, each with its own uncanceled context,
				// satisfy "B did not error" trivially. Measured: with the
				// releasegroups cache key made unique per call, the subtest
				// still passed until this assertion existed.
				fetches, dedups := ec.Counters()
				if fetches != 1 {
					t.Errorf("upstream fetches = %d, want 1 -- the release-group fetch was not "+
						"coalesced, so this subtest proves nothing about shared work", fetches)
				}
				if dedups == 0 {
					t.Error("dedup count = 0 -- the second caller never joined the shared fetch, " +
						"so no sharing was exercised")
				}
				return
			}
			if calls != 1 {
				t.Errorf("upstream calls = %d, want 1 -- the fetch was not coalesced, so this test proves nothing", calls)
			}
			if ctxErr != nil {
				t.Errorf("the SHARED fetch observed a canceled context (%v) -- caller A's cancellation "+
					"reached work caller B was waiting on", ctxErr)
			}
		})
	}
}

// TestEvaluationContext_CanceledCallerDoesNotPoisonTheCache pins consequence
// (b) directly and durably: a caller that arrives AFTER the cancellation, and
// so never raced anyone, must still read a clean cached result. A fix that
// detached the context but left a cancellation-derived error in the cache
// would pass the test above (B was already waiting) and fail this one.
func TestEvaluationContext_CanceledCallerDoesNotPoisonTheCache(t *testing.T) {
	prov := newBlockingEvalProvider()
	ec := NewEvaluationContext(&artist.Artist{ID: "artist-1", Name: "Artist"}, prov, testLogger())

	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = ec.Search(ctxA, "Artist")
	}()

	<-prov.started
	cancelA()
	close(prov.release)
	<-done

	// A fresh, never-canceled caller reads the coalescer's cached entry.
	results, err := ec.Search(context.Background(), "Artist")
	if err != nil {
		t.Fatalf("a later caller read a POISONED cache entry left by a canceled caller: %v", err)
	}
	if len(results) == 0 {
		t.Error("the cached entry is empty, so the shared fetch never produced a usable result")
	}

	calls, _ := prov.observed()
	if calls != 1 {
		t.Errorf("upstream calls = %d, want 1 -- the second caller re-fetched instead of reading the cache", calls)
	}
}

// TestEvaluationContext_DirectPathStaysCallerScoped is the NEGATIVE CONTROL.
// Detachment is correct only where work is SHARED. A non-coalesced fetch is
// caller-scoped by construction -- nobody else can be waiting on it -- so
// canceling the caller must still cancel it. Without this, a later
// "detach everything" simplification would make genuinely per-caller fetches
// uncancellable, leaking goroutines on every abandoned request.
//
// This drives fetchReleaseGroupsCoalesced with NO EvaluationContext on the
// context, which is exactly the direct branch real callers take on the
// single-violation and bulk-executor paths.
func TestEvaluationContext_DirectPathStaysCallerScoped(t *testing.T) {
	fetcher := newBlockingReleaseGroupFetcher([]provider.ReleaseGroupInfo{{Title: "OK Computer"}})
	e := &Engine{logger: testLogger()}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		// No EvaluationContext attached -> the direct branch.
		_, err := e.fetchReleaseGroupsCoalesced(ctx, fetcher, "mbid-1")
		done <- err
	}()

	<-fetcher.started
	cancel()
	close(fetcher.release)

	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("direct fetch error = %v, want context.Canceled -- the non-coalesced path "+
			"must stay caller-scoped; detaching it would leak work for abandoned requests", err)
	}

	if _, ctxErr := fetcher.observed(); !errors.Is(ctxErr, context.Canceled) {
		t.Errorf("the direct fetch observed ctx.Err() = %v, want context.Canceled -- "+
			"the caller's cancellation must reach a fetch nobody else shares", ctxErr)
	}
}

// TestEvaluationContext_PassCachedFetchSurvivesOneCallersCancellation covers
// the branch PRODUCTION ACTUALLY USES. dispatch has two fetch sites: the plain
// per-artist one, and the pass-cache one taken whenever a PassContext rides the
// context. RunAllScoped -- the library-wide fix pass -- installs a PassContext
// (fixer.go, gated on hasOrch), so the highest-traffic path is the pass-cache
// branch exclusively.
//
// Found by hostile review: the original tests all drove the plain branch, and
// reverting ONLY the pass-cache detachment left the entire suite green. A
// regression on the busiest path would have shipped silently. The mutation that
// motivated this test now fails it.
//
// Two SEPARATE EvaluationContexts share one PassContext. They carry the same
// artist ID because evalCacheKey includes it, so that is what a pass-cache hit
// actually looks like in production: the same artist RE-ENTERED within one
// RunAllScoped pass (the pre-fix and post-fix evaluations of a scoped run each
// build their own EC), where the second entry must find the first's result in
// the pass cache rather than re-fetching. Two distinct ECs is the part that
// matters -- neither one's LOCAL cache can serve the other, so a hit here
// proves the pass-cache branch ran.
func TestEvaluationContext_PassCachedFetchSurvivesOneCallersCancellation(t *testing.T) {
	prov := newBlockingEvalProvider()
	pc := NewPassContext(8, testLogger())

	ecA := NewEvaluationContext(&artist.Artist{ID: "artist-1", Name: "Artist"}, prov, testLogger())
	ecB := NewEvaluationContext(&artist.Artist{ID: "artist-1", Name: "Artist"}, prov, testLogger())

	ctxA, cancelA := context.WithCancel(WithPassContext(context.Background(), pc))
	defer cancelA()
	ctxB := WithPassContext(context.Background(), pc)

	errA := make(chan error, 1)
	go func() {
		_, err := ecA.Search(ctxA, "Shared Artist")
		errA <- err
	}()

	<-prov.started

	errB := make(chan error, 1)
	resB := make(chan int, 1)
	go func() {
		results, err := ecB.Search(ctxB, "Shared Artist")
		resB <- len(results)
		errB <- err
	}()

	// Caller A abandons the pass while the shared fetch is still in flight.
	cancelA()
	close(prov.release)

	<-errA
	if err := <-errB; err != nil {
		t.Errorf("the SECOND caller (a separate EvaluationContext sharing the pass cache) got an error "+
			"from a fetch it never canceled: %v", err)
	}
	if n := <-resB; n == 0 {
		t.Error("the second caller got an empty result -- the shared pass-cached fetch produced nothing")
	}

	calls, ctxErr := prov.observed()
	if calls != 1 {
		t.Errorf("upstream calls = %d, want 1 -- the pass cache did not collapse the two callers, "+
			"so this test does not exercise the shared-fetch path", calls)
	}
	if ctxErr != nil {
		t.Errorf("the SHARED pass-cached fetch observed a canceled context (%v) -- caller A's "+
			"cancellation reached work another evaluation was waiting on", ctxErr)
	}
}

// TestEvaluationContext_CoalescedFetchIsBoundedWithoutACallerDeadline pins the
// #2880 hazard that detachment CREATES rather than fixes. context.WithoutCancel
// strips the caller's deadline as well as its cancellation, so a coalesced
// fetch inherits no bound from its caller. A closure that applies no cap of its
// own would therefore run FOREVER -- where before the fix it inherited the
// caller's deadline. Detachment must not convert a safe default into an
// unsafe one.
//
// GetReleaseGroups is the exposed surface with that risk (it takes an arbitrary
// caller closure), so it carries a backstop timeout. This asserts the backstop
// exists by handing it a closure that ignores the deadline question entirely
// and never returns on its own.
func TestEvaluationContext_CoalescedFetchIsBoundedWithoutACallerDeadline(t *testing.T) {
	ec := NewEvaluationContext(&artist.Artist{ID: "artist-1", Name: "Artist"}, newBlockingEvalProvider(), testLogger())

	// No deadline on the caller: the fetch's only possible bound is the one
	// the coalescer applies.
	sawDeadline := make(chan bool, 1)
	_, err := ec.GetReleaseGroups(context.Background(), "mbid-1", func(fetchCtx context.Context) ([]provider.ReleaseGroupInfo, error) {
		_, ok := fetchCtx.Deadline()
		sawDeadline <- ok
		return []provider.ReleaseGroupInfo{{Title: "OK Computer"}}, nil
	})
	if err != nil {
		t.Fatalf("fetch failed: %v", err)
	}

	if !<-sawDeadline {
		t.Error("the coalesced fetch context carries NO deadline: a closure that does not cap " +
			"itself would run unbounded. WithoutCancel stripped the caller's deadline and " +
			"nothing replaced it.")
	}
}
