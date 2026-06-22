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

// TestIsPublicIP verifies the exported inverse of isBlocked that ACME IP-SAN
// validation reuses. It must agree with isBlocked on every input (public iff
// not blocked) and fail closed on nil/empty, so config validation never treats
// an unclassifiable address as a routable public IP.
func TestIsPublicIP(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		ip   string
		want bool
	}{
		{"public IPv4", "8.8.8.8", true},
		{"public IPv6", "2606:4700:4700::1111", true},
		{"loopback", "127.0.0.1", false},
		{"loopback IPv6", "::1", false},
		{"rfc1918 10", "10.0.0.1", false},
		{"rfc1918 172", "172.16.0.1", false},
		{"rfc1918 192", "192.168.1.1", false},
		{"link-local", "169.254.0.1", false},
		{"unspecified", "0.0.0.0", false},
		{"cgnat rfc6598", "100.64.0.1", false},
		{"rfc2544 benchmark", "198.18.0.1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ip := net.ParseIP(tc.ip)
			if ip == nil {
				t.Fatalf("test setup: %q failed to parse", tc.ip)
			}
			if got := IsPublicIP(ip); got != tc.want {
				t.Fatalf("IsPublicIP(%s) = %v; want %v", tc.ip, got, tc.want)
			}
			// Lock in the inverse relationship with the source-of-truth blocklist.
			if got, blocked := IsPublicIP(ip), isBlocked(ip); got == blocked {
				t.Fatalf("IsPublicIP(%s)=%v must be the inverse of isBlocked=%v", tc.ip, got, blocked)
			}
		})
	}

	// Fail closed: nil and empty are not public.
	if IsPublicIP(nil) {
		t.Fatal("IsPublicIP(nil) = true; want false (fail closed)")
	}
	if IsPublicIP(net.IP{}) {
		t.Fatal("IsPublicIP(empty) = true; want false (fail closed)")
	}
}
