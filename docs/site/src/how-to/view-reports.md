---
description: Browse the two-pane Reports workspace at /next/reports to view compliance, library health, metadata completeness, and rule pass-rate data across your library.
---

<!-- code: web/templates/next/reports.templ, internal/api/handlers_next_report.go -->

# View reports

The **Reports** workspace is a two-pane screen at `/next/reports` that gives you a bird's-eye view of your library's health, compliance, and metadata state. The left rail lists every available report; the right pane renders the one you select.

## Open the Reports workspace

Click **Reports** in the sidebar. The workspace opens with the Compliance overview active by default. You can also navigate directly to `/next/reports/{report-name}` (for example, `/next/reports/health`).

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

## Background appearance

The card surfaces in the Reports workspace follow the **Background Opacity** preference. See [Customize preferences](customize-preferences.md#background-opacity) to adjust the frosted-glass opacity of cards and panels.
