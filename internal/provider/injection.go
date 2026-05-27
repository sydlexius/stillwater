package provider

import (
	"errors"
	"os"
	"strings"
	"sync"
)

// ErrInjectedFailure is returned by provider methods when the fault-injection
// hook is active for that provider. The message is intentionally explicit so
// that any developer who sees it in logs immediately recognizes this as a
// self-inflicted test condition, not a real provider outage.
var ErrInjectedFailure = errors.New("provider call rejected by injection (SW_FORCE_PROVIDER_ERROR)")

// injectedSet is the set of provider names that should return ErrInjectedFailure
// on every outbound call. Initialized from SW_FORCE_PROVIDER_ERROR at process
// start and may be replaced at any time via SetInjectedProviders. Guarded by
// injectedMu because provider methods (readers) are invoked concurrently from
// net/http handlers, while tests may swap the set via SetInjectedProviders.
var (
	injectedMu  sync.RWMutex
	injectedSet = parseInjectedSet(os.Getenv("SW_FORCE_PROVIDER_ERROR"))
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
// is preserved with zero overhead.
func ShouldInjectFailure(name ProviderName) bool {
	injectedMu.RLock()
	defer injectedMu.RUnlock()
	if len(injectedSet) == 0 {
		return false
	}
	_, ok := injectedSet[strings.ToLower(string(name))]
	return ok
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
