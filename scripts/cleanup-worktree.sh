#!/bin/bash
# cleanup-worktree.sh <issue-number> -- remove worktree and branches after PR merge
set -euo pipefail

if [ -z "${1:-}" ]; then
  echo "Usage: $0 <issue-number>"
  exit 1
fi

if ! command -v jq &>/dev/null; then
  echo "Error: jq is required but not installed."
  exit 1
fi

if ! command -v gh &>/dev/null; then
  echo "Error: gh (GitHub CLI) is required but not installed."
  exit 1
fi

issue="$1"
if ! [[ "$issue" =~ ^[0-9]+$ ]]; then
  echo "Error: issue-number must be numeric (got: $issue)"
  exit 1
fi
# Match both stillwater-<issue> and stillwater-m<N>-<issue>
pattern="stillwater(-m[0-9]+)?-${issue}$"

# Find worktree path and branch by pattern
worktree_path=$(git worktree list --porcelain \
  | awk -v p="$pattern" '/^worktree / { wt=$2 } /^branch / && wt ~ p { print wt; exit }')
branch=$(git worktree list --porcelain \
  | awk -v p="$pattern" '/^worktree / { wt=$2 } /^branch / && wt ~ p { gsub("refs/heads/","",$2); print $2; exit }')

if [ -z "$worktree_path" ]; then
  echo "No worktree found matching pattern: $pattern"
  echo "Current worktrees:"
  git worktree list
  exit 1
fi

echo "Worktree: $worktree_path"
echo "Branch:   $branch"
echo ""

# Remove worktree
echo "=== Removing worktree ==="
git worktree remove "$worktree_path"

# Delete local branch
if [ -n "$branch" ]; then
  echo "=== Deleting local branch: $branch ==="
  git branch -d "$branch" || git branch -D "$branch"

  # Delete remote branch
  echo "=== Deleting remote branch: $branch ==="
  repo=$(gh repo view --json nameWithOwner -q .nameWithOwner)
  encoded=$(printf '%s' "$branch" | jq -sRr @uri)
  gh api "repos/$repo/git/refs/heads/$encoded" -X DELETE 2>/dev/null \
    && echo "Remote branch deleted." \
    || echo "Remote branch not found or already deleted."
fi

# Prune stale tracking refs
echo "=== Pruning stale refs ==="
git fetch --prune

echo ""
echo "Done. Update ~/.claude/projects/.../memory/worktrees.md to reflect the change."
