# PR Workflow

## Pre-push gate

Run `bash scripts/pre-push-gate.sh` before anything else. It runs tests, OpenAPI consistency check, and generated-file staleness check. The LLM is not needed for these -- they are deterministic.

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
