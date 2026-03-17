# M28 UX Audit: Bliss-Inspired Design Patterns

## Purpose

This document maps Bliss (blisshq.com) UX patterns to Stillwater's current implementation,
identifies gaps, and provides prioritized recommendations for #310 (Notifications table UX)
and #286 (Actionable fix options).

## Bliss Core Design Patterns

### 1. Assess-Fix-Maintain Cycle

Bliss structures its entire UX around a three-stage loop:

1. **Assess** -- Rules evaluate albums automatically when added/changed.
   Albums are marked "compliant" or "non-compliant".
2. **Fix** -- Non-compliant items get a recommended fix. Fixes can be applied
   automatically or presented for manual confirmation.
3. **Maintain** -- Rules run continuously in the background, re-evaluating
   after changes.

**Stillwater equivalent:** The rule pipeline already implements this cycle
(Engine evaluates, Pipeline fixes, Scheduler maintains). The gap is in how
results are *presented* -- Stillwater dumps violations into a flat table
rather than guiding users through the assess-fix workflow.

### 2. Inbox Paradigm

Bliss presents outstanding issues in an "Inbox" -- a prioritized list of
actionable items, not a raw database dump. The inbox:

- Groups issues by type/category
- Shows a recommended fix for each item
- Provides "Fix All by type" to batch-fix an entire category
- Separates "needs your input" from "can be auto-fixed"

**Stillwater equivalent:** The notifications page shows a flat table sorted
by created_at DESC. No grouping, no fix recommendations, no distinction
between "needs input" and "auto-fixable".

### 3. Fix All with Progress

Bliss's "Fix All" feature:

- User selects which fix types to include
- "Execute" button confirms (no accidental bulk fixes)
- Dedicated progress page with:
  - Overall progress bar
  - Per-item status (success/failure with localized error messages)
- Results persist after completion for review

**Stillwater equivalent:** No bulk fix capability exists. The only bulk
action is "Dismiss all".

### 4. One-Click Fixes

For straightforward fixes (e.g. artwork needs resizing, missing artwork
with a high-confidence match), Bliss offers one-click resolution directly
in the inbox row.

**Stillwater equivalent:** Image candidate selection requires expanding a
row, viewing candidates, and clicking "Apply". No one-click path for
high-confidence fixes.

### 5. Compliance Visualization

Bliss shows compliance at the album level -- each album is either
"compliant" (green) or "non-compliant" (red/orange). This binary framing
makes the library health instantly scannable.

**Stillwater equivalent:** Health scores (0-100) exist on each artist, but
the notifications page does not show compliance status. The dashboard has
a health distribution chart but it is disconnected from the notifications
workflow.

### 6. Background Processing

Bliss runs unobtrusively in the background. The UI does not block while
rules evaluate. Users are notified when new issues appear.

**Stillwater equivalent:** The rule scheduler runs in the background. The
navbar badge shows violation counts. This is already aligned with Bliss.

## Feature Comparison

| Feature | Bliss | Stillwater (Current) | Gap |
|---------|-------|---------------------|-----|
| Rule evaluation | Automatic on file change | Scheduled + manual trigger | Minor |
| Inbox / violations list | Grouped by type, prioritized | Flat table, newest first | Major |
| Sorting | By type, severity implied | None (created_at DESC only) | Major |
| Filtering | By rule type | Status only (open/dismissed/resolved) | Major |
| Grouping | By album, by rule type | None | Major |
| Fix recommendations | One-click with confidence | Candidate grid for images only | Major |
| Fix All | By type, with progress | Not implemented | Major |
| Per-item fix status | Success/failure with error | Not implemented | Major |
| Compliance view | Binary per-album | Health score per-artist | Minor |
| Background processing | Continuous | Scheduled interval | Aligned |
| Navbar notification | Not documented | Badge with severity counts | Aligned |
| Dismiss/snooze | Not documented | Dismiss individual + bulk | Aligned |
| Manual mode | Confirm fixes before applying | Candidate discovery, pending_choice | Aligned |

## Prioritized Recommendations

### P1 -- Must Have (target for #310 and #286)

#### R1: Server-side sorting with clickable column headers (#310)

Allow sorting by any column (artist, rule, severity, age). Show sort direction
arrows. Default: severity DESC, then age DESC (most urgent first, not newest
first).

**Stillwater component:** `handlers_notifications.go`, `notifications.templ`,
`service.go` (ListViolations query).

#### R2: Filtering by severity, category, and rule (#310)

Add filter controls above the table. Severity: error/warning/info. Category:
nfo/image/metadata. Rule: dropdown of active rule IDs. All filters applied
server-side via query params, preserved across HTMX partial updates.

**Stillwater component:** `handlers_notifications.go`, `notifications.templ`,
`service.go`.

#### R3: Grouping by artist, rule, severity, or category (#310)

Collapsible group headers with count badges. Default grouping: by artist (most
natural for "fix this artist's issues" workflow). Group headers should show
aggregate severity (worst in group).

**Stillwater component:** `handlers_notifications.go` (post-query grouping),
`notifications.templ`.

#### R4: Inline fix panels per violation type (#286)

Replace the current candidate-only expandable row with a generalized fix panel
that varies by violation type:

| Violation Type | Fix Panel Content |
|---------------|-------------------|
| Missing image (with candidates) | Candidate grid with "Apply" (existing) |
| Missing image (no candidates) | "Fetch from providers" button |
| Missing biography | Pre-filled provider text with "Apply" |
| Missing NFO | "Create NFO" one-click button |
| Low-res image | "Replace with higher-res" if candidate available |
| Extraneous images | File list with "Delete All" |
| Directory name mismatch (#313) | Current vs. proposed name, "Rename" button |
| Image duplicate (#287) | Side-by-side comparison, "Keep A" / "Keep B" |

**Stillwater component:** `notifications.templ`, `handlers_notifications.go`.

#### R5: Fix recommendation badge (#286)

For violations where the system has high confidence in the right fix, show a
"Recommended" badge on the best option. Criteria:

- Image candidates: highest-resolution from primary provider (Fanart.tv > others)
- NFO: always recommend creation (no ambiguity)
- Directory rename: always recommend the canonical name
- Image dedup: recommend keeping the higher-resolution version

**Stillwater component:** `notifications.templ`, fix panel rendering logic.

#### R6: "Fix All" with progress (#286)

Global "Fix All" button in the toolbar. Workflow:

1. User clicks "Fix All" -- confirmation dialog shows count by type
2. "Execute" button starts async processing
3. Progress indicator replaces the button (progress bar + count)
4. Per-item results shown inline (success/failure badge per row)
5. Summary toast on completion

Only applies recommended fixes. Violations without a recommended fix are
skipped.

**Stillwater component:** New endpoints (`POST /api/v1/notifications/fix-all`,
`GET /api/v1/notifications/fix-all/status`), `handlers_notifications.go`,
`notifications.templ`, `fixer.go` (Pipeline.FixViolation method).

### P2 -- Should Have (target for M28 or fast follow)

#### R7: Fix dispatch endpoint (#286)

Generic `POST /api/v1/notifications/{id}/fix` that looks up the violation,
finds the appropriate fixer, and applies the recommended fix. This enables
the "Fix" button per-row and powers the "Fix All" batch.

**Stillwater component:** `handlers_notifications.go`, `fixer.go`.

#### R8: Severity-first default sort (#310)

Change default sort from `created_at DESC` to `severity DESC, created_at DESC`.
Errors should appear before warnings, warnings before info. This matches Bliss's
prioritization of urgent items.

**Stillwater component:** `service.go` (ListViolations query).

#### R9: Violation count per group header (#310)

When grouped, show "Artist Name (3 violations)" or "Missing Image (12)" in the
group header. Collapsible with chevron animation.

**Stillwater component:** `notifications.templ`.

### P3 -- Nice to Have (future milestones)

#### R10: Compliance summary per artist

Show a green/yellow/red compliance indicator on the artist list page based on
whether the artist has any open violations. Green = no violations, yellow =
warnings only, red = errors.

**Stillwater component:** Artist list template, artist service.

#### R11: Violation trend chart

Show violations created vs. resolved over time on the dashboard. Helps users
see if their library health is improving.

**Stillwater component:** Dashboard template, new service method.

#### R12: Undo/revert after fix

After a fix is applied, show an "Undo" option for a short window (30 seconds).
NFO snapshots already support this; extend to image saves.

**Stillwater component:** Fix panel, snapshot service.

#### R13: Export violations

CSV/JSON export of current filtered violation list for external analysis.

**Stillwater component:** New handler, notifications page toolbar.

## Guidance for #310 (Notifications Table UX)

1. Implement R1 (sorting), R2 (filtering), R3 (grouping), R8 (default sort).
2. Use existing artist list page as the pattern for sort arrows and filter UI.
3. All filtering/sorting is server-side via query params. HTMX preserves state
   by including current params in each partial update request.
4. The `ViolationListParams` struct should capture all filter/sort/group state.
5. Grouping is applied in the Go handler after the DB query (keeps SQL simple).
6. Add empty fix-panel placeholder rows (hidden `<tr>`) that #286 will populate.

## Guidance for #286 (Actionable Fix Options)

1. Implement R4 (inline fix panels), R5 (recommendation badges), R6 (Fix All),
   R7 (fix dispatch endpoint).
2. Generalize the existing candidate row into a per-violation-type fix panel.
3. The fix dispatch endpoint should be type-agnostic: look up violation, find
   fixer, call Fix. The fixer knows what to do.
4. "Fix All" is async (202 + polling). Use the existing event bus pattern for
   progress updates.
5. Toast notifications via existing `showSuccessToast` / `showToast` JS helpers.
6. Do not auto-fix violations without a recommendation. "Fix All" only touches
   violations with a clear recommended action.

## Follow-up Issues (Out of Scope for M28)

- R10: Compliance summary per artist (new issue, M29+)
- R11: Violation trend chart (new issue, M29+)
- R12: Undo/revert after fix (new issue, M29+)
- R13: Export violations (new issue, M29+)
