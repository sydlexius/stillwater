package httpsafe

import (
	"net"
	"testing"
)

// TestIsBlockedFailClosedMalformedIP locks in the fail-closed branch of
// isBlocked at the point where netip.AddrFromSlice rejects an input slice
// whose length is neither 4 nor 16. The external SafeTransport tests cannot
// reach this branch because callers feed it net.IP values produced by the Go
// resolver, which always returns 4- or 16-byte representations. A future
// refactor that silently changes that branch to return false (fail-open)
// would reopen an SSRF vector for any non-resolver call site -- this test
// is the guard.
func TestIsBlockedFailClosedMalformedIP(t *testing.T) {
	t.Parallel()
	// Length 5: not a valid IPv4 or IPv6 byte representation.
	malformed := net.IP{1, 2, 3, 4, 5}
	if !isBlocked(malformed) {
		t.Fatalf("isBlocked(%v) = false; want true (fail-closed on malformed slice)", malformed)
	}
}
