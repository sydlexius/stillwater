# Milestone 28 -- Bliss-Inspired Rule & Fix UX

## Goal

Redesign the rule violation and notification experience to match Bliss-quality UX:
filterable/sortable notifications table, inline fix panels with one-click resolution,
image deduplication via perceptual hashing, directory rename rule, and per-artist
rule execution.

## Acceptance Criteria

- [ ] UX audit findings documented with actionable recommendations tied to Stillwater components
- [ ] Notifications table supports column sorting, filtering, and grouping
- [ ] Each fixable violation shows inline fix options with a recommended action
- [ ] Global "Fix All" applies recommended fixes in bulk
- [ ] Image deduplication rule detects exact and perceptually similar duplicates
- [ ] Directory rename rule detects and optionally fixes artist folder name mismatches
- [ ] Per-artist "Run Rules" button on artist detail page
- [ ] Rule violation artist names are clickable hyperlinks with library context
- [ ] Backdrop/fanart sequencing rule detects and fixes numbering gaps (enabled toggle + automation mode: manual/auto)
- [ ] Extraneous image rule is backdrop-sequencing-aware (does not flag valid sequences)
- [ ] Logo padding rule detects excessive transparent borders (enabled toggle + automation mode: manual/auto)
- [ ] Per-rule "Run Now" button for individual rule evaluation

## Dependency Map

```
#288 (UX audit) --> #286 (actionable fixes)
                \-> #310 (notifications table)
#310 (notifications table) --> #286 (fix panels live in the notifications table)

#287 (image dedup) -- independent
#313 (directory rename) -- independent
#314 (per-artist Run Rules) -- independent
```

#288 should be completed first to inform the design of #286 and #310.
#310 should be completed before #286 (fix panels are rendered within the table).
#287, #313, and #314 are independent and can proceed in parallel.

#512 (rule hyperlinks) -- independent UX improvement
#519 (backdrop sequencing rule) -- independent, new rule
#520 (extraneous image rule update) -- depends on #519 (must know what valid sequences look like)
#521 (logo padding rule) -- independent, new rule
#523 (per-rule Run Now) -- independent, extends rule engine
#526 (junk metadata detection) -- independent, new rule + ingestion filter

## Checklist

### Issue #288 -- UX audit: Bliss-inspired design patterns
- [ ] Review Bliss tour page and documentation
- [ ] Categorized findings document with recommendations
- [ ] Each recommendation tied to a specific Stillwater component or page
- [ ] Follow-up issues created for approved recommendations
- [ ] Findings posted to issue
- [ ] Issue closed

### Issue #310 -- Filterable, sortable, and groupable Notifications table
- [ ] Clickable column headers with sort direction indicators
- [ ] Column-level filter dropdowns (artist, rule, severity, category, date range)
- [ ] Collapsible group headers with count badges
- [ ] Default grouping: artist then rule category
- [ ] Tests

### Issue #286 -- Bliss-like actionable fix options on Notifications page
- [ ] Collapsible inline fix panels below each violation row
- [ ] Image violations: candidate grid with "Apply" buttons
- [ ] Missing metadata violations: pre-filled provider data with "Apply"
- [ ] Format/missing NFO violations: one-click fix buttons
- [ ] Fix recommendation badges with highest-confidence option
- [ ] Global "Fix All" button for bulk application
- [ ] Tests

### Issue #287 -- Image deduplication rule using perceptual hashing
- [ ] Hashing module in `internal/image/` (SHA-256 exact + pHash/dHash perceptual)
- [ ] New rule in `internal/rule/` with configurable similarity threshold
- [ ] Two automation modes: auto (keep higher quality), manual (flag for review)
- [ ] Scope options: per-artist, per-library, or global
- [ ] Violation display with thumbnails, similarity score, and file paths
- [ ] Tests
- [ ] Docs updated (Architecture wiki: new rule type)

### Issue #313 -- Artist directory rename rule (directory_name_mismatch)
- [ ] Detection: compare directory name against MusicBrainz canonical name
- [ ] Configurable article dictionary for sort-name handling
- [ ] Two automation modes: manual (confirm rename), auto (rename + notify)
- [ ] Safety: verify no file locks, atomic rename with copy+delete fallback
- [ ] Update all internal database references to old path
- [ ] Tests
- [ ] Docs updated (Architecture wiki: new rule type)

### Issue #314 -- Per-artist Run Rules button on artist detail page
- [ ] New endpoint: `POST /api/v1/artists/{id}/run-rules`
- [ ] Button in artist detail context menu
- [ ] Reuse existing `RunRules()` engine scoped to single artist ID
- [ ] Spinner/progress indicator while running
- [ ] Result count and link to filtered Notifications view
- [ ] Tests

### Issue #353 -- Remove redundant Disabled option from rule automation mode dropdown
- [ ] Remove `AutomationModeDisabled` constant from `internal/rule/model.go`
- [ ] Remove "Disabled" option from settings template dropdown
- [ ] Remove "disabled" from valid automation_mode values in handler
- [ ] Migration to convert existing `automation_mode = 'disabled'` rows
- [ ] Update tests referencing AutomationModeDisabled
- [ ] Add test: PATCH with `automation_mode: "disabled"` returns 400

### Issue #512 -- Rule violation artist names as hyperlinks with library context
- [ ] Artist name in violation entries links to `/artists/{id}`
- [ ] Library name/path displayed alongside artist name for disambiguation
- [ ] Works in table and any other violation display modes
- [ ] Tests
- [ ] PR merged

### Issue #519 -- Backdrop/fanart image sequencing gap rule
- [ ] Detection: gaps in backdrop/fanart sequences (filesystem + Emby/Jellyfin API)
- [ ] Detection: `backdrop1.ext` without `backdrop.ext` (wrong first position)
- [ ] Enabled toggle; manual mode (preview renames) / auto mode (fix during evaluation)
- [ ] Platform-aware naming (fanart for Kodi, backdrop for Emby/Jellyfin)
- [ ] Atomic file renames for filesystem fixes
- [ ] Tests
- [ ] PR merged

### Issue #520 -- Update extraneous image rule for backdrop sequencing awareness
- [ ] Correctly allows sequential backdrop/fanart files as non-extraneous
- [ ] Flags non-standard naming patterns (`backdrop_old.png`, etc.)
- [ ] Enabled toggle + automation mode: manual/auto
- [ ] No overlap with sequencing rule (#519)
- [ ] Tests
- [ ] PR merged

### Issue #521 -- Logo padding detection rule
- [ ] Detection: logos with padding exceeding configurable threshold
- [ ] Enabled toggle; manual mode (auto-trim + open trim tool) / auto mode (trim during evaluation)
- [ ] Configurable padding threshold and trim margin
- [ ] Integrates with existing manual trim tool
- [ ] Tests
- [ ] PR merged

### Issue #526 -- Detect and filter low-quality placeholder metadata
- [ ] Ingestion filter rejects junk values ("?", "N/A", "Unknown", etc.) during fetch
- [ ] Rejected values fall through to next provider in priority chain
- [ ] Detection rule finds existing junk values in the database
- [ ] Enabled toggle; manual mode (clear/re-fetch/keep) / auto mode (clear + optional re-fetch)
- [ ] Configurable junk patterns and field-specific minimum lengths
- [ ] Tests
- [ ] PR merged

### Issue #523 -- Per-rule "Run Now" button
- [ ] "Run Now" button on each enabled rule row in Rules settings
- [ ] Evaluates single rule against all applicable artists
- [ ] Progress indicator and result summary
- [ ] API: `POST /api/v1/rules/{rule_id}/evaluate`
- [ ] Tests
- [ ] PR merged

## UAT / Merge Order

Session 0 (quick fix):
0. PR for #353 (base: main) -- remove redundant disabled option (small, independent)

Session 1 (foundations):
1. PR for #288 (base: main) -- UX audit findings (research only)
2. PR for #314 (base: main) -- per-artist Run Rules (small, independent)

Session 2 (notifications overhaul):
3. PR for #310 (base: main) -- notifications table UX
4. PR for #512 (base: main) -- rule violation artist hyperlinks with library context
5. PR for #286 (base: main, after #310 merges) -- inline fix panels

Session 3 (new rules -- image sequencing):
6. PR for #519 (base: main) -- backdrop/fanart sequencing rule
7. PR for #520 (base: main, after #519 merges) -- extraneous image rule update

Session 4 (new rules -- independent):
8. PR for #287 (base: main) -- image deduplication rule
9. PR for #313 (base: main) -- directory rename rule
10. PR for #521 (base: main) -- logo padding rule
11. PR for #526 (base: main) -- junk metadata detection + ingestion filter

Session 5 (rule engine enhancements):
12. PR for #523 (base: main) -- per-rule "Run Now" button

## Notes

- #288: `[mode: plan]` -- scope: medium, UX research
- #286: `[mode: plan] [model: opus]` -- scope: large, complex UI work
- #287: `[mode: plan] [model: opus]` -- scope: large, new image processing + rule
- #310: scope: medium -- HTMX-driven table enhancements
- #313: scope: large -- filesystem mutations require careful safety handling
- #314: scope: small -- reuses existing rule engine infrastructure
- #353: `[mode: direct] [model: sonnet]` -- scope: small, remove redundant UI control
- Closed issues in this milestone: #312 (logo trimming), #307 (manual mode candidates), #303 (folder name mismatch detection)
