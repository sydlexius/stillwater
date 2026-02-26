# Milestone 18 -- OOBE & Onboarding

## Goal

Enhance the OOBE wizard and add in-app documentation. Covers provider configuration
in OOBE Step 3, library structure guidance in OOBE Step 1, and the /guide page.

## Acceptance Criteria

- [x] TheAudioDB shows as "Free tier" with optional premium key input in OOBE and Settings
- [x] DuckDuckGo web search toggle appears in OOBE Step 3
- [x] HTMX re-renders use the correct card template based on page context (OOBE vs Settings)
- [x] All existing provider behavior is unchanged
- [x] Unit tests cover OptionalKey field logic
- [x] OOBE introduction step (#203)
- [ ] Library structure info callout in OOBE Step 1
- [ ] In-app /guide page with all sections
- [ ] Guide page linked in navbar
- [ ] Guide handler tests pass
- [ ] GitHub wiki pages (#125)

## Dependency Map

#200 (OptionalKey field) and #201 (web search in OOBE) are independent but share
enough OOBE template surface area to ship as a single PR.
#203 (OOBE intro) ships separately.
#202 (library structure + guide) depends on #203 being merged.
#125 (wiki) ships after #202.

## Checklist

### Issue #200 -- Restore TheAudioDB optional premium API key input
- [x] Implementation
- [x] Tests
- [x] PR opened (#214)
- [x] PR merged

### Issue #201 -- Add web scraper enable/disable to OOBE
- [x] Implementation
- [x] PR opened (#214, same as #200)
- [x] PR merged

### Issue #203 -- OOBE introduction step
- [x] Implementation
- [x] PR opened (#215)
- [x] PR merged

### Issue #202 -- Library structure requirements in OOBE and user guide
- [x] OOBE Step 1 info callout
- [x] Guide page template (guide.templ)
- [x] Guide handler (handlers_guide.go)
- [x] Route registration in router.go
- [x] Navbar link in layout.templ (desktop + mobile)
- [x] Handler tests (handlers_guide_test.go)
- [ ] PR opened
- [ ] CI passing
- [ ] PR merged

### Issue #125 -- GitHub wiki
- [ ] Wiki pages created
- [ ] Issue closed

## UAT / Merge Order

1. PR #214 covering #200 and #201 (base: main) -- merged
2. PR #215 covering #203 (base: main) -- merged
3. PR for #202 (base: main)
4. Wiki pages for #125 (after #202 merges)

## Notes

- 2026-02-26: Issues #200, #201, #203 complete. PRs #214 and #215 merged.
- 2026-02-26: Issue #202 implementation complete. Guide page, OOBE callout, tests all passing.
