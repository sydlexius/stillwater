# Milestone 22 -- Platform Profiles & Images

## Goal

Support multiple image filenames per platform profile and make custom profiles fully
editable. Update seed data to reflect what each platform actually supports, expose
array-based naming in the UI, and add validation for custom filename entry.

## Acceptance Criteria

- [x] Built-in platform profiles seed all supported filenames per image type (not just the primary)
- [x] `getActiveNamingConfig()` returns full filename arrays, not just the primary
- [x] Image save writes copies for every filename in the active profile
- [x] Extraneous images checker/fixer accounts for all canonical filenames
- [x] Custom platform profile allows adding/removing filenames per image type in Settings UI
- [x] Filename validation rejects path separators and non-.jpg/.png extensions; logos require .png
- [x] Existing tests updated for multi-filename behavior
- [x] New migration seeds array-format naming data

## Dependency Map

#210 (multiple image copies) and #211 (custom profile editing) share UI surface area
in the platform profile settings section. #210 provides the backend + migration;
#211 adds validation and the editable UI. Shipped as a combined PR stack.

```
#210 --> #211
```

## Checklist

### Issue #210 -- Multiple image copies per platform profile
- [x] New migration `004_platform_naming_arrays.sql` with updated seed data
- [x] `NamesForType()` helper on platform model
- [x] `getActiveNamingConfig()` returns full arrays from `ImageNaming.ToMap()`
- [x] Update extraneous images checker in `rule/checkers.go`
- [x] Update extraneous images fixer in `rule/fixers.go`
- [x] Update tests (extraneous images, naming config)
- [x] PRs opened (#252-#261)
- [x] CI passing
- [x] PRs merged

### Issue #211 -- Custom platform profile -- fully editable filenames
- [x] `ValidateImageNaming()` in `platform/model.go`
- [x] Wire validation into `handleCreatePlatform()` and `handleUpdatePlatform()`
- [x] `profileNamingEditor` templ component in `settings.templ`
- [x] Inline JS for add/remove filename chips + JSON PUT
- [x] Tests for validation logic
- [x] PRs opened (#252-#261)
- [x] CI passing
- [x] PRs merged

### Review followup
- [x] SSRF transport hardening (Copilot + CodeQL feedback)
- [x] Fanart index mismatch fix (NextFanartIndex helper)
- [x] CodeQL unsafe-quoting fix (hxValsJSON replaces fmt.Sprintf)
- [x] Platform handler test coverage
- [x] NextFanartIndex doc comment fix
- [x] PR opened (#273)
- [ ] CI passing
- [ ] PR merged

## UAT / Merge Order

1. PRs #252-#261 (base: main) -- full M22 implementation (merged)
2. PR #273 (base: main) -- review followup fixes

## Notes

- Research needed: exact filenames Emby, Jellyfin, and Kodi support for each image type
- `artist.jpg` is canonical for Kodi thumb, so extraneous images tests need updating
- Backend `Save()` already loops over arrays; the gap is in config retrieval and seed data
- 2026-02-27: M22 PR stack (#252-#261) merged to main
- 2026-02-27: PR #272 opened for review followup, then closed in favor of PR #273 with issue references
- 2026-02-27: CodeQL alert #9 (go/request-forgery) dismissed as false positive -- SSRF protection at transport level
- 2026-02-27: CodeQL alerts #14-18 (go/unsafe-quoting) fixed by hxValsJSON, will auto-close after PR #273 merge
- 2026-02-27: Copilot review comments addressed -- doc fix committed, SSRF test rationale explained
