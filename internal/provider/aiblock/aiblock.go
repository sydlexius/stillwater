// Package aiblock hides web image search results hosted on sites from a
// community-maintained list of AI-image sources (#2310).
//
// The list is laylavish's uBlockOrigin-HUGE-AI-Blocklist (CC0-1.0), vendored
// in ai_blocklist.list in uBlacklist match-pattern format. Filtering happens
// on the Stillwater side, after a search provider has returned its results, so
// it never changes the request a provider sends and works for any web image
// provider. It is best-effort: it drops results from listed sites, and AI
// images hosted elsewhere still pass.
//
// Supported line forms (everything else is counted as skipped):
//
//	*://*.host/*        host and all its subdomains
//	*://*.host/path*    same host match, plus a path-prefix match
//	*://*.host/a/*/b*   a path with "*" wildcards inside it
//	*://*.host/a.htm    no trailing "*": the path (and query) must match exactly
//	/regex/, /regex/i   an RE2 regular expression matched against the full URL
//	                    (only the "i" flag is understood)
package aiblock

import (
	_ "embed"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"sync"

	"github.com/sydlexius/stillwater/internal/provider"
)

//go:embed ai_blocklist.list
var vendoredList string

// Matcher holds a compiled blocklist. The zero value matches nothing.
type Matcher struct {
	// hosts maps a host to its path rules. A rule with no prefix and no regexp
	// matches every path on that host.
	hosts   map[string][]pathRule
	regexes []*regexp.Regexp
	// Skipped counts non-comment lines that were not understood.
	Skipped int
}

// pathRule is one path condition on a host. Match-pattern paths are anchored
// at both ends and "*" is the only wildcard, so a literal path with no
// trailing "*" must match exactly (exact), a literal path with a trailing "*"
// is a prefix (prefix), and a path with an inner "*" is a regexp (re) that is
// end-anchored unless the pattern ended in "*". The candidate is the escaped
// path plus "?query", because the pattern's path part covers the query too.
type pathRule struct {
	prefix string
	exact  string
	re     *regexp.Regexp
}

func (r pathRule) match(pathQuery string) bool {
	switch {
	case r.re != nil:
		return r.re.MatchString(pathQuery)
	case r.exact != "":
		return pathQuery == r.exact
	}
	return strings.HasPrefix(pathQuery, r.prefix)
}

// Parse compiles a uBlacklist-format list. Malformed lines are skipped and
// counted in Matcher.Skipped; Parse never panics on bad input.
func Parse(list string) *Matcher {
	m := &Matcher{hosts: make(map[string][]pathRule)}
	for _, line := range strings.Split(list, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "/") {
			re, ok := parseRegexLine(line)
			if !ok {
				m.Skipped++
				continue
			}
			m.regexes = append(m.regexes, re)
			continue
		}
		rest, ok := strings.CutPrefix(line, "*://")
		if !ok {
			m.Skipped++
			continue
		}
		rest = strings.TrimPrefix(rest, "*.")
		host, path, _ := strings.Cut(rest, "/")
		host = strings.ToLower(host)
		// A single-label host (no dot) is treated as malformed: a rule like
		// "*.com" would otherwise hide every result on a TLD.
		if !strings.Contains(host, ".") || strings.ContainsAny(host, "*?") {
			m.Skipped++
			continue
		}
		// A trailing "*" is the usual "any suffix": it is dropped but
		// remembered, because without one the path must match to its end.
		open := strings.HasSuffix(path, "*")
		path = strings.TrimSuffix(path, "*")
		rule := pathRule{}
		switch {
		case path == "":
		case strings.Contains(path, "*"):
			parts := strings.Split("/"+strings.TrimPrefix(path, "/"), "*")
			for i := range parts {
				parts[i] = regexp.QuoteMeta(parts[i])
			}
			// Built only from QuoteMeta'd literals and ".*", so it always
			// compiles.
			expr := "^" + strings.Join(parts, ".*")
			if !open {
				expr += "$"
			}
			rule.re = regexp.MustCompile(expr)
		case open:
			rule.prefix = "/" + strings.TrimPrefix(path, "/")
		default:
			rule.exact = "/" + strings.TrimPrefix(path, "/")
		}
		m.hosts[host] = append(m.hosts[host], rule)
	}
	return m
}

// parseRegexLine compiles a "/re/" or "/re/i" line.
func parseRegexLine(line string) (*regexp.Regexp, bool) {
	end := strings.LastIndex(line, "/")
	if end < 2 {
		return nil, false
	}
	body, flags := line[1:end], line[end+1:]
	switch flags {
	case "":
	case "i":
		body = "(?i)" + body
	default:
		return nil, false
	}
	re, err := regexp.Compile(body)
	return re, err == nil
}

// MatchURL reports whether rawURL is blocked. An unparsable URL is not.
func (m *Matcher) MatchURL(rawURL string) bool {
	if m == nil || rawURL == "" {
		return false
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return false
	}
	// Normalize like a browser would: lowercase host, no trailing root dot.
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	// Walk the host and its parents: a rule for example.com covers
	// img.example.com.
	// Host rules see the escaped path plus query, the way a browser applies a
	// match pattern to the serialized URL; this is the same RequestURI form the
	// regex branch below uses, so both rule kinds agree on escaping.
	pathQuery := u.RequestURI()
	for h := host; h != ""; {
		for _, rule := range m.hosts[h] {
			if rule.match(pathQuery) {
				return true
			}
		}
		_, parent, found := strings.Cut(h, ".")
		if !found {
			break
		}
		h = parent
	}
	// Regexes see the whole URL with a lowercase scheme and host.
	full := strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host) + u.RequestURI()
	for _, re := range m.regexes {
		if re.MatchString(full) {
			return true
		}
	}
	return false
}

// Filter returns the results whose URL is not blocked, and how many were
// removed. ImageResult carries only the image URL (providers do not return a
// source-page URL), so that is the field matched.
func (m *Matcher) Filter(in []provider.ImageResult) (kept []provider.ImageResult, removed int) {
	kept = make([]provider.ImageResult, 0, len(in))
	for _, r := range in {
		if m.MatchURL(r.URL) {
			removed++
			continue
		}
		kept = append(kept, r)
	}
	return kept, removed
}

var (
	defaultOnce    sync.Once
	defaultMatcher *Matcher
)

// Default returns the matcher for the vendored list, compiled once on first
// use; a server may call it at startup to surface parse problems early. The
// skipped-line count is logged once.
func Default() *Matcher {
	defaultOnce.Do(func() {
		defaultMatcher = Parse(vendoredList)
		if defaultMatcher.Skipped > 0 {
			slog.Warn("AI blocklist: skipped unparsable lines", "skipped", defaultMatcher.Skipped)
		}
	})
	return defaultMatcher
}
