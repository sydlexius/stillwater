# Milestone 23 -- Reports & Table UX (v0.23.0)

## Goal

Add column visibility controls to the Artists and Compliance tables, then redesign the Reports/Compliance page with library summary cards, Chart.js coverage chart, enhanced filters (library, health score range, sorting), bulk actions (fetch metadata/images for selected artists), and CSV export.

## Acceptance Criteria

- [x] Column toggle dropdown on Artists table (table view only) with localStorage persistence
- [x] Column toggle dropdown on Compliance table with localStorage persistence
- [x] Column visibility survives HTMX swaps (pagination, filter changes)
- [x] Library summary cards with Chart.js grouped bar chart on compliance page
- [x] Library dropdown filter on compliance page
- [x] Health score range filter (min/max) on compliance page
- [x] Sortable table headers (Name, Score) on compliance page
- [x] Bulk select checkboxes with floating action bar (Fetch Metadata, Fetch Images)
- [x] CSV export of compliance data with current filters
- [x] All filters carry through pagination
- [x] "NFO" label replaced with "Metadata" in artists table StatusBadge

## Dependency Map

```
#212 (Column Visibility) --> #164 (Reports Redesign)
```

PR 1 merges first; PR 2 is stacked on PR 1 and retargeted to main after merge.

## Checklist

### Issue #212 -- Column Visibility Controls
- [x] Implementation
  - [x] Create `web/components/column_toggle.templ`
  - [x] Update `web/templates/artists.templ` with data-col attributes and toggle
  - [x] Update `web/templates/compliance.templ` with data-col attributes and toggle
- [x] Build passes (`make build`)
- [x] Tests pass (`go test ./...`)
- [x] PR opened (#274)
- [ ] CI passing
- [ ] PR merged

### Issue #164 -- Reports Tab Redesign
- [x] Implementation
  - [x] `internal/artist/params.go` -- HealthScoreMin/Max fields
  - [x] `internal/artist/service.go` -- buildWhereClause health score conditions
  - [x] `internal/rule/bulk.go` -- ArtistIDs field on BulkRequest
  - [x] `internal/api/handlers_report.go` -- new endpoints + updated handlers
  - [x] `internal/api/router.go` -- register new routes
  - [x] `web/components/pagination.templ` -- new PaginationData fields
  - [x] `web/templates/compliance.templ` -- full redesign
  - [x] `web/templates/artists.templ` -- fix NFO -> Metadata label
- [x] Build passes (`make build`)
- [x] Tests pass (`go test ./...`)
- [x] PR opened (#275)
- [ ] CI passing
- [ ] PR merged

## UAT / Merge Order

1. PR #274 -- Column Visibility (base: main)
2. PR #275 -- Reports Redesign (stacked on #274, retarget to main after #274 merges)

## Notes

- 2026-02-27: Plan created. Starting with PR 1 (column visibility).
- 2026-02-27: Both PRs implemented, UAT passed, PRs opened. Awaiting CI and merge.
