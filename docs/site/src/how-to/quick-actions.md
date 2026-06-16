---
description: Use the Quick Actions toolbar at the bottom of the sidebar to cycle themes, open the keyboard-shortcut reference, and log out without navigating away from the current page.
---

<!-- code: web/templates/next/sidebar.templ (sw-sidebar-actions group), web/static/js/sidebar.js (cycleTheme, toggleHelpOverlay). -->

# Quick Actions

The **Quick Actions** toolbar is the row of icon buttons at the very bottom of the sidebar, just above the user identity area. It provides instant access to three utility actions without navigating away from your current page.

## The three actions

### Cycle theme

The sun/moon icon cycles the color scheme through **dark, light, and system** (follows your OS preference) in that order.

The theme preference saves automatically and carries over to subsequent sessions. For finer control, or to use the full set of per-user appearance options, open the [Preferences drawer](customize-preferences.md).

### Keyboard shortcuts

The question-mark-circle icon opens the keyboard-shortcut reference overlay. The overlay lists every available shortcut grouped by context (global, Artists page, artist detail, Dashboard, and so on).

The shortcut `?` (question mark) triggers the same overlay from anywhere in the app.

Keyboard hints (the inline `kbd` badges next to individual controls) can be shown or hidden independently via [Preferences](customize-preferences.md#keyboard-hints). The full overlay always shows the complete list regardless of that setting.

### Log out

The arrow-out-of-bracket icon ends your current session and returns you to the login page.

## Sidebar state

When the sidebar is in **Icon-only** mode, the Quick Actions buttons remain visible as glyph-only icons. When the sidebar is **Hidden**, the Quick Actions row is also hidden; use the `[` key to restore the sidebar, or open the [Preferences drawer](customize-preferences.md#sidebar-state) to change the default sidebar state.

## See also

- [Customize preferences](customize-preferences.md) for the full set of per-user appearance and layout controls, including theme and sidebar state.
