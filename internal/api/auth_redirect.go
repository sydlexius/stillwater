package api

import (
	"log/slog"
	"net/url"
	"strings"
)

// maxReturnURLLength caps the size of a return URL accepted by
// sanitizeReturnTo. Header-injection vectors aside, no legitimate intra-app
// path approaches this length.
const maxReturnURLLength = 2048

// sanitizeReturnTo validates a candidate post-login redirect target and
// returns either the path itself (when safe) or the configured base path
// fallback (when invalid). The result is always safe to pass to
// http.Redirect or HX-Redirect without further escaping.
//
// Rules:
//   - Reject empty input.
//   - Reject anything longer than maxReturnURLLength.
//   - Reject inputs containing CR, LF, or other control characters.
//   - Reject inputs with a scheme ("https://...", "javascript:...") or
//     host. Path-only relative references only.
//   - Reject protocol-relative inputs ("//evil.com/...").
//   - Reject inputs that do not start with "/".
//   - If basePath is non-empty, the input must start with basePath; the
//     base path itself (no trailing slash) and basePath+"/..." are both
//     accepted.
//   - Reject "/login" exactly and any path under "/api/" -- those targets
//     either re-render the login form or expect a JSON client.
//
// When the input is rejected, sanitizeReturnTo returns basePath+"/" (the
// dashboard at the configured deployment root). On accept it returns the
// input unchanged so caller display logic (HX-Redirect, Location header)
// can hand it straight back to the browser.
func sanitizeReturnTo(raw, basePath string) string {
	fallback := basePath + "/"

	if raw == "" {
		return fallback
	}
	if len(raw) > maxReturnURLLength {
		slog.Debug("rejected return URL: too long", "len", len(raw))
		return fallback
	}

	// Reject any ASCII control characters. CR/LF in particular would let an
	// attacker smuggle header lines through HX-Redirect or Location.
	for _, c := range raw {
		if c < 0x20 || c == 0x7f {
			slog.Debug("rejected return URL: control character")
			return fallback
		}
	}

	// Reject protocol-relative URLs before url.Parse normalizes them.
	if strings.HasPrefix(raw, "//") {
		slog.Debug("rejected return URL: protocol-relative")
		return fallback
	}

	// Reject backslashes: some browsers normalize them to forward slashes,
	// which can sneak past the no-scheme/no-host checks. They have no
	// legitimate use in same-origin path-only return URLs.
	if strings.ContainsRune(raw, '\\') {
		slog.Debug("rejected return URL: contains backslash")
		return fallback
	}

	if !strings.HasPrefix(raw, "/") {
		slog.Debug("rejected return URL: no leading slash")
		return fallback
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		slog.Debug("rejected return URL: parse error", "error", err)
		return fallback
	}
	if parsed.Scheme != "" || parsed.Host != "" {
		slog.Debug("rejected return URL: has scheme or host")
		return fallback
	}

	// The path component must be the same path we parsed from. If url.Parse
	// emitted something different the input had embedded sentinels we don't
	// trust.
	path := parsed.Path

	if basePath != "" {
		if path != basePath && !strings.HasPrefix(path, basePath+"/") {
			slog.Debug("rejected return URL: outside base path")
			return fallback
		}
	}

	// The login page and API surface are not legitimate return targets:
	// landing on /login after login is a loop, and /api/* paths are JSON
	// endpoints the browser cannot render.
	rel := path
	if basePath != "" {
		rel = strings.TrimPrefix(rel, basePath)
		if rel == "" {
			rel = "/"
		}
	}
	if rel == "/login" || strings.HasPrefix(rel, "/login/") {
		slog.Debug("rejected return URL: login path")
		return fallback
	}
	if rel == "/api" || strings.HasPrefix(rel, "/api/") {
		slog.Debug("rejected return URL: api path")
		return fallback
	}

	return raw
}
