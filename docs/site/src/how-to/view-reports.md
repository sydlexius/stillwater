---
description: Browse the two-pane Reports workspace at /reports to view compliance, library health, metadata completeness, and rule pass-rate data across your library.
---

<!-- code: web/templates/reports_page.templ, internal/api/handlers_report.go -->

# View reports

The **Reports** workspace is a two-pane screen at `/reports` that gives you a bird's-eye view of your library's health, compliance, and metadata state. The left rail lists every available report; the right pane renders the one you select.

## Open the Reports workspace

Click **Reports** in the sidebar. The workspace opens with the Compliance overview active by default. You can also navigate directly to `/reports/{report-name}` (for example, `/reports/health`).

## The reports rail

The narrow rail on the left lists all built-in reports. A filter box at the top of the rail lets you type to narrow the list by name. Click any entry to load it in the right pane - the active entry is highlighted and the URL updates to match.

## Live reports

Four reports have fully implemented right panes:

### Compliance overview

Shows field and rule coverage across your library as a paginated artist table. You can search, filter by status, library, or health-score range, sort by any column, and export the current view to CSV.

The pane has two tabs:

- **Results** - the standard compliance list with per-artist health scores and violation counts.
- **Matrix** - a scrollable rule-by-artist compliance matrix for an at-a-glance view of which rules pass or fail across the catalog.

Use the **Run** button to refresh the pane from the latest data without navigating away.

### Library health

Shows a compliance score summary, total and compliant artist counts, a breakdown of missing metadata types (NFO, thumb, fanart, MBID), and a ranked list of the top failing rules with per-rule pass rates.

### Metadata completeness

Shows field-coverage percentages across your entire library and a table of the ten artists with the lowest completeness scores, broken down by library.

### Rule pass rates

Lists every configured rule with its pass count, evaluation count, and pass percentage for the current library state. Pass rates are color-coded: green at 80% or above, amber between 50-79%, and red below 50%.

## Additional reports

Six further reports appear in the rail and are coming in a future release:

| Report | What it will cover |
|---|---|
| Underrated artists | Artists with strong library presence but a low health score |
| Image coverage | Thumb, fanart, logo, banner, and backdrop coverage |
| Connection sync | Where each artist is registered across connected platforms |
| ID/Metadata coverage | Per-provider linking and metadata status |
| State records | Artist lifecycle and refresh history |
| Weekly review queue | Artists scheduled for manual review |

## Duplicate and foreign-file reports

The sidebar's **Reports** group also lists **Duplicates**, **Backdrop Duplicates**, and **Foreign Files**. These are dedicated pages at their own URLs (`/reports/duplicates`, `/reports/backdrop-duplicates`, `/reports/foreign-files`), not entries in the two-pane workspace's rail. The Duplicates and Foreign Files pills show a live count when there's something to review. See [Merge duplicate artists](merge-duplicate-artists.md) for the duplicate-detection and merge workflow.

## Backdrop duplicates

<!-- code: web/templates/backdrop_duplicates.templ, internal/api/handlers_backdrop_repair.go, internal/rule/fanart_repair.go -->

The **Backdrop Duplicates** report finds cases where the *same* backdrop picture has been written into several of one artist's backdrop (fanart) slots. This commonly happens when a media server's own image fetcher saves the same artwork under many tags, so one artist ends up with the same image repeated across `fanart.jpg`, `fanart2.jpg`, `fanart3.jpg`, and so on. It is an admin-only page; click **Backdrop Duplicates** in the sidebar's Reports group to open it.

The report scans every artist's backdrops on disk and finds **exact duplicates**: byte-for-byte identical files, matched by a content hash. Because a removed copy is identical to the one kept, collapsing them loses nothing.

The page summarizes how many artists are affected and how many exact redundant slots exist, with a per-artist breakdown. If some artists could not be scanned, a **Partial Scan** notice reports how many were skipped, so a partial result is never mistaken for a clean library.

### Collapse exact duplicates

Click **Remediate Exact Duplicates** to collapse the exact (byte-identical) redundant slots across the whole library in one pass. For each affected artist, the lowest-numbered backdrop slot is kept and the identical copies are removed, with the remaining backdrops renumbered into a gap-free sequence.

Remediation is safe by design:

- A backdrop you set or locked yourself is never removed -- operator-curated artwork is protected from this tool.
- An artist always keeps one copy of each distinct backdrop; the tool only removes proven duplicates.
- Only one remediation runs at a time, and it will not run while a bulk image action is in progress, so the two can never touch the same files at once.

Only the copies stored locally are collapsed; backdrops already pushed to a connected platform are not removed by this action.

## Background appearance

The card surfaces in the Reports workspace follow the **Background Opacity** preference. See [Customize preferences](customize-preferences.md#background-opacity) to adjust the frosted-glass opacity of cards and panels.
