package provider

import (
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Clock abstracts the two time operations DoWithRetry needs: reading "now"
// (to turn an HTTP-date Retry-After into a duration) and sleeping in a way that
// respects context cancellation. Production code uses the real clock; tests
// inject a stub that records sleeps instead of actually waiting, which makes the
// backoff schedule assertable without real delays.
type Clock interface {
	Now() time.Time
	// Sleep blocks for d or until ctx is done, whichever comes first. It
	// returns ctx.Err() if the context is canceled or its deadline is exceeded
	// before d elapses, and nil otherwise.
	Sleep(ctx context.Context, d time.Duration) error
}

// realClock is the production Clock backed by the standard library.
type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		// Nothing to wait for, but still honor an already-canceled context so
		// callers see cancellation promptly rather than silently proceeding.
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// SystemClock returns the production Clock. Provider adapters pass this to
// DoWithRetry; tests pass a stub implementation instead.
func SystemClock() Clock { return realClock{} }

// RetryPolicy configures how DoWithRetry reacts to rate-limit (429) and
// service-unavailable (503) responses. A single policy governs both, but 503 is
// always handled conservatively (see DoWithRetry): an unhealthy server is
// unlikely to recover within a short window and retrying it aggressively only
// risks piling on.
type RetryPolicy struct {
	// MaxAttempts is the total number of HTTP attempts allowed for a 429
	// response, including the first. Must be >= 1; a value of 1 disables
	// retrying entirely.
	MaxAttempts int
	// MaxAttempts503 is the total number of attempts allowed for a 503 response,
	// including the first. It is kept lower than MaxAttempts because a 503 may
	// mean the server is genuinely unhealthy (not merely throttling), so we
	// retry more cautiously. Must be >= 1. Some providers (notably MusicBrainz)
	// signal rate limiting with a bare 503 and no Retry-After, so a bounded
	// jittered retry here lets those calls recover rather than fail outright.
	MaxAttempts503 int
	// BaseDelay is the starting delay for the exponential-backoff fallback used
	// when a 429 or 503 carries no Retry-After header.
	BaseDelay time.Duration
	// MaxDelay caps any single wait, whether it came from a Retry-After header
	// or from the exponential fallback. It bounds how long one rate-limited
	// call can stall.
	MaxDelay time.Duration
}

// DefaultRetryPolicy is the standard policy applied by provider adapters:
// up to three attempts for a 429 and two for a 503, starting at a 1-second
// backoff and capped at 30 seconds per wait.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxAttempts:    3,
		MaxAttempts503: 2,
		BaseDelay:      1 * time.Second,
		MaxDelay:       30 * time.Second,
	}
}

// DoWithRetry executes do, retrying on rate-limit (429) and, conservatively,
// service-unavailable (503) responses according to policy. It returns the first
// response whose status is neither 429 nor 503 (success or otherwise) for the
// caller to handle with its own status switch; the caller owns closing that
// response body. On a 429 it honors the server's Retry-After header (both the
// delta-seconds and HTTP-date forms) and otherwise falls back to full-jitter
// exponential backoff. All waits respect ctx cancellation via clk.Sleep.
//
// When retries are exhausted (or a 503 is not retried), it returns a typed
// *ErrProviderUnavailable whose RetryAfter carries the last server-advised
// backoff, so upstream logging and a future adaptive limiter can observe it.
// Transport-level errors from do (e.g. an unreachable host) are returned as-is
// and never retried: backoff is for rate limiting, not for connectivity faults.
//
// do must NOT close the response body; DoWithRetry drains and closes any
// response it decides to discard before retrying.
func DoWithRetry(
	ctx context.Context,
	clk Clock,
	name ProviderName,
	policy RetryPolicy,
	do func(ctx context.Context) (*http.Response, error),
) (*http.Response, error) {
	if clk == nil {
		clk = realClock{}
	}
	if policy.MaxAttempts < 1 {
		policy.MaxAttempts = 1
	}
	if policy.MaxAttempts503 < 1 {
		policy.MaxAttempts503 = 1
	}

	var lastRetryAfter time.Duration
	for attempt := 0; ; attempt++ {
		resp, err := do(ctx)
		if err != nil {
			// Transport/setup error (or a limiter error already wrapped by the
			// closure). Not retryable here; hand it straight back.
			return nil, err
		}

		status := resp.StatusCode
		if status != http.StatusTooManyRequests && status != http.StatusServiceUnavailable {
			// Success or a definitive non-retryable status: the caller's own
			// switch handles it (and closes the body).
			return resp, nil
		}

		retryAfter, hasHeader := parseRetryAfter(resp.Header.Get("Retry-After"), clk.Now())
		lastRetryAfter = retryAfter
		// We are discarding this response; drain a little and close so the
		// connection can be reused.
		drainAndClose(resp)

		wait, retry := nextWait(policy, status, attempt, retryAfter, hasHeader)
		if !retry {
			return nil, &ErrProviderUnavailable{
				Provider:   name,
				Cause:      fmt.Errorf("HTTP %d (no retry remaining after %d attempt(s))", status, attempt+1),
				RetryAfter: lastRetryAfter,
			}
		}

		if err := clk.Sleep(ctx, wait); err != nil {
			// Context canceled or deadline exceeded during the backoff wait.
			return nil, &ErrProviderUnavailable{
				Provider:   name,
				Cause:      err,
				RetryAfter: lastRetryAfter,
			}
		}
	}
}

// nextWait decides whether DoWithRetry should retry after a rate-limit /
// unavailable response, and if so how long to wait. attempt is zero-based (0 is
// the first request just made).
func nextWait(policy RetryPolicy, status, attempt int, retryAfter time.Duration, hasHeader bool) (time.Duration, bool) {
	switch status {
	case http.StatusServiceUnavailable:
		// Conservative but not give-up: 503 gets fewer attempts than 429
		// (MaxAttempts503) because the server may be genuinely unhealthy rather
		// than merely throttling. We still back off and retry within that bound,
		// honoring Retry-After when present and otherwise using a jittered
		// exponential delay. The jitter is what keeps a fleet from re-firing in
		// lockstep against a struggling host. This is the path MusicBrainz takes:
		// it signals rate limiting with a bare 503 and no Retry-After, so the
		// jittered fallback lets those calls recover instead of failing outright.
		if attempt+1 >= policy.MaxAttempts503 {
			return 0, false
		}
		if hasHeader {
			return capDelay(retryAfter, policy.MaxDelay), true
		}
		return jitteredBackoff(policy.BaseDelay, policy.MaxDelay, attempt), true

	case http.StatusTooManyRequests:
		// attempt is zero-based, so attempt+1 is the number of requests made so
		// far. Stop once that reaches MaxAttempts.
		if attempt+1 >= policy.MaxAttempts {
			return 0, false
		}
		if hasHeader {
			return capDelay(retryAfter, policy.MaxDelay), true
		}
		return jitteredBackoff(policy.BaseDelay, policy.MaxDelay, attempt), true

	default:
		return 0, false
	}
}

// capDelay clamps d to [0, maxDelay].
func capDelay(d, maxDelay time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	if maxDelay > 0 && d > maxDelay {
		return maxDelay
	}
	return d
}

// jitteredBackoff returns a full-jitter exponential backoff for the given
// zero-based attempt: a uniformly random duration in [0, min(maxDelay,
// base*2^attempt)]. Full jitter (random across the whole interval rather than a
// fixed delay plus a small random nudge) spreads out clients that all hit a
// rate limit at the same moment, which is what prevents a retry storm when many
// workers back off together.
func jitteredBackoff(base, maxDelay time.Duration, attempt int) time.Duration {
	ceiling := base << attempt
	// Guard against overflow: a large attempt count can shift ceiling past the
	// int64 sign bit and flip it negative (two's complement wraparound), or it
	// can simply exceed maxDelay. Either way, clamp to maxDelay.
	if ceiling <= 0 || (maxDelay > 0 && ceiling > maxDelay) {
		ceiling = maxDelay
	}
	if ceiling <= 0 {
		return 0
	}
	// rand.Int64N panics on n <= 0; +1 makes the upper bound inclusive.
	return time.Duration(rand.Int64N(int64(ceiling) + 1)) //nolint:gosec // jitter does not need a cryptographic RNG
}

// parseRetryAfter interprets an HTTP Retry-After header value. Per RFC 9110 it
// may be either a number of seconds (delta-seconds, e.g. "120") or an HTTP-date
// (e.g. "Wed, 21 Oct 2015 07:28:00 GMT"). now is supplied (rather than read from
// the real clock) so the HTTP-date branch is deterministic under test. The
// returned bool reports whether a usable value was parsed; a negative delta or a
// date already in the past is floored to zero.
func parseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(value); err == nil {
		if secs < 0 {
			secs = 0
		}
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(value); err == nil {
		d := t.Sub(now)
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, false
}

// drainAndClose discards a bounded amount of a discarded response body and
// closes it, so the underlying connection can be returned to the pool. The cap
// keeps a misbehaving server from making us read an unbounded error body.
func drainAndClose(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4*1024))
	_ = resp.Body.Close()
}
