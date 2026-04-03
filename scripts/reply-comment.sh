#!/bin/bash
# reply-comment.sh -- post or reply to a PR comment
#
# Usage:
#   reply-comment.sh <pr> <body>                  -- top-level PR comment
#   reply-comment.sh <pr> <comment-id> <body>     -- reply to an inline review thread
set -euo pipefail

if [ "${#}" -lt 2 ]; then
  echo "Usage:"
  echo "  $0 <pr> <body>                 -- top-level PR comment"
  echo "  $0 <pr> <comment-id> <body>    -- reply to inline review thread"
  exit 1
fi

if ! command -v gh &>/dev/null; then
  echo "Error: gh (GitHub CLI) is required but not installed."
  exit 1
fi

repo=$(gh repo view --json nameWithOwner -q .nameWithOwner 2>/dev/null) || {
  echo "Error: could not determine repository. Run from inside a git repo with a GitHub remote."
  exit 1
}
pr="$1"

if [ "${#}" -eq 2 ]; then
  # Top-level PR comment
  body="$2"
  gh api "repos/$repo/issues/$pr/comments" -f body="$body" --silent
  echo "Posted comment on PR #$pr"
else
  # Reply to inline review thread
  comment_id="$2"
  body="$3"
  gh api "repos/$repo/pulls/$pr/comments/$comment_id/replies" -f body="$body" --silent
  echo "Replied to comment $comment_id on PR #$pr"
fi
