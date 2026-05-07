# Milestone 55 — v1.6.0 UX Refresh

> **Status:** spec / pre-implementation
> **Owner:** @syd
> **Implementer audience:** an LLM coding agent picking up issues from this milestone, with @syd reviewing.
> **Source of truth for the v1 → v2 mapping:** [`migration-plan.md`](milestone-55/migration-plan.md), which is grounded in the actual `web/templates/*.templ` source. The per-screen issues defer to it where they conflict.
> **Source of truth for designs:** the in-repo prototypes under `docs/prototypes/screens/*.html` (proposed) and `docs/prototypes/screens/*-current.html` (today's UI, recreated for contrast). The companion JSX/CSS bundle sits at `docs/prototypes/`.

## Goals

v1.6.0 is a coordinated UX **redesign** across Stillwater's main screens — not a rewrite and not a feature release. The HTMX + templ + Tailwind stack stays. Every interactive surface continues to be `hx-get` / `hx-post` returning HTML fragments. v2 is new templ files under `web/templates/v2/` rendered by the same handlers against the same `/api/v1/*` endpoints; the existing global JS contracts (CSRF + base-path injection, toast manager, `dashboard:action-resolved` event, off-page selection via `?ids=`) remain.

The six screen-level changes ship together because they share infrastructure (the v2 layout wrapper `LayoutV2`, the new sidebar/bottom-tab chrome, and a small set of new SSE channels). Shipping them piecemeal would mean writing the chrome twice.

## Non-goals (v1.6.0)

These are explicitly **out of scope**, even though the prototype gestures at them:

- Plugin system / third-party metadata providers beyond the existing six (MusicBrainz, Fanart.tv, TheAudioDB, Discogs, Last.fm, Spotify, Wikipedia).
- Multi-user roles beyond the existing `admin` / `viewer` split. The Users settings pane keeps its current shape.
- i18n / localization. Copy is English-only this milestone; the `Languages` settings pane only governs *biography* language fallbacks.
- Mobile / small-screen layouts. The proposal targets ≥ 1100 px; phone work is its own milestone.
- The matcher engine itself. Reports show the *output* of matching; nothing in v1.6.0 changes how matches are produced.
- Any change to the on-disk artist directory layout (`/music/{Artist}/{Album}/`).

## Scope at a glance

| # | Screen / Concern | Issue | Prototype |
|---|---|---|---|
| 01 | Dashboard / Action queue | [`docs/milestone-55/01-dashboard.md`](milestone-55/01-dashboard.md) | [proposed](milestone-55/prototypes/standalone/dashboard.html) · [current](milestone-55/prototypes/standalone/dashboard-current.html) |
| 02 | Artists list (table + grid) | [`docs/milestone-55/02-artists.md`](milestone-55/02-artists.md) | [proposed](milestone-55/prototypes/standalone/artists.html) |
| 03 | Artist detail | [`docs/milestone-55/03-artist-detail.md`](milestone-55/03-artist-detail.md) | [hi-fi](prototypes/screens/artist-detail.html) |
| 04 | Reports | [`docs/milestone-55/04-reports.md`](milestone-55/04-reports.md) | [proposed](milestone-55/prototypes/standalone/reports.html) |
| 05 | Logs viewer | [`docs/milestone-55/05-logs.md`](milestone-55/05-logs.md) | [hi-fi](prototypes/screens/logs.html) |
| 06 | Settings — sectioned rail | [`docs/milestone-55/06-settings.md`](milestone-55/06-settings.md) | [proposed](milestone-55/prototypes/standalone/settings.html) · [current](milestone-55/prototypes/standalone/settings-current.html) |
| 07 | Cross-cutting backend (SSE channels, v2 chrome) | [`docs/milestone-55/07-backend.md`](milestone-55/07-backend.md) | — |

Open them in order — each issue assumes you've at least skimmed the previous one. The backend doc is referenced from every screen issue and consolidates new endpoints, SSE event names, and DB-shape changes.

Standalone prototype bundles under [`docs/milestone-55/prototypes/standalone/`](milestone-55/prototypes/standalone/) are self-contained single-file HTML — drag-droppable onto any GitHub issue, work offline.

## Cross-cutting: dual-route the new UI behind a flag

v2 is a UX flag, not an API version. Both UIs render against the **same** `/api/v1/*` endpoints and the same handlers — only the templ files (and a thin chrome wrapper) differ.

**Mechanism:**

- Mount the new UI at `/v2/*` and the existing UI at `/*` for the entire milestone. Same handlers, same endpoints, swapped templates.
- Add an env var `STILLWATER_UX` with values `v1` (default), `v2`, or `dual` (default during the milestone).
  - `v1` — only `/` is mounted; `/v2/*` returns 404.
  - `v2` — `/` redirects to `/v2/`; old paths return a deprecation HTML page with a link.
  - `dual` — both mount points are live; root serves an opt-in chooser the first time a session arrives without a cookie.
- Per-user opt-in cookie `sw_ux=v2` (or `=v1`) overrides the env default. Set it from a toggle in the new Settings → General pane: **"Use v1.6 interface · beta"**.
- A response header `X-Stillwater-UX: v1|v2` so QA can verify which build served a request without inspecting cookies.
- Log line on every request: `ux=v1|v2 user=<id> path=<…>` so we can see uptake during the beta window.

**Removal plan:** when v1.6.0 is GA + 1 minor release, flip default to `v2`, keep `v1` available behind the env var for one more minor release, then delete the v1 templates and the dual-routing code in v1.8.0. Tracked separately, not in this milestone.

The per-screen issues each assume both surfaces ship together; none of them depends on the other being deployed first.

## Global keyboard shortcuts

Defined globally in v1.6.0; per-screen shortcuts are listed in each screen's own issue.

| Key | Action |
|---|---|
| `⌘K` / `Ctrl+K` | Open command palette (from anywhere) |
| `Esc` | Close command palette / dismiss drawer / clear focused field |
| `g` then `d` | Go to **D**ashboard |
| `g` then `a` | Go to **A**rtists |
| `g` then `r` | Go to **R**eports |
| `g` then `l` | Go to **L**ogs |
| `g` then `f` | Go to **F**indings (admin diagnostic page — kept for ops) |
| `g` then `s` | Go to **S**ettings |
| `/` | Focus the screen's primary search/filter input |
| `?` | Open the keyboard-shortcuts cheat sheet (small modal listing this table) |

The leader (`g`) is captured globally and ignored when focus is in an `<input>`, `<textarea>`, or `contenteditable` element. The leader window is **1.5 s**; press the second key within that window or the leader resets silently.

`?` (the cheat sheet) is new in v1.6.0.

## Acceptance for the milestone as a whole

- [ ] All seven issues closed.
- [ ] `STILLWATER_UX=dual` runs both UIs against the same DB without errors in a clean container.
- [ ] `STILLWATER_UX=v2` boot serves `/v2/dashboard` as the index for an authenticated session.
- [ ] No regression in v1 paths under `STILLWATER_UX=v1` (smoke test: dashboard, artists list, artist detail with field-level locks, refresh-panel after Refresh Metadata, undo flow on a fix, off-page selection via `?ids=`).
- [ ] All `/api/v1/*` endpoints continue to serve both v1 and v2 chrome with byte-identical payloads.
- [ ] `CHANGELOG.md` entry under `## [1.6.0]` lists every user-visible change with screenshots or short clips.
- [ ] Docs site updated: `docs/configuration.md` for the new env var, `docs/keyboard.md` for the shortcuts table.

## How to read each issue

Each `docs/milestone-55/*.md` file uses the same five-block structure:

1. **Today / Proposed / Why** — three short paragraphs framing the change.
2. **UX requirements** — component breakdown, states, edge cases, copy strings.
3. **Keyboard / interaction surface** — what the screen adds to the global shortcut table.
4. **Backend dependencies** — endpoints, SSE events, DB touchpoints (cross-linked to issue 07).
5. **Acceptance checklist** — checkboxes grouped under **UI / API / Tests / Docs**.

Anything in code-fence blocks is a strawman — names, payload shapes, copy strings — meant to be edited during implementation, not transcribed verbatim.
