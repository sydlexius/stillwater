---
description: Read the Dashboard's health bubbles, work the action queue with filters and bulk fixes, and follow the live recent-activity feed.
---

<!-- code: web/templates/index.templ, web/templates/dashboard.templ, internal/api/handlers_dashboard.go. -->

# The Dashboard

The **Dashboard** is Stillwater's home page (`/`). It summarizes your library's health, lists the violations you can act on right now, and streams recent changes as they happen.

## Header bubbles

Five stat cards sit at the top of the page:

- **Library health** -- a compact ring plus your overall compliance score.
- **Artists** -- total artist count; click through to the Artists list.
- **Last evaluated** -- how long ago rules last ran; click to run rules now.
- **Auto-fixable** -- count of active violations Stillwater can fix with one click. Click through to the queue, pre-filtered to fixable items.
- **Needs you** -- count of active violations that need manual review. Click through to the queue, pre-filtered to non-fixable items.

If stats fail to load, a bubble shows a dash instead of a stale or fabricated number.

## Work the action queue

The action queue lists open violations, one card per artist/rule pair: an avatar, the artist name, a severity badge, and the violation message. Click a truncated message to expand it.

Each card offers up to two actions:

- **Fix** (or a rule-specific label like **Fetch Image**) -- fixable violations get a one-click fix button.
- **Re-identify** -- MBID-related violations link to the artist page instead of fixing inline.
- **Dismiss** -- available on every card, regardless of fixability.

### Search and filter

The toolbar's search box (`/` to focus it) matches on artist name, violation message, or rule. The **Filters** button (`f`) opens a flyout with five tri-state facets -- click a value once to include it, again to exclude it, a third time to clear it:

- **Severity** (error, warning, info)
- **Category** (Metadata, Images, NFO / MBID)
- **Library**
- **Rule**
- **Fixable** (yes / no)

Each facet value shows a live count. Filters and search both write to the page URL, so a filtered view is bookmarkable and shareable.

### Bulk actions

Check individual cards, or use **Select all**, to select multiple violations at once. Once at least one card is checked, **Fix selected** and **Dismiss selected** (plus a "more bulk actions" menu) appear in the queue header.

### Undo

After a fix or dismiss, the affected card shows an undo strip briefly. Press `u` while a card is focused to undo it before the strip disappears.

## Recent activity

The right-hand rail lists the library's most recent field changes -- set, changed, cleared, or reverted -- updating live over a server-sent-events stream as they happen. A status indicator shows whether the feed is live or reconnecting. Click **View all activity** at the bottom of the rail to open the full [Activity](activity-feed.md) page.

## Keyboard shortcuts

| Key | Action |
| --- | --- |
| `/` | Focus search |
| `f` | Open filters |
| `r` | Run rules |
| `j` / `k` | Move focus to the next / previous card |
| `h` / `l` | Previous / next page of results |
| `Enter` | Open the focused card's artist |
| `u` | Undo the focused card's last fix or dismiss (while its undo strip is showing) |

A shortcut legend with the same keys is printed at the bottom of the page.

## See also

- [Refresh metadata](refresh-metadata.md) for what "Run rules" and provider refreshes actually do.
- [Enable and configure rules](enable-and-configure-rules.md) to change which rules generate violations.
- [Activity feed](activity-feed.md) for the full change history behind the recent-activity rail.
