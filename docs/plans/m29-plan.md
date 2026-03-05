# Milestone 29 -- Release Pipeline

## Goal

Add differentiated Docker release channels (nightly, beta, versioned) and improve
release note quality with grouped, user-friendly summaries.

## Acceptance Criteria

- [ ] Nightly Docker images built from main on a schedule (tagged `nightly`)
- [ ] Beta images built from pre-release tags (e.g., `v0.6.0-beta.1`, tagged `beta`)
- [ ] Stable release tags unchanged (`latest`, `<version>`, `<major>.<minor>`)
- [ ] Release notes grouped by: New Features, Improvements, Bug Fixes, Breaking Changes
- [ ] Release notes include links to related issues and PRs
- [ ] Pre-release versions clearly marked in release notes
- [ ] CHANGELOG.md accumulates across releases

## Dependency Map

```
#289 (release tags + notes) -- single issue, no dependencies
```

## Checklist

### Issue #289 -- Docker release tags and user-friendly release notes
- [ ] Add scheduled GitHub Actions workflow for nightly builds (daily at midnight UTC)
- [ ] Update GoReleaser config for beta/pre-release tag patterns
- [ ] Nightly workflow pushes `nightly` tag to GHCR
- [ ] Pre-release tags (e.g., `v*-beta.*`) push `beta` tag to GHCR
- [ ] Release note template with grouped sections
- [ ] Auto-generate release notes from PR labels/commit messages
- [ ] CHANGELOG.md generation or accumulation strategy
- [ ] Tests (workflow syntax validation, dry-run)

## UAT / Merge Order

1. PR for #289 (base: main) -- single PR covering all release pipeline changes

## Notes

- `[mode: plan]` -- scope: large
- Key files: `.github/workflows/release.yml`, `.github/workflows/ci.yml`, `.goreleaser.yml`
- Current state: GoReleaser already creates `latest`, `<major>.<minor>`, and `<version>` tags
- CI already pushes `latest` and `<sha>` on main branch
- Nightly builds should use the same Dockerfile but with a distinct tag
