package provider

import (
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
)

// ErrInjectedFailure is returned by provider methods when the fault-injection
// hook is active for that provider. The message is intentionally explicit so
// that any developer who sees it in logs immediately recognizes this as a
// self-inflicted test condition, not a real provider outage.
var ErrInjectedFailure = errors.New("provider call rejected by injection (SW_FORCE_PROVIDER_ERROR)")

// injectionMarker is the log message emitted at Info level every time
// ShouldInjectFailure returns true. The smoke harness greps the server log
// for this exact substring to assert that injection was actually exercised
// by the surfaces it drove (issue #1697). The string is intentionally
// distinctive so a fuzzy match cannot collide with unrelated log lines.
const injectionMarker = "provider injection hook fired"

// injectedSet is the set of provider names that should return ErrInjectedFailure
// on every outbound call. Initialized from SW_FORCE_PROVIDER_ERROR at process
// start and may be replaced at any time via SetInjectedProviders. Guarded by
// injectedMu because provider methods (readers) are invoked concurrently from
// net/http handlers, while tests may swap the set via SetInjectedProviders.
var (
	injectedMu  sync.RWMutex
	injectedSet = parseInjectedSet(os.Getenv("SW_FORCE_PROVIDER_ERROR"))

	// injectedFiredCount counts the number of times ShouldInjectFailure
	// returned true. Exposed via injectedFailureCount so test harnesses in
	// this package can assert the injection hook was actually exercised
	// (issue #1697). An atomic int64 avoids contending with injectedMu on
	// the hot path.
	injectedFiredCount atomic.Int64
)

// SetInjectedProviders replaces the active injected-failure set.
// Pass nil or an empty slice to clear all injected failures.
//
// This is exported so that test binaries in sibling packages can activate
// injection without relying on the env var (which is parsed once at init
// time). Production code must never call this; use SW_FORCE_PROVIDER_ERROR
// to configure injection at process start instead.
//
// Typical usage in a test:
//
//	provider.SetInjectedProviders([]string{"musicbrainz", "fanarttv"})
//	t.Cleanup(func() { provider.SetInjectedProviders(nil) })
func SetInjectedProviders(names []string) {
	injectedMu.Lock()
	defer injectedMu.Unlock()
	if len(names) == 0 {
		injectedSet = map[string]struct{}{}
		return
	}
	m := make(map[string]struct{}, len(names))
	for _, n := range names {
		key := strings.ToLower(strings.TrimSpace(n))
		if key != "" {
			m[key] = struct{}{}
		}
	}
	injectedSet = m
}

// ShouldInjectFailure reports whether calls from the named provider should
// return ErrInjectedFailure instead of making real network requests.
//
// Usage: at the top of every provider outbound method, add:
//
//	if provider.ShouldInjectFailure(p.Name()) {
//	    return nil, provider.ErrInjectedFailure
//	}
//
// The check is a no-op (returns false) when SW_FORCE_PROVIDER_ERROR is unset
// and SetInjectedProviders has not been called, so normal production behavior
// is preserved with zero overhead. On a true return the function also
// increments an atomic counter (see injectedFailureCount) and emits a single
// Info-level log line carrying the provider name, so smoke harnesses can
// assert the hook was actually reached by the surfaces they drive.
func ShouldInjectFailure(name ProviderName) bool {
	injectedMu.RLock()
	if len(injectedSet) == 0 {
		injectedMu.RUnlock()
		return false
	}
	_, ok := injectedSet[strings.ToLower(string(name))]
	injectedMu.RUnlock()
	if ok {
		injectedFiredCount.Add(1)
		slog.Info(injectionMarker, slog.String("provider", string(name)))
	}
	return ok
}

// injectedFailureCount returns the number of times ShouldInjectFailure has
// returned true since process start (or since the last resetInjectedFailureCount
// call). Unexported so only same-package test code can read it; exporting it
// would let unrelated production callers depend on a counter intended only
// for the smoke harness (issue #1697 finding). The counter is process-global
// and atomic; concurrent provider calls cannot lose increments.
func injectedFailureCount() int64 {
	return injectedFiredCount.Load()
}

// resetInjectedFailureCount zeroes the counter. Intended for tests that want
// to assert "this surface incremented the counter from N to N+M". Unexported
// for the same reason as injectedFailureCount.
func resetInjectedFailureCount() {
	injectedFiredCount.Store(0)
}

// parseInjectedSet parses the SW_FORCE_PROVIDER_ERROR environment variable
// value into a set of lowercase provider name strings. Accepts a
// comma-separated list; extra whitespace around entries is trimmed.
// Returns an empty (non-nil) map when raw is empty.
func parseInjectedSet(raw string) map[string]struct{} {
	out := make(map[string]struct{})
	if raw == "" {
		return out
	}
	for _, part := range strings.Split(raw, ",") {
		name := strings.ToLower(strings.TrimSpace(part))
		if name != "" {
			out[name] = struct{}{}
		}
	}
	return out
}
