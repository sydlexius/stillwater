# Milestone 55 — v1 → v2 Migration Plan

This plan reflects the **actual** v1 implementation as read directly from the
codebase (`web/templates/layout.templ`, `dashboard.templ`, `artists.templ`,
`artist_field.templ`). It supersedes any earlier prototype that was written
without seeing the source.

## 1. Architecture (do not rewrite)

The v1 stack is HTMX + templ + Tailwind, server-rendered. v2 is a **redesign**,
not a rewrite. Keep:

- **Server-side templ + HTMX** — every interactive surface is `hx-get` /
  `hx-post` returning HTML fragments. Do not introduce a SPA.
- **CSRF + base path injection** — the global `htmx:configRequest` listener in
  `layout.templ` prefixes paths and adds `X-CSRF-Token`. v2 markup must use
  absolute paths (`/api/v1/...`) so this hook applies.
- **Dual routing flag** — `STILLWATER_UX=v1|v2|dual`, `/v2/*` paths, per-user
  opt-in cookie. New templ files live under `web/templates/v2/`; the chrome in
  `layout.templ` is shared via a small `LayoutV2` wrapper that swaps in the v2
  Sidebar/BottomTabs but keeps every script tag, modal, and toast handler.
- **Asset pipeline** — `AssetPaths` already carries cache-busted URLs for
  every JS bundle. v2 reuses it; do not fork.

## 2. Surfaces I had wrong in earlier prototypes — must be fixed in v2

### 2.1 Artist Detail
- **Fanart slideshow header** is the page's signature: full-bleed cycling
  background, white-on-image title (or artist logo image instead of H1).
  Earlier prototype had no version of this. **Required for v2.**
- **Real tab set**: Overview / Images / Providers / Discography / History /
  Violations / Debug (debug is conditional on a setting + Emby/Jellyfin
  connection). Earlier prototype invented Overview/Activity/Issues/Files.
- **Field-level locks** via `FieldLockToggle` — every field has its own
  open/closed lock icon next to the label. There's also a chip panel listing
  locked fields. Whole-artist lock is separate. v2 must surface both.
- **Action menu** — Re-identify (or Identify if no MBID), Rename Directory,
  Refresh Metadata, Run Rules, View on Platform (one per Emby/Jellyfin/Lidarr
  connection). Earlier prototype omitted Re-identify and Rename Directory.
- **Aliases section** + **Members section** with field-provider provenance —
  both completely absent from earlier prototype.
- **Provider IDs as their own tab**: MBID, AudioDB, Discogs, Wikidata, Deezer,
  each clickable as an external link. Below: per-connection Platform State
  cards (Emby/Jellyfin/Lidarr), lazy-loaded.
- **Refresh-panel pattern** — Refresh Metadata and Run Rules drop a status
  panel below the header (`#refresh-panel`) showing what providers were
  queried and what changed. SSE-driven; do not collapse this into a toast.
- **`FieldDisplay` is the workhorse** — every metadata field renders through
  it. v2 must keep the same component contract:
  - Display mode shows value + label + lock toggle + edit/clear/fetch actions
  - Edit mode swaps the same id for an inline form (`hx-patch ...fields/{name}`)
  - Provider-fetch opens `FieldProviderModal` with side-by-side comparison
    and Use this / Merge buttons
  - Locked fields hide pencil/clear/fetch — lock is **enforced visually**

### 2.2 Dashboard / Action Queue
- The action queue is **already polished**. v2 changes are restraint-first:
  - Keep the search bar + filter flyout + active-filter chips + load-more.
  - Keep severity badges, category-color left border, fixable / dismiss / view
    buttons per card.
  - The header's compact metrics (artist count | health % | last-evaluated)
    must remain — but the layout can compress to a single bar.
- **Undo flow is real product** — every fix lands an `UndoToast` (30-ish
  second window), and the global toast manager has a dedicated `'undo'` type
  with countdown + reload of the action queue on revert. v2 must not break
  this; in particular the queue reload preserves the current filter query.
- **`dashboard:action-resolved`** is dispatched on body when an undo timer
  expires — the violations-tab badge listens for it. Keep the event name.
- **Health-stats fallback** — when GetHealthStats fails the data carries
  `HealthStatsError=true` and the UI must show `---` instead of a confident
  zero. v2 redesigns must preserve this distinction.

### 2.3 Artists List
- **Two views** — table and grid, toggled via dedicated buttons. Both share
  the same toolbar (search, library dropdown, filters flyout, sort, column
  toggle for table only).
- **Filter flyout** is multi-state: each filter is `include` / `exclude` /
  `neutral` (tri-state), not just on/off. Active chips render with different
  classes for include vs exclude (blue vs red). v2 must keep the tri-state.
- **Compliance dot** — green/yellow/red 2.5px circle next to artist name,
  driven by `ComplianceMap`. `nil` map = data unavailable, hide dot. Don't
  swallow this into a "status" column — the dot is intentional.
- **Platform badges** — Filesystem, Lidarr, Emby, Jellyfin badges in a
  specific order (discovery source first), with localised tooltips
  ("Found in {path}", "Managed by {connection}", "Present in {connection}").
- **Bulk actions** — select-all, bulk run-rules / re-identify / fetch-images
  with progress pill (`#bulk-progress-pill`) that survives HTMX swaps.
  Bulk only enabled when a filter narrows the visible set. **v2 must keep
  this safety rail.**
- **Off-page selection scope (`ids` URL param, #1227)** — when a user
  selects N artists then paginates, the selection persists via `?ids=...`.
  A `data-clear-ids` attribute opts a request out. v2 must replicate.

## 3. Phasing

### Phase 0 (one PR, foundation)
1. Add `web/templates/v2/` directory + `LayoutV2(title, assets)` that wraps
   `Layout` but pulls v2 sidebar/bottom-tab components.
2. Add the `STILLWATER_UX` env flag and `/v2/*` route registration in
   `internal/server/router.go` (no handlers yet — the flag just controls
   which template the existing handlers render).
3. Add the per-user opt-in cookie (`sw_ux=v2`) read by middleware.

### Phase 1 (Dashboard v2 — issue 01)
- New `DashboardV2ActionQueue` template. Reuse `ActionQueueData` unchanged.
- Change is purely visual + density; the API (`/dashboard/actions`,
  `/api/v1/notifications/{id}/fix`, `/api/v1/fix-undo/{id}`) is identical.
- Verify undo flow, filter persistence, SSE reload still work.

### Phase 2 (Artists list v2 — issue 02)
- New `ArtistsV2Page`, `ArtistV2Table`, `ArtistV2Grid`. Same `ArtistListData`.
- Reuse `BulkActionBar`, `BulkProgressPill`, `Pagination`, `FilterFlyout`,
  `ColumnToggle`, `StatusBadge`, `ImageStatusBadge` — only the page chrome
  changes.
- Tri-state filters, compliance dot, platform badges, off-page-selection
  contract all preserved.

### Phase 3 (Artist detail v2 — issue 03)
- This is the heaviest one. New `ArtistV2DetailPage` with the fanart
  slideshow header, real tab set, refresh-panel, aliases/members sections.
- **Reuse `FieldDisplay`/`FieldEdit` as-is.** Do not fork the field-display
  workhorse — wrap it in new section layouts instead.
- Reuse `FieldLockToggle`, `FieldProviderModal`, the action-menu confirm
  modal hooks.
- Add the platform-state lazy-load cards by hitting existing
  `/api/v1/artists/{id}/platform-state/{conn}` (already used by v1 debug tab).

### Phase 4 (Reports / Logs / Settings — issues 04, 05, 06)
- Lower-traffic surfaces. Pure visual redesign; APIs unchanged.

### Phase 5 (Backend — issue 07)
- No backend changes are required for v1 parity. The "backend" issue covers
  only the new SSE channels and any v2-specific endpoints (e.g. the
  fanart-slideshow source list, if not already exposed).

## 4. What we are NOT doing

- Not rewriting to React/Vue/Svelte. HTMX + templ stays.
- Not consolidating tabs into a single scroll page on artist detail —
  the tabbed layout is load-on-demand for a reason (debug + platform state
  are expensive).
- Not removing the ambient backdrop, login background image, or the
  shared-filesystem bar — these are part of the visual identity.
- Not changing the toast manager API (`showToast`, `showSuccessToast`,
  `showWarningToast`, `showStickyToast`, `showUndoToast`,
  `showConfirmDialog`). v2 markup calls the same globals.

## 5. Acceptance for milestone 55 as a whole

- `STILLWATER_UX=v1` (default): byte-identical to today.
- `STILLWATER_UX=v2`: every page renders the v2 template; every interaction
  in the v1 acceptance criteria still works (undo, bulk, filters,
  field-locks, identify, refresh-panel, SSE updates).
- `STILLWATER_UX=dual` + cookie: per-user opt-in, easy rollback.
- No backend migration required to flip the flag.
