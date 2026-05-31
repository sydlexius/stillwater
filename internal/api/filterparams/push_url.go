// Package filterparams holds tiny helpers shared by handlers that drive the
// FilterFlyout component (web/components/filter_flyout.templ). The
// FilterFlyout's URL-state contract is uniform across pages (dashboard,
// reports/compliance, artists) so these helpers keep handler code small and
// the contract auditable in one place.
package filterparams

import (
	"net/http"
	"net/url"
)

// WriteHXPushURL sets the HX-Push-Url response header so the browser address
// bar tracks the user-facing URL ("$basePath/?...") after an HTMX swap,
// rather than the internal fetch endpoint the swap actually requested.
// basePath is the application base path (relative URLs do not work with
// HX-Push-Url under a reverse-proxy sub-path); vals is the encoded query
// string. An empty vals omits the "?" entirely so the bar shows a clean
// "$basePath/" when no filter is active.
//
// The handler should call this only on HTMX requests (req.Header.Get
// ("HX-Request") == "true"); non-HTMX callers already have the correct URL
// in their address bar.
func WriteHXPushURL(w http.ResponseWriter, basePath string, vals url.Values) {
	base := basePath
	if base == "" || base[len(base)-1] != '/' {
		base += "/"
	}
	if len(vals) == 0 {
		w.Header().Set("HX-Push-Url", base)
		return
	}
	w.Header().Set("HX-Push-Url", base+"?"+vals.Encode())
}

// WriteHXPushURLForPath is like WriteHXPushURL but pushes an explicit,
// fully-qualified path (already including the basePath) instead of the
// basePath root. Channel-aware handlers use it when the user-facing URL is
// not the application root: the stable dashboard lives at "$basePath/" (use
// WriteHXPushURL), but the next/ dashboard lives at "$basePath/next/dashboard"
// (use this). The path is emitted verbatim with no forced trailing slash, so
// it must already be the canonical screen URL.
func WriteHXPushURLForPath(w http.ResponseWriter, path string, vals url.Values) {
	if len(vals) == 0 {
		w.Header().Set("HX-Push-Url", path)
		return
	}
	w.Header().Set("HX-Push-Url", path+"?"+vals.Encode())
}
