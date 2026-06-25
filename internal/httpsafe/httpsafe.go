// Package httpsafe provides an SSRF-safe HTTP transport and client that block
// connections to loopback, link-local, and RFC 1918 private addresses. All IP
// resolution happens at dial time to defend against DNS-rebinding attacks where
// a hostname appears safe at pre-check time but resolves to a private address
// when the actual connection is made.
package httpsafe

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

// extraBlockedPrefixes covers reserved IPv4 ranges that Go's stdlib helpers
// (net.IP.IsPrivate, IsLoopback, IsLinkLocalUnicast, IsLinkLocalMulticast,
// IsUnspecified) do not flag but which should never be reachable from
// outbound HTTP:
//
//   - 100.64.0.0/10  CGNAT shared address space (RFC 6598). Often used as a
//     transit range inside ISP networks and on tunnel/VPN interfaces. Reaching
//     it from an SSRF context is almost always unintended.
//   - 198.18.0.0/15  RFC 2544 benchmark / interconnect range. Reserved for
//     device-to-device testing and sometimes routed internally; treat as
//     non-public.
//   - 192.0.2.0/24, 198.51.100.0/24, 203.0.113.0/24  IPv4 documentation ranges
//     (RFC 5737, TEST-NET-1/2/3). Reserved for examples and docs; never
//     globally routed, so a real outbound request must never target them and a
//     CA can never validate an ACME order for one.
//   - 2001:db8::/32  IPv6 documentation range (RFC 3849). Same rationale as the
//     IPv4 documentation ranges.
//
// IPv6 ULA (fc00::/7) and link-local (fe80::/10) are already caught by
// net.IP.IsPrivate and net.IP.IsLinkLocalUnicast respectively, so they do not
// appear here -- they are exercised by the test suite for lock-in. General
// multicast (224.0.0.0/4 and ff00::/8) is caught by net.IP.IsMulticast in
// isBlocked rather than by a prefix here.
var extraBlockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("2001:db8::/32"),
}

// ErrPrivateAddress is returned by SafeTransport's DialContext when a target
// host resolves to a loopback, link-local, or RFC 1918 address. Exposed as a
// sentinel so callers (especially tests) can distinguish the SSRF rejection
// from incidental dial failures (timeouts, connection refused). Wrap with
// `fmt.Errorf("...: %w", ErrPrivateAddress)` when adding context.
var ErrPrivateAddress = errors.New("address is private or reserved")

// SafeTransport returns a cloned *http.Transport with a custom DialContext that
// rejects connections to loopback, link-local, and RFC 1918 private addresses.
//
// Pool settings per C8:
//   - MaxIdleConnsPerHost: 32
//   - IdleConnTimeout: 90s
//
// All other settings (TLS timeouts, HTTP/2) are preserved from
// http.DefaultTransport via Clone(). Proxy is explicitly disabled (t.Proxy = nil)
// so DialContext always receives the request's real destination -- forward
// proxies would otherwise hide the target host and let private addresses bypass
// the SSRF guard. Operators who need an egress proxy must wire one after
// constructing SafeTransport.
func SafeTransport() *http.Transport {
	return SafeTransportWithAllowedHosts()
}

// SafeTransportWithAllowedHosts is SafeTransport with a trusted-host exemption.
// Each host in allowedHosts (matched case-insensitively against the literal dial
// host, before DNS resolution) bypasses the entire isBlocked check, so a
// configured host that resolves to a loopback/private/link-local address can be
// reached. The connection is still resolved and dialed normally; only the
// private-address rejection is skipped. With no allowedHosts the behavior is
// byte-for-byte identical to the default guarded transport, so SafeTransport is
// just the zero-allowlist case.
//
// SECURITY -- DNS-rebinding: for an allowlisted host the resolved-IP recheck is
// deliberately skipped. That is the point: the operator's own hostname is
// expected to resolve to a private/LAN IP (e.g. a self-hosted MusicBrainz
// mirror at 192.168.x.x). This is safe ONLY because the allow-set is populated
// exclusively from trusted operator configuration (an admin-typed provider base
// URL), never from request input or any untrusted source. A host name that
// reaches this transport from request input must NEVER be added to the allow-set
// -- doing so would reopen the SSRF/DNS-rebinding vector the guard exists to
// close. Non-allowlisted hosts retain the full all-resolved-IPs rebinding check.
func SafeTransportWithAllowedHosts(allowedHosts ...string) *http.Transport {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		base = &http.Transport{}
	}
	t := base.Clone()

	// Strip ProxyFromEnvironment inherited from http.DefaultTransport. When
	// Proxy is non-nil, DialContext is called with the PROXY's address as the
	// first hop -- not the request's final host. The SSRF guard below would
	// then validate the proxy (typically public) and let the proxy route
	// requests to private targets, defeating the entire point of this
	// transport. Operators who need an egress proxy must wire one explicitly
	// after constructing SafeTransport; the default is "no proxy".
	t.Proxy = nil

	// Pool tuning (C8): increase per-host idle connections to better match
	// real-world burst patterns and align with the DefaultMaxIdleConnsPerHost
	// recommendation for services making many upstream requests.
	t.MaxIdleConnsPerHost = 32
	t.IdleConnTimeout = 90 * time.Second

	// Build the allow-set once at construction, lowercased, so the per-dial
	// lookup is a constant-time map check. Empty hosts are ignored so a stray
	// "" can never match an empty SplitHostPort result.
	allowed := make(map[string]struct{}, len(allowedHosts))
	for _, h := range allowedHosts {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			allowed[h] = struct{}{}
		}
	}

	dialer := &net.Dialer{Timeout: 10 * time.Second}
	t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("split host/port %q: %w", addr, err)
		}

		// Trusted-host exemption: when the literal dial host (pre-resolution)
		// is in the operator-configured allow-set, skip the private-address
		// guard entirely and dial it as-is. See the SECURITY note on
		// SafeTransportWithAllowedHosts for why skipping the resolved-IP
		// recheck is safe here (allow-set is trusted config, never request
		// input).
		if _, ok := allowed[strings.ToLower(host)]; ok {
			conn, dialErr := dialer.DialContext(ctx, network, addr)
			if dialErr != nil {
				return nil, fmt.Errorf("dial allowlisted host %s: %w", host, dialErr)
			}
			return conn, nil
		}

		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("resolve %q: %w", host, err)
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("DNS lookup for %s returned no addresses", host)
		}

		// Reject any IP that falls in a blocked range. Checking all resolved IPs
		// prevents DNS-rebinding: the attacker cannot mix a public IP with a private
		// one to sneak past a per-IP allow-check.
		var safe []net.IPAddr
		for _, ip := range ips {
			if isBlocked(ip.IP) {
				return nil, fmt.Errorf("resolved address %s: %w", ip.IP, ErrPrivateAddress)
			}
			safe = append(safe, ip)
		}

		// Try each safe IP in order so that round-robin DNS and transient failures
		// on individual IPs do not break the request.
		var lastErr error
		for _, ip := range safe {
			conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
			if dialErr == nil {
				return conn, nil
			}
			lastErr = dialErr
		}
		return nil, fmt.Errorf("all %d IPs failed for %s (last: %w)", len(safe), host, lastErr)
	}
	return t
}

// SafeClient returns an *http.Client using SafeTransport with the given request
// timeout. It is a convenience constructor for callers that need a one-liner.
func SafeClient(timeout time.Duration) *http.Client {
	return SafeClientWithAllowedHosts(timeout)
}

// SafeClientWithAllowedHosts is SafeClient with the trusted-host exemption of
// SafeTransportWithAllowedHosts. See that function's SECURITY note: allowedHosts
// must come only from trusted operator config, never request input. With no
// allowedHosts it is identical to SafeClient.
func SafeClientWithAllowedHosts(timeout time.Duration, allowedHosts ...string) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: SafeTransportWithAllowedHosts(allowedHosts...),
	}
}

// isBlocked returns true for IP addresses that must never be contacted from
// outbound HTTP requests: loopback, link-local unicast, multicast (any --
// 224.0.0.0/4 and ff00::/8), RFC 1918 private ranges, the unspecified address,
// and the extra reserved ranges in extraBlockedPrefixes (CGNAT, RFC 2544, and
// the RFC 5737 / RFC 3849 documentation ranges).
func isBlocked(ip net.IP) bool {
	if ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsMulticast() ||
		ip.IsUnspecified() {
		return true
	}
	// Convert the net.IP to a netip.Addr for prefix matching. AddrFromSlice
	// accepts both 4-byte and 16-byte representations; we normalise to a 4-byte
	// IPv4 form (via To4) when applicable so the IPv4 prefixes in
	// extraBlockedPrefixes match correctly even when ip is a 4-in-6 mapped
	// address.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		// Fail closed. The only documented callers pass net.IP values from
		// the Go resolver, which always produces 4-byte or 16-byte slices.
		// Reaching this branch means the input has a non-standard length
		// (zero, 5-15, 17+) -- either a bug in the caller or a future
		// non-resolver code path. We refuse to dial an address we cannot
		// classify; safer to reject than to silently allow.
		return true
	}
	addr = addr.Unmap()
	for _, prefix := range extraBlockedPrefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// IsPublicIP reports whether ip is a routable, public unicast address that is
// safe to expose to or request from the public internet. It is the inverse of
// the internal SSRF blocklist (isBlocked): an address is "public" only when it
// is NOT loopback, RFC 1918 private, link-local unicast, multicast (any),
// unspecified, nor one of the extra reserved ranges (CGNAT 100.64.0.0/10,
// RFC 2544 198.18.0.0/15, and the RFC 5737 / RFC 3849 documentation ranges
// 192.0.2.0/24, 198.51.100.0/24, 203.0.113.0/24, 2001:db8::/32).
//
// It exists so callers outside this package (notably ACME IP-SAN validation in
// internal/config) reuse the SAME vetted blocklist rather than re-deriving it,
// which would drift over time. A nil or zero-length ip returns false (fail
// closed): an address we cannot classify is never treated as public.
func IsPublicIP(ip net.IP) bool {
	if len(ip) == 0 {
		return false
	}
	return !isBlocked(ip)
}
