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
3. Run `bash scripts/pre-push-gate.sh` before pushing. The accessibility
   (axe-core) smoke tests are opt-in and skipped by default; run them with
   `RUN_A11Y=1 bash scripts/pre-push-gate.sh` (downloads a Chromium browser
   and boots an ephemeral server, so it adds minutes). CI runs them
   unconditionally in its dedicated a11y job.
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
