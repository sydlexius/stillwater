# Open Issues Workplan

## Goal

Work through all 9 open issues in priority order, respecting blocking relationships. Each issue gets its own feature branch, PR, and testing cycle before merge.

## Progress Summary

- **Completed:** 7 of 9 issues (#90, #97, #88, #98, #100, #91, #95)
- **Remaining:** 2 issues (#99, #94)
- **Next up:** #99 (image management improvements)
- **New issues (not in original scope):** #105 (hamburger menu nav), #106 (contextual menus audit)

## Dependency Graph

```
#90 DONE (critical bug, no blockers) -- PR #101 merged
#97 DONE (high bug, no blockers) -- PR #102 merged
#88 DONE (medium, no blockers) -- PR #103 merged
#98 DONE (high, soft depends on #97) -- PR #104 merged
#100 DONE (medium, no blockers) -- PR #107 merged
#91 + #95 DONE (medium, no blockers) -- PR #110 merged
#99 (medium, blocked by #98) -- now unblocked
#94 (low, no blockers, intentionally last)
```

## Issues

| # | Title | Priority | Scope | Mode | Model | Status |
|---|-------|----------|-------|------|-------|--------|
| 90 | Connections bug: stale encrypted row poisons List() | critical | small | direct | sonnet | DONE (PR #101) |
| 97 | Fanart.tv images missing dimensions and rendering blank | high | small | direct | sonnet | DONE (PR #102) |
| 88 | NFO and artwork clobber risk detection and UI warnings | medium | medium | plan | sonnet | DONE (PR #103) |
| 98 | Display existing local images on artist detail page | high | medium | plan | opus | DONE (PR #104) |
| 100 | Per-artist metadata refresh with field-level provider selection | medium | large | plan | opus | DONE (PR #107) |
| 91 | Provider Priority UI redesign: drag-drop chips | medium | large | plan | opus | DONE (PR #110, combined with #95) |
| 95 | Settings page: add tab navigation for sections | medium | medium | direct | sonnet | DONE (PR #110, combined with #91) |
| 99 | Image management improvements: unified search, edit, upload UX | medium | large | plan | opus | open (unblocked) |
| 94 | Developer documentation overhaul | low | medium | direct | haiku | open (last) |

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
- [x] PR checks pass (no CI failures)
- [x] PR reviewed (Copilot feedback addressed in commit 502a166)

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
- [x] PR checks pass (no CI failures)
- [x] PR reviewed (Copilot: no comments, clean)

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

- [x] Investigate Emby/Jellyfin APIs for detecting NFO/artwork write settings
- [x] Implement detection for Lidarr (existing `CheckNFOWriterEnabled`)
- [x] Implement detection for Emby (`/Library/VirtualFolders` + `LibraryOptions.MetadataSavers`)
- [x] Implement detection for Jellyfin (same API pattern as Emby)
- [x] Surface persistent UI warning banner when risk detected (yellow banner on Settings page)
- [x] Show warning during onboarding if risk present (HTMX `clobberRecheck` event)
- [x] Graceful fallback when platform settings cannot be queried (log warning, return false)
- [x] Tests pass: `go test ./...` (8 new tests across Emby/Jellyfin)
- [x] Lint passes: `golangci-lint run ./...`
- [ ] Manual acceptance test: warning appears when Lidarr has NFO writer enabled
- [x] PR created and merged -- PR #103
- [x] PR checks pass (no CI failures)
- [x] PR reviewed (Copilot: 2 comments addressed -- unified signatures, extracted shared helper)

**Files:**
- `internal/api/handlers_nfo.go` -- existing `handleNFOConflictCheck`
- `internal/connection/lidarr/client.go` -- existing `CheckNFOWriterEnabled`
- `internal/connection/emby/client.go` and `jellyfin/client.go` -- new detection methods
- `web/templates/` -- warning banner component

---

### 4. #98 -- Display existing local images on artist detail page

`[mode: plan]` `[model: opus]`

**Why next:** High priority, foundational for #99 image management improvements.

**Branch:** `feature/98-artist-detail-images`

**Closes:** #98

**Soft dependency:** #97 (merged)

#### Checklist

Phase 0: Icon infrastructure
- [x] Create reusable Templ components `IconPencilSquare` and `IconTrash` in `web/components/icon.templ`
- [x] SVG paths from Heroicons outline set (24x24), stroke-based, `currentColor`

Phase 1: Local image serving endpoint
- [x] Add `GET /api/v1/artists/{id}/images/{type}/file` route in `router.go`
- [x] Handler finds image file using naming patterns, serves with Content-Type and caching headers

Phase 2: Local image metadata endpoint
- [x] Add `GET /api/v1/artists/{id}/images/{type}/info` returning JSON or HTMX partial
- [x] Uses `img.GetDimensions()` for width/height, `os.Stat` for file size

Phase 3-5: Replace status badges with image previews + hover overlay
- [x] Replace StatusBadge calls in `artist_detail.templ` with `ImagePreviewCard` components
- [x] Show actual image thumbnails with lazy loading
- [x] Show placeholder with image icon for missing types
- [x] HTMX-loaded resolution and file size badge below each image
- [x] Hover overlay with pencil (edit) and trash (delete) icons
- [x] Checkered background for logo type (transparency detection)

Phase 6: Image delete endpoint
- [x] Add `DELETE /api/v1/artists/{id}/images/{type}` handler
- [x] Deletes all matching pattern files and clears artist image flag
- [x] HTMX response returns updated card in placeholder state
- [x] Native `hx-confirm` dialog (custom "Don't ask again" deferred to #100)

Testing and PR
- [x] Tests pass: `go test ./...`
- [x] Lint passes: `golangci-lint run ./...`
- [ ] Manual acceptance test: image previews, hover icons, delete, info badge loading
- [x] PR created and merged -- PR #104
- [x] PR checks pass (no CI failures)
- [x] PR reviewed

**Files:**
- `web/components/icon.templ` -- reusable Heroicons SVG components (new)
- `web/templates/artist_detail.templ` -- `ImagePreviewCard`, `ImageInfoBadge` components, replaced File Status section
- `internal/api/handlers_image.go` -- `handleServeImage`, `handleImageInfo`, `handleDeleteImage` + helpers
- `internal/api/router.go` -- 3 new routes

---

### 5. #100 -- Per-artist metadata refresh with field-level provider selection

`[mode: plan]` `[model: opus]`

**Why next:** Medium priority, large scope. Builds on the Heroicons icon component from #98.

**Branch:** `feature/100-artist-metadata-refresh`

**Closes:** #100

#### Icon guidance

Use the Heroicons component (created in #98 PR) for edit, delete, and refresh icons. Add `arrow-path` (refresh) icon to `icon.templ`.

#### Checklist

Phase 1: Provider fetch modal with field-level comparison

- [x] Modal dialog for fetching metadata from providers (MusicBrainz, Fanart.tv, etc.)
- [x] Side-by-side comparison of current vs. fetched values per field
- [x] Accept/reject per field with HTMX-driven updates

Phase 2: Per-field inline editing

- [x] HTMX swap to editable form on pencil icon click
- [x] Add `PATCH /api/v1/artists/{id}/fields/{field}` endpoint
- [x] Add `DELETE /api/v1/artists/{id}/fields/{field}` endpoint

Phase 3: Global "Refresh Metadata" button

- [x] Add `POST /api/v1/artists/{id}/refresh` endpoint
- [x] Trigger full metadata fetch via provider orchestrator
- [x] Auto-refresh artist detail page after mutations

Phase 4: Image auto-refresh after mutations

- [x] Form-encoded image fetch support
- [x] Auto-refresh image cards after save/delete operations

Testing and PR

- [x] Tests pass: `go test ./...`
- [x] Lint passes: `golangci-lint run ./...`
- [ ] Manual acceptance test: metadata edit/delete, provider fetch, global refresh
- [x] PR created and merged -- PR #107
- [x] PR checks pass (no CI failures)
- [x] PR reviewed (Copilot feedback addressed in commits 6af4197, 6b46b2a)

**Files:**
- `web/components/icon.templ` -- add `IconArrowPath` (refresh)
- `web/templates/artist_detail.templ` -- editable fields, provider panel, refresh button
- `internal/api/handlers_field.go` -- field PATCH/DELETE/GET handlers (new)
- `internal/api/handlers_refresh.go` -- refresh handler (new)
- `internal/api/router.go` -- field and refresh routes
- `internal/provider/orchestrator.go` -- `FetchField()` method
- `web/components/confirm_modal.templ` -- custom confirmation dialog (new)
- `internal/api/handlers_settings.go` -- confirmation dialog preferences
- `web/templates/settings.templ` -- confirmation dialog toggle

---

### 6. #91 + #95 -- Settings page: tab navigation and provider priority chips

`[mode: plan]` `[model: opus]`

**Why together:** Both issues rewrite `settings.templ`. #91 redesigns the provider priority section as drag-and-drop chips; #95 wraps all sections in tab navigation. Doing them together avoids merge conflicts and produces a coherent settings page layout.

**Branch:** `feature/91-95-settings-overhaul`

**Closes:** #91, #95

#### Checklist

Phase 1: Tab navigation (#95)
- [x] Group settings sections into 5 tabs: General, Providers, Connections, Notifications, Maintenance
- [x] Implement JS-driven tab switching with `showTab()` function (hides/shows tab content divs)
- [x] Deep-linking via `?tab=` query param, parsed in `handleSettingsPage` and passed as `ActiveTab` in `SettingsData`
- [x] Mobile-friendly: tabs use `overflow-x-auto` horizontal scroll with `whitespace-nowrap`

Phase 2: Provider priority chip redesign (#91)
- [x] Replace `PriorityRow` table rows with `PriorityChipRow` drag-and-drop chip components
- [x] Each chip: `IconBars2` drag handle, provider name, green/red enable/disable toggle button
- [x] Drag-and-drop reordering via vendored SortableJS v1.15.7 (`web/static/js/Sortable.min.js`)
- [x] Per-field provider enable/disable toggle: `PUT /api/v1/providers/priorities/{field}/{provider}/toggle`
- [x] `Disabled` field added to `FieldPriority` struct in `internal/provider/settings.go`
- [x] `EnabledProviders()` method filters out disabled providers; used by `FetchMetadata` and `FetchFieldFromProviders`
- [x] `SetDisabledProviders()` persists disabled state; `priorityDisabledKey()` generates storage keys
- [x] Removed dead code: old `PriorityRow` component and `prioritySwapJSON` helper

Testing and PR
- [x] Tests pass: `go test ./...`
- [x] Lint passes: `golangci-lint run ./...`
- [x] Docker build succeeds and container starts cleanly (31 static assets including Sortable.min.js)
- [x] Manual acceptance test: tabs switch, deep links work, drag reorder works, toggle persists
- [x] PR created and merged -- PR #110
- [x] PR checks pass (no CI failures)
- [x] PR reviewed (Copilot: 10 comments across 2 rounds, all addressed)

**Files:**
- `web/templates/settings.templ` -- major rewrite: tab navigation, `PriorityChipRow` replacing `PriorityRow`
- `web/components/icon.templ` -- added `IconBars2` (drag handle)
- `web/static/js/Sortable.min.js` -- vendored SortableJS v1.15.7
- `web/templates/layout.templ` -- added SortableJS to `AssetPaths`
- `internal/api/handlers.go` -- added SortableJS path to `assets()`
- `internal/api/handlers_platform.go` -- parse `?tab` query param, add `ActiveTab` to `SettingsData`
- `internal/api/handlers_provider.go` -- updated `handleSetPriorities` to use `PriorityChipRow`, added `handleToggleFieldProvider`
- `internal/api/router.go` -- added `PUT /api/v1/providers/priorities/{field}/{provider}/toggle` route
- `internal/provider/settings.go` -- `Disabled` field, `EnabledProviders()`, `SetDisabledProviders()`, `priorityDisabledKey()`
- `internal/provider/orchestrator.go` -- `FetchMetadata` and `FetchFieldFromProviders` use `EnabledProviders()`

---

### 7. #99 -- Image management improvements: unified search, edit, upload UX

`[mode: plan]` `[model: opus]`

**Why separate:** Primarily touches `image_search.templ` and image components, not `artist_detail.templ`. The local image serving endpoint it depends on is now merged from #98.

**Branch:** `feature/99-image-management-ux`

**Blocked by:** #98 (merged)

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
- [ ] Save and delete use confirmation dialog with "Don't ask again" (shared from #100)

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

### 8. #94 -- Developer documentation overhaul

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

- Items 1-5 (bugs + clobber detection + image previews + metadata refresh) are complete.
- #98 and #100 were originally combined but split back into separate PRs to manage session scope. #98 provides the icon infrastructure and image serving endpoints. #100 builds on that with metadata editing and provider fetch. Both now merged.
- Item 6 consolidates #91 and #95 into one PR since both rewrite `settings.templ`. Tab navigation wraps around the new provider chip design.
- Item 7 (#99) depends on #98 for the local image serving endpoint and edit icon entry point. Now unblocked.
- Item 8 (#94) should be the last item. It cleans up completed plan files and this workplan.
- All icon work uses Heroicons inline SVGs via reusable Templ components in `web/components/icon.templ`. No Unicode emoji, no icon fonts.
- Two new issues (#105 hamburger menu nav, #106 contextual menus audit) were opened but are not part of this workplan.
