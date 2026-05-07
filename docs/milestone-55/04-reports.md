# 04 — Reports

> Part of [Milestone 55 — v1.6.0 UX Refresh](../milestone-55.md).
>
> **Prototype (standalone, open offline):** [Proposed](prototypes/standalone/reports.html)

## Today / Proposed / Why

**Today.** Reports is a single page with a fixed list of canned reports as cards (`Missing artwork`, `Stale biographies`, `Empty albums`, etc.). Clicking a card runs the report and shows results in a table below the cards, replacing whatever was there. There is no concept of saving, scheduling, or comparing reports, and no compliance-matrix view.

**Proposed.** Reports becomes a two-pane workspace, but the **only behavioural addition this milestone is the compliance-matrix view**. Saved/named reports and scheduling are mentioned as a follow-up but are **out of scope for v1.6.0** to keep the milestone tight. Specifically:

- **Left pane (260 px):** the existing canned-report list, restyled. Each row shows name + last-run timestamp.
- **Right pane:** the active report — header (name, last run, run-now button) → filters bar (the same tri-state flyout used on Artists / Dashboard) → results table with row selection. Below the table, a **Compliance matrix** tab: artists × rules grid, cells coloured by status.
- The "Save as report" / "Schedule" / "Pin" affordances seen in earlier prototypes are **deferred**; the rail shows only built-in reports for v1.6.0.

**Why.** The compliance question — "which artists violate which rules" — is what the canned reports approximate badly. A real matrix view answers it directly and is cheap to add (the data already exists via `compliance.templ`). Saved reports + scheduling are a real feature but they introduce migrations and a runner that this milestone explicitly is **not** doing (see `migration-plan.md` §1: no DB migrations for v1.6.0). Punt them.

## UX requirements

### Reports rail (left)

- The list of built-in canned reports, in v1 order.
- Per item: name (truncated to 1 line), `last run · 4h ago`.
- Active item gets a 3 px left-edge accent (`--sw-blue`); no background swap.
- Empty state: not applicable — built-ins always exist.

### Report header (right pane top)

- Title (h1, 22 px) + last-run pill (`Last run · 4h ago`, click → opens an inline run-history disclosure listing the last 10 runs from server logs — no DB change required since runs are already log-line events).
- `Run now` primary button (calls existing `POST /api/v1/reports/{name}/run`).
- `…` overflow with `Export current results as CSV`.

### Filters bar

- Reuse the **same tri-state filter flyout** used on Artists and Dashboard. Filter families exposed for reports: severity, rule, tag, library, last-seen.
- Filter state syncs to the URL: `/v2/reports/missing-artwork?include[severity]=high&exclude[tag]=demo`.
- The flyout is a single component instance; **do not fork**.

### Results table

- Columns: severity dot · rule · artist (link to artist detail) · detail · last seen · `…`.
- Header is sticky.
- Row click opens artist detail (full page; not a drawer — drawer was an earlier prototype invention).
- **Severity `err` rows surface a `View logs` link** in the row's `…` overflow that deep-links to Logs with the filter pre-applied (`/v2/logs?rule={rule}&level=error&since=...`). Warn/info rows do not get this link. URL shape and rationale: see [`07-backend.md` § Error-only Logs deep-links](07-backend.md#error-only-logs-deep-links).
- Multi-select: shift-click range, `Cmd/Ctrl-click` toggle. Bulk actions appear in a context bar at the bottom: `Acknowledge`, `Mark resolved`, `Open in Artists`. The bar dispatches via the existing artist-list bulk endpoints (issue 02), filtered to the selected artist ids.
- Pagination is the existing `Pagination` component (no virtualization). 50 rows / page is the v1 default; keep it.

### Compliance matrix tab

A second tab at the top of the right pane: `Results | Matrix`. The matrix is the existing `compliance.templ` rendering, restyled.

- Artists down the rows, rules across the columns.
- Cells: `●` (fail, severity-tinted) / `○` (pass) / `–` (n/a).
- Hover cell → tooltip with the rule name and the most recent evidence excerpt.
- Click cell → opens the violations tab on that artist's detail page, scoped to the rule (using existing `?rule=` URL param on the detail page).
- Header-row controls: `Group rows by …` (folder, label, library), `Sort by …` (most failing first, alphabetical), `Hide passing rows` toggle.
- Sticky first column for artist names; horizontal scroll for the rule columns when more than ~30 fit.
- Performance: the existing endpoint already paginates artists; the matrix is a different presentation of the same data, not a different query.

### Edge cases

- **Run in progress** — header shows a determinate progress bar; SSE-driven via the existing `report.run` channel (no new event names this milestone).
- **No violations at all** — Results tab shows the empty state; Matrix tab shows: `Everything passes the current rules.`
- **User on `viewer` role** — same access as v1; this issue does not change roles.

### Copy strings

- Page title: `Reports`
- Empty results: `Everything passes for "{report}".`
- Empty matrix: `Everything passes the current rules.`
- Last-run pill: `Last run · 4h ago` (relative; tooltip with absolute)
- Bulk-action bar reuses the same copy as Artists list.

## Keyboard / interaction surface

| Key | Action |
|---|---|
| `/` | Focus the filters input |
| `r` | Run the current report |
| `m` | Toggle between **R**esults and **M**atrix |
| `j` / `k` | Move row focus in results table |
| `x` | Toggle row selection |
| `Shift+x` | Range-select up to focused row |
| `Esc` | Cancel range select |

## Backend dependencies

The reports page's existing endpoints are unchanged. v2 reuses:

- `GET /reports` (templ-rendered page).
- `GET /reports/{name}` — HTMX fragment for the active report's results.
- `POST /api/v1/reports/{name}/run` — runs the report; SSE events follow.
- `GET /api/v1/reports/{name}/export.csv` — CSV export (already exists for built-ins).
- `GET /api/v1/compliance/matrix?groupBy=&hidePassing=` — same endpoint that powers `compliance.templ` today.

New SSE channels (see [`07-backend.md`](07-backend.md)):

- `report.run.progress` — already emitted; now formally documented.

DB touchpoints: **none.** Saved reports / scheduling / snapshot tables are **out of scope**.

## Acceptance

### UI
- [ ] Two-pane layout matches the prototype; rail lists the v1 built-in reports in v1 order.
- [ ] Filters bar is the same tri-state flyout used by Artists and Dashboard (no fork).
- [ ] Results / Matrix tab toggle preserves filter state across the swap.
- [ ] Matrix renders artists × rules from the existing endpoint; sticky first column, horizontal scroll for rule columns.
- [ ] Bulk actions on selected results dispatch via the existing artist bulk endpoints.
- [ ] Empty states render for: zero results, zero failures in matrix, run-in-progress.

### API
- [ ] No backend changes required. Smoke test confirms v2 calls only the v1 endpoints listed above.
- [ ] `report.run.progress` SSE delivered within 250 ms of state change.

### Tests
- [ ] Cypress: open Missing artwork, apply `severity=high` include filter, assert row count drops appropriately.
- [ ] Cypress: switch to Matrix tab, click a failing cell, assert artist detail opens with the right rule scope.
- [ ] Cypress: select 5 results, mark resolved, assert the bulk action dispatches via the artist bulk endpoint.
- [ ] Existing compliance tests continue to pass.

### Docs
- [ ] `docs/reports.md` updated with the matrix tab and the explicit note that saved reports / scheduling are out of scope for v1.6.0.
- [ ] `CHANGELOG.md` line.
