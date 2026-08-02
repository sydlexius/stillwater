package provider

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

// newTimeoutTestOrchestrator wires an orchestrator over the given providers,
// discarding logs so a deliberately-failing provider does not spam the output.
func newTimeoutTestOrchestrator(t *testing.T, aimd *AIMDController, provs ...Provider) *Orchestrator {
	t.Helper()
	registry := NewRegistry()
	for _, p := range provs {
		registry.Register(p)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	return NewOrchestrator(registry, nil, logger, aimd)
}

// TestSearchForLinking_ProvidersRunConcurrently pins the fan-out. Sequentially
// two providers that each block for the same interval take twice as long as one;
// concurrently they take about as long as one. This is what stops a slow
// provider's latency from being additive to a healthy one's (#2818).
//
// The assertion is deliberately generous -- it only requires the total to come
// in under the sum -- so it fails on a genuine regression to sequential
// execution without turning into a timing-flake on a loaded machine.
func TestSearchForLinking_ProvidersRunConcurrently(t *testing.T) {
	t.Parallel()

	const block = 200 * time.Millisecond

	slow := func(name ProviderName) *mockProvider {
		return &mockProvider{
			name: name,
			searchFn: func(ctx context.Context, _ string) ([]ArtistSearchResult, error) {
				select {
				case <-time.After(block):
					return []ArtistSearchResult{{Name: string(name)}}, nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			},
		}
	}

	o := newTimeoutTestOrchestrator(t, nil, slow(NameMusicBrainz), slow(NameDiscogs))

	start := time.Now()
	results, statuses, err := o.SearchForLinking(context.Background(), "x",
		[]ProviderName{NameMusicBrainz, NameDiscogs})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}
	if len(statuses) != 2 {
		t.Fatalf("statuses = %d, want 2", len(statuses))
	}
	// Order must survive the fan-out: the UI lines its failed-provider banner
	// up with the provider the operator configured.
	if statuses[0].Provider != NameMusicBrainz || statuses[1].Provider != NameDiscogs {
		t.Errorf("status order = %v, %v; want MusicBrainz, Discogs", statuses[0].Provider, statuses[1].Provider)
	}
	if elapsed >= 2*block {
		t.Errorf("elapsed %v >= %v: providers appear to run sequentially, so a slow provider's latency is still additive",
			elapsed, 2*block)
	}
}

// TestSearchForLinking_SlowProviderDoesNotBlockHealthyResult is the operator-
// facing symptom from #2818: one provider hangs far longer than anyone will
// wait, and the healthy provider's match must still come back, with the hung
// provider reported as errored rather than silently swallowed.
//
// The hung provider blocks until its context is canceled, so this test also
// proves the deadline is actually wired to the provider call -- without it the
// test would hang until the Go test timeout rather than fail.
func TestSearchForLinking_SlowProviderDoesNotBlockHealthyResult(t *testing.T) {
	t.Parallel()

	healthy := &mockProvider{
		name: NameMusicBrainz,
		searchFn: func(_ context.Context, _ string) ([]ArtistSearchResult, error) {
			return []ArtistSearchResult{{Name: "Healthy Match", MusicBrainzID: "mbid-1"}}, nil
		},
	}
	// Blocks until canceled. If the per-provider deadline were missing this
	// would never return on its own.
	hung := &mockProvider{
		name: NameDiscogs,
		searchFn: func(ctx context.Context, _ string) ([]ArtistSearchResult, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}

	o := newTimeoutTestOrchestrator(t, nil, healthy, hung)

	// A short caller deadline stands in for the per-provider ceiling so the
	// test stays fast. The mechanism under test -- the search returning while
	// one provider is still stuck -- is identical.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	var results []ArtistSearchResult
	var statuses []ProviderSearchStatus
	go func() {
		defer close(done)
		results, statuses, _ = o.SearchForLinking(ctx, "x", []ProviderName{NameMusicBrainz, NameDiscogs})
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("SearchForLinking did not return: a hung provider still blocks the whole search")
	}

	if len(results) != 1 || results[0].MusicBrainzID != "mbid-1" {
		t.Errorf("results = %+v, want the healthy provider's single match", results)
	}
	if len(statuses) != 2 {
		t.Fatalf("statuses = %d, want 2", len(statuses))
	}
	if statuses[0].Errored {
		t.Errorf("healthy provider reported errored: %+v", statuses[0])
	}
	if !statuses[1].Errored {
		t.Error("hung provider must surface as an errored status, not as a silent empty result")
	}
}

// TestSearchForLinking_DeadlineIsSelfImposed proves the per-provider deadline
// EXISTS, independently of any caller deadline.
//
// The sibling hung-provider test passes a short caller context, so a provider
// blocking on ctx.Done() unblocks whether or not SearchForLinking imposes its
// own ceiling -- that test measures cancellation propagation, not the deadline.
// Here the caller context has NO deadline at all, so the only thing that can
// ever release the provider is the orchestrator's own timeout. Replacing
// WithTimeout with WithCancel makes this hang until the Go test timeout.
//
// perProviderSearchTimeout is deliberately read rather than hardcoded: the
// point is that SOME finite self-imposed ceiling applies, not what its value
// is this week.
func TestSearchForLinking_DeadlineIsSelfImposed(t *testing.T) {
	t.Parallel()

	if perProviderSearchTimeout <= 0 {
		t.Fatalf("perProviderSearchTimeout = %v, want a positive ceiling", perProviderSearchTimeout)
	}

	released := make(chan struct{})
	hung := &mockProvider{
		name: NameDiscogs,
		searchFn: func(ctx context.Context, _ string) ([]ArtistSearchResult, error) {
			<-ctx.Done() // only our own deadline can fire here
			close(released)
			return nil, ctx.Err()
		},
	}
	o := newTimeoutTestOrchestrator(t, nil, hung)
	// Shrink the ceiling so the test is fast. The MECHANISM under test is
	// unchanged: without a self-imposed deadline of any size, a deadline-free
	// caller context leaves the provider blocked forever.
	o.searchTimeout = 150 * time.Millisecond

	done := make(chan struct{})
	go func() {
		defer close(done)
		// context.Background(): no deadline, no cancel. If SearchForLinking
		// does not impose one, this never returns.
		o.SearchForLinking(context.Background(), "x", []ProviderName{NameDiscogs})
	}()

	// Generous margin over the real ceiling so this is not a timing flake: it
	// fails only if NO self-imposed deadline exists at all.
	limit := 5 * time.Second
	select {
	case <-done:
	case <-time.After(limit):
		t.Fatalf("SearchForLinking did not return within %v with a deadline-free caller context: no per-provider deadline is being imposed", limit)
	}

	select {
	case <-released:
	default:
		t.Error("the provider context was never canceled: the deadline is not wired to the provider call")
	}
}

// TestSearchForLinking_TimeoutDoesNotSignalAIMD covers the subtle half of the
// fix. A deadline firing during DoWithRetry's backoff surfaces wrapped as
// ErrProviderUnavailable, which IsRateLimitError accepts -- so without an
// explicit exclusion the orchestrator reads its OWN impatience as evidence the
// provider is rate-limiting us, and throttles a healthy provider on the next
// search. A genuine rate-limit error must still signal.
func TestSearchForLinking_TimeoutDoesNotSignalAIMD(t *testing.T) {
	t.Parallel()

	// The probe is whether AIMD is SIGNALED, not whether the rate limit
	// visibly drops. A first RecordFailure can never lower the limit for any
	// provider: currentLimit starts at the provider default and the decrease
	// is floored at that same default, so the multiplicative decrease clamps
	// straight back. Only a limit previously ratcheted ABOVE the default by
	// RecordSuccess can be seen to fall. Asserting on the limit therefore
	// passes no matter what the orchestrator does, which is the trap this
	// comment exists to stop the next reader falling into.
	//
	// So both subtests drive AIMD to a raised limit first, which makes a
	// subsequent decrease observable.
	const prov = NameDeezer

	raise := func(t *testing.T, aimd *AIMDController) rate.Limit {
		t.Helper()
		// Enough successes to ratchet the limit above its default.
		for i := 0; i < 50; i++ {
			aimd.RecordSuccess(prov)
		}
		got := aimd.GetCurrentLimit(prov)
		if got <= defaultRateLimits[prov] {
			t.Fatalf("fixture defect: limit %v is not above the default %v, so a later decrease would be invisible",
				got, defaultRateLimits[prov])
		}
		return got
	}

	t.Run("our own deadline does not record a failure", func(t *testing.T) {
		t.Parallel()
		aimd := newTestAIMD(newFakeClock(time.Now()))
		before := raise(t, aimd)

		hung := &mockProvider{
			name: prov,
			searchFn: func(ctx context.Context, _ string) ([]ArtistSearchResult, error) {
				<-ctx.Done()
				// Mirror the real wrapping: a deadline that fires inside
				// DoWithRetry's backoff comes back as provider-unavailable,
				// which IsRateLimitError accepts.
				return nil, &ErrProviderUnavailable{Provider: prov, Cause: ctx.Err()}
			},
		}
		o := newTimeoutTestOrchestrator(t, aimd, hung)
		// The caller context must have NO deadline, so the ONLY thing that can
		// end the provider call is the orchestrator's own ceiling. An earlier
		// version passed a 100ms caller deadline, which made ctx.Err() non-nil
		// and let the callerGone clause carry the assertion -- so deleting the
		// ourDeadline check entirely still passed. Verified: with a caller
		// deadline the mutation is NOT caught; with this shape it is.
		o.searchTimeout = 150 * time.Millisecond

		_, statuses, err := o.SearchForLinking(context.Background(), "x", []ProviderName{prov})
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if len(statuses) != 1 || !statuses[0].Errored {
			t.Fatalf("statuses = %+v, want one errored status", statuses)
		}
		if got := aimd.GetCurrentLimit(prov); got != before {
			t.Errorf("AIMD limit moved %v -> %v on our OWN deadline: the orchestrator throttled a provider for its own impatience",
				before, got)
		}
	})

	t.Run("a genuine rate limit still records a failure", func(t *testing.T) {
		t.Parallel()
		aimd := newTestAIMD(newFakeClock(time.Now()))
		before := raise(t, aimd)

		limited := &mockProvider{
			name: prov,
			searchFn: func(_ context.Context, _ string) ([]ArtistSearchResult, error) {
				return nil, &ErrProviderUnavailable{Provider: prov, Cause: errors.New("429 too many requests")}
			},
		}
		o := newTimeoutTestOrchestrator(t, aimd, limited)

		// No caller deadline, so nothing here is our own timeout.
		_, statuses, err := o.SearchForLinking(context.Background(), "x", []ProviderName{prov})
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if len(statuses) != 1 || !statuses[0].Errored {
			t.Fatalf("statuses = %+v, want one errored status", statuses)
		}
		if got := aimd.GetCurrentLimit(prov); got >= before {
			t.Errorf("AIMD limit %v -> %v on a REAL rate limit: the timeout exclusion is too broad and now swallows genuine signals",
				before, got)
		}
	})
}

// TestSearchForLinking_PanicIsContained pins that one misbehaving adapter
// cannot take the process down. This runs on the request path, so an
// unrecovered panic in a provider goroutine would kill every in-flight request,
// not just this search.
func TestSearchForLinking_PanicIsContained(t *testing.T) {
	t.Parallel()

	panicky := &mockProvider{
		name: NameDiscogs,
		searchFn: func(_ context.Context, _ string) ([]ArtistSearchResult, error) {
			panic("provider adapter exploded")
		},
	}
	healthy := &mockProvider{
		name: NameMusicBrainz,
		searchFn: func(_ context.Context, _ string) ([]ArtistSearchResult, error) {
			return []ArtistSearchResult{{Name: "Survivor", MusicBrainzID: "mbid-1"}}, nil
		},
	}

	o := newTimeoutTestOrchestrator(t, nil, healthy, panicky)

	results, statuses, err := o.SearchForLinking(context.Background(), "x",
		[]ProviderName{NameMusicBrainz, NameDiscogs})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	// The healthy provider's result must survive its sibling's panic.
	if len(results) != 1 || results[0].MusicBrainzID != "mbid-1" {
		t.Errorf("results = %+v, want the healthy provider's match", results)
	}
	if len(statuses) != 2 {
		t.Fatalf("statuses = %d, want 2 (a panicking provider still occupies its slot)", len(statuses))
	}
	if statuses[0].Provider != NameMusicBrainz || statuses[1].Provider != NameDiscogs {
		t.Errorf("status order = %v, %v; want MusicBrainz, Discogs", statuses[0].Provider, statuses[1].Provider)
	}
	// The panicking provider must report as ERRORED under its own name. A
	// regression leaving the slot zero-valued still satisfies the count and
	// order checks above, but renders an unnamed provider that quietly matched
	// nothing -- which is exactly what the failed-provider banner cannot show.
	if statuses[0].Errored {
		t.Errorf("healthy provider reported errored: %+v", statuses[0])
	}
	if !statuses[1].Errored {
		t.Error("the panicking provider must report as errored, not as an empty success")
	}
	if statuses[1].ScrubbedMessage == "" {
		t.Error("the panicking provider's status carries no message for the banner to show")
	}
}

// TestSearchForLinking_ConcurrentWritesAreRaceFree exercises the fan-out with
// enough providers that a shared-slice append would be caught by -race. Each
// provider returns a distinct result so the flattening order can be asserted
// too: the per-index slots must reassemble in input order.
func TestSearchForLinking_ConcurrentWritesAreRaceFree(t *testing.T) {
	t.Parallel()

	names := []ProviderName{NameMusicBrainz, NameDiscogs, NameAudioDB, NameDeezer}
	provs := make([]Provider, 0, len(names))
	var calls atomic.Int32

	for _, n := range names {
		n := n
		provs = append(provs, &mockProvider{
			name: n,
			searchFn: func(_ context.Context, _ string) ([]ArtistSearchResult, error) {
				calls.Add(1)
				return []ArtistSearchResult{{Name: string(n)}}, nil
			},
		})
	}

	o := newTimeoutTestOrchestrator(t, nil, provs...)
	results, statuses, err := o.SearchForLinking(context.Background(), "x", names)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got := calls.Load(); got != int32(len(names)) {
		t.Errorf("provider calls = %d, want %d", got, len(names))
	}
	if len(results) != len(names) {
		t.Fatalf("results = %d, want %d", len(results), len(names))
	}
	for i, n := range names {
		if statuses[i].Provider != n {
			t.Errorf("statuses[%d] = %v, want %v", i, statuses[i].Provider, n)
		}
		if results[i].Name != string(n) {
			t.Errorf("results[%d] = %q, want %q (flattening must preserve input order)", i, results[i].Name, string(n))
		}
	}
}
