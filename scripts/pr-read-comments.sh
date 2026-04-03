#!/bin/bash
# pr-read-comments.sh -- Read full bodies of PR review comments
#
# Usage:
#   pr-read-comments.sh <pr> [comment-id...]
#
# With no IDs: prints full bodies of all unreplied bot inline comments.
# With IDs:    prints full bodies of only those specific comment IDs.
#
# Intended as a complement to pr-unreplied-comments.sh -- use that to
# get the list of IDs, then this to read the full bodies of specific ones.
set -euo pipefail

if [ "${#}" -lt 1 ]; then
  echo "Usage: $0 <pr> [comment-id...]"
  exit 1
fi

if ! command -v gh &>/dev/null; then
  echo "Error: gh (GitHub CLI) is required but not installed."
  exit 1
fi

pr="$1"
shift
repo=$(gh repo view --json nameWithOwner -q .nameWithOwner)
me=$(gh api user --jq .login)

BOT_LOGIN_FILTER='(
  .user.login == "coderabbitai[bot]" or
  .user.login == "Copilot" or
  .user.login == "copilot-pull-request-reviewer[bot]" or
  .user.login == "github-advanced-security[bot]"
)'

all_comments=$(gh api "repos/$repo/pulls/$pr/comments" --paginate)

if [ "${#}" -eq 0 ]; then
  # No IDs given: print all unreplied bot comments in full
  bot_ids=$(echo "$all_comments" | jq "[.[] | select($BOT_LOGIN_FILTER and .in_reply_to_id == null) | .id]")
  my_reply_targets=$(echo "$all_comments" | jq --arg me "$me" \
    '[.[] | select(.user.login == $me and .in_reply_to_id != null) | .in_reply_to_id]')
  unreplied_ids=$(jq -n --argjson bot "$bot_ids" --argjson replied "$my_reply_targets" \
    '[$bot[] | . as $id | if ($replied | any(. == $id)) then empty else $id end]')

  count=$(echo "$unreplied_ids" | jq 'length')
  if [ "$count" -eq 0 ]; then
    echo "No unreplied bot comments on PR #$pr."
    exit 0
  fi

  echo "$all_comments" | jq --argjson ids "$unreplied_ids" \
    '[.[] | select(.id as $id | $ids | any(. == $id))] | sort_by(.original_line) |
     .[] | "---\nID:   \(.id)\nFile: \(.path):\(.original_line // "?")\nBy:   \(.user.login)\n\n\(.body)\n"' -r
else
  # Specific IDs given: print just those
  ids_json=$(printf '%s\n' "$@" | jq -R 'tonumber' | jq -s '.')
  echo "$all_comments" | jq --argjson ids "$ids_json" \
    '[.[] | select(.id as $id | $ids | any(. == $id))] | sort_by(.original_line) |
     .[] | "---\nID:   \(.id)\nFile: \(.path):\(.original_line // "?")\nBy:   \(.user.login)\n\n\(.body)\n"' -r
fi
