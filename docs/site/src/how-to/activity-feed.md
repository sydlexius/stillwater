---
description: Browse and filter the library-wide Activity feed, and undo an individual metadata change straight from its entry.
---

<!-- code: web/templates/activity_page.templ, web/templates/activity.templ, internal/api/handlers_history.go. -->

# Activity feed

The **Activity** page (`/activity`) lists every metadata change across your whole library -- manual edits, provider refreshes, rule fixes, and reverts -- newest first.

## Open the Activity page

Click **Activity** in the sidebar, or **View all activity** at the bottom of the Dashboard's recent-activity rail.

## Read an entry

Each entry shows the artist, a source badge (Manual, Scan, Import, Revert, Provider, or Rule), and a relative timestamp. Click the entry to expand it and see the field's old and new values side by side.

## Undo a change

Entries for trackable fields carry an **Undo** button (hidden on revert entries themselves, since you can't revert a revert). Undoing asks for confirmation, then restores the field's prior value and adds a new "Revert" entry at the top of the feed -- the original entry stays in place as a record of what happened.

## Filter

Click **Filters** to open the flyout, which has two facets:

- **Change Type** -- the field that changed (Biography, Genres, Styles, Moods, Type, Gender, and other trackable fields), plus a synthetic "Rule fix" entry that surfaces automated repairs the rule engine performed (filesystem, image, and NFO fixes).
- **Trigger Source** -- Manual, Scan, Import, Revert, Provider, or Rule.

Each is a standard include-list of checkboxes (not tri-state); select one or more values per facet and they combine with an AND across facets.

## Filter by date

The date bar above the feed lets you set a **From** and **To** date and click **Apply** to narrow the feed to that range. **Clear** appears once a date filter is active and resets it.

## Pagination

The feed loads a page of entries at a time with a "Showing X of Y" counter at the bottom; scroll to the bottom to load more.

## See also

- [Edit an artist](edit-artist.md#revert-a-field-to-a-prior-value) for the per-field prior-values popover, a scoped alternative to reverting from the activity feed.
- [Dashboard](dashboard.md#recent-activity) for the live-updating recent-activity rail this page's history feeds.
