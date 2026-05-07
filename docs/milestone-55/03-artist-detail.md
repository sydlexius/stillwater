# 03 — Artist detail

> Part of [Milestone 55 — v1.6.0 UX Refresh](../milestone-55.md). Read [`migration-plan.md`](migration-plan.md) §2.1 first — this is the heaviest issue in the milestone and the one that earlier prototypes got wrong.
>
> Prototype: [`docs/prototypes/screens/artist-detail.html`](../prototypes/screens/artist-detail.html) (working hi-fi, drives `artist-detail.jsx` + `artist-detail-data.jsx` from `docs/prototypes/`).

## Today / Proposed / Why

**Today.** `artist_detail.templ` is the largest single template in the app (≈70 KB). It already implements:

- **Fanart slideshow header** — a full-bleed cycling background of fanart sources behind the page title. When an `artistlogo` image exists, it's rendered in place of the H1; otherwise the artist name is in white-on-image. This is the page's signature.
- **Real tab set:** Overview · Images · Providers · Discography · History · Violations · Debug. Debug is conditional on a setting + an Emby/Jellyfin connection.
- **Field-level locks** via `FieldLockToggle` — every field has its own open/closed lock icon next to the label. There is also a chip panel listing locked fields. A separate whole-artist lock exists alongside.
- **Action menu** (top-right) — `Re-identify` (or `Identify` if no MBID), `Rename Directory`, `Refresh Metadata`, `Run Rules`, `View on Platform` (one entry per Emby/Jellyfin/Lidarr connection).
- **Aliases section** + **Members section** (for groups/bands) with field-provider provenance.
- **Provider IDs tab** — MBID, AudioDB, Discogs, Wikidata, Deezer; each is a clickable external link. Below the IDs: per-connection **Platform State cards** (Emby/Jellyfin/Lidarr), lazy-loaded from `/api/v1/artists/{id}/platform-state/{conn}`.
- **Refresh-panel pattern** — `Refresh Metadata` and `Run Rules` drop a status panel below the header (`#refresh-panel`) showing what providers were queried and what changed. SSE-driven.
- **`FieldDisplay` workhorse** — every metadata field renders through it. Display mode shows value + label + lock toggle + edit/clear/fetch actions. Edit mode swaps the same id for an inline form (`hx-patch …/fields/{name}`). Provider-fetch opens `FieldProviderModal` with side-by-side comparison + `Use this` / `Merge` buttons. **Locked fields hide pencil/clear/fetch — the lock is enforced visually.**

**Proposed.** A v2 redesign that keeps every behaviour above and changes only:

- Header layout — the slideshow stays, but the title block, action menu, and refresh-panel mount point are repositioned for clearer hierarchy and to leave a stable surface for the `#refresh-panel` insertion.
- Tab strip — restyled, but **the tab set and the conditional-debug rule are unchanged**.
- Aliases + Members — promoted into Overview-tab cards (rather than buried in a sub-accordion) so the field-provider provenance is more visible.
- Provider IDs tab — restyled as a pair of card stacks (IDs, then Platform State), keeping the lazy-load.
- `FieldDisplay` itself: **unchanged.** v2 wraps it in new section layouts but does not fork it.

**Why.** Every earlier prototype invented a different tab set (`Overview/Activity/Issues/Files`), forked or reimagined `FieldDisplay`, and dropped the Aliases/Members sections. The migration plan is unambiguous: those are required v1 surfaces, and the field workhorse is not negotiable. This issue exists primarily to **prevent the redesign from regressing v1**.

## UX requirements

### Page chrome

- `LayoutV2` shell, sidebar visible.
- Full-bleed slideshow header, 280 px tall (was 320 px in v1; the modest tightening is intentional). Image-cycle interval and source list are unchanged.
- **Sticky mini-header** appears once the slideshow header scrolls out of the viewport (~280 px scroll). Contents: back link · artist name · finding count by severity (small chip cluster) · `Re-run rules` · `Edit`. Height 48 px, `--sw-paper` background, `--sw-divider` bottom border. The mini-header is **not** rendered until needed; toggle on `IntersectionObserver` against a sentinel placed at the bottom of the slideshow header. The page's normal action menu (kebab) remains in the slideshow header; the mini-header surfaces only the two most-used actions.
- Top-right of the header: action menu trigger (kebab). Menu contents:
  - `Identify` (when `MBID` is empty) **or** `Re-identify` (when set) — opens existing confirm modal
  - `Rename Directory` — opens existing confirm modal
  - `Refresh Metadata` — drops the refresh-panel below the header
  - `Run Rules` — drops the refresh-panel
  - `View on Emby` / `View on Jellyfin` / `View on Lidarr` — one entry per configured connection; suppressed when none
- Below the header: the `#refresh-panel` mount point (empty by default; populated by HTMX swap when an action runs). The id is preserved literally — v1 JS hooks against it.

### Tab strip

Tabs in this exact order:

1. `Overview`
2. `Images`
3. `Providers`
4. `Discography`
5. `History`
6. `Violations`
7. `Debug` (conditional: setting enabled **and** an Emby/Jellyfin connection exists)

Tab strip is sticky to the top of the scroll container under the slideshow. Active tab gets a 2 px bottom accent (`--sw-blue`); no background swap.

### Overview tab

The default landing tab. Contents, in order:

1. **Locked fields panel** — chip list of the artist's locked fields (e.g. `name · biography · disambiguation`). Clicking a chip scrolls to that field in the layout below. Hidden when no fields are locked. The whole-artist lock toggle sits on the right side of this panel.
2. **Identity card** — name (FieldDisplay), sort name (FieldDisplay), disambiguation (FieldDisplay), type (group/person, FieldDisplay).
3. **Biography card** — biography (FieldDisplay, multi-line), language fallbacks honoured.
4. **Aliases card** — list of aliases with per-row field-provider provenance (the existing FieldProvider chips). `+ Add alias` link in the card header.
5. **Members card** (only when `type=group`) — member rows with field-provider provenance per row. `+ Add member` link in the card header.
6. **Tags card** — current tag chips + `+ Add tag` inline.

### Images tab

Reuses `artist_images_tab.templ` byte-for-byte. v2 wraps it; v2 does not fork it.

**Lightbox — Crop & Trim (new in v1.6.0).** When a user opens an image in the lightbox, the footer surfaces two new actions in addition to the existing ones (`★ Set as primary`, `Replace`, `Download`, `Delete`):

- **Crop** — visible for all artwork kinds. Opens an in-lightbox crop overlay with rule-of-thirds grid, 8 handles, dim mask, and aspect chips. Default aspect chip matches the kind: Primary 1:1, Backdrop 16:9, Banner 5.33:1, Logo Free; "Free" is always available as an alternate chip. Apply creates a **new** artwork item alongside the original (does not replace); the lightbox re-targets to the new item so the user can promote it with the star. Cancel reverts to view; Esc in crop mode reverts (Esc in view closes the lightbox).
- **Trim** — logo only (chip hidden for other kinds). Side-by-side before/after preview with a single "edge threshold" slider (how aggressive to trim transparent / near-solid borders). Dashed inset on the After preview shows what's getting cut. Apply also creates a **new** artwork item.

Neither action mutates the original. Both go through the existing `POST /api/v1/artists/{id}/artwork` endpoint with a `derived_from` field referencing the source artwork id; the server stores the new image as a new artwork row in the existing artwork table. **No DB migration.**

### Providers tab

Two stacked sections:

1. **Provider IDs** — MBID, AudioDB, Discogs, Wikidata, Deezer. Each row: provider name · ID value (FieldDisplay, edit allowed unless locked) · external-link icon. Clicking the icon opens the provider's site for that ID in a new tab.
2. **Platform State** — one card per Emby/Jellyfin/Lidarr connection configured. Cards are **lazy-loaded** via `hx-get="/api/v1/artists/{id}/platform-state/{conn}" hx-trigger="revealed"` (same pattern v1 uses on the debug tab; reuse it here).

### Discography tab

Reuses `artist_discography.templ`.

### History tab

Reuses `artist_history.templ`.

### Violations tab

Reuses `artist_violations_tab.templ`. Listens for `dashboard:action-resolved` (body event from issue 01) to update its badge count.

### Debug tab (conditional)

Reuses the existing debug tab. Only rendered when:

- `Settings → General → Show debug tab` is on, **and**
- At least one Emby or Jellyfin connection exists.

When the conditions aren't met the tab is omitted from the strip entirely (not greyed out).

### Field-level finding chips

When a `FieldDisplay` row's underlying field carries a `finding`, render a severity-tinted chip inline with the value (immediately after the source pill). Chip label is `finding.title` only — never `message`. Hover/focus opens a popover with `message`, `suggestedFix`, and (when `severity === "err"` and `evidence` is set) a `View logs` link that deep-links to Logs with the filter pre-applied.

The finding string contract (`title`/`message`/`suggestedFix`/`rule`/`evidence`), i18n key path (`findings.{rule}.{title|message|fix}`), and error-only deep-link URL shape are defined in [`07-backend.md` § Finding string contract](07-backend.md#finding-string-contract). Do not reproduce the rules here — reference the canonical doc.

### `FieldDisplay` — preserve the contract

Every metadata field on this page renders via `FieldDisplay`. The contract from v1 must hold:

| State | Renders |
|---|---|
| Display, unlocked | value · label · lock-open toggle · pencil · clear · fetch (provider) |
| Display, locked | value · label · lock-closed toggle. **No pencil, no clear, no fetch.** |
| Edit | inline form (`hx-patch /api/v1/artists/{id}/fields/{name}`); same id as display so HTMX swaps cleanly |
| Provider-fetch | opens `FieldProviderModal` with side-by-side compare + `Use this` / `Merge` buttons |

The lock is enforced *visually* in v1 (the buttons are removed from the DOM, not just disabled). v2 keeps this.

### Refresh panel

When the action menu fires `Refresh Metadata` or `Run Rules`:

1. Server returns an HTMX fragment that replaces `#refresh-panel`'s contents with a status panel.
2. The panel shows: action name · spinner · provider list (one row per provider being queried) · cumulative changes log.
3. Provider rows update via SSE on the existing channel; on completion each row shows `✓ {n} fields updated` or `✓ no changes` or `✗ {error}`.
4. The panel can be dismissed via a close button; dismissing does not cancel the action (the action is fire-and-forget once dispatched).
5. **Do not collapse this into a toast.** The migration plan is explicit. Toasts are for confirmations; the refresh panel is informational and lives below the header until dismissed.

### Per-user section reorder/hide preference (deferred surface; render-side wired now)

The artist detail page renders sections in the order specified by the per-user preference key `artist_detail_section_order` (see [`07-backend.md`](07-backend.md#per-user-preferences-issue-06-new-section)) and hides sections listed in `artist_detail_hidden_sections`. The renderer **always pins Identifiers to the top** regardless of stored value — it is the page's anchor (artist name + MBID + monitor toggle). Reorderable items: Bio, Artwork, Providers, Findings, Activity, Compliance.

The **UI to edit** these keys lives in Settings (issue 06, new "Per-user preferences" section). v1.6.0 ships the renderer-side support; the editing UI is delivered alongside in issue 06. Clients that have neither key set get the default order documented in 07-backend.

### Edge cases

- **No fanart configured** — slideshow falls back to a solid `--sw-paper-2` header. Title is rendered in the foreground colour, not white-on-image.
- **`artistlogo` exists but loads slowly** — H1 is rendered as text first, then swapped in when the logo loads. Avoid layout shift via reserved height.
- **Locked field with a pending provider-fetch** — disallowed; the lock removes the fetch button before it can be clicked. Defensive server-side check rejects a stray request with 409.
- **Identify with no MBID** — `Identify` not `Re-identify` in the action menu; copy difference is intentional.
- **Lidarr-only artist (no Emby/Jellyfin)** — Platform State card stack still renders for Lidarr; debug tab is suppressed (debug is gated on Emby/Jellyfin specifically).
- **Members section with provenance from multiple providers per row** — render up to 3 provider chips, then `+N` for the rest, in a tooltip.

### Copy strings

- Action menu items: `Identify`, `Re-identify`, `Rename Directory`, `Refresh Metadata`, `Run Rules`, `View on {connection}` — preserve verbatim from v1.
- Locked-fields panel header: `Locked fields · click to jump`
- Whole-artist lock label: `Lock this artist` / `Unlock this artist`
- Refresh-panel close: `Dismiss`

## Keyboard / interaction surface

| Key | Action |
|---|---|
| `e` (when a field row is focused) | Open that field's edit form |
| `l` (when a field row is focused) | Toggle that field's lock |
| `f` (when a field row is focused) | Open the provider-fetch modal for that field |
| `r` | Open `Refresh Metadata` confirm |
| `R` | Open `Run Rules` confirm |
| `1` … `7` | Jump to tab N |
| `Esc` | Close action menu / refresh-panel / provider-fetch modal |

## Backend dependencies

The detail page's existing endpoints are unchanged. v2 reuses:

- `GET /artists/{id}` (templ-rendered page).
- `GET /artists/{id}/tab/{tab}` — HTMX tab swap.
- `PATCH /api/v1/artists/{id}/fields/{name}` — field edit.
- `POST /api/v1/artists/{id}/fields/{name}/lock` — field lock toggle.
- `POST /api/v1/artists/{id}/lock` — whole-artist lock toggle.
- `GET /api/v1/artists/{id}/fields/{name}/providers` — fetch comparison data for `FieldProviderModal`.
- `POST /api/v1/artists/{id}/refresh` — drops refresh panel.
- `POST /api/v1/artists/{id}/run-rules` — drops refresh panel.
- `POST /api/v1/artists/{id}/identify` — identify / re-identify.
- `POST /api/v1/artists/{id}/rename` — rename directory.
- `GET /api/v1/artists/{id}/platform-state/{conn}` — lazy-load Platform State card.

New SSE channels (see [`07-backend.md`](07-backend.md)):

- `artist.refresh.progress` — already emitted by v1 from the refresh runner; now formally documented.

DB touchpoints: **none.**

## Acceptance

### UI
- [ ] Slideshow header renders with image cycling at the v1 cadence; falls back gracefully on no-fanart.
- [ ] `artistlogo` is preferred over the text H1 when present; reserved height prevents layout shift.
- [ ] **Sticky mini-header** appears when the slideshow header scrolls out, contains back link · name · finding count chips · Re-run rules · Edit; disappears when scrolling back up.
- [ ] Tab set matches v1 (Overview · Images · Providers · Discography · History · Violations · Debug); debug only shows when both gating conditions hold.
- [ ] Action menu has all six entries, with the `Identify` / `Re-identify` swap based on MBID presence.
- [ ] Refresh panel mounts at `#refresh-panel` and SSE-updates per provider row; dismiss does not cancel.
- [ ] `FieldDisplay` is the same component v1 uses (no fork); locked fields hide pencil/clear/fetch in the DOM.
- [ ] **Field-level finding chips** render `finding.title` only; popover surfaces `message` + `suggestedFix`; severity `err` rows surface a `View logs` link with deep-link URL.
- [ ] Aliases and Members cards render with field-provider provenance per row.
- [ ] Provider IDs tab shows all five IDs as external links; Platform State cards lazy-load on reveal.
- [ ] Whole-artist lock toggle is separate from per-field locks and persists.
- [ ] **Lightbox Crop** action visible for all artwork kinds; aspect chip defaults match kind; Apply creates new artwork (does not replace); lightbox re-targets to new item.
- [ ] **Lightbox Trim** action visible only for `kind=logo`; before/after preview with edge-threshold slider; Apply creates new artwork.
- [ ] Sections render in the order from `artist_detail_section_order` preference; sections in `artist_detail_hidden_sections` are omitted; **Identifiers always renders first** regardless of stored value.

### API
- [ ] No backend changes required. Smoke test confirms v2 calls only the v1 endpoints listed above.
- [ ] Defensive 409 on field-edit / field-fetch when the field is locked.

### Tests
- [ ] Cypress: edit a field via inline form, assert the FieldDisplay swaps back with the new value.
- [ ] Cypress: lock a field, assert pencil/clear/fetch are removed from the DOM (not just disabled).
- [ ] Cypress: `Refresh Metadata`, assert refresh-panel appears, SSE rows update, dismiss panel without cancelling the run.
- [ ] Cypress: artist with no MBID shows `Identify`; an artist with MBID shows `Re-identify`.
- [ ] Cypress: debug tab suppressed when no Emby/Jellyfin connection exists.
- [ ] Existing `artist_violations_tab_test.go` and `artist_field_members_apply_test.go` pass unchanged against v2 templates.

### Docs
- [ ] `docs/artist-detail.md` (new) — documents the v2 layout, calls out that `FieldDisplay`, refresh-panel, action menu, and tab set are preserved from v1.
- [ ] `CHANGELOG.md` line.
