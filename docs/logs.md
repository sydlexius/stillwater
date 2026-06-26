# Live Log Viewer (next/ channel)

The next/ channel ships a live, streaming log viewer at `/next/logs`. It tails
the in-memory ring buffer over Server-Sent Events (SSE), renders each record as a
structured line (timestamp, severity, component, message, pinned attributes), and
supports filtering, pausing, wrapping, a full-entry drawer, and a one-click bug
report export. The page is administrator-only.

## How it works

The page itself renders only the chrome. Log lines arrive over a single SSE
connection to `GET /api/v1/logs/stream`, which **self-backfills** on connect: it
replays recent ring-buffer entries (oldest-first, capped at 500) and then
live-tails new records on the same connection. There is no separate backfill
fetch.

The stream is administrator-only (the route is wrapped in
`middleware.RequireAdmin`). The page route uses optional auth with an in-handler
admin check, so an unauthenticated browser visitor gets the login page rather
than a JSON 401.

## Query parameters

The stream accepts three server-side filters:

| Param   | Meaning                                                     |
| ------- | ----------------------------------------------------------- |
| `level` | Minimum severity (`trace`, `debug`, `info`, `warn`, `error`) |
| `scope` | Component, exact match                                      |
| `q`     | Case-insensitive substring match on the message            |

The page reads these from its own URL as `level`, `component`, and `q`. Two extra
deep-link filters, `artist_id` and `rule`, are applied client-side against each
record's structured attributes (the stream predicate covers only level / scope /
search). All active filters AND-combine.

The viewer keeps the URL in sync via `history.replaceState`, so the current view
is always bookmarkable. Example deep-links:

- `/next/logs?level=error` — errors only
- `/next/logs?level=error&artist_id=1234` — errors for one artist
- `/next/logs?component=scanner&q=timeout` — scanner timeouts

`artist:NNNN` chips on a line link out to `/next/artists/{id}`; `rule:XXX` chips
filter the view by that rule.

## Reconnect and replay

Each emitted line carries an SSE `id` (the record's RFC3339Nano timestamp). On a
dropped connection the browser's `EventSource` automatically reconnects with the
`Last-Event-ID` header, and the server replays only entries newer than that
cursor. The status indicator shows Live / Connecting / Reconnecting accordingly.

## Rate limiting (throttle banner)

Each subscriber has a bounded buffer. When a burst overflows it, the broadcaster
drops the overflow line rather than blocking the logging goroutine and raises a
throttle signal; the server emits a `logs.throttled` event carrying the dropped
count. The viewer surfaces this as a banner ("Dropped N entries to keep up.").
This is back-pressure protection, not a hard 200 lines/s cap.

## Controls

- **Search** (`/`) — message substring filter (debounced).
- **Level buttons** — set the minimum severity; click the active level again to
  clear it.
- **Filters** (`f`) — flyout for component, artist ID, and rule.
- **Pause / Resume** — stop auto-following the tail; new lines still arrive and
  buffer, with a count shown on the "Jump to Bottom" button.
- **Wrap** — toggle line wrapping; the preference is persisted via
  `swPreferences` when available.
- **Clear** — `DELETE /api/v1/logs` clears the ring buffer.
- **Export for Bug Report** — copies a Markdown summary (recent error lines,
  active filters, page URL, user agent) to the clipboard.
- **Jump to Bottom** — resumes auto-follow and scrolls to the newest line.
- Clicking a line opens a side drawer with the full entry and a Copy JSON button.

Keyboard: `/` focus search, `f` open filters, `j` / `k` scroll the viewer, and
`g l` (owned by the shared navigation registry) jumps to this page.

## Historical logs beyond the ring buffer

The live tail is bounded by the in-memory ring buffer. For older entries, use the
disk log files when file logging is configured; the stable settings log viewer
exposes a file picker over `GET /api/v1/logs/files`.

## Troubleshooting

- **No live lines / status stuck on Connecting** — confirm you are signed in as an
  administrator and the `next` UX channel is enabled (`SW_UX=next` or `dual`). A
  reverse proxy that buffers responses can also stall SSE; the stream sets
  `X-Accel-Buffering: no` for nginx, but other proxies may need response
  buffering disabled for `text/event-stream`.
- **Throttle banner keeps appearing** — the log volume is outrunning the
  subscriber buffer. Narrow the view with a level or component filter to reduce
  the matched volume.
- **A deep-link opens unfiltered on one axis** — an invalid `level` value is
  dropped (not rejected) so a stale bookmark still loads.
