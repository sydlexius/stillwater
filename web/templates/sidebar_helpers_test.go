package templates

// sidebar_helpers_test.go -- unit coverage for the shared sidebar display-name
// and avatar-initial helpers (#1944). Relocated from web/templates/next/ when
// the helpers were unified into the templates package.

import (
	"testing"
	"unicode/utf8"
)

func TestSidebarDisplayName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		desc        string
		displayName string
		username    string
		want        string
	}{
		{"display name present", "Alice Smith", "asmith", "Alice Smith"},
		{"display name with surrounding whitespace", "  Alice Smith  ", "asmith", "Alice Smith"},
		{"display name empty", "", "asmith", "asmith"},
		{"display name whitespace only", "   ", "asmith", "asmith"},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			if got := SidebarDisplayName(c.displayName, c.username); got != c.want {
				t.Errorf("SidebarDisplayName(%q, %q) = %q, want %q", c.displayName, c.username, got, c.want)
			}
		})
	}
}

func TestSidebarInitial(t *testing.T) {
	t.Parallel()
	cases := []struct {
		desc string
		in   string
		want string
	}{
		{"lowercase first rune uppercased", "alice", "A"},
		{"already uppercase", "Bob", "B"},
		{"normal ascii admin", "admin", "A"},
		{"unicode first rune", "えみ", "え"}, // non-ASCII; no case change for CJK
		// 'ß' uppercases to a single code point via unicode.ToUpper (vs
		// strings.ToUpper, which would expand it to the two-glyph "SS").
		{"sharp s stays single glyph", "ßeta", "ß"},
		{"empty string", "", "?"},
		{"whitespace only", "   ", "?"},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			if got := SidebarInitial(c.in); got != c.want {
				t.Errorf("SidebarInitial(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestSidebarInitialSingleGlyph guards the exact Codoki finding: the avatar
// initial must always be a single code point, even for runes whose
// strings.ToUpper expansion is multi-glyph (e.g. 'ß' -> "SS").
func TestSidebarInitialSingleGlyph(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"ßeta", " alice", "Bob"} {
		got := SidebarInitial(in)
		if n := utf8.RuneCountInString(got); n != 1 {
			t.Errorf("SidebarInitial(%q) = %q, want a single code point, got %d", in, got, n)
		}
	}
}
