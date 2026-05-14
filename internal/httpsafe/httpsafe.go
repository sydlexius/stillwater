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
	"time"
)

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

	dialer := &net.Dialer{Timeout: 10 * time.Second}
	t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("split host/port %q: %w", addr, err)
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
	return &http.Client{
		Timeout:   timeout,
		Transport: SafeTransport(),
	}
}

// isBlocked returns true for IP addresses that must never be contacted from
// outbound HTTP requests: loopback, link-local unicast, link-local multicast,
// RFC 1918 private ranges, and the unspecified address.
func isBlocked(ip net.IP) bool {
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified()
}
