# 01 — Dashboard

> Part of [Milestone 55 — v1.6.0 UX Refresh](../milestone-55.md). Read [`migration-plan.md`](migration-plan.md) §2.2 first — it lists the v1 dashboard behaviour that must survive the redesign.
>
> **Prototypes (standalone, open offline):**
> · [Proposed](prototypes/standalone/dashboard.html)
> · [Current](prototypes/standalone/dashboard-current.html)

## Today / Proposed / Why

**Today.** The v1 dashboard is `dashboard.templ` with `ActionQueueData`. It is **already polished**: a header strip with three compact metrics (`artist count` · `health %` · `last evaluated`), then the action queue — a search bar, a filter flyout, active-filter chips, severity badges, a category-color left-border per card, and per-card `Fix` / `Dismiss` / `View` buttons with a `Load more` at the bottom. Fixes drop an `UndoToast` (~30 s window) and dispatch the `dashboard:action-resolved` body event when the timer expires; the violations-tab badge listens for it. When `GetHealthStats` fails the page carries `HealthStatsError=true` and the metrics render `---` instead of a confident zero.

**Proposed.** A v2 redesign that is **restraint-first**. Same `ActionQueueData`, same `/api/v1/notifications/{id}/fix`, `/api/v1/fix-undo/{id}`, and dashboard-action endpoints. Visual changes only:

- Compress the three header metrics into a single bar that sits above the queue (instead of three stacked tiles), keeping the `---` health-error fallback.
- Tighten the action-card row so two-line cards fit in the viewport at 1100 px (severity dot + category accent + artist + reason + age + actions, all on one row when copy fits, otherwise stacking the action row beneath).
- Put the search input + filter flyout + active-filter chips on a single sticky toolbar that follows the queue scroll.
- Replace the v1 right-side empty whitespace at ≥1440 px with a sticky **Recent activity** rail (SSE-driven, capped at 50, drops on overflow — Logs is for the rest).

Everything else — the undo flow, filter-preserving reload after revert, the `dashboard:action-resolved` event, the load-more affordance, the `Fix` / `Dismiss` / `View` button set per card, the severity colour tokens — is unchanged.

**Why.** The v1 dashboard is the most-loved screen in the app; users complained about it taking a tall column and leaving the right half of the viewport blank, not about how it works. The redesign reclaims horizontal space (activity rail) and density (single-row cards) without reaching for any of the buttons that already work.

## UX requirements

### Layout

- Two-column grid, `minmax(0, 1fr) 320px`. Right rail collapses below 1100 px to a stacked card under the queue.
- Page chrome from the new `LayoutV2` wrapper (sidebar + page header). No screen-specific chrome.

### Header strip (compact)

A single row, `height: 56px`, `--sw-paper` background, `--sw-divider` bottom border. Three groups, separated by a vertical divider.

| Group | Label | Value | Notes |
|---|---|---|---|
| 1 | `Artists` | `2,847` | Click → `/v2/artists` |
| 2 | `Library health` | `87%` (with a 28 px inline donut to the left of the number) | Clicking does not filter — there is no per-tile filter affordance in v2; filtering happens in the queue toolbar |
| 3 | `Last evaluated` | `4h ago` (relative; tooltip with absolute) | Click → opens the run-rules confirm modal |

When `HealthStatsError=true`, group 2 reads `Library health · ---` with a tooltip `Health stats unavailable — check Logs`. Do not render a `0%` donut in this case; render the donut as a faint dashed circle.

### Action queue (left column)

- Sticky toolbar:
  - Search input (`placeholder="Search violations…"`, `/` focus shortcut, debounce 200 ms, hits the existing endpoint with `q=`)
  - Filter flyout button (preserves the v1 flyout markup; tri-state per filter family — see Artists list, the same component is reused)
  - Active-filter chips (close-icon per chip, `Clear all` when ≥ 2)
  - Right-aligned: `Re-run rules` button (opens the existing confirm modal) and a count meta `7 of 64`.
- Cards: severity dot · category-color left border (4 px, `--sw-cat-{category}`) · artist name (link → artist detail) · short reason · age (relative) · action row (`Fix`, `Dismiss`, `View`).
- Card states match v1: a card in the middle of a fix shows a spinner inside `Fix`; a fixed card collapses to a 28 px-tall undo strip (`Fixed · undo` + countdown) for the duration of the undo window; on undo timer expiry the strip animates out and `dashboard:action-resolved` is dispatched.
- Empty state: `No action needed — your library is compliant against the current rules.` with a `Re-run rules` button.
- Loading state: 7 skeleton rows (preserve v1 skeleton).
- Error state: a single banner row `Couldn't load violations — retry` with retry button.
- `Load more` button at the bottom — same contract as v1 (HTMX swap appends without losing scroll position).

### Recent activity (right column, ≥1100 px only)

- Header: `Recent activity` + meta `SSE live` (a small green dot animating).
- Row schema: timestamp (relative) · icon · short text · optional artist link.
- Capped at 50 in-memory; older items are dropped with no "load more" — Logs is for that.
- When the SSE stream disconnects, the meta switches to `Reconnecting…` (yellow dot, 2 s pulse) and items keep accruing locally; they backfill on reconnect.
- Below 1100 px the rail is hidden entirely. (It is a "nice to have" sidebar, not a primary surface — Logs is the primary.)

### Filter persistence (must not regress)

The v1 filter flyout writes filters to the URL (`?severity=high&category=artwork&…`). On undo, the queue reloads via HTMX **with the current query string preserved** so the user does not lose context. v2 must replicate this byte-for-byte; this is one of the easy regressions to introduce when reskinning.

### Edge cases

- **`HealthStatsError=true`** — group 2 shows `---`, donut is faint dashed, tooltip explains. Do not show `0%`.
- **No violations at all** — header group 3 still shows last-evaluated; queue body shows the empty state.
- **More than 99 violations** — meta reads `7 of 412`; no abbreviation in the meta.
- **Recent activity empty on first boot** — `Stillwater is idle — kick off a scan from Artists ▸ Run rules now.` with a link.
- **Undo timer expires while the queue is mid-load-more** — the load-more append must not duplicate the resolved row; rely on the existing server-side cursor (don't dedupe client-side).
- **Two cards from the same artist** — both render; v1 already supports this; v2 must not collapse them.

### Copy strings

- Page title: `Dashboard`
- Donut label: `Library health`
- Health-error tooltip: `Health stats unavailable — check Logs`
- Time-ago copy: `12s ago`, `4m ago`, `2h ago`, `yesterday`, `Mar 14`. No "just now".
- Undo strip: `Fixed · undo · 28s`

## Keyboard / interaction surface

| Key | Action |
|---|---|
| `/` | Focus the action-queue search input |
| `r` (when queue focused, no input focused) | Open `Re-run rules` confirm |
| `j` / `k` | Move card focus down / up in the queue |
| `Enter` | Open the focused card's artist detail |
| `f` | Toggle the filter flyout |
| `u` (while undo strip is visible on focused card) | Trigger undo |

Global shortcuts (`g d`, `g a`, etc.) are listed in [`milestone-55.md`](../milestone-55.md#global-keyboard-shortcuts).

## Backend dependencies

The dashboard's existing endpoints are unchanged. v2 reuses:

- `GET /dashboard` (templ-rendered HTML) — same handler renders v2 template under `/v2/dashboard`.
- `GET /dashboard/actions?q=&severity=&category=&cursor=` — same HTMX fragment endpoint.
- `POST /api/v1/notifications/{id}/fix` — unchanged.
- `POST /api/v1/fix-undo/{id}` — unchanged.
- `POST /api/v1/rules/run` — unchanged.
- `GET /api/v1/health/stats` — unchanged; v2 must respect the existing `HealthStatsError=true` carrier.

New SSE channel (see [`07-backend.md`](07-backend.md)):

- `activity.recent` — for the right-rail. Payload `{ ts, kind, text, artistId? }`. The channel exists in v1 but is not currently consumed; v2 wires it up to the rail.

DB touchpoints: **none.**

## Acceptance

### UI
- [ ] Compact header strip matches `screens/dashboard.html` proposal at ≥ 1100 px and falls back gracefully under it.
- [ ] `HealthStatsError=true` renders `---` and the dashed donut.
- [ ] Action queue cards keep all v1 affordances (`Fix` / `Dismiss` / `View`, severity dot, category accent, undo strip, load-more).
- [ ] Filter flyout is the **same** templ component used on the Artists list (no fork).
- [ ] On undo, the queue reloads preserving the current query string.
- [ ] `dashboard:action-resolved` body event fires on undo-timer expiry; the violations-tab badge updates.
- [ ] Recent activity rail reflects live `activity.recent` SSE events within 1 s of dispatch.
- [ ] All states implemented: loading, empty, error, disconnected-SSE.

### API
- [ ] No backend changes required for v1 parity. Smoke test confirms v2 calls only the v1 endpoints listed above.
- [ ] `activity.recent` SSE stream survives a backend restart on the client side (auto-reconnect with backoff capped at 30 s).

### Tests
- [ ] Cypress: load `/v2/dashboard`, fix a violation, assert `UndoToast`, click undo, assert the row reappears in the same filter context.
- [ ] Cypress: simulate `HealthStatsError=true` (test fixture), assert `---` and dashed donut.
- [ ] Cypress: dispatch a synthetic `activity.recent` SSE event, assert it lands in the rail.
- [ ] Cypress: filter by `severity=high`, fix a card, undo, assert the URL still carries `severity=high`.

### Docs
- [ ] `docs/dashboard.md` updated with the new layout + screenshots; explicitly notes that endpoints are unchanged.
- [ ] `CHANGELOG.md` line under `## [1.6.0]`.
