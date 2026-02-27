# Milestone 23 -- Reports & Table UX (v0.23.0)

## Goal

Add column visibility controls to the Artists and Compliance tables, then redesign the Reports/Compliance page with library summary cards, Chart.js coverage chart, enhanced filters (library, health score range, sorting), bulk actions (fetch metadata/images for selected artists), and CSV export.

## Acceptance Criteria

- [ ] Column toggle dropdown on Artists table (table view only) with localStorage persistence
- [ ] Column toggle dropdown on Compliance table with localStorage persistence
- [ ] Column visibility survives HTMX swaps (pagination, filter changes)
- [ ] Library summary cards with Chart.js grouped bar chart on compliance page
- [ ] Library dropdown filter on compliance page
- [ ] Health score range filter (min/max) on compliance page
- [ ] Sortable table headers (Name, Score) on compliance page
- [ ] Bulk select checkboxes with floating action bar (Fetch Metadata, Fetch Images)
- [ ] CSV export of compliance data with current filters
- [ ] All filters carry through pagination
- [ ] "NFO" label replaced with "Metadata" in artists table StatusBadge

## Dependency Map

```
#212 (Column Visibility) --> #164 (Reports Redesign)
```

PR 1 merges first; PR 2 is based on main after PR 1.

## Checklist

### Issue #212 -- Column Visibility Controls
- [ ] Implementation
  - [ ] Create `web/components/column_toggle.templ`
  - [ ] Update `web/templates/artists.templ` with data-col attributes and toggle
  - [ ] Update `web/templates/compliance.templ` with data-col attributes and toggle
- [ ] Build passes (`make build`)
- [ ] Tests pass (`go test ./...`)
- [ ] PR opened (#?)
- [ ] CI passing
- [ ] PR merged

### Issue #164 -- Reports Tab Redesign
- [ ] Implementation
  - [ ] `internal/artist/params.go` -- HealthScoreMin/Max fields
  - [ ] `internal/artist/service.go` -- buildWhereClause health score conditions
  - [ ] `internal/rule/bulk.go` -- ArtistIDs field on BulkRequest
  - [ ] `internal/api/handlers_report.go` -- new endpoints + updated handlers
  - [ ] `internal/api/router.go` -- register new routes
  - [ ] `web/components/pagination.templ` -- new PaginationData fields
  - [ ] `web/templates/compliance.templ` -- full redesign
  - [ ] `web/templates/artists.templ` -- fix NFO -> Metadata label
- [ ] Build passes (`make build`)
- [ ] Tests pass (`go test ./...`)
- [ ] PR opened (#?)
- [ ] CI passing
- [ ] PR merged

## UAT / Merge Order

1. PR #? -- Column Visibility (base: main)
2. PR #? -- Reports Redesign (base: main, after PR 1 merges)

## Notes

- 2026-02-27: Plan created. Starting with PR 1 (column visibility).
