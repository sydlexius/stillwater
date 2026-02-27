# Milestone 22 -- Platform Profiles & Images

## Goal

Support multiple image filenames per platform profile and make custom profiles fully
editable. Update seed data to reflect what each platform actually supports, expose
array-based naming in the UI, and add validation for custom filename entry.

## Acceptance Criteria

- [ ] Built-in platform profiles seed all supported filenames per image type (not just the primary)
- [ ] `getActiveNamingConfig()` returns full filename arrays, not just the primary
- [ ] Image save writes copies for every filename in the active profile
- [ ] Extraneous images checker/fixer accounts for all canonical filenames
- [ ] Custom platform profile allows adding/removing filenames per image type in Settings UI
- [ ] Filename validation rejects path separators and non-.jpg/.png extensions; logos require .png
- [ ] Existing tests updated for multi-filename behavior
- [ ] New migration seeds array-format naming data

## Dependency Map

#210 (multiple image copies) and #211 (custom profile editing) share UI surface area
in the platform profile settings section. #210 provides the backend + migration;
#211 adds validation and the editable UI. Ship as a single PR or stack #211 on #210.

```
#210 --> #211
```

## Checklist

### Issue #210 -- Multiple image copies per platform profile
- [ ] New migration `004_platform_naming_arrays.sql` with updated seed data
- [ ] `NamesForType()` helper on platform model
- [ ] `getActiveNamingConfig()` returns full arrays from `ImageNaming.ToMap()`
- [ ] Update extraneous images checker in `rule/checkers.go`
- [ ] Update extraneous images fixer in `rule/fixers.go`
- [ ] Update tests (extraneous images, naming config)
- [ ] PR opened (#?)
- [ ] CI passing
- [ ] PR merged

### Issue #211 -- Custom platform profile -- fully editable filenames
- [ ] `ValidateImageNaming()` in `platform/model.go`
- [ ] Wire validation into `handleCreatePlatform()` and `handleUpdatePlatform()`
- [ ] `profileNamingEditor` templ component in `settings.templ`
- [ ] Inline JS for add/remove filename chips + JSON PUT
- [ ] Tests for validation logic
- [ ] PR opened (#?)
- [ ] CI passing
- [ ] PR merged

## UAT / Merge Order

1. PR for #210 (base: main) -- migration + backend changes
2. PR for #211 (base: main or stacked on #210) -- validation + UI

If changes are small enough, combine into a single PR.

## Notes

- Research needed: exact filenames Emby, Jellyfin, and Kodi support for each image type
- `artist.jpg` is canonical for Kodi thumb, so extraneous images tests need updating
- Backend `Save()` already loops over arrays; the gap is in config retrieval and seed data
