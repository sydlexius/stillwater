package next

// sidebar_helpers_test.go -- unit coverage for the sidebar display-name and
// avatar-initial helpers (#1778).

import (
	"testing"

	"github.com/sydlexius/stillwater/web/templates"
)

func TestSidebarName(t *testing.T) {
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
			a := templates.AssetPaths{DisplayName: c.displayName, Username: c.username}
			if got := sidebarName(a); got != c.want {
				t.Errorf("sidebarName(%q, %q) = %q, want %q", c.displayName, c.username, got, c.want)
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
		{"unicode first rune", "えみ", "え"}, // non-ASCII; no case change for CJK
		{"empty string", "", "?"},
		{"whitespace only", "   ", "?"},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			if got := sidebarInitial(c.in); got != c.want {
				t.Errorf("sidebarInitial(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
