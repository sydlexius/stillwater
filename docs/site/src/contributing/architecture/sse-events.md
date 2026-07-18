---
description: The Stillwater server-sent events (SSE) catalog, frame format, and Last-Event-ID reconnect/replay semantics.
---

# SSE event catalog

Stillwater pushes live updates to the browser over a single server-sent events
(SSE) stream. This page is the contract for that stream: the one endpoint, the
frame format, every event a client can subscribe to, and how reconnect replays
missed events. For how events flow internally from producers to the SSE hub, see
[Event bus and workers](event-bus-and-workers.md).

## Endpoint

```
GET /api/v1/events/stream
```

One long-lived connection per browser tab, `Content-Type: text/event-stream`.
The handler clears the write deadline so the stream stays open, sends an initial
`connected` frame, and then forwards events as they are broadcast, with a
`: heartbeat` comment every 30 seconds to keep intermediaries from closing an
idle connection.

## Frame format

Every event is written as:

```
id: <monotonic id>
event: <type>
data: <json>

```

The JSON `data` is the full event envelope:

| Field | Meaning |
|---|---|
| `id` | Monotonic per-process event id. Mirrors the SSE `id:` line; the browser echoes it back as `Last-Event-ID` on reconnect. Absent on transport-only frames (`connected`). |
| `type` | The event name (same as the `event:` line). |
| `title` | Short human-readable summary, used as a plain-toast fallback. |
| `message` | Full notification body text. |
| `timestamp` | When the event occurred (RFC 3339). |
| `data` | Optional per-event structured payload (see the catalog below). |

The `id:` line is emitted only when the event carries an id, so the `connected`
handshake never advances the client's `Last-Event-ID`.

## Catalog

These are the events broadcast to browser clients over the stream above. Their
consumers read the structured `data`; events flagged "toast" also surface a
notification on their own.

| Event | Surface | `data` payload |
|---|---|---|
| `connected` | Transport handshake; carries `{replayed, bufferLoss}` (see below). | `{replayed, bufferLoss}` |
| `scan.completed` | toast | scan summary fields |
| `bulk.completed` | toast | `{type, status}` |
| `artist.new` | toast | artist fields |
| `artist.updated` | toast | artist fields |
| `metadata.fixed` | toast | `{message}` |
| `rule.violation` | toast | `{message}` |
| `conflict.changed` | conflict banner refetch | `{banner_state}` |
| `operation.progress` | ProgressPill | `{op_id, label, processed, total, status, cancel_url?}` |
| `backdrop.collision` | warning toast + Dashboard Action Queue entry | `{dest_artist_id, dest_artist_name, colliding_artist_id, colliding_artist_name, similarity, match_count, message}` |
| `connection.push_failed` | error toast | `{connection, error_class, artist_name?}` |
| `activity.recent` | next dashboard activity rail | `{ts, kind, text, artistId?}` |
| `settings.changed` | cross-tab settings refetch/toast | `{sectionId, updatedBy, ts}` |
| `dashboard.action-resolved` | cross-tab action-queue + badge refresh | none (signal only) |

`dashboard.action-resolved` is the cross-tab counterpart of the
`dashboard:action-resolved` HTMX trigger that a resolving handler sets on its
own response: the resolving tab updates via HTMX, and the SSE event drives the
same refresh in other open tabs.

### Logs events (planned dedicated stream)

`logs.line` and `logs.throttled` are reserved in this catalog but are **not**
broadcast over `/api/v1/events/stream` -- a raw log firehose must not fan out to
every connected tab. They are **not emitted yet**: a dedicated logs stream
(`GET /api/v1/logs/stream`) is planned in #1338 and will produce them. The event
names and envelopes are documented here so #1338 can rely on a frozen contract:

| Event | `data` payload |
|---|---|
| `logs.line` | a structured log record |
| `logs.throttled` | `{dropped, window}` when the server-side rate limit sheds lines |

## Reconnect and replay

The browser `EventSource` reconnects automatically on a dropped connection,
resending the last `id:` it saw as the `Last-Event-ID` request header. The hub
retains recent events in a bounded in-memory ring buffer -- **1,000 events or 5
minutes, whichever limit is reached first** -- and on reconnect replays the
events the client missed (those with an id greater than `Last-Event-ID`) before
resuming live delivery.

The `connected` frame reports the outcome in its `data`:

| Field | Meaning |
|---|---|
| `replayed` | Number of buffered events replayed for this reconnect. |
| `bufferLoss` | `true` when the requested `Last-Event-ID` is no longer recoverable from the buffer (evicted, never issued, or unparsable). |

When `bufferLoss` is `true` the client cannot trust replay to bridge the gap and
should refetch derived state instead. This happens when the client was offline
longer than the buffer window, or after a server restart resets the id counter.

Because event ids are monotonic, the client tracks a high-water mark and toasts
each event at most once: replayed frames at or below the mark are suppressed,
while events genuinely missed while offline (ids above the mark) still toast.

## Where to look

| Topic | File |
|---|---|
| SSE hub, `Broadcast`, replay ring buffer, `Replay`, `SubscribeToEventBus` | `internal/api/handlers_sse.go` |
| Stream handler, `Last-Event-ID` replay, heartbeat | `internal/api/handlers_sse.go` (`handleSSEStream`) |
| Event type constants and bus `Publish`/`Subscribe` | `internal/event/bus.go` |
| Browser client, reconnect backoff, toast dedupe, CustomEvent dispatch | `web/static/js/sse.js` |
