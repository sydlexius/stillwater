---
description: "Automate post-merge cleanup: update main, remove worktree, delete branches, prune refs"
argument-hint: "<issue-number> (e.g. 315)"
allowed-tools: ["Bash", "Glob", "Grep", "Read", "Edit"]
---

# Post-Merge Cleanup

Run after a PR is merged to clean up the working environment.

**Issue number:** $ARGUMENTS

If no issue number is provided, stop and ask: "Which issue number was just merged?"

---

## Step 1 -- Derive names

From the issue number, determine:

```bash
# List worktrees to find the one matching this issue
git worktree list
```

Look for a worktree directory containing the issue number (e.g. `stillwater-315`,
`stillwater-m17-315`). Extract the branch name from the worktree listing.

If no worktree is found for this issue, note it and skip worktree removal in step 3.

---

## Step 2 -- Update local main

```bash
cd /root/Dev/stillwater && git checkout main && git pull --ff-only
```

If `pull --ff-only` fails, stop and explain. Do not force-pull.

---

## Step 3 -- Remove worktree (if found in step 1)

```bash
git worktree remove ../stillwater-<issue>
```

If the worktree has uncommitted changes, stop and warn:
"Worktree has uncommitted changes. Clean up manually or pass --force."

---

## Step 4 -- Delete local branch

```bash
git branch -d "<branch-name>"
```

If the branch is not fully merged, stop and warn. Do not force-delete.

---

## Step 5 -- Delete remote branch

```bash
gh api "repos/sydlexius/stillwater/git/refs/heads/<branch-name>" -X DELETE
```

If the remote branch is already deleted (404), note it and continue.

---

## Step 6 -- Prune stale remote refs

```bash
git fetch --prune
```

---

## Step 7 -- Update memory/worktrees.md

Read `~/.claude/projects/-root-Dev-stillwater/memory/worktrees.md` and:

1. Move the completed worktree entry from "In Progress" to "Completed"
2. Add the current date and PR number (if known) to the completed entry

---

## Step 8 -- Clean agentic plan files

Check `/root/.claude/plans/` for any `.md` files that reference the merged issue number.
Delete any that are found -- these are ephemeral working files.

---

## Step 9 -- Check milestone plan files

```bash
ls docs/plans/m*-plan.md 2>/dev/null
```

For each plan file found, check if all issues referenced in it are closed:

```bash
# Extract issue numbers from the plan file
grep -oP '#\d+' docs/plans/<plan-file> | sort -u
```

For each issue number, check if it is closed:

```bash
gh issue view <number> --json state --jq '.state'
```

If ALL issues in a plan file are CLOSED, note it and suggest:
"All issues in <plan-file> are closed. Delete it? (yes/no)"

Do not delete without confirmation.

---

## Summary

After all steps, report:
- Whether main was updated
- Which worktree was removed (or "none found")
- Which branches were deleted
- Whether any plan files are eligible for cleanup
- Any warnings or errors encountered
