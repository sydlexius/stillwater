# Milestone 29 -- Release Pipeline

## Goal

Add differentiated Docker release channels (nightly, beta, versioned), improve
release note quality, harden the container for public release via penetration testing,
and prepare the codebase for initial release.

## Acceptance Criteria

- [ ] Nightly Docker images built from main on a schedule (tagged `nightly`)
- [ ] Beta images built from pre-release tags (e.g., `v0.6.0-beta.1`, tagged `beta`)
- [ ] Stable release tags unchanged (`latest`, `<version>`, `<major>.<minor>`)
- [ ] Release notes grouped by: New Features, Improvements, Bug Fixes, Breaking Changes
- [ ] Release notes include links to related issues and PRs
- [ ] Pre-release versions clearly marked in release notes
- [ ] CHANGELOG.md accumulates across releases
- [ ] SBOM generated and vulnerability-scanned on every release build
- [ ] CI/quality badges in README.md
- [ ] SQL migrations squashed into single initial migration for v1.0.0
- [ ] Container pen-tested with zero critical/high findings

## Dependency Map

```
#289 (release tags + notes) -- independent
#499 (pen testing) -- independent, benefits from #500 SBOM
#500 (SBOM) -- independent
#505 (badges) -- independent
#509 (SQL squash) -- independent
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

### Issue #500 -- SBOM generation and vulnerability scanning with Syft and Grype
- [ ] Syft generates CycloneDX SBOM from Docker image
- [ ] Grype scans SBOM and reports vulnerabilities
- [ ] CI workflow runs SBOM + vuln scan on release builds
- [ ] Build fails on critical/high severity vulnerabilities
- [ ] SBOM artifact attached to GitHub Releases
- [ ] govulncheck integrated as supplementary check
- [ ] Tests
- [ ] PR merged

### Issue #505 -- CI/quality badges in README.md
- [ ] CI status, release, Go Report Card, license, Go version badges
- [ ] Badges render correctly on GitHub
- [ ] PR merged

### Issue #509 -- Squash SQL migrations for initial release
- [ ] Single migration file with complete current schema
- [ ] All development migration files removed
- [ ] Fresh database initializes correctly
- [ ] All tests pass against new migration
- [ ] goose version tracking works with reset numbering
- [ ] PR merged

### Issue #499 -- Container security penetration testing
- [ ] Container scanned with Trivy and Dockle
- [ ] Dockerfile audited with Hadolint
- [ ] govulncheck passes (zero known vulnerabilities)
- [ ] OWASP ZAP baseline scan passes
- [ ] API endpoints fuzz-tested
- [ ] SQL injection, XSS, SSRF, file upload tests pass
- [ ] Security scanning CI workflow added
- [ ] Findings documented
- [ ] PR merged

## Worktrees

| Directory | Branch | Issue | Status |
|-----------|--------|-------|--------|
| (created when work begins) | | | |

## UAT / Merge Order

Session 1 (quick wins):
1. PR for #505 (base: main) -- README badges

Session 2 (release foundation):
2. PR for #509 (base: main) -- SQL migration squash
3. PR for #500 (base: main) -- SBOM scanning CI integration

Session 3 (security):
4. PR for #499 (base: main) -- pen testing and CI security workflow

Session 4 (release pipeline):
5. PR for #289 (base: main) -- Docker release tags, nightly/beta, release notes

## Notes

- `[mode: plan]` -- scope: large
- Key files: `.github/workflows/release.yml`, `.github/workflows/ci.yml`, `.goreleaser.yml`
- Current state: GoReleaser already creates `latest`, `<major>.<minor>`, and `<version>` tags
- CI already pushes `latest` and `<sha>` on main branch
- Nightly builds should use the same Dockerfile but with a distinct tag
- #509 (SQL squash) is the successor to #350 (consolidated in M30, now merged); primary ownership here in M29 for release readiness
- #500 (SBOM): recommended tools are Syft+Grype (free, Apache 2.0) + govulncheck (Go-specific)
