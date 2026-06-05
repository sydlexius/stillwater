package next

import (
	"strings"

	"github.com/sydlexius/stillwater/web/templates"
)

// sidebarName returns the display name for the sidebar user footer,
// falling back to the username. Spike helper for the #1778 redesign.
func sidebarName(a templates.AssetPaths) string {
	if dn := strings.TrimSpace(a.DisplayName); dn != "" {
		return dn
	}
	return a.Username
}

// sidebarInitial returns the uppercased first rune of s, or "?" when empty.
func sidebarInitial(s string) string {
	for _, r := range strings.TrimSpace(s) {
		return strings.ToUpper(string(r))
	}
	return "?"
}
