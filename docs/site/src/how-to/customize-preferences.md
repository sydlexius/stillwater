---
description: Adjust Stillwater's appearance, layout, typography, and behavior through the Preferences drawer -- changes apply instantly without reloading the page.
---

<!-- code: web/templates/next/prefs_drawer.templ, web/static/js/prefs-drawer.js, internal/api/handlers_preferences.go (PATCH /api/v1/preferences). -->

# Customize preferences

Stillwater's **Preferences** drawer lets each user tune the interface to their liking. Changes are per-user, saved automatically, and take effect without reloading the page. Preferences are separate from application **Settings** (which control server behavior and require admin access).

## Open the drawer

Three entry points in the next/ UI open the drawer:

- Click **Preferences** in the left sidebar (the sliders icon near the bottom of the nav).
- Click your **avatar / user name** at the very bottom of the sidebar.
- Navigate directly to `/next/preferences` (a full-page fallback for bookmarks or when JavaScript is disabled).

The drawer slides in from the left, overlaying the sidebar. Press **Esc** or click the **X** button in the drawer header to close it.

## Search preferences

A search box at the top of the drawer filters settings by name or help text as you type. Sections whose settings don't match the query collapse automatically; type **opacity** to jump straight to Background Opacity, **font** to see all typography controls, and so on. Clear the box to return to the full view.

## Appearance

The **Appearance** section is expanded when the drawer opens.

### Theme

Switches the color scheme for the entire application.

| Option | Behavior |
|---|---|
| Light | White surfaces, dark text |
| Dark | Dark surfaces, light text |
| System | Follows your operating system's dark/light preference (auto) |

### Background Opacity

Sets the opacity of frosted-glass card surfaces, the sidebar, and the drawer itself. The slider ranges from **85% to 100%** in steps of 5.

The global opacity floor is 85%. No surface in the app goes below this value -- at 85% cards and panels are subtly translucent over the backdrop; at 100% every surface is fully solid. Lite Mode (see below) ignores this slider and forces full opacity.

## Layout

### Sidebar State

Controls how the left navigation bar appears when you load the app.

| Option | Appearance |
|---|---|
| Full | Navigation icons plus text labels |
| Icon-only | Icons only; labels are hidden to reclaim horizontal space |
| Hidden | Sidebar removed entirely; access pages from the keyboard shortcut or the `[` key |

You can also cycle the sidebar state at any time using the collapse/expand button in the sidebar header, or the `[` key.

### Content Width

Determines how wide the main content column grows.

| Option | Description |
|---|---|
| Narrow | Caps line length at a comfortable reading measure regardless of screen size |
| Full | Expands content to the viewport edge so wide tables and image grids use every available pixel |

### Thumbnail Size

Sets the size of artist artwork in lists and the Action Queue on the Dashboard.

| Option | Pixel size |
|---|---|
| Small | 32 px |
| Medium | 44 px (default) |
| Large | 56 px |

### Layout Density

Controls vertical spacing in lists and grids.

| Option | Effect |
|---|---|
| Compact | Tighter padding; fits more rows on screen |
| Comfortable | Balanced spacing (default) |
| Spacious | Extra padding; easier to scan each row |

### Page Size

Sets how many rows appear per page on the Artists list and Reports screens. The value is applied server-side and normalizes to the nearest multiple of 5 on blur. Accepted range: 10 to 500.

## Typography

### Font Family

The typeface used for all body copy, table rows, and UI labels.

| Option | Description |
|---|---|
| Inter | Default; a geometric sans-serif designed for screens |
| System | Your operating system's native system font |
| Atkinson | Atkinson Hyperlegible; designed to boost legibility for low-vision readers |

Inter and Atkinson Hyperlegible are bundled under the SIL Open Font License 1.1.

### Monospace Font

The typeface used for keyboard shortcut badges, IDs, timestamps, and code values.

| Option | Description |
|---|---|
| System | Your operating system's native monospace font |
| JetBrains | JetBrains Mono; a programming font with clear glyph differentiation |
| Cascadia | Cascadia Code; Microsoft's coding font with ligature support |

### Font Size

Scales all text in the application off a single base size. The control is a five-stop slider.

| Stop | Approximate size |
|---|---|
| Small | 13 px |
| Medium | 14 px (default) |
| Large | 16 px |
| X-Large | 18 px |
| XX-Large | 20 px |

### Letter Spacing

Adjusts the horizontal gap between characters across the entire UI. Wide and Extra Wide are useful readability aids for users with dyslexia or low vision.

| Option | Tracking |
|---|---|
| Normal | 0 (default) |
| Wide | 0.025 em |
| Extra Wide | 0.05 em |

## Accessibility and Performance

### Reduced Motion

Controls whether transitions and animations play.

| Option | Behavior |
|---|---|
| Auto | Follows your operating system's "reduce motion" accessibility setting |
| Full | Plays all animations regardless of OS setting |
| Reduced | Disables animations regardless of OS setting |

### Lite Mode

Turns off visually expensive features in one switch: frosted-glass blur, CSS transitions, BlurHash image placeholders, and full-resolution prefetching. Use it if pages feel sluggish.

| Option | Behavior |
|---|---|
| Off | All visual features enabled (default) |
| On | All expensive features disabled |
| Auto | Enables Lite Mode automatically when fewer than 4 CPU cores or less than 4 GB RAM are detected |

When Lite Mode is active it overrides Background Opacity and forces fully solid surfaces.

### Keyboard Hints

Toggles the inline keyboard-shortcut badges shown throughout the UI (for example the `r` badge next to Run Rules on the Dashboard).

Turning hints off removes the visual clutter; the full shortcut cheat sheet (accessible with `?`) stays available regardless of this setting.

## Behavior

### Prefetch Images

When enabled, Stillwater automatically searches providers for artwork when you open an artist's image page. Turn it off if you prefer to trigger image searches manually or want to reduce bandwidth usage.

### Notifications

Shows in-app toast messages when rule violations are detected during background evaluations. This controls in-app toasts only; email and webhook notifications are configured separately in **Settings > Automation**.

### Language

Sets the interface language. Only English ships today; additional languages appear here as translations are contributed. Note: Biography language preferences (which metadata language to prefer from providers) are configured separately in **Settings > Providers**.

## Artist Detail Layout

Reorder and show or hide the sections on every artist's detail page. The sections available are:

| Section | Contents |
|---|---|
| Details | Name, Sort Name, Type, Born/Formed, and other metadata fields |
| Artwork | Thumb, Fanart, Logo, and Banner image slots |
| Findings | Open rule violations for the artist |
| Providers | Source attributions per field |
| Discography | Album entries from the artist NFO |
| Identifiers | External provider IDs (MusicBrainz, AudioDB, Discogs, etc.) |

To reorder sections:

1. Grab the **drag handle** (the dot-grid icon on the left of a row) and drag it to the desired position, or use the **up/down arrow** buttons on the right (keyboard-accessible fallback).
2. The new order saves automatically and takes effect on the next artist page you open.

To hide a section:

1. Click the **eye** button on the right of the row. The eye becomes crossed out and the row dims.
2. The section disappears from artist pages immediately.
3. Click the eye again to restore it.

The hero image, artist name, and action bar at the top of each artist page are fixed chrome and are not reorderable.

Click **Reset layout** to restore the default section order and visibility.

## Reset all preferences

The **Reset to defaults** button in the drawer footer restores every preference to its factory default. This affects all groups at once. Individual sections with a separate reset button (Artist Detail Layout) can be reset independently.

## See also

- [Preferences reference](../reference/preferences.md) for a quick-lookup table of every preference key, its default, and its allowed values.
- [Find a setting](find-a-setting.md) for application-level configuration that requires admin access.
- [Edit an artist](edit-artist.md) for the per-field clock (prior value revert) workflow.
