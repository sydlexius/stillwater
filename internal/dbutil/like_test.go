package dbutil

import "testing"

// TestEscapeLike verifies that every SQL LIKE metacharacter (%, _, and the
// escape character \) is escaped with a backslash, matching the ESCAPE '\'
// clause callers pair with the pattern, and that ordinary text and the
// empty string pass through unchanged.
func TestEscapeLike(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty string", in: "", want: ""},
		{name: "plain string unchanged", in: "The Beatles", want: "The Beatles"},
		{name: "percent escaped", in: "100%", want: `100\%`},
		{name: "underscore escaped", in: "a_b", want: `a\_b`},
		{name: "backslash escaped", in: `a\b`, want: `a\\b`},
		{name: "mixed metacharacters", in: `50%_off\now`, want: `50\%\_off\\now`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EscapeLike(tt.in); got != tt.want {
				t.Errorf("EscapeLike(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
