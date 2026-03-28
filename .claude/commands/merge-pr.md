---
description: "Merge a PR with CodeRabbit status check, squash, and post-merge cleanup"
argument-hint: "<PR-number> (e.g. 738)"
allowed-tools: ["Bash", "Glob", "Grep", "Read", "Edit", "Write", "Agent"]
---

# Merge PR

Safe merge workflow: verify CodeRabbit status, check CI, squash-merge, and clean up.

**PR number:** $ARGUMENTS

If `$ARGUMENTS` is a number, use it directly. Otherwise detect from the current branch:

```bash
pr_number="$ARGUMENTS"

if [ -z "$pr_number" ]; then
  pr_number=$(gh pr view --json number --jq .number 2>/dev/null)
fi
```

If still no PR found, stop and ask: "Which PR number should I merge?"

---

## Step 1 -- Pre-flight checks

Run these in parallel:

```bash
repo=$(gh repo view --json nameWithOwner --jq .nameWithOwner)

# CI status
gh pr view $pr_number --json mergeStateStatus,mergeable \
  --jq '{mergeStateStatus, mergeable}'

# Merge blockers (active CHANGES_REQUESTED -- latest review per reviewer)
gh api "repos/$repo/pulls/$pr_number/reviews" --paginate \
  --jq '[ group_by(.user.login)[]
         | sort_by(.submitted_at) | last
         | select(.state == "CHANGES_REQUESTED")
         | {id, user: .user.login}
       ]'

# Unreplied bot comments
bash $HOME/.claude/scripts/pr-unreplied-comments.sh $pr_number
```

If there are CHANGES_REQUESTED reviews or unreplied inline comments, stop and report.
Suggest running `/handle-review $pr_number` first.

If CI is not CLEAN/MERGEABLE, stop and explain why.

---

## Step 2 -- CodeRabbit status check

Post a status check comment and wait for CR's response:

```bash
# Set baseline BEFORE posting so a fast CR response is not missed
baseline=$(date -u +%Y-%m-%dT%H:%M:%SZ)

gh pr comment $pr_number --body "@coderabbitai status"
```

Poll for CR's response by checking for a new issue-level comment from `coderabbitai`
that appeared AFTER the baseline timestamp.

```bash
# Poll at 10s intervals, max 6 attempts (60s total)
for i in 1 2 3 4 5 6; do
  sleep 10
  response=$(gh pr view $pr_number --comments --json comments \
    --jq "[.comments[] | select(.author.login == \"coderabbitai\" and .createdAt > \"$baseline\")] | length")
  if [ "$response" -gt 0 ]; then
    echo "CR responded"
    break
  fi
  echo "Poll $i: waiting for CR response..."
done
```

Once CR responds, fetch and parse the response:

```bash
gh pr view $pr_number --comments --json comments \
  --jq '[.comments[]
         | select(.author.login == "coderabbitai" and .createdAt > "'"$baseline"'")
        ] | sort_by(.createdAt) | last | .body'
```

### Parse the response

Look for these indicators in CR's status response:

**Safe to merge:**
- "all major/critical findings have been addressed"
- "ready to merge"
- All items show checkmarks or "Fixed"/"Resolved" status
- No items marked as "Open" with Major/Critical severity

**Not safe to merge:**
- "review in progress" or "pending review"
- Items marked "Open" with Major or Critical severity
- "rate limited" (CR hasn't reviewed yet)

If CR indicates a review is in progress, wait and re-poll (up to 3 minutes total).

If CR flags open Major/Critical items, stop and report them. Suggest running
`/handle-review $pr_number` first.

If CR doesn't respond within 60 seconds, fall back to the commit-based check:

```bash
head_sha=$(gh pr view $pr_number --json headRefOid --jq .headRefOid)
cr_reviewed=$(gh api "repos/$repo/pulls/$pr_number/reviews" --paginate \
  --jq '[.[] | select(.user.login == "coderabbitai[bot]") | .commit_id] | last')

if [ "$cr_reviewed" != "$head_sha" ]; then
  echo "WARNING: CR has not reviewed HEAD ($head_sha). Last reviewed: $cr_reviewed"
  # Ask user whether to proceed
fi
```

---

## Step 3 -- Merge

```bash
gh pr merge $pr_number --squash --delete-branch
```

If merge fails, stop and explain. Common causes:
- Branch protection rules not met
- Merge conflicts (need rebase)
- Required checks not passing

---

## Step 4 -- Post-merge cleanup

```bash
# Update local main
git checkout main && git pull --ff-only

# Prune stale remote tracking refs
git fetch --prune

# Delete local feature branch if it still exists
branch=$(gh pr view $pr_number --json headRefName --jq .headRefName 2>/dev/null || true)
if [ -n "$branch" ]; then
  git branch -d "$branch" 2>/dev/null || true
fi

# Check for and remove any worktrees associated with this branch
if [ -n "$branch" ]; then
  git worktree list --porcelain \
    | awk -v b="refs/heads/$branch" '
        $1=="worktree"{wt=$2}
        $1=="branch" && $2==b{print wt}
      ' \
    | while IFS= read -r wt; do
        [ -n "$wt" ] && git worktree remove "$wt" 2>/dev/null || true
      done
fi
```

---

## Step 5 -- Summary

```
## Merged

- PR: #$pr_number
- CR status: <verified clean / fallback check / skipped>
- Commit: <squash merge SHA>
- Cleanup: main updated, refs pruned, local branch deleted
```
