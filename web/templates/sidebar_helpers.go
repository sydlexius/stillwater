// Package templates provides shared sidebar helper functions used by both the
// stable (web/templates/) and next (web/templates/next/) sidebar components.
// Placing these here avoids behavioral drift between channels.
package templates

import "strings"

// SidebarInitial returns the uppercased first rune of s, or "?" when s is
// empty or whitespace-only. Adopts the next-channel behavior (uppercase) for
// visual consistency with conventional avatar initials.
func SidebarInitial(s string) string {
	for _, r := range strings.TrimSpace(s) {
		return strings.ToUpper(string(r))
	}
	return "?"
}

// SidebarDisplayName returns the trimmed displayName when non-empty, falling
// back to username. Adopts the next-channel behavior (trim + fallback).
func SidebarDisplayName(displayName, username string) string {
	if dn := strings.TrimSpace(displayName); dn != "" {
		return dn
	}
	return username
}
