# 05 — Logs viewer

> Part of [Milestone 55 — v1.6.0 UX Refresh](../milestone-55.md).
>
> Prototype: [`docs/prototypes/screens/logs.html`](../prototypes/screens/logs.html).
>
> Source-truth references: [`internal/logging/ringbuffer.go`](../../internal/logging/ringbuffer.go),
> [`internal/logging/ringhandler.go`](../../internal/logging/ringhandler.go),
> [`internal/logging/filereader.go`](../../internal/logging/filereader.go).
> The `LogEntry` shape (`Time`, `Level`, `Message`, `Component`, `Source`, `Attrs`)
> and the `LogFilter` shape (`Level`, `Search`, `Component`, `After`, `Limit`)
> are already implemented — v2 is a UI on top of contracts that exist.

## Today / Proposed / Why

**Today.** Logs is a Settings sub-section: a textarea-style scroll of the last N log lines from the server, with a level filter dropdown (`debug` / `info` / `warn` / `error`) and a refresh button. Lines are rendered as plain monospace text; nothing is clickable. To follow a long-running scan you reload the page.

**Proposed.** Promote Logs to a top-level screen (`/v2/logs`, with `g l` shortcut) and replace the static dump with a **live tail** backed by SSE. Specifically:

- Stream new log lines into the bottom of the view as they're written, with auto-follow that pauses when the user scrolls up.
- Per-line structured rendering: timestamp · level pill · scope chip · message · optional trailing meta (`artist_id:1234`, `rule:bio.length`, `duration:412ms`).
- Click an `artist_id:NNNN` chip → open that artist detail in a new tab.
- Filter facets: level, scope (e.g. `scanner`, `rules`, `provider.musicbrainz`), free-text. Filters apply both to backfill and to the live tail.
- Search across the **on-disk** log buffer (the last ~100 MB; existing rotated-log pattern), not just what's currently in memory.

**Why.** Self-hosters debug Stillwater by tailing the server log. They are already SSH'ing in and running `tail -f`; bringing that into the UI removes the SSH step for the common case (scan looks stuck, did artwork fetch fail, why is provider X slow). It also gives the dashboard's right-rail "Recent activity" a destination — `View all in Logs` — which it currently doesn't have.

## Decisions locked with prototype

The prototype reconciles five points the original spec didn't pin down:

1. **Rotated logs are a follow-up, but the UI is shaped for them now.** The header has a time-range picker (`Last 1h` / `Last 24h` / `Last 7d` / Custom). Until the rotated-log search backend lands, anything beyond the in-memory ring buffer renders as a greyed-out range with a tooltip: `Rotated-log search is not yet available.` This means the rotated-log issue ships as a backend-only PR that lights up an existing UI affordance — no v2 UI redesign needed.
2. **Errors-first is a tally chip, not a default filter.** Above the stream: `12 errors · 4 warns in last hour`, click-to-filter. The default stream is all-levels.
3. **Attrs are hybrid-density.** `LogEntry.Attrs` is rendered as **pinned chips inline** (`artist_id`, `request_id`, `provider`, `error` — see PINNED_ATTRS below) plus a chevron that opens an inline panel under the row showing the rest, with raw JSON and the `Source` (`probe.go:142`).
4. **Row density follows the existing `data-density` token.** Comfortable (default) and Dense, persisted to the same user preference key the rest of the app uses.
5. **Bug-report export prefills the existing GitHub issue template.** `Open in bug report` opens a new tab with the body pre-filled with: filter state, last 200 visible lines (already filtered), Stillwater version, and the same redaction rules used by the support-bundle exporter (path strings, MBIDs preserved; bearer tokens, API keys, and absolute home paths redacted).

`PINNED_ATTRS = ["artist_id", "request_id", "provider", "error", "rule"]` — first match wins; remaining attrs go behind the chevron. The list is a per-page constant in v1.6.0; making it user-configurable is a follow-up.

## UX requirements

### Layout

- Single-column page under `LayoutV2`. No sidebar within the page.
- Header strip:
  - Title `Logs` + a `Live · paused` pill that toggles auto-follow.
  - Right side: `Download buffer` (downloads the last 100 MB of rotated logs as a `.tar.gz`, server-side stream; existing endpoint).
- Filter bar (sticky, below header):
  - Level dropdown (multi-select chips: `debug` · `info` · `warn` · `error`).
  - Scope multi-select — populated from the distinct scopes seen in the buffer; same component pattern as Artists tag chips.
  - Free-text search (`/` focus, debounce 200 ms).
- Log body — a single virtualized scroller filling the rest of the viewport.

### Line rendering

Each row renders one `LogEntry`. Default density is Comfortable; Dense is one click away in the toolbar (and persists via the same preference key as Artists density).

```
14:22:03.412   INFO    scanner   Started scan for library "Main"   library:1   artists:2,847
14:22:03.418   WARN    provider.musicbrainz   429 backoff 30s   provider:musicbrainz   retry:1                ⌄
14:22:04.001   ERROR   rules    Rule body too long for "Mastodon"   artist_id:8821   rule:bio.length          ⌄
```

- **Timestamp**: `HH:mm:ss.SSS`, monospace, faint colour.
- **Level pill**: severity-tinted (`debug` faint grey, `info` neutral, `warn` `--sw-warn`, `error` `--sw-err`). Click → adds `level:X` to the active filter.
- **Component chip** (`LogEntry.Component`): monospace, max 28 chars, truncated with `title=`. Click → adds `component:X` to the active filter.
- **Message**: regular weight in Comfortable, monospace in Dense. Truncated with title attr if it exceeds 1 line; expand opens the row.
- **Pinned attr chips**: rendered inline in row order, monospace, `key:value`. Click → adds that attr to the active filter. `artist_id:NNNN` is also a deep link to artist detail (modifier-click or middle-click opens in the artist tab; plain click adds the filter).
- **Chevron** (`⌄`): only present when the row has un-pinned attrs OR a `Source`. Toggles an inline expander showing remaining attrs as chips, the `Source` (`probe.go:142`, copy-on-click), and the raw JSON line under a `Show JSON` disclosure.
- **Hover**: shows a small `Copy line` button at the right edge.

### Live-tail behaviour

- On open, the page backfills the last 500 lines (server endpoint cursor-based) and then subscribes to the live SSE stream.
- New lines append to the bottom. If the user is within 60 px of the bottom, the view auto-follows (`Live · streaming` pill, green dot). If they scroll up, auto-follow pauses (`Live · paused` pill, yellow dot) and a `Jump to bottom` button appears in the bottom-right; clicking it resumes auto-follow.
- Pasting the URL `/v2/logs?since=2025-03-14T14:22:00Z` opens the page anchored at that timestamp and disables auto-follow until the user clicks `Jump to bottom`.

### Search and filters

- Filters are AND-combined; multi-select facets are OR within a facet.
- Filter state syncs to the URL.
- Both backfill and the live stream are filtered server-side; the SSE channel takes the active filter as a query string and only emits matching lines.
- Empty state when filters yield nothing in the backfill: `No log lines match these filters in the buffer. — Live tail still active.` with a `Clear filters` button.

### Inbound deep-links (error-only contract)

Other surfaces in the app deep-link to Logs **only when reporting an error**. Logs must accept all of the following query-string parameters as initial filter state and pre-populate the filter bar accordingly:

| Param | Source | Effect |
|---|---|---|
| `component` | finding `evidence: "logs:component:level"` parsing | seeds the scope multi-select with that single component |
| `level` | same; or explicit `level=error` from a toast | seeds the level multi-select; if `error`, deselects everything else |
| `artist_id` | finding's owning artist; toast's failed-action artist | adds an `artist_id:NNNN` chip to the active filter |
| `rule` | reports error chip | adds a `rule:XXX` chip to the active filter |
| `since` | absolute or relative (`since=ago.1h`) | anchors the time-range picker; auto-follow is **disabled** until the user clicks `Jump to bottom` |

When inbound deep-link params are present, the page boots with the filter bar visibly populated and a small dismissable banner at the top: `Filtered from a {finding|toast|report} · Clear filters to see everything.` The banner's action clears all params and resets to live tail.

The contract is **inbound-only and URL-only**. Logs does not call back into other surfaces. See [`07-backend.md` § Error-only Logs deep-links](07-backend.md#error-only-logs-deep-links) for the full surface list.

### Edge cases

- **Buffer rolled mid-search** — the page shows a banner: `Older lines have rolled out of the buffer. — Reload to start fresh.`
- **SSE disconnect** — pill switches to `Reconnecting…` (yellow dot, 2 s pulse). Backfill catches up on reconnect (server replays from `Last-Event-ID`).
- **Massive line volume** (10k+ lines/s during a heavy scan) — server rate-limits the SSE channel to 200 lines/s and posts a `Throttled · 8,200 lines suppressed in last 5s` banner. Full data is still on disk; the buffer download is the escape hatch.
- **Line longer than 2 KB** — truncated in the row with an `Expand` toggle that reveals the full line in a side drawer.

### Copy strings

- Page title: `Logs`
- Live pill: `Live · streaming`, `Live · paused`, `Reconnecting…`
- Throttle banner: `Throttled · {n} lines suppressed in last 5s`
- Empty filter result: `No log lines match these filters in the buffer. — Live tail still active.`
- Buffer-rolled banner: `Older lines have rolled out of the buffer. — Reload to start fresh.`
- Copy toast: `Copied`

## Keyboard / interaction surface

| Key | Action |
|---|---|
| `/` | Focus the search input |
| `j` / `k` | Move row focus |
| `Enter` | Open the focused line in the side drawer (full raw line + parsed meta) |
| `c` | Copy focused line to clipboard |
| `g g` | Jump to top of buffer |
| `G` | Jump to bottom and resume auto-follow |
| `f` | Toggle `Wrap` |
| `Esc` | Close the side drawer |

## Backend dependencies

Logs already has v1 endpoints; v2 formalises the SSE channel.

Existing:

- `GET /api/v1/logs?level=&scope=&q=&cursor=&limit=` — backfill with cursor pagination.
- `GET /api/v1/logs/download` — buffer `.tar.gz` download.

New (this milestone):

- `GET /api/v1/logs/stream?level=&scope=&q=` (SSE) — live-tail. Filters are honoured server-side. Rate-limited to 200 lines/s with a `logs.throttled` event when shedding.

SSE channel (see [`07-backend.md`](07-backend.md)):

- `logs.line` — `{ ts, level, scope, message, meta }`.
- `logs.throttled` — `{ suppressedCount, windowMs }`.

DB touchpoints: **none.** Logs are file-backed (rotated on disk).

## Acceptance

### UI
- [ ] Page reachable via sidebar nav and `g l` shortcut.
- [ ] Backfill loads the last 500 lines on open; live tail subscribes after.
- [ ] Auto-follow pauses on scroll-up, resumes on `Jump to bottom`.
- [ ] Per-line: timestamp, level pill, scope chip, message, trailing meta chips render correctly.
- [ ] `artist_id:NNNN` and `rule:XXX` chips are clickable and open the right destinations.
- [ ] Filters AND-combine; URL syncs; backfill and live-tail both honour filters.
- [ ] Throttle banner appears when `logs.throttled` SSE fires.
- [ ] Wrap toggle, side-drawer expand for long lines, copy-on-click — all work.

### API
- [ ] `GET /api/v1/logs/stream` honours `level`, `scope`, `q` server-side.
- [ ] Stream rate-limits at 200 lines/s and emits `logs.throttled` correctly.
- [ ] `Last-Event-ID` replay works for reconnects within the in-memory ring buffer.

### Tests
- [ ] Cypress: open `/v2/logs`, assert the last 500 lines render and live tail begins.
- [ ] Cypress: scroll up, assert pill switches to `paused` and `Jump to bottom` appears.
- [ ] Cypress: filter `level=error`, assert only error lines render in both backfill and live.
- [ ] Cypress: trigger a heavy scan in a fixture, assert the `Throttled` banner appears.
- [ ] Unit: SSE filter parser — `level=warn,error&scope=scanner&q=foo` produces the expected predicate.

### Docs
- [ ] `docs/logs.md` (new) — describes the live-tail flow and the disk-buffer escape hatch.
- [ ] `CHANGELOG.md` line.
