# M6: Reports and Dashboard (v0.6.0)

## Goal

Build the library health dashboard, compliance reports, and NFO diff preview with rollback support. This gives users visibility into their library's metadata/image completeness and tools to review changes.

## Prerequisites

- M5 complete (rule engine for compliance evaluation)

## Issues

| # | Title | Mode | Model |
|---|-------|------|-------|
| 21 | Library health dashboard with compliance scores | plan | opus |
| 22 | Compliance report: filterable table with action buttons | plan | sonnet |
| 23 | NFO diff preview and rollback | plan | sonnet |

## Implementation Order

### Step 1: Health Score Calculation

**Packages:** `internal/rule/`, `internal/database/`

1. Health score logic:
   - Overall score = (passing rule checks / total rule checks) * 100
   - Per-artist score = (passing rules for artist / enabled rules) * 100
   - Record health history in `health_history` table (already created in 001_initial)
   - Configurable recording interval (daily, on-demand, after bulk operations)

2. Health history repository:
   - `RecordHealth(ctx, score, breakdown) error`
   - `GetHistory(ctx, from, to) ([]HealthSnapshot, error)`

### Step 2: Library Health Dashboard (#21)

**Templates and Chart.js:**

1. Vendor Chart.js (~65KB) in `web/static/js/chart.min.js`

2. Dashboard page (`web/templates/dashboard.templ`):
   - Replace current placeholder with real dashboard
   - Large compliance score display (circular gauge or percentage)
   - Quick stats: total artists, compliant artists, violations count
   - Trend chart showing health score over time (Chart.js line chart)
   - Top violations list (most common rule failures)
   - Recent activity feed (last auto-fixes, scans, manual changes)

3. Chart.js integration:
   - HTMX loads chart data via API endpoint
   - Minimal JS: Chart.js config passed via data attributes or inline JSON
   - Responsive chart container

4. API endpoints:
   - `GET /api/v1/reports/health` -- current health score + breakdown
   - `GET /api/v1/reports/health/history` -- historical data points for charts

5. Update index handler to render dashboard for authenticated users

### Step 3: Compliance Report (#22)

**Filterable table with actions:**

1. Report page (`web/templates/compliance_report.templ`):
   - Table columns: Artist Name (link), NFO, Thumb, Fanart, Logo, MBID, Actions
   - Status cells use badge components from M2
   - Tooltips on status badges with specifics (e.g., "missing: biography, disbanded")
   - Column sorting (click headers, HTMX swap)
   - Filter bar: status filter (all, compliant, non-compliant), search by name
   - Pagination

2. Per-row action buttons:
   - Fetch Metadata (triggers provider fetch for this artist)
   - Fetch Images (opens image search for this artist)
   - View NFO Diff (navigates to diff page)
   - More actions dropdown: Clear Metadata, Delete Images

3. Bulk action toolbar (reuse from M5):
   - Appears when rows are selected
   - Actions: Fetch Metadata, Fetch Images, Run Rules

4. API endpoint:
   - `GET /api/v1/reports/compliance` -- returns per-artist compliance data
   - Supports query params: status, sort, order, page, limit, search

### Step 4: NFO Diff Preview and Rollback (#23)

**Packages:** `internal/nfo/`, `internal/database/`

1. NFO diff logic in `internal/nfo/diff.go`:
   - Compare two ArtistNFO structs field by field
   - Return list of changed fields with old/new values
   - Handle slice fields (genres, styles) with set-based diff

2. Snapshot management:
   - Before writing any NFO changes, save current content to `nfo_snapshots` table
   - Snapshot includes: artist ID, full XML content, timestamp, source of change

3. Snapshot repository:
   - `CreateSnapshot(ctx, artistID, content, source) error`
   - `ListSnapshots(ctx, artistID) ([]Snapshot, error)`
   - `GetSnapshot(ctx, id) (*Snapshot, error)`

4. Diff preview UI (`web/templates/nfo_diff.templ`):
   - Side-by-side view: current NFO vs proposed changes
   - Color-coded: green (added), red (removed), yellow (changed)
   - Field-level diff (not raw XML diff)
   - "Apply Changes" and "Cancel" buttons

5. Rollback UI:
   - List of snapshots for an artist (with timestamps and source)
   - "Restore this version" button per snapshot
   - Confirmation dialog before rollback

6. API endpoint:
   - `GET /api/v1/artists/{id}/diff` -- returns diff between current and proposed
   - `GET /api/v1/artists/{id}/snapshots` -- list snapshots
   - `POST /api/v1/artists/{id}/snapshots/{snapshotId}/restore` -- rollback

## Key Design Decisions

- **Chart.js via data attributes:** Minimize inline JavaScript. Pass chart configuration as JSON in a data attribute; a small initialization script reads it and creates the chart. This keeps the Templ templates clean.
- **Field-level diff, not XML diff:** Showing raw XML diffs is confusing. The diff view shows human-readable field names and values.
- **Snapshots are full copies:** Each snapshot stores the complete NFO XML. This is simple and reliable, at the cost of some storage (NFO files are typically under 10KB).
- **Health history is append-only:** Snapshots are never deleted automatically. Users can manually purge old history if storage is a concern.

## Verification

- [ ] Dashboard displays correct compliance scores
- [ ] Trend chart renders with historical data
- [ ] Compliance report filters and sorts correctly
- [ ] Per-row actions trigger correct operations
- [ ] NFO diff shows field-level changes with color coding
- [ ] Snapshot creation happens before NFO writes
- [ ] Rollback restores previous NFO content correctly
- [ ] `make test` and `make lint` pass
- [ ] Bruno collection updated
