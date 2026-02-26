# Milestone 18 -- OOBE & Onboarding

## Goal

Enhance the OOBE wizard (Step 3: Providers) to surface TheAudioDB optional premium
key input and the DuckDuckGo web search enable/disable toggle, so users can configure
all provider options during initial setup without needing to visit Settings afterwards.

## Acceptance Criteria

- [x] TheAudioDB shows as "Free tier" with optional premium key input in OOBE and Settings
- [x] DuckDuckGo web search toggle appears in OOBE Step 3
- [x] HTMX re-renders use the correct card template based on page context (OOBE vs Settings)
- [x] All existing provider behavior is unchanged
- [x] Unit tests cover OptionalKey field logic

## Dependency Map

#200 (OptionalKey field) and #201 (web search in OOBE) are independent but share
enough OOBE template surface area to ship as a single PR.

## Checklist

### Issue #200 -- Restore TheAudioDB optional premium API key input
- [x] Add `OptionalKey` field to `ProviderKeyStatus`
- [x] Add `providerHasOptionalKey` helper
- [x] Update `ListProviderKeyStatuses` status logic
- [x] Update `onboardingProviderCard` template (exported as `OnboardingProviderCard`)
- [x] Update `ProviderKeyCard` template (Settings)
- [x] Update `getKeyLinkText` for freemium tier
- [x] Fix `handleSetProviderKey` OOBE context detection
- [x] Export `OnboardingProviderCard`
- [x] Tests for OptionalKey
- [x] PR opened (#214)
- [ ] CI passing
- [ ] PR merged

### Issue #201 -- Add web scraper enable/disable to OOBE
- [x] Add `WebSearchProviders` to `OnboardingData`
- [x] Add web search subsection to OOBE Step 3
- [x] Create `OnboardingWebSearchToggle` component
- [x] Load web search data in `handleOnboardingPage`
- [x] Fix `handleSetWebSearchEnabled` OOBE context detection
- [x] PR opened (#214, same as #200)
- [ ] CI passing
- [ ] PR merged

## UAT / Merge Order

1. PR #214 covering both #200 and #201 (base: main)

## Notes

- 2026-02-26: Both issues target OOBE Step 3 and share template code, shipping together.
- 2026-02-26: All implementation complete. PR #214 opened. Awaiting CI and review.
