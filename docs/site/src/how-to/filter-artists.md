---
description: Narrow the Artists list with the Filters flyout: tri-state field, image, platform, and status filters.
---

<!-- code: web/templates/artists.templ (filter flyout), internal/artist/scan.go (artistFilterPredicates), internal/api/handlers_artist.go (parseFlyoutFilters). -->

# Filter the artists list

The Artists page can hold thousands of entries. The Filters flyout narrows that
list to just the artists you want to act on, which is useful before a bulk scan,
a metadata refresh, or an image fetch.

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
- **Artist Type** and **Library** -- narrow to a specific type or library.

## Active filters and sharing a view

The **Filters** button shows a count badge once one or more filters are active,
so you always know the list is narrowed. The filter state is also written into
the page URL: bookmark the page, or copy the link, to return to or share the
exact same filtered view.

To clear everything, open the flyout and use **Clear all**.
