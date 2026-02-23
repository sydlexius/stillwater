# Open Issues Workplan

## Goal

Work through all 9 open issues in priority order, respecting blocking relationships. Each issue gets its own feature branch, PR, and testing cycle before merge.

## Dependency Graph

```
#90 (critical bug, no blockers)
#97 (high bug, no blockers)
#88 (medium, no blockers)
#98 (high, soft depends on #97)
#100 (medium, no blockers)
#91 (medium, no blockers)
#95 (medium, no blockers)
#99 (medium, blocked by #98)
#94 (low, no blockers, intentionally last)
```

## Issues

| # | Title | Priority | Scope | Mode | Model |
|---|-------|----------|-------|------|-------|
| 90 | Connections bug: stale encrypted row poisons List() | critical | small | direct | sonnet |
| 97 | Fanart.tv images missing dimensions and rendering blank | high | small | direct | sonnet |
| 88 | NFO and artwork clobber risk detection and UI warnings | medium | medium | plan | sonnet |
| 98 | Display existing local images on artist detail page | high | medium | plan | opus |
| 100 | Per-artist metadata refresh with field-level provider selection | medium | large | plan | opus |
| 91 | Provider Priority UI redesign: drag-drop chips | medium | large | plan | opus |
| 95 | Settings page: add tab navigation for sections | medium | medium | direct | sonnet |
| 99 | Image management improvements: unified search, edit, upload UX | medium | large | plan | opus |
| 94 | Developer documentation overhaul | low | medium | direct | haiku |

## Execution Order

Work items are ordered by priority, then by blocking relationships. Items at the same priority level and with no dependencies between them can be worked in parallel.

---

### 1. #90 -- Connections bug: stale encrypted row poisons List()

`[mode: direct]` `[model: sonnet]`

**Why first:** Critical severity. All connection-dependent features (library sync, metadata push, refresh) are broken. Small scope, well-defined fix.

**Branch:** `fix/90-connection-list-resilience`

#### Checklist

- [x] Make `List()` skip undecryptable rows with `slog.Warn` instead of aborting
- [x] Apply same pattern to `ListByType()`
- [x] Guard empty `encrypted_api_key` in `scanConnection()` (reset-credentials sets it to `""`)
- [x] Add `Connections` field to `OnboardingData` and load in `handleOnboardingPage`
- [x] Update OOBE template to render saved connection state on page load
- [x] Prevent duplicate connection creation when type+url already exists
- [x] Add migration or startup cleanup for duplicate rows
- [x] Tests pass: `go test ./internal/connection/... ./internal/api/...`
- [x] Lint passes: `golangci-lint run ./...`
- [ ] Manual acceptance test: connections page loads, OOBE shows saved connections
- [x] PR created and merged -- PR #101
- [ ] PR checks pass (no CI failures)
- [ ] PR reviewed (check for copilot feedback)

**Files:**
- `internal/connection/service.go` -- resilient `List()` / `ListByType()`
- `internal/api/handlers.go` -- load connections in `handleOnboardingPage`
- `web/templates/onboarding.templ` -- add `Connections` field, render saved state

---

### 2. #97 -- Fanart.tv images missing dimensions and rendering blank

`[mode: direct]` `[model: sonnet]`

**Why second:** High priority bug, small scope. Unblocks accurate dimension display for #98 and #99.

**Branch:** `fix/97-fanarttv-image-dimensions`

#### Checklist

- [x] Add remote image probing utility (HTTP GET + `image.DecodeConfig` for dimensions, Content-Length for file size)
- [x] Call probing in `handleImageSearch` for results with `Width == 0 && Height == 0`
- [x] Add `onerror` fallback placeholder in `image_card.templ` for broken image URLs
- [ ] Investigate Fanart.tv URL accessibility (CORS, hotlink protection, redirects)
- [x] Tests pass: `go test ./internal/image/... ./internal/api/...`
- [x] Lint passes: `golangci-lint run ./...`
- [ ] Manual acceptance test: Fanart.tv search results show dimensions, broken images show placeholder
- [x] PR created and merged -- PR #102
- [ ] PR checks pass (no CI failures)
- [ ] PR reviewed (check for copilot feedback)

**Files:**
- `internal/image/` or new probing utility
- `internal/api/handlers_image.go` -- call probing after search
- `web/components/image_card.templ` -- broken-image fallback

---

### 3. #88 -- NFO and artwork clobber risk detection and UI warnings

`[mode: plan]` `[model: sonnet]`

**Why third:** Medium priority but prevents data loss. Independent of other issues.

**Branch:** `feature/88-nfo-clobber-detection`

#### Checklist

- [ ] Investigate Emby/Jellyfin APIs for detecting NFO/artwork write settings
- [ ] Implement detection for Lidarr (existing `CheckNFOWriterEnabled`)
- [ ] Implement detection for Emby (API query or documented manual check)
- [ ] Implement detection for Jellyfin (API query or documented manual check)
- [ ] Surface persistent UI warning banner when risk detected
- [ ] Show warning during onboarding if risk present
- [ ] Graceful fallback when platform settings cannot be queried
- [ ] Tests pass: `go test ./...`
- [ ] Lint passes: `golangci-lint run ./...`
- [ ] Manual acceptance test: warning appears when Lidarr has NFO writer enabled
- [ ] PR created and merged
- [ ] PR checks pass (no CI failures)
- [ ] PR reviewed (check for copilot feedback)

**Files:**
- `internal/api/handlers_nfo.go` -- existing `handleNFOConflictCheck`
- `internal/connection/lidarr/client.go` -- existing `CheckNFOWriterEnabled`
- `internal/connection/emby/client.go` and `jellyfin/client.go` -- new detection methods
- `web/templates/` -- warning banner component

---

### 4. #98 -- Display existing local images on artist detail page

`[mode: plan]` `[model: opus]`

**Why fourth:** High priority, medium scope. Foundational for the image UX overhaul. Must land before #99.

**Branch:** `feature/98-local-image-display`

**Soft dependency:** #97 (dimensions are more useful with probing, but not strictly required)

#### Checklist

Phase 1: Local image serving endpoint
- [ ] Add `GET /api/v1/artists/{id}/images/{type}` route in `router.go`
- [ ] Handler finds image file using scanner patterns, serves with Content-Type and caching headers

Phase 2: Local image metadata endpoint
- [ ] Add `GET /api/v1/artists/{id}/images/{type}/info` returning JSON (width, height, fileSize, filename, format)
- [ ] Reuse `getThumbDimensions()` pattern from `checkers.go`

Phase 3: Replace status badges with image previews
- [ ] Replace StatusBadge calls in `artist_detail.templ` with `<img>` tags for existing images
- [ ] Show placeholder for missing image types

Phase 4: Resolution and file size display
- [ ] Show dimensions and file size beneath each image preview (HTMX load from info endpoint)

Phase 5: Hover overlay with pencil and trashcan icons
- [ ] Pencil icon navigates to `/artists/{id}/images?type={type}`
- [ ] Trashcan icon triggers delete with confirmation

Phase 6: Delete endpoint and confirmation dialog
- [ ] Add `DELETE /api/v1/artists/{id}/images/{type}` handler
- [ ] Confirmation dialog with "Don't ask again" checkbox
- [ ] Store preference in settings key-value store
- [ ] Add toggle in Settings page to re-enable dialog

Testing and PR
- [ ] Tests pass: `go test ./...`
- [ ] Lint passes: `golangci-lint run ./...`
- [ ] Manual acceptance test: artist detail shows image previews, hover icons work, delete works
- [ ] PR created and merged
- [ ] PR checks pass (no CI failures)
- [ ] PR reviewed (check for copilot feedback)

**Files:**
- `internal/api/router.go` -- new routes
- `internal/api/handlers_image.go` -- serve, info, delete handlers
- `web/templates/artist_detail.templ` -- image preview sidebar
- `internal/api/handlers_settings.go` -- confirmation preference
- `web/templates/settings.templ` -- confirmation toggle

---

### 5. #100 -- Per-artist metadata refresh with field-level provider selection

`[mode: plan]` `[model: opus]`

**Why fifth:** Medium priority, large scope. Independent of image issues. Complex multi-file change with architectural decisions.

**Branch:** `feature/100-per-artist-metadata-refresh`

#### Checklist

Phase 1: Pencil and trashcan icons on metadata fields
- [ ] Add icon buttons next to Biography, Genres, Styles, Moods, Life events, Members in `artist_detail.templ`

Phase 2: Inline manual edit per field
- [ ] HTMX swap to editable form on pencil click (textarea, tag input, date input)
- [ ] Add `PATCH /api/v1/artists/{id}/fields/{field}` endpoint
- [ ] Add `DELETE /api/v1/artists/{id}/fields/{field}` endpoint

Phase 3: Per-field provider fetch
- [ ] Add `GET /api/v1/artists/{id}/fields/{field}/providers` endpoint
- [ ] Call orchestrator for specific field, return results from all enabled providers
- [ ] Inline expandable panel showing provider results side-by-side

Phase 4: Global "Refresh Metadata" button
- [ ] Add `POST /api/v1/artists/{id}/refresh` endpoint
- [ ] Trigger full metadata fetch via orchestrator

Phase 5: Delete/reset field with confirmation
- [ ] Trashcan triggers DELETE with configurable confirmation dialog
- [ ] "Don't ask again" checkbox, stored in settings

Phase 6: Settings toggle for confirmation dialogs
- [ ] Add toggle in `settings.templ` (shared with #98 image deletion confirmation)

Testing and PR
- [ ] Tests pass: `go test ./...`
- [ ] Lint passes: `golangci-lint run ./...`
- [ ] Manual acceptance test: per-field edit, provider fetch, global refresh all work
- [ ] PR created and merged
- [ ] PR checks pass (no CI failures)
- [ ] PR reviewed (check for copilot feedback)

**Files:**
- `web/templates/artist_detail.templ` -- icons, inline edit forms, provider panel
- `internal/api/router.go` -- new field-level routes
- `internal/api/handlers_artist.go` -- field PATCH/DELETE/GET handlers
- `internal/provider/orchestrator.go` -- per-field fetch method
- `internal/provider/settings.go` -- priority lookups
- `internal/api/handlers_settings.go` -- confirmation preferences
- `web/templates/settings.templ` -- confirmation toggle

---

### 6. #91 -- Provider Priority UI redesign: drag-drop chips

`[mode: plan]` `[model: opus]`

**Why sixth:** Medium priority, large scope. Standalone UX improvement. No blockers or dependents.

**Branch:** `feature/91-provider-priority-chips`

#### Checklist

- [ ] Design chip/card component for each provider (drag handle, name, enable/disable toggle)
- [ ] Implement drag-and-drop reordering (consider SortableJS or native HTML5 drag)
- [ ] Grey out chips for unconfigured providers (no valid API key)
- [ ] Persist reorder and enable/disable state via existing provider priority API
- [ ] Mobile support: tap-to-toggle, swipe-to-reorder or fallback to up/down buttons
- [ ] Tests pass: `go test ./...`
- [ ] Lint passes: `golangci-lint run ./...`
- [ ] Manual acceptance test: drag reorder works, toggle persists, unconfigured providers greyed
- [ ] PR created and merged
- [ ] PR checks pass (no CI failures)
- [ ] PR reviewed (check for copilot feedback)

**Files:**
- `web/templates/settings.templ` -- replace provider priority section
- `web/static/` -- vendored drag-drop library if needed
- `internal/api/handlers_settings.go` -- no backend changes expected (reuse existing API)

---

### 7. #95 -- Settings page: add tab navigation for sections

`[mode: direct]` `[model: sonnet]`

**Why seventh:** Medium priority, medium scope. Pure frontend change. Consider coordinating with #91 (provider priority redesign) if both are in flight.

**Branch:** `feature/95-settings-tabs`

#### Checklist

- [ ] Group settings sections into tabs: General, Providers, Scraper, Connections, Notifications, Maintenance
- [ ] Implement tab switching (HTMX or JS-driven, preserving scroll position)
- [ ] Ensure deep-linking works (e.g., `/settings?tab=providers`)
- [ ] Mobile-friendly tab navigation (horizontal scroll or dropdown)
- [ ] Tests pass: `go test ./...`
- [ ] Lint passes: `golangci-lint run ./...`
- [ ] Manual acceptance test: tabs switch correctly, deep links work, mobile layout works
- [ ] PR created and merged
- [ ] PR checks pass (no CI failures)
- [ ] PR reviewed (check for copilot feedback)

**Files:**
- `web/templates/settings.templ` -- tab navigation wrapper around existing sections
- `internal/api/handlers_settings.go` -- accept `tab` query param if needed

---

### 8. #99 -- Image management improvements: unified search, edit, upload UX

`[mode: plan]` `[model: opus]`

**Why eighth:** Medium priority, large scope. Blocked by #98 (local image display provides the pencil icon entry point and serving endpoint).

**Branch:** `feature/99-image-management-ux`

**Blocked by:** #98

#### Checklist

Phase 1: Type pre-selection and auto-search
- [ ] Accept `?type=thumb` query param to pre-select image type
- [ ] Auto-trigger search on page load via HTMX when type is pre-selected

Phase 2: Show current local image alongside results
- [ ] Display current local image at top of results area (using endpoint from #98)
- [ ] Show resolution and file size

Phase 3: Enhanced compare panel
- [ ] Update `image_compare.templ` for local vs. fetched side-by-side with full metadata

Phase 4: Consolidate upload, URL-fetch, and crop
- [ ] Bring upload form and URL-fetch inline on same page
- [ ] Crop tool accessible from any image source

Phase 5: Resolution and file size for fetched results
- [ ] Display dimensions on every image card (benefits from #97 probing)

Phase 6: Configurable confirmation on save/delete
- [ ] Save and delete use confirmation dialog with "Don't ask again" (shared with #98/#100)

Testing and PR
- [ ] Tests pass: `go test ./...`
- [ ] Lint passes: `golangci-lint run ./...`
- [ ] Manual acceptance test: auto-search works, local image shown, compare panel works, upload/crop inline
- [ ] PR created and merged
- [ ] PR checks pass (no CI failures)
- [ ] PR reviewed (check for copilot feedback)

**Files:**
- `web/templates/image_search.templ` -- auto-search, type pre-selection, layout consolidation
- `web/components/image_card.templ` -- resolution/file size display
- `web/components/image_compare.templ` -- enhanced side-by-side
- `web/components/image_upload.templ` -- inline on same page
- `internal/api/handlers_image.go` -- adjust search handler

---

### 9. #94 -- Developer documentation overhaul

`[mode: direct]` `[model: haiku]`

**Why last:** Low priority, documentation-only. The cleanup of `docs/plans/` should happen after all other issues are resolved so that this workplan file remains available during active development. This file (`docs/plans/workplan-open-issues.md`) should be preserved until all items above are complete, then archived or removed as part of this issue.

**Branch:** `docs/94-developer-documentation`

#### Checklist

- [ ] Review and update `docs/dev-setup.md` for accuracy and completeness
- [ ] Document full development lifecycle: building, running, testing, linting, contributing, deploying
- [ ] Clarify developer dependencies, environment variables, local setup steps (docker and non-docker)
- [ ] Document Makefile targets and recommended tools (Air for hot reload, etc.)
- [ ] Add contribution guidance (conventions, PRs, code review, architectural hints)
- [ ] Remove outdated milestone plan files from `docs/plans/` (completed milestones M1-M7)
- [ ] Preserve `overview.md` and any active/in-progress plan files
- [ ] Preserve `workplan-open-issues.md` only if open items remain; remove if all complete
- [ ] Tests pass: `go test ./...` (no code changes, but verify docs build if applicable)
- [ ] Lint passes: `golangci-lint run ./...`
- [ ] PR created and merged
- [ ] PR checks pass (no CI failures)
- [ ] PR reviewed (check for copilot feedback)

**Files:**
- `docs/dev-setup.md` -- update
- `docs/plans/m1-scaffolding.md` through `m7-integrations.md` -- remove (completed milestones)
- `docs/plans/overview.md` -- preserve and update status
- `docs/plans/workplan-open-issues.md` -- preserve or archive depending on remaining items

---

## PR Workflow

Each issue follows this workflow:

1. **Branch** from `main` using the branch name listed above
2. **Implement** following the checklist and agentic hints (`[mode]` and `[model]`)
3. **Test** -- run `make test` and `make lint` before opening PR
4. **Acceptance test** -- manual verification of the feature/fix in a running instance
5. **PR** -- open against `main` with issue reference (e.g., "Fixes #90")
6. **Monitor** -- watch for:
   - CI check failures (GitHub Actions)
   - Copilot review feedback (address or dismiss with explanation)
   - Merge conflicts with concurrent work
7. **Merge** -- squash-merge after checks pass and feedback is addressed
8. **Update** -- check off the PR items in this workplan

## Agentic AI Hints Reference

These tags go in the GitHub issue body to guide Claude Code and Copilot:

| Tag | Meaning |
|-----|---------|
| `[mode: plan]` | Start in Plan Mode. Explore codebase and design approach before writing code. |
| `[mode: direct]` | Skip planning. Task is well-defined enough to implement directly. |
| `[model: opus]` | Complex architecture, multi-file changes, nuanced design decisions. |
| `[model: sonnet]` | Standard feature work, good balance of capability and speed. |
| `[model: haiku]` | Simple, well-defined changes (typo fixes, small additions, config). |

## Notes

- Items 1-2 (bugs) can be done quickly and merged before starting feature work.
- Items 4 and 5 both modify `artist_detail.templ` extensively. If worked in parallel, coordinate to avoid merge conflicts. Consider working #98 first, merging, then starting #100.
- Items 6 and 7 both modify `settings.templ`. Same coordination applies. Consider working #91 first (provider chips), then #95 (tabs) wraps around all sections including the new chips.
- Item 8 (#99) cannot start until item 4 (#98) is merged.
- Item 9 (#94) should be the last item. It cleans up completed plan files and this workplan.
