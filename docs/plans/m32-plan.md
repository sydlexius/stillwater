# Milestone 32 (v0.32.0) -- Documentation & Guided Tour

## Goal

Improve application discoverability and documentation accuracy. Add an OpenAPI reference link in Settings, audit and update both user-facing and developer documentation, and implement a post-OOBE guided tour to introduce users to the application workflow.

## Acceptance Criteria

- [ ] Settings page links to interactive API documentation
- [ ] User guide reflects all current features and providers
- [ ] Developer documentation and wiki pages are accurate and current
- [ ] Post-OOBE guided tour walks users through key UI elements

## Dependency Map

```
#343 (OpenAPI in Settings) -- no deps, standalone
#344 (user guide audit)    -- no deps, standalone
#345 (developer docs audit)-- no deps, standalone
#346 (guided tour)         -- no deps, standalone (OOBE already stable)
```

All four issues are independent and can be worked in parallel.

## Checklist

### Issue #343 -- Add OpenAPI documentation reference to Settings page
- [ ] Add "API Documentation" card to Settings General tab
- [ ] Link to `/api/v1/docs` (opens in new tab)
- [ ] Dark mode compatible styling
- [ ] Update user guide if API/integration section exists
- [ ] PR opened (#?)
- [ ] CI passing
- [ ] PR merged

### Issue #344 -- Audit and update user guide for current features
- [ ] Audit provider documentation (all 10 providers listed)
- [ ] Audit platform profiles documentation
- [ ] Audit scanner behavior documentation
- [ ] Audit image management documentation
- [ ] Audit server connections documentation
- [ ] Audit rule engine documentation
- [ ] Audit automation documentation
- [ ] Audit settings documentation
- [ ] Verify OOBE steps match current implementation
- [ ] Update `web/templates/guide.templ`
- [ ] PR opened (#?)
- [ ] CI passing
- [ ] PR merged

### Issue #345 -- Audit and update developer documentation and wiki
- [ ] Audit Architecture wiki page
- [ ] Audit Developer Guide wiki page
- [ ] Audit Contributing wiki page
- [ ] Audit `docs/dev-setup.md`
- [ ] Audit `CLAUDE.md` for accuracy
- [ ] Push wiki updates
- [ ] PR opened (#?) for repo file changes
- [ ] CI passing
- [ ] PR merged

### Issue #346 -- Post-OOBE guided tour with tooltip walkthrough
- [ ] Evaluate and vendor Driver.js (~5KB gzip, MIT, zero deps)
- [ ] Create tour configuration (5-8 steps covering key UI elements)
- [ ] Implement auto-trigger after OOBE completion
- [ ] Implement manual restart from user guide
- [ ] Dark mode styling
- [ ] Cache-bust vendored assets via StaticAssets
- [ ] Manual testing: OOBE -> tour -> dismissal -> no re-trigger
- [ ] PR opened (#?)
- [ ] CI passing
- [ ] PR merged
- [ ] Docs updated

## Worktrees

| Directory | Branch | Issue | Status |
|-----------|--------|-------|--------|
| stillwater-343 | feat/343-openapi-settings | #343 | pending |
| stillwater-344 | feat/344-user-guide-audit | #344 | pending |
| stillwater-345 | feat/345-developer-docs-audit | #345 | pending |
| stillwater-346 | feat/346-guided-tour | #346 | pending |

## UAT / Merge Order

All PRs are independent. Merge order does not matter, but a suggested sequence:

1. PR for #343 (base: main) -- smallest change, quick win
2. PR for #344 (base: main) -- user guide audit
3. PR for #345 (base: main) -- developer docs audit
4. PR for #346 (base: main) -- guided tour (largest scope)

## New Dependencies

- [Driver.js](https://driverjs.com/) v1.x -- lightweight tooltip tour library (~5KB gzip, MIT license, zero dependencies). Vendored into `web/static/js/` and `web/static/css/` per project conventions.

## Wiki Pages Affected

- [Architecture](https://github.com/sydlexius/stillwater/wiki/Architecture) -- updated during #345
- [Developer Guide](https://github.com/sydlexius/stillwater/wiki/Developer-Guide) -- updated during #345
- [Contributing](https://github.com/sydlexius/stillwater/wiki/Contributing) -- updated during #345
- User-facing wiki pages -- updated during #344 if applicable

## Notes

- 2026-03-02: Milestone created. OpenAPI docs already exist at `/api/v1/docs` (Scalar API Reference); this milestone adds discoverability from within the app.
- 2026-03-02: Driver.js chosen over Shepherd.js for guided tour due to smaller bundle size (5KB vs 25KB) and zero dependencies, aligning with the project's minimal JS philosophy.
- 2026-03-02: User guide audit scope covers all features added since the guide was last updated, including: Genius, Spotify, Deezer, DuckDuckGo providers; scraper configuration; rule engine; MusicBrainz mirror support; image cropping; API tokens.
