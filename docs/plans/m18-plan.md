# Milestone 18 -- OOBE & Onboarding

## Goal

Enhance the OOBE wizard (Step 3: Providers) to surface TheAudioDB optional premium
key input and the DuckDuckGo web search enable/disable toggle, so users can configure
all provider options during initial setup without needing to visit Settings afterwards.

## Acceptance Criteria

- [ ] TheAudioDB shows as "Free tier" with optional premium key input in OOBE and Settings
- [ ] DuckDuckGo web search toggle appears in OOBE Step 3
- [ ] HTMX re-renders use the correct card template based on page context (OOBE vs Settings)
- [ ] All existing provider behavior is unchanged
- [ ] Unit tests cover OptionalKey field logic

## Dependency Map

#200 (OptionalKey field) and #201 (web search in OOBE) are independent but share
enough OOBE template surface area to ship as a single PR.

## Checklist

### Issue #200 -- Restore TheAudioDB optional premium API key input
- [ ] Add `OptionalKey` field to `ProviderKeyStatus`
- [ ] Add `providerHasOptionalKey` helper
- [ ] Update `ListProviderKeyStatuses` status logic
- [ ] Update `onboardingProviderCard` template
- [ ] Update `ProviderKeyCard` template (Settings)
- [ ] Update `getKeyLinkText` for freemium tier
- [ ] Fix `handleSetProviderKey` OOBE context detection
- [ ] Export `OnboardingProviderCard`
- [ ] Tests for OptionalKey
- [ ] PR opened (#?)
- [ ] CI passing
- [ ] PR merged

### Issue #201 -- Add web scraper enable/disable to OOBE
- [ ] Add `WebSearchProviders` to `OnboardingData`
- [ ] Add web search subsection to OOBE Step 3
- [ ] Create `OnboardingWebSearchToggle` component
- [ ] Load web search data in `handleOnboardingPage`
- [ ] Fix `handleSetWebSearchEnabled` OOBE context detection
- [ ] PR opened (same as #200)
- [ ] CI passing
- [ ] PR merged

## UAT / Merge Order

1. Single PR covering both #200 and #201 (base: main)

## Notes

- 2026-02-26: Both issues target OOBE Step 3 and share template code, shipping together.
