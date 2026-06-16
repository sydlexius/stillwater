package templates

// sidebar_helpers_test.go -- unit coverage for the shared sidebar display-name
// and avatar-initial helpers (#1944). Relocated from web/templates/next/ when
// the helpers were unified into the templates package.

import "testing"

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
		{"unicode first rune", "えみ", "え"}, // non-ASCII; no case change for CJK
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
