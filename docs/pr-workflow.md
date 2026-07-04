# PR Workflow

## Pre-push gate

Run `bash scripts/pre-push-gate.sh` before anything else. It runs tests, OpenAPI consistency check, generated-file staleness check, and other deterministic checks (patch coverage, Bruno route parity, UI-preference coverage, and more). The LLM is not needed for these -- they are deterministic.

The UI-preference coverage step (`scripts/prefs-coverage.py`, backed by `.prefs.toml`) is Layer 1 only, and it is REGRESSION-ONLY: for a changed surface file that matches a tracked preference's surface glob, it flags a failure only when the file referenced the preference's driving token/class at the base revision and no longer does at HEAD. It does not require every file matching a (possibly broad) surface glob to carry every preference's token -- a file that never carried it, including a brand-new file, is never flagged. It does not catch a CSS cascade-override (a more specific rule beating a preference-driven variable) -- confirming that requires rendered evidence (a computed-style assertion against the live page), which is out of scope for this static, diff-scoped check.

Then run `/pr-review-toolkit:review-pr` for code review. Fix all critical/important findings before pushing.

## Parallel pushes from sibling worktrees

Multiple agents pushing concurrently from sibling worktrees (`../stillwater-<slug>/`) is supported. The gate path is fully per-worktree isolated:

- `$SW_RUN_DIR` (which holds `cover.out` and `openapi-base.yaml`) keys off the worktree's basename plus a 12-char sha256 of its absolute path -- see `scripts/lib/run-paths.sh`. Two worktrees that share a basename across separate parent directories still get disjoint dirs.
- The pre-push gate takes an atomic `mkdir`-based lock at `$SW_RUN_DIR/.gate-lock` to block same-worktree concurrent runs (PR #1481). Sibling worktrees take separate locks.
- `golangci-lint`'s shared cache is safe under `allow-parallel-runners: true` in `.golangci.yml`.
- Go's build cache (`$GOCACHE`) is documented concurrent-safe.

What is **not** robust is the common invocation pattern that hides push failures:

```bash
git push -u origin "$branch" 2>&1 | tail -30   # AVOID
```

Without `set -o pipefail`, the pipeline returns `tail`'s exit code (always 0), masking a real `git push` failure -- transient SSH blip, remote ref rejection, hook abort, network drop. The visible output then looks identical to a quiet success, and the next step opens a PR against a branch that never reached the remote.

Use `scripts/safe-push.sh` instead:

```bash
bash scripts/safe-push.sh                  # push current branch -u origin
bash scripts/safe-push.sh "$branch"        # push named branch
bash scripts/safe-push.sh "$branch" --force-with-lease   # extra flags forwarded
```

It writes the full push transcript to `<git-dir>/safe-push.log` (`.git/safe-push.log` for the main worktree, or the worktree-specific `.git/worktrees/<name>/safe-push.log` for linked worktrees), with mode `0600` so the transcript is private to the current user. The wrapper then queries `git ls-remote origin <branch>` after the push returns and exits non-zero with a clear message if the remote SHA does not match the local HEAD. Catches silent failures that `cmd | tail` would have swallowed.

## Squash before first push

Squash all development/fixup commits into clean, logical commits before the first push. Copilot's initial review covers the full diff present when the PR is first opened; incremental commits added after opening are not automatically re-reviewed (see Copilot review policy below).

```bash
git rebase -i main
# mark first: "pick", rest: "squash" or "fixup"
```

For larger PRs, two or three coherent commits is fine. **Do not squash after opening the PR** -- it resets Copilot's diff window.

## Creating issues

When creating a GitHub issue with `gh issue create`:

1. Pick the right template from `.github/ISSUE_TEMPLATE/`: `feature.md`, `bug.md`, or `task.md`.
2. Read the template and fill in every section, including the `[mode:]`, `[model:]`, and `[effort:]` hints.
3. Write the populated body to a temp file, then:
   ```bash
   gh issue create --title "<title>" --body-file <path> --label <label>
   ```
4. Delete the temp file after creation.

## PR templates

Two PR templates are available:

- **Default** (`.github/pull_request_template.md`) -- for feature, bug, and user-visible change PRs. Applied automatically when opening a PR via `gh pr create` with no `--body`/`--body-file` flag, or via the GitHub compare URL without a `?template=` parameter.
- **Chore** (`.github/PULL_REQUEST_TEMPLATE/chore.md`) -- for chore, CI, refactor, and dependency PRs. Omits screenshot, UAT, OpenAPI, and `templ generate` rows. Select it with:

  ```bash
  # gh pr create: pass the template file as the body
  gh pr create --body-file .github/PULL_REQUEST_TEMPLATE/chore.md --label chore
  ```

  In a browser, append `?template=chore.md&expand=1` to the compare URL:

  ```
  https://github.com/sydlexius/stillwater/compare/main...<branch>?expand=1&template=chore.md
  ```

## Reading PR comments (gh API)

The `!` character triggers bash history expansion inside double quotes. **Never use `!=` in `--jq` expressions.** Use `select(.field == "value" | not)` instead:

```bash
# List all PR review comments:
gh api "repos/{owner}/{repo}/pulls/{number}/comments" --paginate \
  --jq '[.[] | select(.body | length > 0) | {id, user: .user.login, path, line, body}]'

# Filter out a specific user:
gh api "repos/{owner}/{repo}/pulls/{number}/comments" --paginate \
  --jq '[.[] | select(.user.login == "some-bot" | not) | {id, user: .user.login, body}]'

# Reply to a review comment:
gh api "repos/{owner}/{repo}/pulls/{number}/comments/{comment_id}/replies" \
  -f body='Fixed in <commit>.'
```

## Copilot review policy

Automatic re-review on push is **disabled** (`review_on_push: false`). Re-review must be triggered manually from the GitHub PR page. The GitHub API does not support re-requesting review from bot accounts (422 error).

## Required-check x paths-filter invariant (#2199, #2200)

`.github/workflows/ci.yml` gates most work on the `changes` job's `dorny/paths-filter` output (`code`, `js`, `a11y`) so docs-only PRs skip the expensive Go/Node jobs. The live "Protect main" branch-protection ruleset requires **five** status contexts: `Build`, `Test`, `Lint`, `Coverage Floor`, and `Bruno API Tests`. Every one of them must still report **success** (not skipped) on every PR, including docs-only ones, and must **fail closed** if the `changes` (or equivalent) detector job itself errors. Two defects motivated the current shape (issue #2199):

- **D1 (skip instead of pass):** a required context produced by a job gated `if: needs.changes.outputs.code == 'true'` skips entirely on a non-code PR. GitHub never resolves a skipped required check to success, so the PR becomes permanently unmergeable.
- **D2 (trusting outputs without checking result):** `needs.<job>.outputs` is empty both when the job's condition was `false` AND when the job crashed/was cancelled. A wrapper that reads `outputs.code != 'true'` as "safe to pass" also passes when the `changes` job itself failed, silently satisfying the gate without lint/test/build/coverage-floor ever running.

The fix: every required context in `ci.yml` is owned by an always-running (`if: always()`) aggregator/wrapper job that (1) asserts `needs.changes.result == 'success'` before trusting `needs.changes.outputs.*` (fails closed on a crashed detector), (2) exits 0 if `code != 'true'` (docs-only PR: pass, not skip), and (3) otherwise mirrors the underlying worker job's result (and, for `build`/`coverage-floor-summary`, also fails closed if the worker was left `skipped` by an upstream dependency failure on an actual code change rather than genuinely running). See `lint`/`lint-summary`, `test`/`test-summary`, `coverage-floor`/`coverage-floor-summary`, and `build-matrix`/`build` in `ci.yml` for the pattern.

`Bruno API Tests` (`.github/workflows/bruno-ci.yml`) is already compliant: its `bruno` job carries no job-level `if` at all (only individual steps are conditioned on `dorny/paths-filter`), so the job always runs and always reports a real result -- no wrapper needed. `gate.yml`'s required Pre-Push Gate jobs are the same shape (job never gated, only inner steps skipped) and also need no change.

`JS Unit Tests` and `A11y Smoke Tests` are gated the same D1-prone way as the five above, but there is no in-repo evidence (no `.github/settings.yml` or ruleset-as-code; confirmed against the live ruleset, which lists only the five contexts above) that they are currently required. If either is promoted to required, it needs the same always-running wrapper treatment described above.

### Workflow-only trim (#2131)

A diff where every changed file is under `.github/workflows/**` -- in practice a Dependabot GitHub Actions version bump (`actions/setup-go`, `actions/cache`, etc.) -- no longer re-runs the full app-test fan-out. The application code is byte-identical to `main` on such a diff, so the 5-platform build matrix, the 9-shard race suite, Docker, CodeQL's Go analysis, and the a11y/axe smoke add zero marginal signal over a green CI; what actually needs validating is "do the jobs that use the bumped action still run."

`ci.yml`'s `changes` job computes a `workflow_only` output (mutually exclusive with `code`) by diffing the actual changed-file list against the PR base SHA -- this is NOT expressible as a `dorny/paths-filter` output, since paths-filter answers "did any file match a pattern", not "do all files match". An unresolvable base SHA (e.g. an unexpected trigger shape) defaults `workflow_only` to `false`, i.e. fails toward running the full stack rather than under-testing a real code change. `.github/workflows/ci.yml` itself was removed from the `code`/`js`/`a11y` `dorny/paths-filter` filters that used to force a full run on any CI-workflow edit; that job is now `workflow_only`'s exclusive concern.

When `workflow_only` is true, `changes.outputs.build_matrix` and `changes.outputs.test_matrix` collapse to a single REAL leg/shard (linux/amd64 build, the `rest` test shard) instead of either the full fan-out or a `skip:true` no-op placeholder -- this is the representative build+test path that exercises the bumped action for real (checkout, setup-go, cache restore/save, upload-artifact). `go-cache-primer` and the `test` job's `if` were extended to also run on `workflow_only == 'true'`; `test-summary`'s pass-through logic was extended the same way so the required `Test` context reflects the real (single-shard) result instead of treating it as a docs-only skip.

`Coverage Floor` stays gated on `code == 'true'` only (unchanged) -- a single shard's coverage profile only covers a fraction of `internal/**`, so running the per-package floor ratchet against it would spuriously fail every package the `rest` shard doesn't touch. The existing `coverage-floor-summary` docs-only pass-through already covers the `workflow_only` case for free, since `workflow_only` implies `code != 'true'`. `Docker Build` and `Lint` are likewise left gated on `code` only -- no app code changed, so there is nothing new to build or lint.

CodeQL's `Analyze Go` job is not a required status check, so it gets a plain skip (own `changes`/`workflow_only` detector, no satisfier) rather than a wrapper; `Analyze Actions` always runs regardless, since it directly targets the changed workflow files. `Bruno API Tests` needed no change: its per-step `dorny/paths-filter` already excludes `ci.yml` from its `relevant` filter (only `.github/workflows/bruno-ci.yml` triggers it), so a `ci.yml`-only diff already gets the cheap always-runs-but-does-nothing path; a diff that bumps an action inside `bruno-ci.yml` itself correctly runs the full Bruno suite for real, which *is* the representative path for that file.

## Copilot instruction files

Global instructions: `.github/copilot-instructions.md` (must stay under 4,000 characters). Domain-specific guidance in `.github/instructions/`:

- `go-api.instructions.md` -- OpenAPI semantic review, error paths, concurrency
- `go-tests.instructions.md` -- data races, multipart errors, assertion quality
- `ci-actions.instructions.md` -- version pinning, smoke test alignment

## Pre-push checklist (categories Copilot consistently flags)

**OpenAPI spec:**
- Every new/changed response field has a matching entry in `internal/api/openapi.yaml`
- Descriptions accurately describe the invariant
- `$ref` schemas match their actual shape

**Error path completeness:**
- Functions emitting user-visible warnings do so on ALL error paths
- No raw `error.Error()` output in client-visible warning strings
- Full errors logged server-side; sanitized message sent to client

**Generated files:**
- If any `.templ` changed, `templ generate` was run and `*_templ.go` committed
- If any HTTP status code changed, `scripts/smoke.sh` and integration test assertions updated

**SQL correctness:**
- ORDER BY on enum-like string columns uses CASE expression, not lexicographic sort
- Dynamic SQL builders use whitelisted column maps, not user input

**Accessibility:**
- Interactive elements have `aria-label`, `aria-expanded`, or `aria-controls` as appropriate

**Frontend fetch calls:**
- All `fetch()` calls check `resp.ok` before parsing the response body

**Concurrency:**
- Background goroutines use `context.WithoutCancel(reqCtx)`, not `context.Background()` (gosec G118)

**Test code:**
- No unprotected shared variables written in test handler goroutines and read in the test goroutine
- `multipart.Writer` method errors checked in test helpers
- `io.ReadAll(r.Body)` errors checked before using the result
- Engine/rule tests assert relative properties, not exact counts

**PR closing keywords:**
- PR body includes `Closes #N` for every issue the branch addresses
