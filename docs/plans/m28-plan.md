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
- [ ] PR opened (#?)
- [ ] CI passing
- [ ] PR merged

### Issue #286 -- Bliss-like actionable fix options on Notifications page
- [ ] Collapsible inline fix panels below each violation row
- [ ] Image violations: candidate grid with "Apply" buttons
- [ ] Missing metadata violations: pre-filled provider data with "Apply"
- [ ] Format/missing NFO violations: one-click fix buttons
- [ ] Fix recommendation badges with highest-confidence option
- [ ] Global "Fix All" button for bulk application
- [ ] Tests
- [ ] PR opened (#?)
- [ ] CI passing
- [ ] PR merged

### Issue #287 -- Image deduplication rule using perceptual hashing
- [ ] Hashing module in `internal/image/` (SHA-256 exact + pHash/dHash perceptual)
- [ ] New rule in `internal/rule/` with configurable similarity threshold
- [ ] Two automation modes: auto (keep higher quality), manual (flag for review)
- [ ] Scope options: per-artist, per-library, or global
- [ ] Violation display with thumbnails, similarity score, and file paths
- [ ] Tests
- [ ] PR opened (#?)
- [ ] CI passing
- [ ] PR merged
- [ ] Docs updated (Architecture wiki: new rule type)

### Issue #313 -- Artist directory rename rule (directory_name_mismatch)
- [ ] Detection: compare directory name against MusicBrainz canonical name
- [ ] Configurable article dictionary for sort-name handling
- [ ] Two automation modes: manual (confirm rename), auto (rename + notify)
- [ ] Safety: verify no file locks, atomic rename with copy+delete fallback
- [ ] Update all internal database references to old path
- [ ] Tests
- [ ] PR opened (#?)
- [ ] CI passing
- [ ] PR merged
- [ ] Docs updated (Architecture wiki: new rule type)

### Issue #314 -- Per-artist Run Rules button on artist detail page
- [ ] New endpoint: `POST /api/v1/artists/{id}/run-rules`
- [ ] Button in artist detail context menu
- [ ] Reuse existing `RunRules()` engine scoped to single artist ID
- [ ] Spinner/progress indicator while running
- [ ] Result count and link to filtered Notifications view
- [ ] Tests
- [ ] PR opened (#?)
- [ ] CI passing
- [ ] PR merged

### Issue #353 -- Remove redundant Disabled option from rule automation mode dropdown
- [ ] Remove `AutomationModeDisabled` constant from `internal/rule/model.go`
- [ ] Remove "Disabled" option from settings template dropdown
- [ ] Remove "disabled" from valid automation_mode values in handler
- [ ] Migration to convert existing `automation_mode = 'disabled'` rows
- [ ] Update tests referencing AutomationModeDisabled
- [ ] Add test: PATCH with `automation_mode: "disabled"` returns 400
- [ ] PR opened (#?)
- [ ] CI passing
- [ ] PR merged

## UAT / Merge Order

Session 0 (quick fix):
0. PR for #353 (base: main) -- remove redundant disabled option (small, independent)

Session 1 (foundations):
1. PR for #288 (base: main) -- UX audit findings (research only)
2. PR for #314 (base: main) -- per-artist Run Rules (small, independent)

Session 2 (notifications overhaul):
3. PR for #310 (base: main) -- notifications table UX
4. PR for #286 (base: main, after #310 merges) -- inline fix panels

Session 3 (new rules):
5. PR for #287 (base: main) -- image deduplication rule
6. PR for #313 (base: main) -- directory rename rule

## Notes

- #288: `[mode: plan]` -- scope: medium, UX research
- #286: `[mode: plan] [model: opus]` -- scope: large, complex UI work
- #287: `[mode: plan] [model: opus]` -- scope: large, new image processing + rule
- #310: scope: medium -- HTMX-driven table enhancements
- #313: scope: large -- filesystem mutations require careful safety handling
- #314: scope: small -- reuses existing rule engine infrastructure
- #353: `[mode: direct] [model: sonnet]` -- scope: small, remove redundant UI control
- Closed issues in this milestone: #312 (logo trimming), #307 (manual mode candidates), #303 (folder name mismatch detection)
