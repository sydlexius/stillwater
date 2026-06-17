---
description: Complete reference for all Stillwater user preference keys, their defaults, and valid values.
---

# Preferences reference

Every user preference key that Stillwater accepts via `PUT /api/v1/preferences/{key}` or `PATCH /api/v1/preferences` is listed below. Preferences are per-user and persisted in the database; they can also be read with `GET /api/v1/preferences`.

The table is generated from the Go preference registry (`internal/api.PreferenceRegistry`). Run `make generate-docs` to regenerate it after editing the registry.

**Keys not in this table** (complex types documented by prose only):

- `metadata_languages` - JSON array of BCP 47 language tags; see the Language section in Settings.
- `artist_detail_section_order`, `artist_detail_hidden_sections` - JSON arrays of section identifiers; managed by the drag-reorder UI.
- `suppress_confirm_{action}` - dynamic per-action confirm-dialog suppression keys; `"true"` or `"false"`.

<!-- BEGIN GENERATED: prefs-reference -->
| Key | Label | Default | Allowed Values | Description |
|---|---|---|---|---|
| <a id="pref-auto_fetch_images"></a>`auto_fetch_images` | Prefetch Images | `false` | `true`, `false` | Automatically search providers for images when opening an artist's image page. |
| <a id="pref-bg_opacity"></a>`bg_opacity` | Background Opacity | `85` | 20..100 (step 5) | How opaque cards and the top bar are over the backdrop. The 85% floor keeps text AA-legible over the artwork; raise it toward 100% for fully solid surfaces. Lite mode forces opaque. |
| <a id="pref-content_width"></a>`content_width` | Content Width | `narrow` | `narrow`, `wide` | Narrow caps line length for comfortable reading. Wide fills the screen to fit more per row. |
| <a id="pref-density"></a>`density` | Layout Density | `comfortable` | `compact`, `comfortable`, `spacious` | Controls vertical spacing in lists and grids. Compact fits more content; Spacious adds breathing room. |
| <a id="pref-font_family"></a>`font_family` | Font Family | `inter` | `system`, `inter`, `atkinson` | Inter is the default. System uses your operating system's native font. Atkinson Hyperlegible boosts legibility for low-vision readers. |
| <a id="pref-font_size"></a>`font_size` | Font Size | `medium` | `small`, `medium`, `large`, `x-large`, `xx-large` | Scales text across the entire app. |
| <a id="pref-kbd_hints"></a>`kbd_hints` | Keyboard Hints | `show` | `show`, `hide` | Show or hide keyboard shortcut badges throughout the UI. |
| <a id="pref-language"></a>`language` | Language | `en` | `en` | Sets the interface language. Biography language fallbacks are configured separately in Settings. |
| <a id="pref-letter_spacing"></a>`letter_spacing` | Letter Spacing | `normal` | `normal`, `wide`, `extra-wide` | Tracks the whole UI. Extra Wide (0.05em) is a dyslexia-readability aid; Wide is 0.025em. |
| <a id="pref-lite_mode"></a>`lite_mode` | Lite Mode | `off` | `off`, `on`, `auto` | Lite mode is a single switch that turns off every visually expensive feature at once: frosted-glass blur, transitions, BlurHash placeholders that render before images load, and full-resolution image fetching. Use it if pages feel sluggish or if you would rather see thumbnails immediately than wait for full-size art. |
| <a id="pref-metadata_name_romanization_fallback"></a>`metadata_name_romanization_fallback` | Metadata Name Romanization Fallback | `true` | `true`, `false` |  |
| <a id="pref-mono_font"></a>`mono_font` | Monospace Font | `jetbrains` | `system`, `jetbrains`, `cascadia` | Used for keyboard badges, IDs, and timestamps. |
| <a id="pref-notification_enabled"></a>`notification_enabled` | Notifications | `true` | `true`, `false` | Show in-app toast notifications for rule violations. |
| <a id="pref-page_size"></a>`page_size` | Page Size | `50` | 10..500 | Rows per page on list screens (Artists, Reports). Server-side, so there is no live preview here; the value normalizes on blur. |
| <a id="pref-reduced_motion"></a>`reduced_motion` | Reduced Motion | `system` | `system`, `on`, `off` | Some users find page transitions and animated state changes distracting or motion-sensitivity inducing. Turning this on suppresses or removes those animations across the app, so panels swap and toasts appear instantly instead of fading. |
| <a id="pref-sidebar_state"></a>`sidebar_state` | Sidebar Default State | `full` | `full`, `icon-only`, `hidden` | The sidebar holds the primary navigation and can be collapsed to icons-only or expanded to show labels. This setting picks which state you land on every time you load Stillwater; you can still toggle it during a session. |
| <a id="pref-theme"></a>`theme` | Theme | `dark` | `dark`, `light`, `system` | The theme is the overall color scheme: background, panel, and accent colors all derive from it. Light, Dark, and System (follow the operating system) are the available choices. |
| <a id="pref-thumbnail_size"></a>`thumbnail_size` | Thumbnail Size | `medium` | `small`, `medium`, `large` | Size of artist artwork in lists and the action queue. Shown here at the actual pixel sizes. |
<!-- END GENERATED: prefs-reference -->

## See also

- [Customize preferences](../how-to/customize-preferences.md) - step-by-step how-to for every preference group in the drawer UI.
- [Settings reference](settings-by-tab.md) - application-level settings that require admin access.
- [API reference](../api/index.md) - full REST API specification for the `/api/v1/preferences` endpoints.
