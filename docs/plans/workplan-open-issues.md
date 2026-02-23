# Open Issues Workplan

## Goal

Work through all 9 open issues in priority order, respecting blocking relationships. Each issue gets its own feature branch, PR, and testing cycle before merge.

## Progress Summary

- **Completed:** 3 of 9 issues (#90, #97, #88)
- **Remaining:** 6 issues (#98, #100, #91, #95, #99, #94)
- **Next up:** #98 (high priority, unblocks #99)

## Dependency Graph

```
#90 DONE (critical bug, no blockers) -- PR #101 merged
#97 DONE (high bug, no blockers) -- PR #102 merged
#88 DONE (medium, no blockers) -- PR #103 merged
#98 (high, soft depends on #97) <-- NEXT
#100 (medium, no blockers)
#91 (medium, no blockers)
#95 (medium, no blockers)
#99 (medium, blocked by #98)
#94 (low, no blockers, intentionally last)
```

## Issues

| # | Title | Priority | Scope | Mode | Model | Status |
|---|-------|----------|-------|------|-------|--------|
| 90 | Connections bug: stale encrypted row poisons List() | critical | small | direct | sonnet | DONE (PR #101) |
| 97 | Fanart.tv images missing dimensions and rendering blank | high | small | direct | sonnet | DONE (PR #102) |
| 88 | NFO and artwork clobber risk detection and UI warnings | medium | medium | plan | sonnet | DONE (PR #103) |
| 98 | Display existing local images on artist detail page | high | medium | plan | opus | **NEXT**  |
| 100 | Per-artist metadata refresh with field-level provider selection | medium | large | plan | opus | open |
| 91 | Provider Priority UI redesign: drag-drop chips | medium | large | plan | opus | open |
| 95 | Settings page: add tab navigation for sections | medium | medium | direct | sonnet | open |
| 99 | Image management improvements: unified search, edit, upload UX | medium | large | plan | opus | blocked by #98 |
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

### 4. #98 + #100 -- Artist detail page: images, metadata editing, and per-field refresh

`[mode: plan]` `[model: opus]`

**Why next:** #98 is high priority and foundational. #100 touches the same files (`artist_detail.templ`, `router.go`, `handlers_settings.go`, `settings.templ`) so they belong in one PR to avoid repeated merge conflicts and duplicate confirmation-dialog work.

**Branch:** `feature/98-100-artist-detail-overhaul`

**Closes:** #98, #100

**Soft dependency:** #97 (merged)

#### Icon guidance

All edit and delete icons must use **Heroicons** (inline SVG), not Unicode emoji or icon fonts. Heroicons are made by the Tailwind CSS team and are pure SVG with no font files or JS runtime. Create a reusable Templ component (e.g., `Icon(name string)`) that renders the SVG path for each icon. Key icons needed: `pencil-square` (edit), `trash` (delete), `arrow-path` (refresh), `bars-3` (drag handle). Copy SVG paths from the Heroicons "outline" set (24x24). This applies everywhere icons are added in this PR (image hover overlays and metadata field action buttons).

#### Checklist

Phase 0: Icon infrastructure
- [ ] Create a reusable Templ component `Icon(name string)` in `web/components/`
- [ ] Copy SVG paths from Heroicons outline set (24x24) for: `pencil-square`, `trash`, `arrow-path`, `bars-3`
- [ ] Verify icon rendering in layout with Tailwind size/color classes

Phase 1: Local image serving endpoint (#98)
- [ ] Add `GET /api/v1/artists/{id}/images/{type}` route in `router.go`
- [ ] Handler finds image file using scanner patterns, serves with Content-Type and caching headers

Phase 2: Local image metadata endpoint (#98)
- [ ] Add `GET /api/v1/artists/{id}/images/{type}/info` returning JSON (width, height, fileSize, filename, format)
- [ ] Reuse `getThumbDimensions()` pattern from `checkers.go`

Phase 3: Replace status badges with image previews (#98)
- [ ] Replace StatusBadge calls in `artist_detail.templ` with `<img>` tags for existing images
- [ ] Show placeholder for missing image types

Phase 4: Resolution and file size display (#98)
- [ ] Show dimensions and file size beneath each image preview (HTMX load from info endpoint)

Phase 5: Hover overlay with edit and delete icons (#98)
- [ ] `pencil-square` icon navigates to `/artists/{id}/images?type={type}`
- [ ] `trash` icon triggers delete with confirmation

Phase 6: Image delete endpoint and confirmation dialog (#98)
- [ ] Add `DELETE /api/v1/artists/{id}/images/{type}` handler
- [ ] Confirmation dialog with "Don't ask again" checkbox
- [ ] Store preference in settings key-value store
- [ ] Add toggle in Settings page to re-enable dialog

Phase 7: Edit and delete icons on metadata fields (#100)
- [ ] Add `pencil-square` and `trash` icon buttons next to Biography, Genres, Styles, Moods, Life events, Members

Phase 8: Inline manual edit per field (#100)
- [ ] HTMX swap to editable form on `pencil-square` icon click (textarea, tag input, date input)
- [ ] Add `PATCH /api/v1/artists/{id}/fields/{field}` endpoint
- [ ] Add `DELETE /api/v1/artists/{id}/fields/{field}` endpoint

Phase 9: Per-field provider fetch (#100)
- [ ] Add `GET /api/v1/artists/{id}/fields/{field}/providers` endpoint
- [ ] Call orchestrator for specific field, return results from all enabled providers
- [ ] Inline expandable panel showing provider results side-by-side

Phase 10: Global "Refresh Metadata" button (#100)
- [ ] Add `POST /api/v1/artists/{id}/refresh` endpoint
- [ ] Trigger full metadata fetch via orchestrator

Phase 11: Delete/reset field with confirmation (#100)
- [ ] `trash` icon triggers DELETE with configurable confirmation dialog
- [ ] Reuse "Don't ask again" pattern from Phase 6

Testing and PR
- [ ] Tests pass: `go test ./...`
- [ ] Lint passes: `golangci-lint run ./...`
- [ ] Manual acceptance test: image previews, hover icons, metadata edit/delete, provider fetch, global refresh
- [ ] PR created and merged
- [ ] PR checks pass (no CI failures)
- [ ] PR reviewed (check for copilot feedback)

**Files:**
- `web/components/icon.templ` -- reusable Heroicons SVG component
- `web/templates/artist_detail.templ` -- image previews, metadata icons, inline edit, provider panel
- `internal/api/router.go` -- image serve/delete routes, field PATCH/DELETE/GET routes, refresh route
- `internal/api/handlers_image.go` -- serve, info, delete handlers
- `internal/api/handlers_artist.go` -- field PATCH/DELETE/GET, refresh handlers
- `internal/provider/orchestrator.go` -- per-field fetch method
- `internal/api/handlers_settings.go` -- confirmation dialog preferences
- `web/templates/settings.templ` -- confirmation dialog toggle

---

### 5. #91 + #95 -- Settings page: tab navigation and provider priority chips

`[mode: plan]` `[model: opus]`

**Why together:** Both issues rewrite `settings.templ`. #91 redesigns the provider priority section as drag-and-drop chips; #95 wraps all sections in tab navigation. Doing them together avoids merge conflicts and produces a coherent settings page layout.

**Branch:** `feature/91-95-settings-overhaul`

**Closes:** #91, #95

#### Icon guidance

Use the Heroicons component (created in the previous PR) for the drag handle icon (`bars-3`) on provider chips.

#### Checklist

Phase 1: Tab navigation (#95)
- [ ] Group settings sections into tabs: General, Providers, Scraper, Connections, Notifications, Maintenance
- [ ] Implement tab switching (HTMX or JS-driven, preserving scroll position)
- [ ] Ensure deep-linking works (e.g., `/settings?tab=providers`)
- [ ] Mobile-friendly tab navigation (horizontal scroll or dropdown)

Phase 2: Provider priority chip redesign (#91)
- [ ] Design chip/card component for each provider (`bars-3` drag handle icon, name, enable/disable toggle)
- [ ] Implement drag-and-drop reordering (consider SortableJS or native HTML5 drag)
- [ ] Grey out chips for unconfigured providers (no valid API key)
- [ ] Persist reorder and enable/disable state via existing provider priority API
- [ ] Mobile support: tap-to-toggle, swipe-to-reorder or fallback to up/down buttons

Testing and PR
- [ ] Tests pass: `go test ./...`
- [ ] Lint passes: `golangci-lint run ./...`
- [ ] Manual acceptance test: tabs switch, deep links work, drag reorder works, toggle persists, unconfigured greyed
- [ ] PR created and merged
- [ ] PR checks pass (no CI failures)
- [ ] PR reviewed (check for copilot feedback)

**Files:**
- `web/templates/settings.templ` -- tab navigation wrapper + provider chip redesign
- `web/static/` -- vendored SortableJS if needed
- `internal/api/handlers_settings.go` -- accept `tab` query param

---

### 6. #99 -- Image management improvements: unified search, edit, upload UX

`[mode: plan]` `[model: opus]`

**Why separate:** Primarily touches `image_search.templ` and image components, not `artist_detail.templ`. The local image serving endpoint it depends on will already be merged from the #98+#100 PR.

**Branch:** `feature/99-image-management-ux`

**Blocked by:** #98+#100 PR (provides serving endpoint and `pencil-square` icon entry point)

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
- [ ] Save and delete use confirmation dialog with "Don't ask again" (shared from #98+#100)

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

### 7. #94 -- Developer documentation overhaul

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

- Items 1-3 (bugs + clobber detection) are complete.
- Item 4 consolidates #98 and #100 into one PR since both heavily modify `artist_detail.templ`, `router.go`, and `handlers_settings.go`. This avoids merge conflicts and shares the confirmation dialog implementation.
- Item 5 consolidates #91 and #95 into one PR since both rewrite `settings.templ`. Tab navigation wraps around the new provider chip design.
- Item 6 (#99) depends on item 4 for the local image serving endpoint and edit icon entry point.
- Item 7 (#94) should be the last item. It cleans up completed plan files and this workplan.
- All icon work uses Heroicons inline SVGs via a reusable Templ component. No Unicode emoji, no icon fonts.
