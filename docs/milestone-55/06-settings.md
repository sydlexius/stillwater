# 06 — Settings (sectioned rail)

> Part of [Milestone 55 — v1.6.0 UX Refresh](../milestone-55.md).
>
> **Prototypes (standalone, open offline):**
> · [Proposed](prototypes/standalone/settings.html)
> · [Current](prototypes/standalone/settings-current.html)

## Today / Proposed / Why

**Today.** Settings is 11 horizontal tabs above an infinite-scroll content pane: General, Providers, Connections, Libraries, Automation, Rules, Users, Auth, Maintenance, Logs, Updates. Tabs overflow on narrower viewports and hide behind a `…` chevron. There is no search; finding "biography minimum length" means knowing it lives under Rules, scrolling, and Ctrl+F-ing the page. (Note: in v2, **Logs is promoted out of Settings to a top-level screen** — see issue 05.)

**Proposed.** A left rail with four collapsible groups (`Essentials`, `Data`, `Integrations`, `System`) holding 16 sections, plus a filter input at the top of the rail that hits both labels and *content keywords* (so typing "biography" highlights Providers, Languages, and Rules — not just sections with the word in the title). The right pane shows one section at a time and is deep-linkable via `#section-id`.

**Why.** 16 sections is too many for tabs and too few to justify a separate page per section. A rail with a filter is the standard pattern when the section count crosses ~8. The keyword index is what makes the filter useful — without it, search degenerates into "did I remember the section name."

## UX requirements

### Rail (left, 260 px)

- Sticky `position: sticky; top: 0` inside the page scroll container.
- Filter input pinned at the top: `placeholder="Filter settings…"`, `/` focus shortcut (when nothing else holds focus).
- Four groups, each with a chevron header that collapses the group:

| Group | Sections |
|---|---|
| Essentials | General, Music libraries, Platform profile |
| Data | Metadata providers, Languages, Rules & severity, Schedule |
| Integrations | Servers (Emby, Jellyfin, Lidarr), Webhooks & notifications, API tokens |
| System | Users, Auth providers, Configuration file, Maintenance, Updates, Per-user preferences |

(Logs is no longer here — see issue 05.)

- Each rail item shows: icon · label · count badge where applicable (e.g. `9` next to Providers = 9 configured providers, `22` next to Rules = 22 rules).
- Active section: left-edge accent (`--sw-blue`, 3 px), label semi-bold, no background swap.

### Filter behaviour

- Filter matches **labels** *and* **keywords**. The keyword set per section lives in code as a static array — see `settings.jsx` `settingsGroups` for the canonical list.
- Match displays an inline `↳ <matched-keyword>` line under the section in the rail, telling the user *why* the section showed up.
- Empty filter result: the rail reads `No settings match "xyz".` with a `Clear` button.
- Filter persists in `localStorage('sw-settings-filter')` until cleared — convenient for an admin reading docs in another tab.

### Per-user preferences section (new in v1.6.0)

A new section under the **System** group, distinct from the org-level Settings sections above. It edits keys on the per-user preferences endpoint (`PATCH /api/v1/preferences`, see [`07-backend.md`](07-backend.md#per-user-preferences-issue-06-new-section)), not the global settings table.

The section already exists in v1 with cards for Theme, Layout, Typography, Accessibility, Behavior, Language. v1.6.0 adds one new card:

#### Card: "Artist detail layout"

- Two-column layout inside the card.
- **Left column** — a drag-handle list of all reorderable sections (Bio, Artwork, Providers, Findings, Activity, Compliance). Each row: drag handle · section label · eye-toggle (visible / hidden). Drag-reorder updates `artist_detail_section_order`; eye-toggle updates `artist_detail_hidden_sections`.
- **Right column** — a small live preview tile showing the order as a vertical stack of labeled bands. Updates as the user reorders or toggles visibility.
- **Identifiers is shown above the list as a fixed disabled row** with a lock icon and the helper text: `Always pinned to the top.` It cannot be dragged or hidden — the renderer pins it regardless. (Out-of-band attempts to write `identifiers` into either preference key are tolerated by the renderer per the spec; the UI just doesn't expose it.)
- Footer: `Reset to default order` button — clears both keys back to defaults and re-renders the live preview.

The card uses the existing `<SettingRow>` component for the header strip; the drag-list and preview are new sub-components under `web/templates/v2/preferences_artist_detail_layout.templ`.

### Pane (right, fills remainder)

- Single-section scroll. No deep links inside a section — if a section's body needs sub-jumps, that's a sign it should be split.
- Section header: title (h1, 22 px) + one-line description + actions row (right-aligned: `Reset to defaults`, `Export section as TOML`).
- Form rows use the existing `<SettingRow>` (`label`, `desc`, `children`) — same component the v1 prototype uses.
- Save semantics:
  - **Auto-save** on blur for everything except destructive actions (matches v1).
  - **Destructive actions** (delete a library, revoke a token, factory-reset rules) prompt a small confirm modal: `Type the name to confirm.` Existing v1 modal hooks reused.
  - A toast confirms each save: `Saved · undo` (5 s undo window for reversible changes), via the existing `showUndoToast` global.

### Edge cases

- **Filter then collapse a group** → group stays expanded if it has matches; collapse is per-user choice but matched groups override.
- **Section with 0 items** (e.g. zero rules configured) → pane shows the empty state for that section, not a generic empty.
- **Section with > 50 items** (Rules) → the pane gets its own inline filter, separate from the rail filter.
- **Two browser tabs editing the same setting** → last-write-wins; a new SSE event `settings.changed` reloads the form and shows a toast `Updated by another session`.

### Copy strings

- Page title: `Settings`
- Rail filter placeholder: `Filter settings…`
- Empty rail: `No settings match "{q}". · Clear`
- Save toast: `Saved · undo`
- Cross-tab toast: `Updated by another session`

## Keyboard / interaction surface

| Key | Action |
|---|---|
| `/` | Focus the rail filter |
| `Esc` | Clear the rail filter (if focused) |
| `↑` / `↓` | Move selection within the rail (when filter focused) |
| `Enter` | Activate the highlighted rail section |
| `⌘S` (in pane) | Save the focused row immediately (otherwise it auto-saves on blur) |

## Backend dependencies

Settings already has v1 endpoints; v2 reuses them all. v2 adds **only** an SSE channel for cross-tab notifications.

Existing:

- `GET /settings` (templ-rendered page) — same handler renders v2 template under `/v2/settings`.
- `GET /settings/{sectionId}` — HTMX fragment for the active pane.
- `PATCH /api/v1/settings/{sectionId}` — partial update.
- `POST /api/v1/settings/{sectionId}/reset` — reset to defaults.
- `GET /api/v1/settings/{sectionId}/export` — TOML representation.

New (this milestone):

- SSE: `settings.changed` — `{ sectionId, updatedBy, ts }`. Fires on every successful PATCH.

DB touchpoints: **none.** v1 already records `updated_by_user_id` and `updated_at` on the settings table.

## Acceptance

### UI
- [ ] All 16 sections render under the four groups, in the order listed above. Logs is **not** here.
- [ ] Rail filter matches labels + keywords; matching keyword shown inline.
- [ ] Active section deep-links via `#section-id` (`/v2/settings#providers`).
- [ ] Auto-save fires on blur; toast appears with undo via the existing `showUndoToast` global.
- [ ] Destructive actions require typed confirmation (existing modal hooks).
- [ ] No horizontal-tab-overflow regression (the bug we're fixing).
- [ ] **Per-user preferences section** — "Artist detail layout" card renders, drag-reorder writes `artist_detail_section_order`, eye-toggle writes `artist_detail_hidden_sections`, live preview updates without a network round-trip, `Reset to default order` clears both keys.
- [ ] **Identifiers row** in the layout card is fixed at the top, disabled, labeled `Always pinned to the top.` It does not respond to drag.

### API
- [ ] No backend changes required. Smoke test confirms v2 calls only the v1 endpoints listed above.
- [ ] `settings.changed` SSE event fires within 200 ms of a successful PATCH.

### Tests
- [ ] Unit: keyword index — every section has at least 4 keywords.
- [ ] Cypress: open settings, type "biography", assert three sections highlight (Providers, Languages, Rules).
- [ ] Cypress: open settings in two tabs, edit in one, assert the other refreshes within 1 s.

### Docs
- [ ] `docs/settings.md` rewritten around the four-group structure; explicitly notes that Logs has moved to its own top-level screen.
- [ ] `docs/keyboard.md` updated with `/` and `⌘S` for this screen.
- [ ] `CHANGELOG.md` line.
