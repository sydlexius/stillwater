package middleware

import (
	"context"
	"net/http"
	"strings"
)

// UXChannel is the resolved web UI channel for a request: the current ("stable")
// UI or the in-development preview ("next") UI. It is the runtime resolution of
// the SW_UX config flag plus the per-user sw_ux cookie and the request path.
type UXChannel string

const (
	// UXStable is the current/default UI served from web/templates/.
	UXStable UXChannel = "stable"
	// UXNext is the in-development preview UI served from web/templates/next/.
	UXNext UXChannel = "next"
)

// uxCookieName is the per-user opt-in cookie carrying "stable" or "next".
const uxCookieName = "sw_ux"

// uxChannelKey is the request-context key under which the resolved UXChannel is
// stashed by the UX middleware so downstream handlers and the logging middleware
// can read it.
const uxChannelKey contextKey = "uxChannel"

// ResolveUX returns the effective UI channel from the server mode (the SW_UX
// config value: "stable" | "next" | "dual") and the raw sw_ux cookie value
// ("" when absent). It is the pure cookie/mode matrix; path-based opt-in
// (visiting /next/*) is layered on in the UX middleware.
//
//   - stable: preview disabled; always stable (cookie ignored).
//   - next:   default next; a sw_ux=stable cookie opts the user back to stable.
//   - dual:   default stable; a sw_ux=next cookie opts the user into next.
//
// Any unrecognized mode is treated as stable so a misconfiguration never serves
// the preview UI by surprise.
func ResolveUX(mode, cookie string) UXChannel {
	switch mode {
	case "next":
		if cookie == string(UXStable) {
			return UXStable
		}
		return UXNext
	case "dual":
		if cookie == string(UXNext) {
			return UXNext
		}
		return UXStable
	default: // "stable" and any unexpected value
		return UXStable
	}
}

// UX returns middleware that resolves the UI channel for each request, sets the
// X-Stillwater-UX response header, and stashes the channel in the request
// context (read via UXChannelFromContext). mode is the SW_UX config value;
// basePath is the deployment URL prefix (e.g. "" or "/stillwater") used to match
// the /next/* lane.
//
// When the lane is enabled (mode != "stable") a request under basePath+"/next"
// resolves to UXNext regardless of the cookie, because visiting a /next/* URL is
// itself an explicit opt-in to the preview lane. In stable mode the preview is
// fully off and even /next/* paths resolve to stable.
func UX(mode, basePath string) func(http.Handler) http.Handler {
	nextPrefix := basePath + "/next/"
	nextExact := basePath + "/next"
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cookie := ""
			if c, err := r.Cookie(uxCookieName); err == nil {
				cookie = c.Value
			}
			ch := ResolveUX(mode, cookie)
			// The /next lane is only reachable when explicitly enabled (next or
			// dual). In stable mode (or any unrecognized/empty mode) the preview
			// is fully off, so even /next/* paths and the opt-in header resolve
			// to stable.
			laneEnabled := mode == string(UXNext) || mode == "dual"
			if laneEnabled {
				// Path opt-in: visiting a /next/* URL is an explicit request for
				// the preview lane, regardless of cookie.
				if isNextPath(r.URL.Path, nextPrefix, nextExact) {
					ch = UXNext
				}
				// Header opt-in: a next/ page tags its HTMX sub-requests with the
				// X-Stillwater-UX request header (via LayoutNext's hx-headers) so
				// those requests resolve to the same channel as the page that
				// issued them, even though the shared fetch endpoints
				// (e.g. /dashboard/actions) are not under /next/. This is what
				// lets the next UI work without relying on the sw_ux cookie, in
				// both next and dual modes. An explicit "stable" header value
				// opts a request back out.
				switch r.Header.Get("X-Stillwater-UX") {
				case string(UXNext):
					ch = UXNext
				case string(UXStable):
					ch = UXStable
				}
			}
			w.Header().Set("X-Stillwater-UX", string(ch))
			ctx := context.WithValue(r.Context(), uxChannelKey, ch)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// isNextPath reports whether p is the /next lane root or a path beneath it.
func isNextPath(p, prefix, exact string) bool {
	return p == exact || strings.HasPrefix(p, prefix)
}

// UXChannelFromContext returns the UI channel resolved for the request, or
// UXStable when no channel was stashed (request never passed through UX).
func UXChannelFromContext(ctx context.Context) UXChannel {
	if ch, ok := ctx.Value(uxChannelKey).(UXChannel); ok {
		return ch
	}
	return UXStable
}
