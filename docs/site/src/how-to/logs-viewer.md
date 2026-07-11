---
description: Watch Stillwater's live-tailing log viewer, filter by severity/component/artist/rule, inspect a single entry, and export logs for a bug report.
---

<!-- code: web/templates/logs.templ, internal/api/handlers_logs.go. -->

# Logs viewer

The **Logs** page (`/logs`) streams Stillwater's application log in real time. On connect it backfills from the in-memory ring buffer, then live-tails new entries as they're written.

## Open the log viewer

Click **Logs** in the sidebar. A status indicator (Connecting / Connected / Reconnecting / Disconnected) at the top right shows the stream's state.

## Filter

Click **Filters** to open the flyout:

- **Level** -- a single-select minimum severity: trace, debug, info, warn, or error. Selecting a level shows that level and everything more severe.
- **Component** -- a single-select pill list, populated from the components actually present in the buffer.
- **Artist ID** and **Rule** -- free-text fields for deep-linking to a specific artist's or rule's log lines. Unlike level and component, these aren't matched by the server-side stream and are applied client-side against each entry's structured fields.

Active filters appear as dismissible chips above the log; removing a chip reconnects the stream without that facet. The toolbar's search box (`/`) narrows by matching log text.

## Read the log

Each line is color-coded by severity. Click a line to open the side drawer with its full structured content (all attributes, not just what fits the line), plus a **Copy JSON** button. Close the drawer with its close button or `Esc`.

Use **Wrap** to toggle line-wrapping for long entries. The viewer auto-follows new entries as they arrive; scrolling up pauses auto-follow, and a **Jump to bottom** button (showing a count of entries you've missed) appears so you can catch back up. **Pause** stops the stream from appending new lines without disconnecting, so you can read a moment in place.

If entries are arriving faster than the viewer can render, a throttle banner reports how many were dropped to keep the viewer responsive.

## Clear and export

**Clear** empties the on-screen log (it doesn't affect the underlying ring buffer or file). **Export for Bug Report** downloads the currently visible entries so you can attach them to an issue.

## Keyboard shortcuts

| Key | Action |
| --- | --- |
| `/` | Focus search |
| `f` | Open filters |
| `j` / `k` | Scroll down / up |
| `PgDn` / `PgUp` | Page down / up |
| `Shift G` | Jump to the bottom (latest entries) |

A shortcut legend with the same keys is printed at the bottom of the page.
