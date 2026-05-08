package middleware

import (
	"fmt"
	"net/http"
)

// altSvcMaxAge is the cache lifetime (in seconds) clients should remember the
// HTTP/3 advertisement for. 86400 = 24 hours is the value recommended by RFC
// 7838 examples and matches the issue's acceptance criteria.
const altSvcMaxAge = 86400

// AltSvc returns middleware that adds an `Alt-Svc` response header advertising
// HTTP/3 on the supplied UDP port. The header is the standard mechanism
// (RFC 7838) for telling HTTP/1.1 + HTTP/2 clients that the same origin is
// also reachable via HTTP/3 on a given port.
//
// Returns an unmodified middleware (pass-through) when port <= 0 so callers
// can compose AltSvc unconditionally and gate enablement on configuration.
//
// Example header value: `Alt-Svc: h3=":443"; ma=86400`
func AltSvc(port int) func(http.Handler) http.Handler {
	if port <= 0 {
		return func(next http.Handler) http.Handler { return next }
	}
	header := fmt.Sprintf(`h3=":%d"; ma=%d`, port, altSvcMaxAge)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Only advertise HTTP/3 on TLS connections. Emitting Alt-Svc on
			// plain-HTTP responses (split-port mode) would broaden the
			// advertisement beyond the HTTPS-only scope of the QUIC listener.
			if r.TLS != nil {
				w.Header().Set("Alt-Svc", header)
			}
			next.ServeHTTP(w, r)
		})
	}
}
