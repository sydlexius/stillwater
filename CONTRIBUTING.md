# Contributing to Stillwater

Thanks for your interest in contributing to Stillwater. This document explains
how to set up a development environment, the expectations around code style
and pull requests, and where to ask questions.

## Code of conduct

This project follows the [Contributor Covenant](https://github.com/sydlexius/stillwater/blob/main/CODE_OF_CONDUCT.md). By
participating you agree to abide by its terms.

## Development environment

See [Dev setup](https://github.com/sydlexius/stillwater/blob/main/docs/dev-setup.md) for the full setup. Quick summary:

- Go 1.26 or newer
- Tailwind CSS standalone CLI (no Node.js required; see Dev setup for download
  instructions)
- `make build` produces a working binary; `make dev` enables hot reload
  via `air`

## Running tests

```bash
go test -race ./...
# or, equivalent
make test
```

Integration tests use real SQLite via `modernc.org/sqlite`. The race detector
is required when changing concurrent code (goroutines, shared state,
background workers).

## Code style

- No emoji in code, commits, comments, or documentation
- No em-dashes in any output
- Run `make fmt` before committing (Go and templ formatters)
- Run `make lint` (`golangci-lint`) before opening a PR
- Follow the patterns documented in [.golangci.yml](https://github.com/sydlexius/stillwater/blob/main/.golangci.yml) and the
  per-package guidance in `.github/instructions/`

Style and conventions live in [CLAUDE.md](https://github.com/sydlexius/stillwater/blob/main/CLAUDE.md), which doubles as
project-wide guidance for both human contributors and AI tools.

## Pull request workflow

The full workflow is documented in [PR workflow](https://github.com/sydlexius/stillwater/blob/main/docs/pr-workflow.md).
Short version:

1. Branch from `main`. Never commit to `main` directly; branch protection
   enforces this.
2. Use a conventional-commit prefix (`feat:`, `fix:`, `docs:`, `chore:`,
   `refactor:`, `perf:`, `ci:`, `test:`, etc.) on the squash commit.
3. Run `bash scripts/pre-push-gate.sh` before pushing. Every check in its
   default path is **blocking**: if the gate exits 0 and prints "All hard
   checks passed", every check that ran, passed (#2983). A check that should
   not block does not belong in the default path, and
   `scripts/check-gate-invariant.sh` enforces that from inside the gate and
   from CI's `Gate Invariant` job.

   The local test step is a fast, changed-packages-only, non-race run over the
   exact packages whose files changed -- a quick "did I obviously break a
   test" signal, not a full CI-equivalent pass. It blocks on a failing
   assertion as well as on a compile error (the two are reported differently,
   since only the latter leaves no coverage profile). Force the full,
   CI-equivalent local run with `RUN_RACE=1 bash scripts/pre-push-gate.sh`, or
   skip the local test run and patch-coverage check together with
   `RUN_RACE=0` -- worth reaching for when the changed package's own suite is
   expensive. CI's required `Test` job runs the full `-race` suite and its
   `Coverage Floor` job runs the per-package ratchet; both are authoritative.
   The opt-in/opt-out accepts any of `1`, `true`, `yes`, `on`, `0`, `false`,
   `no`, or `off` (case-insensitive, surrounding whitespace ignored).

   The accessibility (axe-core) smoke tests, the provider-failure smoke test,
   and `govulncheck` are **skipped by default** and each has a blocking
   opt-in: `RUN_A11Y=1`, `RUN_PROVIDER_SMOKE=1`, `RUN_VULN=1`. Each boots a
   server, drives a browser, or downloads the vulnerability database, and each
   duplicates a required CI check -- "A11y Smoke Tests (Playwright +
   axe-core)", "Provider Failure Smoke", and "Go Vulnerability Check"
   respectively (all configured as required status checks in the `Protect
   main` ruleset, which lives in repo settings rather than in-repo). Run the
   a11y tier locally when your change touches templates, CSS, or
   `tests/a11y/`: `RUN_A11Y=1 bash scripts/pre-push-gate.sh` downloads the
   Playwright browsers and boots an ephemeral server, so it adds minutes, but
   CI catching the same violation costs a red check and a re-push.

   Bruno route parity is CI-only; the required "Bruno Route Parity" job runs
   `scripts/check-bruno-parity.sh` on every PR.

4. Open one PR per logical change; never stack PRs.
5. Apply at least one of the labels listed below so the release-notes
   generator (`.github/release.yml`) buckets your change correctly.
6. Address review feedback, then squash-merge. Delete the branch after
   merge.

## Labels

The release notes generator buckets changes by label. Apply one or more
when opening a PR or filing an issue:

| Bucket            | Labels                       |
|-------------------|------------------------------|
| Features          | `enhancement`                |
| Bug fixes         | `bug`                        |
| Performance       | `performance`                |
| Security          | `security`                   |
| Documentation     | `documentation`              |
| CI / Build        | `ci`                         |
| Dependencies      | `dependencies`               |
| Refactoring       | `technical-debt`, `chore`    |

Triage-only labels (`duplicate`, `invalid`, `wontfix`, `question`) are
excluded from release notes.

## Suggesting a feature

Open an issue using the appropriate
[issue template](https://github.com/sydlexius/stillwater/issues/new/choose).
For larger ideas, draft a short scope sketch in the issue body so we can
talk through the design before any code lands.

## Questions

Tag your issue with the `question` label, or comment on an existing issue
or pull request.
