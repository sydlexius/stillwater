---
description: "Triage open bot PR review comments (Copilot, CodeRabbit, Dependabot, or any `[bot]`-suffixed reviewer), fix everything in one pass, reply in batch, push once"
argument-hint: "<PR-number> [--wait]"
allowed-tools: ["Bash", "Glob", "Grep", "Read", "Edit", "Agent"]
---

# Handle PR Review Comments

Read, triage, fix, reply, and push all bot PR review comments (Copilot, CodeRabbit,
Dependabot, or any `[bot]`-suffixed reviewer) in a single pass.

**PR number:** $ARGUMENTS (strip `--wait` if present; pass it to the helper script)

If no PR number is provided, detect it:

```bash
gh pr view --json number --jq '.number' 2>/dev/null
```

If that fails, stop and ask: "Which PR number should I handle?"

---

## Step 1 -- Find unreplied bot comments

Use the helper script to reliably detect unreplied bot comments:

```bash
bash /root/.claude/scripts/pr-unreplied-comments.sh [--wait] <PR>
```

Pass `--wait` if the user included it in the arguments. The script will poll
until bot reviews stabilize before returning results.

The script output includes:
- `commit` field: short SHA of the commit the comment was posted on
- `stale` field: `true` if the comment targets an older commit than HEAD

If the script reports 0 unreplied comments, stop: "No open review comments. Nothing to do."

For each unreplied comment, fetch the full body:

```bash
gh api 'repos/sydlexius/stillwater/pulls/<PR>/comments' --paginate \
  --jq '.[] | select(.id == <COMMENT_ID>) | {id, user: .user.login, path, line: .original_line, body}'
```

---

## Step 1.5 -- Stale-diff fast path

If ALL unreplied comments have `stale: true` (their `commit` differs from HEAD),
the bots are reviewing old code that has already been fixed. Offer a fast path:

"All N comments target commit XXXX (HEAD is YYYY). These are likely stale-diff
re-flags of already-fixed code. Batch-reply all as 'already fixed in HEAD'?"

If the user confirms, skip to Step 7 (reply in batch) with "Already addressed
in HEAD." for each comment. No triage table needed.

If the user declines, proceed with normal triage.

---

## Step 2 -- Categorize each comment

For each comment, assign one of these categories:

| Category | Description | Default action |
|----------|-------------|----------------|
| **openapi-drift** | Missing/wrong field in openapi.yaml | Fix now |
| **error-handling** | Missing error check, raw error in response, swallowed error | Fix now |
| **test-gap** | Missing test, untested edge case, data race in test | Fix now |
| **rename-incomplete** | Old name still referenced somewhere | Fix now (batch) |
| **style** | Formatting, naming, comment wording | Fix now |
| **false-positive** | Incorrect or inapplicable suggestion | Rebut |
| **architectural** | Requires design change beyond this PR's scope | Defer (must justify) |

**Known false-positive patterns (auto-classify without investigation):**
- Copilot "PR title scope" complaints about issues added in follow-up commits
- Copilot re-flagging pipeline nil guards (`r.pipeline` is a required dependency)
- Copilot flagging exported-but-not-yet-called methods as unused
- Copilot flagging O(2n) in template code with small N (<10k items)

**Grouping:** If multiple comments share the same root cause (e.g., "rename X" at lines
50, 120, and 200), group them as a single fix item.

---

## Step 3 -- Present triage table

Show a table of all comments with proposed actions:

```
| # | Category | File:Line | Commit | Summary | Action |
|---|----------|-----------|--------|---------|--------|
| 1 | openapi-drift | handlers_artist.go:45 | abc1234 | Missing "aliases" in spec | Fix now |
| 2 | error-handling | handlers_push.go:92 | abc1234 | Raw err.Error() in response | Fix now |
| 3 | false-positive | handlers_image.go:300 | def5678 (stale) | Suggests unnecessary nil check | Rebut |
```

Ask the user to confirm or adjust using selectable choices:
- "Looks good, proceed with fixes"
- "Adjust category for comment N"
- "Show me more context for comment N"

Wait for confirmation before proceeding.

---

## Step 4 -- Execute all fixes in one pass

For each "Fix now" item:
1. Read the relevant file and understand the surrounding context
2. Make the fix
3. Track what was changed for the reply

For each "Fix now (batch)" group:
1. Find ALL instances across the codebase (not just the lines Copilot flagged)
2. Fix all instances in one pass

For each "Defer" item:
1. Create a tracking issue using the `task` template
2. Note the issue number for the reply

**Do not commit between fixes.** Make all changes first, then commit once.

---

## Step 5 -- Run verification

After all fixes are applied:

```bash
go test -count=1 ./... 2>&1
```

If tests fail, fix the failures before proceeding.

If any `.templ` files were changed:
```bash
templ generate
```

---

## Step 6 -- Commit all fixes

Stage all changed files and commit:

```bash
git add <specific files that were changed>
git commit -m "address PR review feedback

- <one-line summary per fix>

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## Step 7 -- Reply to all comments in batch

For each comment, post a reply:

**Fix now:**
```bash
gh api 'repos/sydlexius/stillwater/pulls/<PR>/comments/<comment_id>/replies' \
  -f body='Fixed in <short-sha>.'
```

**Rebut:**
```bash
gh api 'repos/sydlexius/stillwater/pulls/<PR>/comments/<comment_id>/replies' \
  -f body='<evidence-based explanation of why this is not an issue>'
```

**Defer:**
```bash
gh api 'repos/sydlexius/stillwater/pulls/<PR>/comments/<comment_id>/replies' \
  -f body='Tracked in #<issue-number>. Requires <brief justification for deferral>.'
```

---

## Step 8 -- Push and verify

```bash
git push
```

Report the push result and a summary of actions taken:
- N comments fixed
- N comments rebutted
- N comments deferred (with issue numbers)

## Step 9 -- Wait and auto-loop

After pushing, bots will review the new commit. **Do not declare the PR clean
until bot reviews have stabilized.**

```bash
bash /root/.claude/scripts/pr-unreplied-comments.sh --wait <PR>
```

This polls at 15s/30s/60s/120s intervals until the unreplied comment count
stabilizes across two consecutive checks with no pending bot reviewers.

**If new comments appear, go back to Step 1 automatically.** Do not ask the user
to run `/handle-review` again. Handle the new round in the same session.

**Loop limit:** After 5 rounds of fix-push-wait, stop and report:
"Completed 5 review rounds. N comments remain unreplied. Bots may be in a loop.
Run `/handle-review <PR>` again if needed."
