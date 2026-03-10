---
description: "Triage open Copilot/bot PR review comments, fix everything in one pass, reply in batch, push once"
argument-hint: "<PR-number>"
allowed-tools: ["Bash", "Glob", "Grep", "Read", "Edit", "Agent"]
---

# Handle PR Review Comments

Read, triage, fix, reply, and push all PR review comments in a single pass.

**PR number:** $ARGUMENTS

If no PR number is provided, detect it:

```bash
gh pr view --json number --jq '.number' 2>/dev/null
```

If that fails, stop and ask: "Which PR number should I handle?"

---

## Step 1 -- Read all comments

Use this exact command (single-quoted jq to avoid bash history expansion issues with `!`):

```bash
gh api 'repos/sydlexius/stillwater/pulls/<PR>/comments' --paginate \
  --jq '[.[] | select(.body | length > 0) | {id, user: .user.login, path, line, body, created_at}]'
```

Also read the PR review summaries:

```bash
gh api 'repos/sydlexius/stillwater/pulls/<PR>/reviews' --paginate \
  --jq '[.[] | select(.body | length > 0) | {id, user: .user.login, state, body}]'
```

Filter to only unresolved comments (not already replied to with a fix commit).

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

**Grouping:** If multiple comments share the same root cause (e.g., "rename X" at lines
50, 120, and 200), group them as a single fix item.

---

## Step 3 -- Present triage table

Show a table of all comments with proposed actions:

```
| # | Category | File:Line | Summary | Action |
|---|----------|-----------|---------|--------|
| 1 | openapi-drift | handlers_artist.go:45 | Missing "aliases" in spec | Fix now |
| 2 | error-handling | handlers_push.go:92 | Raw err.Error() in response | Fix now |
| 3 | false-positive | handlers_image.go:300 | Suggests unnecessary nil check | Rebut |
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

## Step 8 -- Push

```bash
git push
```

Report the push result and a summary of actions taken:
- N comments fixed
- N comments rebutted
- N comments deferred (with issue numbers)
