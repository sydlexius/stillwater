---
name: helper-scripts
description: Catalogue of Stillwater's scripts/ helpers and the ~/.claude/scripts PR helpers - what each gate check asserts, why it exists, and which CI job mirrors it. Use when a pre-push gate or CI check fails and you need to know what it is actually asserting, when adding a new gate check or its mutation tests, or when reaching for PR review/comment/thread data.
---

# Stillwater Helper Scripts

The gate philosophy: `pre-push-gate.sh` has **no advisory step** in its default
path (#2983). Every check either BLOCKS the push or is not in that path at all,
so "All hard checks passed" means exactly what it says. Most guards ship with a
sibling `test-*.sh` of hermetic mutation tests -- a guard nobody proved can fail
is not a guard.

Three binding rules live in the root `CLAUDE.md`, not here, because they are
directives rather than reference: do not invoke `pre-push-gate.sh` manually as a
standalone pre-PR step; use `dev-restart.sh` and never kill by port; and
`link-worktree-settings.sh` refuses rather than overwrites.

## The gate itself

- `scripts/pre-push-gate.sh` -- deterministic pre-push checks (tests, OpenAPI,
  generated files, lint, patch coverage). Every check in the default path is
  BLOCKING. The expensive integration tiers (a11y, provider-failure smoke,
  govulncheck) default to SKIP behind `RUN_A11Y` / `RUN_PROVIDER_SMOKE` /
  `RUN_VULN`, each of which RUNS and BLOCKS when set truthy. `RUN_RACE` is not
  one of those: unset, the gate already runs a fast changed-packages test;
  `RUN_RACE=1` upgrades that to the full blocking `-race` suite, while
  `RUN_RACE=0` SKIPS the local test run and patch coverage together. All four
  accept `1`/`true`/`yes`/`on` or `0`/`false`/`no`/`off` (case-insensitive); any
  OTHER value is refused with exit 2 rather than read as unset.
- `scripts/check-gate-invariant.sh` -- assert `pre-push-gate.sh` has no advisory
  step in its default path: no `WARN:` verdict, no "not blocking this push", no
  `FAIL:` announcement that fails to exit (#2983). Run by the gate on itself and
  by CI's `Gate Invariant` job (`gate.yml`), so weakening the gate and pushing
  with `--no-verify` still gets caught.
- `scripts/test-check-gate-invariant.sh` -- hermetic mutation tests for the above.
- `scripts/lib/run-flags.sh` -- sourced by the gate; resolves each `RUN_*`
  variable to run / skip / default and REFUSES an unrecognized value (exit 2).
  Without it, a mistyped `RUN_VULN=truee` fell into the default branch, printed
  "skipped by default", and left a green gate over a tier the operator had asked
  to run.
- `scripts/test-run-flag-resolution.sh` -- tests for the above, plus an
  end-to-end assertion that the gate actually consumes it (a correct resolver
  nothing calls is not a fix). Called by pre-push-gate and by CI's
  `Gate Invariant` job.

## Coverage

- `scripts/coverage-floor.sh` -- per-package coverage floor enforcement (called
  by pre-push-gate).
- `scripts/check-codecov-floor-mirror.sh` -- assert `codecov.yml`'s per-package
  project targets mirror `testdata/coverage-floor.json` exactly (#2756).
  `codecov.yml` claims in its own comment to mirror the floor file; this proves
  the claim instead of trusting it.
- `scripts/test-check-codecov-floor-mirror.sh` -- hermetic tests for the above;
  every case builds its own throwaway floor file.
- `~/.claude/scripts/patch-coverage.sh` (orchestrate plugin; not vendored
  in-repo) -- patch-level coverage check, called by pre-push-gate.
- `scripts/prefs-coverage.py` -- `.prefs.toml` UI-preference coverage gate
  (Phase 2, #201). Vendored into `scripts/` (#2195) and invoked as a gate step.

## Git environment and worktrees

- `scripts/lib/git-clean-env.sh` -- sourced by any script that builds a fixture
  git repository; strips the inherited git LOCATION variables (keeping
  `GIT_CONFIG_*` and `GIT_EXEC_PATH`). `git init <path>` re-initializes an
  inherited `GIT_DIR` and IGNORES `<path>`, so a gate helper run by the pre-push
  hook from a worktree -- which shares the main repo's `.git/config` -- wrote
  `core.bare = true` into the MAIN repository, silently disabling its
  mass-deletion guard (#3051). Call `git_clean_env_unset` (default) or, for a
  script that must keep its own git environment, `git_clean_env_array` +
  `"${GIT_CLEAN_ENV[@]}"` as a per-invocation prefix.
- `scripts/check-git-init-guarded.sh` -- refuse a `git init` under `scripts/` or
  `.githooks/` that does not clear the inherited git environment. Static: proves
  a guard is PRESENT, which is what the next such script forgets, and runs as a
  PRE-FLIGHT before the gate's fixture-building checks so it prevents the damage
  rather than reporting it. Fails closed (allow-list): a guard counts only ABOVE
  the invocation it protects, an `env` prefix must remove `GIT_DIR` by name, and
  option forms (`git -C <dir> init`) count. Portable to macOS system bash 3.2 and
  BSD grep -- no `mapfile`, no GNU-only `\b`; a scan that examines zero files
  FAILS rather than reporting OK. `--list` prints the call sites and exits.
- `scripts/test-check-git-init-guarded.sh` -- mutation tests for the above, one
  case per bypass the first implementation shipped (bash-3.2 `mapfile`,
  guard-below-invocation, `env FOO=bar`, `env -u FOO`, `git -C ... init`).
- `scripts/test-git-clean-env.sh` -- behavioral regression tests: runs each
  affected helper against a real throwaway worktree with `GIT_DIR` exported the
  way a hook exports it, and asserts the main repository's whole local config is
  unchanged.
- `scripts/link-worktree-settings.sh <worktree-dir> [main-repo-dir]` -- symlink a
  sibling worktree's `.claude/settings.local.json` at the main repo's copy
  (#2879). Called by `make worktree`. That path is gitignored, so a worktree
  checkout never receives one and an agent there silently falls back to the
  user-global grants -- producing a permission prompt for a command the repo
  already grants, which a backgrounded agent cannot answer, so it stalls with no
  error. A symlink rather than a copy: one source of truth, so a grant added
  later reaches every worktree.
- `scripts/test-link-worktree-settings.sh` -- hermetic tests for the above.
- `~/.claude/scripts/cleanup-worktree.sh <suffix>` -- remove worktree, delete
  local/remote branches, prune refs (repo-agnostic; auto-detects the main
  worktree's basename as the prefix). In Stillwater, prefer
  `make remove-worktree NAME=<slug>`, which wraps this and additionally strips
  the Active-table row in `worktrees.md`.

## Commits, hooks, and generated files

- `scripts/check-commit-signing.sh` -- refuse to create an unsigned commit when
  `.githooks/signed-commits-required` is present (#2625). Two modes:
  `.githooks/pre-commit` runs it bare (probes the real signer before the commit
  exists), `.githooks/post-commit` runs it with `--head` (reads the signature off
  the commit just made, the only stage that can see `git commit --no-gpg-sign`,
  which is a flag rather than config and so is invisible to a pre-commit hook).
  Verifies the raw commit object, never `git log --format=%G?`, which reports `N`
  for genuinely signed commits when `gpg.ssh.allowedSignersFile` is unset. Backed
  by the required `Signed Commits` CI check.
- `scripts/test-check-commit-signing.sh` -- hermetic tests for the above.
- `scripts/check-hooks.sh` -- verify `core.hooksPath` points at `.githooks` and
  the hook files are executable.
- `scripts/check-generated.sh` -- regenerate every `*_templ.go` from source
  and fail if the committed output differs.

## CSS and front-end

- `scripts/check-css-comments.sh` -- fail on a self-terminating CSS comment (a
  `*/` in comment prose closes the comment, so the rest is parsed as CSS; #2525).
  Called by pre-push-gate; mirrored by the `CSS Comments` job in `gate.yml`.
- `scripts/stylelint-diff-gate.sh` -- diff-scoped stylelint ratchet for the
  hand-written CSS layer (`design-tokens.css`, `input.css`, `scalar-theme.css`).
  The design-token migration (#2402) is not complete, so the gate scopes to the
  diff rather than the whole file.
- `scripts/test-stylelint-diff-gate.sh` -- tests for the above, specifically the
  "nothing to check" vs "cannot check" distinction added to fix a fresh-worktree
  false failure.

## CI, supply chain, and docs

- `scripts/check-action-pins.sh` -- assert every sub-action of a single GitHub
  Action repository is pinned to the SAME commit SHA (e.g. all of
  `github/codeql-action/*`).
- `scripts/check-zizmor-suppressions.sh` -- assert no workflow silently widens
  the blast radius of a `# zizmor: ignore[dangerous-triggers]` suppression
  (#2842, #2843).
- `scripts/test-check-zizmor-suppressions.sh` -- hermetic mutation tests; exists
  because the guard's first implementation parsed YAML with awk and was bypassable.
- `scripts/check-goreleaser-extra-files.sh` -- assert every repo-file `COPY`
  source in `build/docker/Dockerfile.goreleaser` is also listed in
  `.goreleaser.yml`'s `extra_files:` (#3034 regression).
- `scripts/test-check-goreleaser-extra-files.sh` -- hermetic tests for the above.
- `scripts/check-tool-versions.sh` -- assert the Tailwind version pinned for CI
  (`.github/actions/setup-tailwind`) agrees with the version pinned for the
  Docker image. `make sync-tool-versions` realigns them.
- `scripts/check-doc-facts.sh` -- assert hand-written docs cite code-derived
  facts correctly. Most reference pages are mechanically generated (`cmd/gen-*`);
  this covers the hand-written ones that quote code.
- `scripts/check-bruno-parity.sh` -- API-route vs Bruno-request parity guard:
  flag API routes registered in `internal/api/router.go` with NO corresponding
  Bruno request. Called by pre-push-gate and CI's `Bruno Route Parity` job.
- `make check-openapi` -- validate the OpenAPI spec against handler
  implementations. Not a script: it runs `TestOpenAPIConsistency` in
  `internal/api/`.

## Runtime and smoke

- `scripts/dev-restart.sh` -- canonical dev rebuild + restart.
- `scripts/smoke.sh` -- API smoke tests against a running instance.
- `scripts/smoke-provider-failure.sh` -- fault-injection smoke harness for
  provider failure surfaces.
- `scripts/smoke-version-injection.sh` -- version-ldflags-injection smoke test.
  Go's linker treats an unresolved `-X` symbol path as a silent no-op: a
  stale or misspelled `-X path=value` does not fail the build, it just leaves the
  version empty. This proves the injection actually landed.
- `~/.claude/scripts/safe-push.sh` -- run `git push` and verify the remote ref
  actually moved. Note there are TWO copies: the repo-agnostic one in
  `~/.claude/scripts/` and a repo copy at `scripts/safe-push.sh` kept in sync
  with it (documented in `docs/pr-workflow.md`). Either is fine; the repo copy
  has no project-specific dependency.

## PR review data (prefer these over raw `gh api`)

**Zero raw `gh api` for PR comment/review/thread data.** The helpers filter and
format correctly where ad-hoc calls drop comment types and mishandle whitespace.
If a case is not covered, improve the script rather than bypass it. GitHub
reactions (bot-root ack) have no wrapper and remain a direct
`gh api ...reactions` call.

- `~/.claude/scripts/pr-unreplied-comments.sh [--allow-stale] [--pending-only] [--count-only] [--coverage-only] [--wait] [--latest-per-reviewer] [--check-resolved] <PR>`
  -- unreplied bot comments + codecov advisory.
- `~/.claude/scripts/pr-read-comments.sh [--reviews] [--issue] <PR>` -- read full
  review/issue comment bodies.
- `~/.claude/scripts/reply-comment.sh` -- post a threaded reply (and
  `@coderabbitai resolve`) to a review comment.
- `~/.claude/scripts/ship-gate-preflight.sh <PR>` -- deterministic merge oracle
  (CI all-green + 0 actionable review-body findings, fail-closed).
