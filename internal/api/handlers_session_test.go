package api

import "testing"

func TestValidateReturnURL(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		// Valid relative paths.
		{name: "root", input: "/", want: "/"},
		{name: "settings page", input: "/settings", want: "/settings"},
		{name: "artists with query", input: "/artists?page=2", want: "/artists?page=2"},
		{name: "nested path", input: "/settings/providers", want: "/settings/providers"},
		{name: "path with fragment", input: "/artists#top", want: "/artists#top"},
		{name: "complex query", input: "/artists?sort=name&dir=asc", want: "/artists?sort=name&dir=asc"},

		// Invalid: empty.
		{name: "empty string", input: "", want: ""},

		// Invalid: absolute URLs with schemes.
		{name: "https absolute", input: "https://evil.com", want: ""},
		{name: "http absolute", input: "http://evil.com", want: ""},
		{name: "ftp absolute", input: "ftp://evil.com/file", want: ""},

		// Invalid: protocol-relative.
		{name: "protocol-relative", input: "//evil.com", want: ""},
		{name: "protocol-relative with path", input: "//evil.com/foo", want: ""},

		// Invalid: javascript scheme.
		{name: "javascript scheme", input: "javascript:alert(1)", want: ""},

		// Invalid: data scheme.
		{name: "data scheme", input: "data:text/html,<h1>evil</h1>", want: ""},

		// Invalid: backslash bypass attempts.
		{name: "backslash evil", input: `\evil.com`, want: ""},
		{name: "double backslash", input: `\\evil.com`, want: ""},
		// After stripping the backslash, "\/evil.com" becomes "/evil.com" which
		// is a valid local relative path (not an open redirect).
		{name: "backslash slash becomes local path", input: `\/evil.com`, want: "/evil.com"},

		// Path traversal: these are valid local relative paths (no open redirect).
		{name: "dot-dot traversal", input: "/foo/../bar", want: "/foo/../bar"},
		{name: "dot segment", input: "/foo/./bar", want: "/foo/./bar"},
		// Traversal that would resolve to protocol-relative is caught by the
		// double-slash check after the browser resolves /../ sequences.
		{name: "traversal to root", input: "/a/../../b", want: "/a/../../b"},

		// Invalid: no leading slash.
		{name: "relative no slash", input: "evil.com", want: ""},
		{name: "relative path", input: "settings", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := validateReturnURL(tt.input)
			if got != tt.want {
				t.Errorf("validateReturnURL(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
