# 02 — Artists list

> Part of [Milestone 55 — v1.6.0 UX Refresh](../milestone-55.md). Read [`migration-plan.md`](migration-plan.md) §2.3 first — it lists the v1 behaviour that must survive the redesign.
>
> **Prototype (standalone, open offline):** [Proposed](prototypes/standalone/artists.html)

## Today / Proposed / Why

**Today.** `artists.templ` already implements a substantial list screen:

- **Two views**, table and grid, toggled via dedicated buttons. Both share one toolbar (search, library dropdown, filter flyout, sort, plus a column-toggle for table only).
- **Tri-state filter flyout** — every filter family is `include` / `exclude` / `neutral`. Active chips render in different classes for include (blue) vs exclude (red).
- **Compliance dot** — a 2.5 px green/yellow/red circle next to the artist name, driven by `ComplianceMap`. `nil` map = data unavailable, dot is hidden.
- **Platform badges** — Filesystem · Lidarr · Emby · Jellyfin in a specific order (discovery source first), with localised tooltips: `Found in {path}`, `Managed by {connection}`, `Present in {connection}`.
- **Bulk actions** — select-all + bulk run-rules / re-identify / fetch-images, with a `#bulk-progress-pill` that survives HTMX swaps. Bulk is **only enabled when a filter narrows the visible set** (a deliberate safety rail).
- **Off-page selection** (#1227) — selections persist across pagination via a `?ids=` URL parameter; HTMX requests can opt out via `data-clear-ids`.

**Proposed.** Visual refresh of the chrome and badges; the behaviours above stay byte-for-byte. Specifically:

- New `ArtistsV2Page` / `ArtistV2Table` / `ArtistV2Grid` templ files under `web/templates/v2/`. Same `ArtistListData`.
- Reuse the existing `BulkActionBar`, `BulkProgressPill`, `Pagination`, `FilterFlyout`, `ColumnToggle`, `StatusBadge`, `ImageStatusBadge` components — only the page-chrome layout changes.
- Tighten the toolbar: search · library · filters · sort · column-toggle · view-toggle on one row at ≥1100 px, wrapping cleanly under it.
- Refresh the platform-badge styling (smaller, monochrome with hover-tint) without changing the order, the tooltip copy, or the underlying class names that JS hooks against.
- Tighten compliance-dot rendering — same 2.5 px circle, same nil-map suppression, but rendered as a sibling of the name rather than an inline absolute.

**Why.** The list screen is the highest-traffic page after the dashboard. It already does the right things; the request from users is "make it scan faster and feel less crowded," not "rethink it." The migration plan is explicit: do not collapse the dot into a status column, do not drop the bulk-narrowing safety rail, do not break off-page selection.

## UX requirements

### Header

- Page title `Artists` + count `2,847` (live; updates with filters: `2,847 of 12,041`).
- Right side: `Run rules now` (primary), `Add artist` (link to existing add flow), `…` overflow with `Export filtered as CSV`.
- View-toggle buttons (`Table` / `Grid`) — same component instances as v1, restyled.

### Toolbar

- Single sticky row containing, left to right: search · library dropdown · filter flyout button · sort · column-toggle (table view only) · view-toggle (right-aligned).
- Filter state syncs to the URL exactly as in v1 (`?include[severity]=high&exclude[tag]=demo&library=lib1&…`). Reload preserves filters. The tri-state encoding (`include[]` / `exclude[]`) is **not** to be changed.
- Active-filter chips render below the toolbar, include-chips with `--sw-blue` border + tint, exclude-chips with `--sw-err` border + tint. Click `×` on a chip to clear that single facet; `Clear all` appears when ≥ 2 chips are active.

### Filter flyout (tri-state — preserve byte-for-byte)

- Each filter family (severity, tag, library, provider, last-scanned, has-artwork, has-biography, language) renders three radio-style chips per option: `include` · `neutral` · `exclude`.
- Default state is `neutral`; an option only enters the URL when toggled to include or exclude.
- The flyout is the **same templ component** the dashboard reuses (issue 01). Do not fork.
- Apply / Reset footer; `Reset` clears all states in the open flyout but does not commit until `Apply`.

### Table

- Columns (default order; rearrangeable + persisted in `localStorage`, same as v1):
  - `□` (checkbox)
  - cover (40 px)
  - name — rendered as `compliance-dot · artist name · platform-badge row`. Compliance dot is 2.5 px, suppressed when the row's compliance entry is `nil`.
  - tags (chips, max 3 visible + `+N`)
  - albums
  - last scanned (relative)
  - status (severity-tinted dot + label, from `StatusBadge`)
  - `…` (row menu — Re-identify / Rename Directory / Refresh Metadata / Run Rules / View on Platform — same set as artist detail's action menu, see issue 03)
- Sticky header.
- Click row body → artist detail (full page).
- Click cover or name → same.
- Row hover: subtle background only; no row-level affordance until the row is selected.

### Grid

- Cards: cover (full width) · name (with compliance dot) · tag chips (max 2) · platform-badge row · status pill at the corner of the cover.
- Card grid uses CSS `grid-template-columns: repeat(auto-fill, minmax(220px, 1fr))`. No virtualization in grid view (the page-size cap from pagination keeps it bounded).

### Pagination + off-page selection (#1227)

- Pagination is **the existing component** (`Pagination` templ); no switch to virtualized infinite scroll. Server-side cursor stays as-is.
- Selection persists across pages via the `?ids=...` URL parameter. Requests that should not propagate `ids` carry `data-clear-ids` on the originating element — the existing global HTMX hook strips it. v2 markup must keep the `data-clear-ids` opt-out wherever v1 has it (search submit, filter apply, pagination next/prev are the canonical examples).
- Selection toolbar above the table reads: `12 selected · Select all matching · Clear`. `Select all matching` adds the current filter expression to `?ids=__filter__` (or whatever the v1 sentinel is — copy from `artists.templ` verbatim; do not invent a new one).

### Bulk actions

- The bulk action bar is the **existing `BulkActionBar`** templ component — only the wrapper layout changes.
- Bulk is **only enabled when a filter narrows the visible set**; the bar shows a tooltip when disabled: `Apply at least one filter to enable bulk actions on the full library.`
- Actions exposed: `Run rules`, `Re-identify`, `Fetch images`. (These are the v1 actions. Do not add new ones in this issue; new bulk actions are a separate milestone.)
- `#bulk-progress-pill` is rendered into the page chrome (not the table) so it survives HTMX swaps. Preserve the id literally — v1 JS targets it.

### Compliance dot — rules

- Source: `ComplianceMap[artistID]` → `pass` / `warn` / `fail` (or whatever v1 enum) → green / yellow / red.
- `nil` map (e.g. compliance hasn't been computed yet) → **hide the dot entirely**, do not render a grey placeholder. The migration plan calls this out specifically.
- Tooltip: the rule body and severity, identical to v1.

### Platform badges — rules

- Order: discovery source first, then the rest in canonical order (Filesystem · Lidarr · Emby · Jellyfin).
- Tooltip strings preserved verbatim — they are localised and live in the existing translation bundles. Do not reword in v2.
- Hidden for connections that don't apply (artist not present in that platform).

### Edge cases

- **Empty (no artists yet)** — `No artists yet — finish onboarding or run a library scan.`
- **Empty (filtered)** — `No artists match these filters. · Clear filters`.
- **Selection across pages with stale ids** — server-side de-stales (existing v1 behaviour); v2 must not introduce client-side dedup that masks the dedupe-on-server.
- **Bulk dispatched then user clears filter** — the running job is unaffected; the bar disables again until a filter is reapplied.

### Copy strings

- Page title: `Artists`
- Selection toolbar: `{n} selected · Select all matching · Clear`
- Bulk-disabled tooltip: `Apply at least one filter to enable bulk actions on the full library.`
- Empty (filtered): `No artists match these filters. · Clear filters`
- Run-rules confirm: same copy as v1; do not reword.

## Keyboard / interaction surface

| Key | Action |
|---|---|
| `/` | Focus the search input |
| `j` / `k` | Move row focus (table view only) |
| `x` | Toggle selection on focused row |
| `Shift+x` | Range-select |
| `Esc` | Clear selection |
| `Enter` | Open focused artist |
| `g` `t` | Switch to **T**able view |
| `g` `g` | Switch to **G**rid view |

## Backend dependencies

The list's existing endpoints are unchanged. v2 reuses:

- `GET /artists` (templ-rendered page) — same handler renders v2 template under `/v2/artists`.
- `GET /artists/list` — HTMX fragment for the table/grid body, query string carries the tri-state filters.
- `POST /api/v1/artists/bulk/run-rules`, `…/bulk/re-identify`, `…/bulk/fetch-images` — unchanged. The bulk-progress-pill subscribes to the existing channel.
- `GET /api/v1/compliance` — feeds `ComplianceMap`; unchanged.

New SSE channels (see [`07-backend.md`](07-backend.md)):

- `bulk.progress` — already emitted by v1, now formally documented.

DB touchpoints: **none.**

## Acceptance

### UI
- [ ] Both views (table + grid) render under `/v2/artists` with the existing toolbar components reused, not forked.
- [ ] Tri-state filter flyout produces the same URL shape as v1 (verify by diffing the query string for an identical filter set).
- [ ] Compliance dot appears at 2.5 px, hidden when `ComplianceMap` is nil.
- [ ] Platform badges render in canonical order with the v1 tooltip strings.
- [ ] Bulk action bar is disabled until a filter narrows the set; tooltip explains.
- [ ] `#bulk-progress-pill` survives HTMX swaps (regression test: trigger a bulk job, navigate to a different filter, pill still shows progress).
- [ ] Off-page selection via `?ids=` works across pagination; `data-clear-ids` opt-outs honored.
- [ ] All states implemented: empty (no artists), empty (filtered), loading, bulk-disabled.

### API
- [ ] No backend changes required. Smoke test confirms v2 calls only the v1 endpoints listed above.

### Tests
- [ ] Cypress: select 5 artists across two pages, run rules, assert all 5 are in the resulting bulk job.
- [ ] Cypress: tri-state filter — set severity `include high` and tag `exclude demo`, assert URL and result count.
- [ ] Cypress: bulk-progress-pill — start a job, navigate to a different filter, pill still visible with current progress.
- [ ] Unit: compliance-dot suppression on nil map (snapshot test of rendered HTML).
- [ ] The existing `artists_offpage_selection_test.go` and `artists_pagination_listener_test.go` continue to pass against v2 templates.

### Docs
- [ ] `docs/artists.md` updated with the new chrome; explicitly notes that endpoints, filter encoding, and selection contract are unchanged.
- [ ] `CHANGELOG.md` line.
