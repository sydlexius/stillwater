---
description: Narrow the Artists list with the Filters flyout: tri-state field, image, platform, and status filters.
---

<!-- code: web/templates/artists.templ (toolbar, view toggle, saved-views row, filter flyout), web/templates/bulk.templ (bulkActions + bulkStrip split), web/templates/artists_table.templ (table/grid body), web/components/column_toggle.templ (Columns control), internal/artist/scan.go (artistFilterPredicates), internal/api/handlers_artist.go (parseFlyoutFilters). -->

# Filter the artists list

The Artists page can hold thousands of entries. The Filters flyout narrows that
list to just the artists you want to act on, which is useful before a bulk scan,
a metadata refresh, an image fetch, or a bulk **Lock** or **Unlock**.

## Open the flyout

1. Open **Artists** in the sidebar.
2. Click **Filters** above the list.

The flyout opens with its filters grouped into collapsible sections: Metadata,
Metadata Fields, Images, Platform, Status, Artist Type, and Library.

## Tri-state filters

Most filters are tri-state. Each one cycles through three positions:

- **Any** (default) -- the filter is off and does not affect the list.
- **Include** -- keep only artists that match.
- **Exclude** -- keep only artists that do not match.

For example, the **Biography** filter set to Include shows only artists that
have a biography; set to Exclude it shows only artists missing one. Include and
Exclude are exact opposites, so the two together account for every artist.

## What you can filter on

- **Metadata Fields** -- presence of biography, years active, formed, disbanded,
  born, died, gender, type, country, genres, styles, moods, members, and
  discography.
- **Images** -- presence of a thumb, fanart, logo, or banner.
- **Platform** -- membership in an Emby or Jellyfin library, or a Lidarr mapping.
- **Status** -- whether an artist has open rule violations.
- **Artist Type** and **Library** -- narrow to a kind of artist or to a library.

## The Artist Type filter

The **Artist Type** section sorts artists into four groups:

- **Person** -- a single individual.
- **Group** -- a band or other group of people.
- **Orchestra/Choir** -- a large ensemble of performers.
- **Other** -- anything that is not one of the above, including fictional or
  character acts and artists that do not have a type set yet.

Together these four groups account for every artist, so **Other** is the
catch-all: anything that is not a person, a group, or an ensemble lands here.

## The Library filter

Library filtering lives entirely in this flyout section. There is no separate
library dropdown in the toolbar: every library is a tri-state pill here, so the
flyout is the one place to scope the list to a library.

The Library section behaves differently from the other sections. As soon as you
set at least one library to **Include**, the Library filter becomes a
whitelist: the list shows only artists whose library memberships fall entirely
within the libraries you included. An artist who is also in some other,
non-included library is left out, even though it is in an included library too.
This makes it a one-click way to see the artists exclusive to a library.

While a whitelist is active, every library not set to Include is dimmed to show
it is outside the current scope. A dimmed library is still clickable: click it
to add that library to the included set and widen the whitelist. Dimmed
libraries cycle between Any and Include only, because an Exclude is redundant
once a whitelist is active.

When no library is set to Include, the Library filter works like the other
sections: each library you set to Exclude simply removes its artists, and
libraries left at Any do not affect the list.

## Switch between table and grid views

The view toggle at the right of the toolbar switches the list between **Table
view** and **Grid view**. Table view is a dense, sortable list; grid view shows
each artist as a card with its artwork.

Sorting works differently in each view. In table view the column headers are the
sort control: click a header to sort by that column, click it again to reverse
the direction, and a third click returns to the default sort (Name, ascending).
Grid view has no headers, so a **Sort** dropdown appears in the toolbar there
instead.

## Choose which columns to show

In table view, the **Columns** control hides columns you do not need: Library,
Type, Country/Origin, Sources, Coverage, and Score. The Name column is always
shown. Your choices are remembered in your browser, so the list keeps the same
columns on your next visit. The Columns control is available in table view only,
because grid view has no columns.

## Active filters and sharing a view

The **Filters** button shows a count badge once one or more filters are active,
so you always know the list is narrowed. The Artists page does not repeat each
filter as its own chip; the badge and the flyout together are where you see and
change what is active. The filter state is written into the page URL: bookmark
the page, or copy the link, to return to or share the exact same filtered view.

To clear everything, open the flyout and use **Clear all**. Clear all leaves
the search box and the current sort untouched, so you can switch filter sets
without retyping or re-sorting.

## Save a filter set as a view

When you return to the same filters often, click **Save view** in the toolbar
and give the combination a name. Saved views appear as chips in a **Saved:** row
just below the toolbar; click a chip to re-apply that search and filter set in
one click, or use the small remove control on the chip to delete a view you no
longer need. The row stays hidden until you have saved at least one view.

## The same flyout on the Dashboard and Reports

The Dashboard and the Reports/Compliance page use the same Filters flyout, so
the tri-state behavior above carries over, and each of those pages shows its
own count badge on the Filters button once a filter is active. The filter state
stays in the page URL on every page, so the link is always shareable.

The Reports/Compliance page additionally keeps a dismissible chip row above its
table: each active filter appears as a chip, and clicking a chip drops just that
filter while preserving the current page, search, and sort. The **Export CSV**
link on that page respects the active filters, so the download reflects what you
are looking at.

## Select every matching artist

A filter usually narrows the list so you can act on the result as a group. The
selection controls are split across the page: the **Select all on page**
checkbox sits in the table header (in grid view it sits in the selection strip
instead), and a thin selection strip at the top of the list shows how many
artists are selected. To select more than one page of results at once:

1. Use **Select all on page** to select every artist on the current page.
2. When the filter matches more artists than fit on one page, a **Select all N
   matching** button appears in the selection strip. Click it to extend the
   selection to every artist the filter matches, across all pages.

With a selection active, choose a verb from the bulk-action dropdown in the
toolbar and click **Apply**. The actions are **Run rules**, **Auto
re-identify**, **Re-identify (review each)**, **Scan**, **Fetch images**,
**Lock**, and **Unlock**. **Lock** and **Unlock** are short-circuited for any
selected artist that is already in the target state, and the completion summary
reports those as skipped.

The cross-page selection is capped at 1000 artists. If the filter matches more
than that, Stillwater selects the first 1000 and tells you so, and any bulk
action applies to those 1000 only. Narrow the filter further to reach the rest.
