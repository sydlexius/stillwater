---
description: "Triage all open PR review comments, fix everything in one pass, reply in batch, push once"
argument-hint: "[PR number -- defaults to current branch's PR]"
allowed-tools: ["Bash", "Glob", "Grep", "Read", "Edit", "Write", "Agent", "Task"]
---

# Handle PR Review

Resolve all open review comments in a single pass. The invariant: **one push, after all
fixes are complete**. Never push per-comment.

**PR number (optional):** "$ARGUMENTS"

---

## Step 1 -- Identify the PR

Resolve `pr_number` and `repo`:

```bash
repo=$(gh repo view --json nameWithOwner --jq .nameWithOwner)
me=$(gh api user --jq .login)
```

If `$ARGUMENTS` is a number, use it directly:

```bash
pr_number="$ARGUMENTS"
```

Otherwise detect from the current branch:

```bash
pr_number=$(gh pr view --json number --jq .number)
```

Print the PR URL for confirmation:

```bash
gh pr view "$pr_number" --json url --jq .url
```

If no PR found, stop: "No open PR found for this branch."

---

## Step 2 -- Fetch all review comments

(`repo`, `me`, and `pr_number` were resolved in Step 1.)

```bash
gh api "repos/$repo/pulls/$pr_number/comments" \
  --paginate \
  --jq '.[] | {id, user: .user.login, body, path, line: .original_line, reply_to: .in_reply_to_id}'
```

Also fetch review-level (non-inline) comments from review bodies if any reviewer left
them as part of a review submission:

```bash
gh api "repos/$repo/pulls/$pr_number/reviews" \
  --jq '.[] | select(.body != "") | {id, user: .user.login, body: .body, type: "review-body"}'
```

---

## Step 3 -- Identify open (unreplied) comments

A comment is **open** if:
1. It is a top-level comment (`reply_to` is null) from a reviewer bot (login contains
   `copilot` or ends with `[bot]`, case-insensitive)
2. AND there is no subsequent comment in the same thread from `$me` (the current user)

Build the open list:
- Collect all comment IDs that have a reply from `$me` (where `reply_to` matches
  the reviewer comment's `id`)
- Subtract from the full list of top-level bot/reviewer comments

Print a numbered list of open comments:
```
Open review comments (N total):
1. [id: 123456] path/to/file.go -- "First line of comment body..."
2. [id: 789012] internal/api/handlers.go -- "First line..."
...
```

If there are no open comments, say: "No open review comments. Nothing to do." and stop.

---

## Step 4 -- Read and categorize each comment

For each open comment:
1. Read the full comment body
2. Read the referenced file and line range in the current codebase
3. Assign one of these categories:

| Category | Meaning |
|----------|---------|
| `bug` | Real code defect -- must fix |
| `spec-drift` | OpenAPI spec doesn't match implementation -- must fix |
| `test-gap` | Missing test coverage for a real gap -- should fix |
| `false-positive` | Established pattern, known behavior, or intentional design |
| `already-fixed` | Was corrected in a later commit; reply needed but no code change |
| `wont-fix` | Valid suggestion but out of scope for this PR |

Print the full triage table before making any changes:

```
## Triage

| # | ID     | Category       | File              | Summary |
|---|--------|----------------|-------------------|---------|
| 1 | 123456 | bug            | handlers_image.go | error path returns empty warnings slice |
| 2 | 789012 | false-positive | handlers_image.go | SSRF transport pattern |
| 3 | 345678 | already-fixed  | openapi.yaml      | description was stale, fixed in abc1234 |
...
```

Ask: "Does this triage look right? (yes / adjust N to <category>)"

Wait for confirmation before proceeding to fixes.

---

## Step 5 -- Implement all fixes

For every comment categorized as `bug`, `spec-drift`, or `test-gap`:

- Read the relevant code
- Make the minimal correct fix
- Do NOT push yet
- Note the fix briefly for use in the reply

Apply fixes for all comments before moving to the next step. Fix order matters --
address `bug` fixes before `spec-drift` since spec updates should reflect final behavior.

After all edits are complete:

```bash
go test ./... 2>&1
```

If tests fail: stop. Fix the test failures before continuing. Do not reply to comments
or push with broken tests.

---

## Step 6 -- Compose replies

For each open comment, draft a reply:

**bug / spec-drift / test-gap (fixed):**
```
Fixed in <sha>. <one-sentence description of what changed>.
```

Get the sha after the fixes are committed (step 7 happens before replies are posted --
see below).

**false-positive:**
```
<Explanation of why this is correct. Reference the specific CLAUDE.md pattern or
architectural decision if applicable. Keep it brief -- one or two sentences.>
```

**already-fixed:**
```
Fixed in <earlier-sha>.
```

**wont-fix:**
```
Acknowledged. This is out of scope for this PR -- tracking separately as #<issue> or
leaving for a follow-up.
```

---

## Step 7 -- Commit, get SHA, then post replies

Commit all fixes in a single commit:

```bash
git add -p  # or git add <specific files>
git commit -m "fix: address PR review findings

<bullet list of what was fixed>

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>"
```

Get the short SHA:
```bash
git rev-parse --short HEAD
```

Now substitute the real SHA into all "Fixed in <sha>" reply drafts from step 6.

Post all replies in one batch (do not wait between them):

```bash
gh api "repos/$repo/pulls/$pr_number/comments/{COMMENT_ID}/replies" \
  -f body='<reply text>'
```

(`$repo` and `$pr_number` were resolved in Step 1.)

Run one `gh api` call per open comment. Log each one as it completes.

---

## Step 8 -- Push

After all replies are posted:

```bash
git push origin $(git branch --show-current) 2>&1
```

Report the result. If the push fails, explain why -- do not retry automatically.

---

## Step 9 -- Summary

Print a final summary:

```
## Done

- Fixed: N comments (bug/spec/test)
- Dismissed: N comments (false-positive/wont-fix)
- Noted: N comments (already-fixed)
- Replied: N total
- Pushed: <sha> to <branch>

Copilot will review the new push automatically. If it flags new issues, run
/handle-review again.
```
