package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// stubClock is a deterministic Clock for tests. It never actually sleeps;
// instead it records the durations it was asked to wait so a test can assert
// the exact backoff schedule. sleepHook, when set, lets a test simulate
// context cancellation (or any other behavior) part-way through a wait.
//
// stubClock is used only from single-goroutine tests, so it needs no locking.
type stubClock struct {
	now       time.Time
	sleeps    []time.Duration
	sleepHook func(ctx context.Context, d time.Duration) error
}

func (c *stubClock) Now() time.Time { return c.now }

func (c *stubClock) Sleep(ctx context.Context, d time.Duration) error {
	c.sleeps = append(c.sleeps, d)
	if c.sleepHook != nil {
		return c.sleepHook(ctx, d)
	}
	return nil
}

// httpGet returns a do-closure suitable for DoWithRetry that issues a GET to
// url. It deliberately does NOT close the response body: DoWithRetry owns the
// lifetime of any response it decides to discard, and the caller closes the
// final one.
func httpGet(url string) func(context.Context) (*http.Response, error) {
	return func(ctx context.Context) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
		if err != nil {
			return nil, err
		}
		return http.DefaultClient.Do(req)
	}
}

// doRetry runs DoWithRetry against a test server URL. It isolates the single
// call that bodyclose cannot trace (the response originates inside the do-closure
// and flows back through the DoWithRetry wrapper); callers receive a plain
// response they close normally.
func doRetry(ctx context.Context, clk Clock, policy RetryPolicy, url string) (*http.Response, error) {
	return DoWithRetry(ctx, clk, NameMusicBrainz, policy, httpGet(url))
}

func TestParseRetryAfter(t *testing.T) {
	// A fixed "now" so HTTP-date math is deterministic.
	now := time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name      string
		header    string
		wantDelay time.Duration
		wantOK    bool
	}{
		{"empty", "", 0, false},
		{"delta seconds", "120", 120 * time.Second, true},
		{"zero seconds", "0", 0, true},
		{"negative seconds floored", "-5", 0, true},
		{"whitespace trimmed", "  30  ", 30 * time.Second, true},
		{"http-date future", now.Add(45 * time.Second).UTC().Format(http.TimeFormat), 45 * time.Second, true},
		{"http-date past floored", now.Add(-45 * time.Second).UTC().Format(http.TimeFormat), 0, true},
		{"garbage", "soon", 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotDelay, gotOK := parseRetryAfter(tt.header, now)
			if gotOK != tt.wantOK {
				t.Fatalf("ok = %v, want %v", gotOK, tt.wantOK)
			}
			if gotDelay != tt.wantDelay {
				t.Fatalf("delay = %v, want %v", gotDelay, tt.wantDelay)
			}
		})
	}
}

func TestDoWithRetrySuccessPassthrough(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	clk := &stubClock{now: time.Now()}
	policy := RetryPolicy{MaxAttempts: 3, BaseDelay: time.Second, MaxDelay: 30 * time.Second}

	resp, err := doRetry(context.Background(), clk, policy, srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("hits = %d, want 1", got)
	}
	if len(clk.sleeps) != 0 {
		t.Fatalf("sleeps = %v, want none", clk.sleeps)
	}
}

func TestDoWithRetry429HonorsRetryAfterDelta(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// First call is rate-limited with an explicit 2-second backoff,
		// the second call succeeds.
		if hits.Add(1) == 1 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	clk := &stubClock{now: time.Now()}
	policy := RetryPolicy{MaxAttempts: 3, BaseDelay: time.Second, MaxDelay: 30 * time.Second}

	resp, err := doRetry(context.Background(), clk, policy, srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("hits = %d, want 2", got)
	}
	if len(clk.sleeps) != 1 || clk.sleeps[0] != 2*time.Second {
		t.Fatalf("sleeps = %v, want [2s]", clk.sleeps)
	}
}

func TestDoWithRetry429HonorsRetryAfterHTTPDate(t *testing.T) {
	// Fixed clock so the HTTP-date delta is exact.
	now := time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC)
	retryAt := now.Add(7 * time.Second).UTC().Format(http.TimeFormat)

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.Header().Set("Retry-After", retryAt)
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	clk := &stubClock{now: now}
	policy := RetryPolicy{MaxAttempts: 3, BaseDelay: time.Second, MaxDelay: 30 * time.Second}

	resp, err := doRetry(context.Background(), clk, policy, srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if len(clk.sleeps) != 1 || clk.sleeps[0] != 7*time.Second {
		t.Fatalf("sleeps = %v, want [7s]", clk.sleeps)
	}
}

func TestDoWithRetry429JitterBounds(t *testing.T) {
	// No Retry-After header: backoff falls back to full-jitter exponential.
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Rate-limit the first two calls, succeed on the third.
		if hits.Add(1) <= 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	base := 100 * time.Millisecond
	maxDelay := 10 * time.Second
	clk := &stubClock{now: time.Now()}
	policy := RetryPolicy{MaxAttempts: 3, BaseDelay: base, MaxDelay: maxDelay}

	resp, err := doRetry(context.Background(), clk, policy, srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if len(clk.sleeps) != 2 {
		t.Fatalf("expected 2 sleeps, got %v", clk.sleeps)
	}
	// Full jitter: attempt n waits a random duration in [0, base*2^n], capped.
	for n, got := range clk.sleeps {
		ceiling := base << n // 100ms, then 200ms
		if ceiling > maxDelay {
			ceiling = maxDelay
		}
		if got < 0 || got > ceiling {
			t.Fatalf("sleep[%d] = %v, want within [0, %v]", n, got, ceiling)
		}
	}
}

func TestDoWithRetry503RetriesConservatively(t *testing.T) {
	// 503 without a Retry-After header (the MusicBrainz throttling case) still
	// backs off and retries, but more cautiously than a 429: it is bounded by
	// MaxAttempts503 and uses a jittered fallback delay. This proves the call
	// recovers-or-fails within that smaller bound rather than giving up on the
	// first 503.
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	clk := &stubClock{now: time.Now()}
	policy := RetryPolicy{MaxAttempts: 3, MaxAttempts503: 2, BaseDelay: time.Second, MaxDelay: 30 * time.Second}

	resp, err := doRetry(context.Background(), clk, policy, srv.URL)
	if resp != nil {
		_ = resp.Body.Close()
		t.Fatalf("expected nil response on failure")
	}
	var unavailable *ErrProviderUnavailable
	if !errors.As(err, &unavailable) {
		t.Fatalf("error = %v, want *ErrProviderUnavailable", err)
	}
	// MaxAttempts503 = 2: the server is hit twice (one initial, one retry), and
	// that is strictly fewer than the 429 budget (3).
	if got := hits.Load(); got != 2 {
		t.Fatalf("hits = %d, want 2 (MaxAttempts503)", got)
	}
	// One jittered backoff between the two attempts, bounded by MaxDelay.
	if len(clk.sleeps) != 1 {
		t.Fatalf("sleeps = %v, want exactly 1", clk.sleeps)
	}
	if clk.sleeps[0] < 0 || clk.sleeps[0] > policy.MaxDelay {
		t.Fatalf("sleep = %v, want within [0, %v]", clk.sleeps[0], policy.MaxDelay)
	}
}

func TestDoWithRetry503FailsFastWhenMaxAttempts503IsOne(t *testing.T) {
	// With MaxAttempts503 = 1, a 503 is not retried at all: the strictly
	// conservative "treat the server as down" stance remains available.
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	clk := &stubClock{now: time.Now()}
	policy := RetryPolicy{MaxAttempts: 3, MaxAttempts503: 1, BaseDelay: time.Second, MaxDelay: 30 * time.Second}

	resp, err := doRetry(context.Background(), clk, policy, srv.URL)
	if resp != nil {
		_ = resp.Body.Close()
	}
	var unavailable *ErrProviderUnavailable
	if !errors.As(err, &unavailable) {
		t.Fatalf("error = %v, want *ErrProviderUnavailable", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("hits = %d, want 1 (no retry)", got)
	}
	if len(clk.sleeps) != 0 {
		t.Fatalf("sleeps = %v, want none", clk.sleeps)
	}
}

func TestDoWithRetry503SingleRetryWithRetryAfter(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	clk := &stubClock{now: time.Now()}
	policy := RetryPolicy{MaxAttempts: 3, MaxAttempts503: 2, BaseDelay: time.Second, MaxDelay: 30 * time.Second}

	resp, err := doRetry(context.Background(), clk, policy, srv.URL)
	if resp != nil {
		_ = resp.Body.Close()
	}
	var unavailable *ErrProviderUnavailable
	if !errors.As(err, &unavailable) {
		t.Fatalf("error = %v, want *ErrProviderUnavailable", err)
	}
	// 503 WITH a Retry-After header honors it for exactly one retry (the 503
	// budget is 2 attempts), so the header wait is used rather than jitter.
	if got := hits.Load(); got != 2 {
		t.Fatalf("hits = %d, want 2", got)
	}
	if len(clk.sleeps) != 1 || clk.sleeps[0] != 3*time.Second {
		t.Fatalf("sleeps = %v, want [3s]", clk.sleeps)
	}
	if unavailable.RetryAfter != 3*time.Second {
		t.Fatalf("RetryAfter = %v, want 3s", unavailable.RetryAfter)
	}
}

func TestDoWithRetryNoRetryStorm(t *testing.T) {
	// A server that always rate-limits must be hit exactly MaxAttempts times,
	// never more: bounded attempts prevent a retry storm.
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Retry-After", "0") // avoid any real delay even on a real clock
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	clk := &stubClock{now: time.Now()}
	const maxAttempts = 3
	policy := RetryPolicy{MaxAttempts: maxAttempts, BaseDelay: time.Second, MaxDelay: 30 * time.Second}

	resp, err := doRetry(context.Background(), clk, policy, srv.URL)
	if resp != nil {
		_ = resp.Body.Close()
		t.Fatalf("expected nil response on exhaustion")
	}
	var unavailable *ErrProviderUnavailable
	if !errors.As(err, &unavailable) {
		t.Fatalf("error = %v, want *ErrProviderUnavailable", err)
	}
	if got := hits.Load(); got != maxAttempts {
		t.Fatalf("hits = %d, want %d", got, maxAttempts)
	}
	// One sleep between each of the (maxAttempts) requests except the last.
	if len(clk.sleeps) != maxAttempts-1 {
		t.Fatalf("sleeps = %v, want %d", clk.sleeps, maxAttempts-1)
	}
}

func TestDoWithRetryExhaustionPopulatesRetryAfter(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Retry-After", "9")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	clk := &stubClock{now: time.Now()}
	policy := RetryPolicy{MaxAttempts: 2, BaseDelay: time.Second, MaxDelay: 30 * time.Second}

	resp, err := doRetry(context.Background(), clk, policy, srv.URL)
	if resp != nil {
		_ = resp.Body.Close()
	}
	var unavailable *ErrProviderUnavailable
	if !errors.As(err, &unavailable) {
		t.Fatalf("error = %v, want *ErrProviderUnavailable", err)
	}
	// The previously-dead RetryAfter field is now populated from the last header.
	if unavailable.RetryAfter != 9*time.Second {
		t.Fatalf("RetryAfter = %v, want 9s", unavailable.RetryAfter)
	}
	if unavailable.Provider != NameMusicBrainz {
		t.Fatalf("Provider = %v, want %v", unavailable.Provider, NameMusicBrainz)
	}
}

func TestDoWithRetryRespectsContextCancellation(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Simulate the context being canceled while we are waiting to retry.
	clk := &stubClock{
		now: time.Now(),
		sleepHook: func(ctx context.Context, d time.Duration) error {
			cancel()
			return ctx.Err()
		},
	}
	policy := RetryPolicy{MaxAttempts: 3, BaseDelay: time.Second, MaxDelay: 30 * time.Second}

	resp, err := doRetry(ctx, clk, policy, srv.URL)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want wrapped context.Canceled", err)
	}
	var unavailable *ErrProviderUnavailable
	if !errors.As(err, &unavailable) {
		t.Fatalf("error = %v, want *ErrProviderUnavailable", err)
	}
	// Cancellation during the wait means the second request never fires.
	if got := hits.Load(); got != 1 {
		t.Fatalf("hits = %d, want 1", got)
	}
}

func TestDoWithRetryTransportErrorNotRetried(t *testing.T) {
	// A connection-level failure (server already closed) is returned as-is and
	// is not retried: backoff is for rate limiting, not for unreachable hosts.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()

	clk := &stubClock{now: time.Now()}
	policy := RetryPolicy{MaxAttempts: 3, BaseDelay: time.Second, MaxDelay: 30 * time.Second}

	resp, err := doRetry(context.Background(), clk, policy, url)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("expected a transport error")
	}
	if len(clk.sleeps) != 0 {
		t.Fatalf("sleeps = %v, want none (transport errors are not retried)", clk.sleeps)
	}
}

func TestSystemClockSleepRespectsContext(t *testing.T) {
	clk := SystemClock()

	// A zero/negative duration returns immediately with no error.
	if err := clk.Sleep(context.Background(), 0); err != nil {
		t.Fatalf("Sleep(0) = %v, want nil", err)
	}

	// An already-canceled context returns its error rather than waiting.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := clk.Sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("Sleep with canceled ctx = %v, want context.Canceled", err)
	}
}

func TestSystemClockNowAndSleepCompletes(t *testing.T) {
	clk := SystemClock()

	// Now returns a sane, non-zero time.
	if clk.Now().IsZero() {
		t.Fatal("Now() returned the zero time")
	}

	// A short sleep that is not interrupted returns nil (covers the timer path).
	if err := clk.Sleep(context.Background(), time.Millisecond); err != nil {
		t.Fatalf("Sleep(1ms) = %v, want nil", err)
	}
}

func TestDoWithRetryCapsRetryAfterAtMaxDelay(t *testing.T) {
	// A Retry-After far larger than MaxDelay must be clamped to MaxDelay so a
	// hostile or misconfigured server cannot stall a call indefinitely.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3600") // 1 hour
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	clk := &stubClock{now: time.Now()}
	policy := RetryPolicy{MaxAttempts: 2, BaseDelay: time.Second, MaxDelay: 5 * time.Second}

	resp, err := doRetry(context.Background(), clk, policy, srv.URL)
	if resp != nil {
		_ = resp.Body.Close()
	}
	var unavailable *ErrProviderUnavailable
	if !errors.As(err, &unavailable) {
		t.Fatalf("error = %v, want *ErrProviderUnavailable", err)
	}
	if len(clk.sleeps) != 1 || clk.sleeps[0] != 5*time.Second {
		t.Fatalf("sleeps = %v, want [5s] (capped at MaxDelay)", clk.sleeps)
	}
	// The surfaced RetryAfter reflects the raw header, not the capped wait.
	if unavailable.RetryAfter != 3600*time.Second {
		t.Fatalf("RetryAfter = %v, want 3600s", unavailable.RetryAfter)
	}
}

func TestDoWithRetryNoHeaderBackoffCappedAtMaxDelay(t *testing.T) {
	// With no Retry-After header and a MaxDelay below the exponential ceiling,
	// the jittered backoff is bounded by MaxDelay.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	clk := &stubClock{now: time.Now()}
	// BaseDelay (1s) already exceeds MaxDelay (10ms), forcing the cap branch.
	policy := RetryPolicy{MaxAttempts: 2, BaseDelay: time.Second, MaxDelay: 10 * time.Millisecond}

	resp, err := doRetry(context.Background(), clk, policy, srv.URL)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("expected an error after exhausting retries")
	}
	if len(clk.sleeps) != 1 {
		t.Fatalf("expected 1 sleep, got %v", clk.sleeps)
	}
	if clk.sleeps[0] < 0 || clk.sleeps[0] > 10*time.Millisecond {
		t.Fatalf("sleep = %v, want within [0, 10ms]", clk.sleeps[0])
	}
}

func TestDoWithRetryPassesThroughNon200(t *testing.T) {
	// A non-retryable error status (e.g. 500) is handed back to the caller
	// untouched for its own handling, not retried.
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	clk := &stubClock{now: time.Now()}
	policy := RetryPolicy{MaxAttempts: 3, BaseDelay: time.Second, MaxDelay: 30 * time.Second}

	resp, err := doRetry(context.Background(), clk, policy, srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("hits = %d, want 1 (500 is not retried)", got)
	}
}

func TestRetryAfterAttr(t *testing.T) {
	// A provider-unavailable error carrying a Retry-After yields a duration attr.
	attr := retryAfterAttr(&ErrProviderUnavailable{Provider: NameMusicBrainz, RetryAfter: 5 * time.Second})
	if attr.Key != "retry_after" {
		t.Fatalf("key = %q, want retry_after", attr.Key)
	}
	if attr.Value.Duration() != 5*time.Second {
		t.Fatalf("value = %v, want 5s", attr.Value.Duration())
	}

	// A wrapped provider-unavailable error is still detected through the chain.
	wrapped := fmt.Errorf("context: %w", &ErrProviderUnavailable{RetryAfter: 2 * time.Second})
	if got := retryAfterAttr(wrapped); got.Value.Duration() != 2*time.Second {
		t.Fatalf("wrapped value = %v, want 2s", got.Value.Duration())
	}

	// No Retry-After, and a non-provider error, both yield an elided (empty) attr.
	if got := retryAfterAttr(&ErrProviderUnavailable{Provider: NameMusicBrainz}); !got.Equal(slog.Attr{}) {
		t.Fatalf("expected empty attr for zero RetryAfter, got %+v", got)
	}
	if got := retryAfterAttr(errors.New("boom")); !got.Equal(slog.Attr{}) {
		t.Fatalf("expected empty attr for non-provider error, got %+v", got)
	}
}

func TestDefaultRetryPolicy(t *testing.T) {
	p := DefaultRetryPolicy()
	if p.MaxAttempts < 2 {
		t.Fatalf("MaxAttempts = %d, want >= 2 so 429s are actually retried", p.MaxAttempts)
	}
	if p.BaseDelay <= 0 || p.MaxDelay <= 0 {
		t.Fatalf("delays must be positive: %+v", p)
	}
	if p.MaxDelay < p.BaseDelay {
		t.Fatalf("MaxDelay (%v) must be >= BaseDelay (%v)", p.MaxDelay, p.BaseDelay)
	}
}
