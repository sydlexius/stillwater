package components

import (
	"strings"
	"testing"
)

// TestSafeBaseURL verifies that safeBaseURL rejects dangerous schemes and
// passes through safe relative paths and absolute http/https URLs.
func TestSafeBaseURL(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		// Safe: relative paths starting with / (and not //)
		{"/artists", "/artists"},
		{"/reports/compliance", "/reports/compliance"},

		// Safe: absolute http/https URLs
		{"http://example.com/page", "http://example.com/page"},
		{"https://example.com/page", "https://example.com/page"},
		{"HTTPS://example.com/page", "HTTPS://example.com/page"},

		// Dangerous: must be replaced with "/"
		{"javascript:alert(1)", "/"},
		{"JAVASCRIPT:alert(1)", "/"},
		{"data:text/html,<script>alert(1)</script>", "/"},
		{"vbscript:msgbox(1)", "/"},
		{"ftp://attacker.example.com", "/"},
		{"", "/"},
		// Dangerous: protocol-relative URLs. Browsers resolve "//host/p" as
		// cross-origin with the page's current scheme, so an attacker who
		// can set BaseURL to "//attacker.tld/..." would otherwise drive
		// pagination links to their own host. Reject the same way as any
		// other dangerous scheme.
		{"//attacker.tld/path", "/"},
		{"//attacker.tld", "/"},
		{"//cdn.example.com/path", "/"},
	}

	for _, tc := range cases {
		got := safeBaseURL(tc.input)
		if got != tc.want {
			t.Errorf("safeBaseURL(%q) = %q; want %q", tc.input, got, tc.want)
		}
	}
}

// TestPageURLAllowlist verifies that pageURL only reflects fields from the
// PaginationData struct and does not pass through unknown query parameters.
// Because PaginationData is a typed struct (not a raw url.Values map), there
// is no code path that copies unknown keys -- this test documents that
// invariant and guards against future regressions.
func TestPageURLAllowlist(t *testing.T) {
	data := PaginationData{
		CurrentPage: 2,
		TotalPages:  10,
		PageSize:    25,
		BaseURL:     "/artists",
		Sort:        "name",
		Order:       "asc",
		Search:      "test",
		Filter:      "compliant",
		View:        "grid",
		LibraryID:   "lib1",
		Status:      "compliant",
	}

	// Simulate an attacker who somehow gets sort=javascript:alert(1) into
	// the struct. Because Sort passes through validateSortParam (allowlist)
	// before being placed in the struct, this cannot happen in production.
	// The test shows that even if it did, the value only appears as a
	// percent-encoded query parameter value, never as the URL scheme.
	maliciousData := PaginationData{
		CurrentPage: 2,
		TotalPages:  10,
		PageSize:    25,
		BaseURL:     "/artists",
		Sort:        "javascript:alert(1)",
	}

	u := maliciousData.pageURL(3)

	if strings.HasPrefix(u, "javascript:") {
		t.Errorf("pageURL returned a javascript: URL: %q", u)
	}
	if !strings.Contains(u, "sort=") {
		t.Errorf("pageURL missing sort parameter: %q", u)
	}
	// The value must be percent-encoded, not raw.
	if strings.Contains(u, "javascript:alert") {
		t.Errorf("pageURL contains un-encoded javascript: in sort value: %q", u)
	}

	// Verify that a normal call produces the expected known-safe parameters.
	u2 := data.pageURL(3)
	for _, param := range []string{"page=3", "page_size=25", "sort=name", "order=asc",
		"search=test", "filter=compliant", "view=grid", "library_id=lib1", "status=compliant"} {
		if !strings.Contains(u2, param) {
			t.Errorf("pageURL missing expected param %q in %q", param, u2)
		}
	}
}

// TestPageURLDataSchemeInSearchValue verifies that a data: URI embedded as a
// query parameter value does not produce a dangerous href. The value will be
// percent-encoded by url.Values.Encode and can only appear in the query
// string, not as the URL scheme.
func TestPageURLDataSchemeInSearchValue(t *testing.T) {
	data := PaginationData{
		CurrentPage: 1,
		TotalPages:  5,
		PageSize:    25,
		BaseURL:     "/artists",
		Search:      "data:text/html,<script>alert(1)</script>",
	}

	u := data.pageURL(2)

	if strings.HasPrefix(u, "data:") {
		t.Errorf("pageURL returned a data: URL: %q", u)
	}
	// data: must not appear before the query string marker
	qmark := strings.Index(u, "?")
	if qmark < 0 {
		t.Fatalf("pageURL produced no query string: %q", u)
	}
	before := u[:qmark]
	if strings.Contains(before, "data:") {
		t.Errorf("data: appears in the URL path portion: %q", before)
	}
}

// TestPageURLMaliciousBaseURL verifies that safeBaseURL prevents a dangerous
// BaseURL from producing a javascript: or data: pagination href.
func TestPageURLMaliciousBaseURL(t *testing.T) {
	dangerous := []string{
		"javascript:alert(1)",
		"JAVASCRIPT:alert(1)",
		"data:text/html,<script>alert(1)</script>",
		"vbscript:msgbox(1)",
	}

	for _, base := range dangerous {
		data := PaginationData{
			CurrentPage: 2,
			TotalPages:  5,
			PageSize:    25,
			BaseURL:     base,
		}

		u := data.pageURL(3)

		lower := strings.ToLower(u)
		for _, scheme := range []string{"javascript:", "data:", "vbscript:"} {
			if strings.HasPrefix(lower, scheme) {
				t.Errorf("pageURL(%q) produced dangerous URL %q", base, u)
			}
		}
		if !strings.HasPrefix(u, "/") {
			t.Errorf("pageURL(%q) should start with /; got %q", base, u)
		}
	}
}

// TestPageURLPreservesPathStripsQueryFragment verifies that pageURL replaces
// any prior `?` query and drops any prior `#` fragment from BaseURL, so the
// pagination component's allowlist is authoritative and stale state cannot
// leak into the generated link.
func TestPageURLPreservesPathStripsQueryFragment(t *testing.T) {
	// BaseURL with a prior query: pageURL must REPLACE the query, not append.
	data := PaginationData{
		CurrentPage: 1,
		TotalPages:  3,
		PageSize:    25,
		BaseURL:     "/artists?leftover=1&another=stale",
	}
	got := data.pageURL(2)
	if strings.Contains(got, "leftover=1") || strings.Contains(got, "another=stale") {
		t.Errorf("pageURL leaked prior query: got %q", got)
	}
	if !strings.Contains(got, "page=2") {
		t.Errorf("pageURL missing page=2: got %q", got)
	}
	if strings.Count(got, "?") != 1 {
		t.Errorf("pageURL produced malformed URL with %d '?' chars: %q", strings.Count(got, "?"), got)
	}

	// BaseURL with a fragment: the fragment must be dropped.
	data.BaseURL = "/artists#top"
	got = data.pageURL(2)
	if strings.Contains(got, "#") {
		t.Errorf("pageURL did not strip fragment: got %q", got)
	}
}

// TestPageURLUnknownParamsNotReflected confirms that parameters not in the
// PaginationData allowlist cannot appear in pagination hrefs. Since
// PaginationData is a struct, callers cannot inject arbitrary keys.
// This test documents the structural invariant.
func TestPageURLUnknownParamsNotReflected(t *testing.T) {
	data := PaginationData{
		CurrentPage: 1,
		TotalPages:  3,
		PageSize:    25,
		BaseURL:     "/artists",
		Sort:        "name",
		Order:       "asc",
	}

	u := data.pageURL(2)

	// These parameter names must not appear; they are not in the struct.
	unknown := []string{"unknown", "evil", "inject", "callback", "redirect"}
	for _, key := range unknown {
		if strings.Contains(u, key+"=") {
			t.Errorf("pageURL contains unexpected parameter %q in %q", key, u)
		}
	}
}
