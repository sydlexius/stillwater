#!/bin/bash
# resolve-threads.sh -- Resolve Copilot review threads on a PR via GraphQL
#
# Usage:
#   resolve-threads.sh <pr> <comment-db-id...>
#
# Resolves the review threads whose first comment matches one of the given
# comment database IDs. Only resolves threads started by Copilot/bot users.
# Prints "Resolved <thread-id> (comment <db-id>)" per thread, or
# "Skipped <db-id> (already resolved or not found)" if nothing matched.
#
# Typical workflow: collect the Copilot comment IDs you replied to, then:
#   bash scripts/resolve-threads.sh 851 1234567 2345678 3456789
set -euo pipefail

if [ "${#}" -lt 2 ]; then
  echo "Usage: $0 <pr> <comment-db-id...>"
  exit 1
fi

if ! command -v gh &>/dev/null; then
  echo "Error: gh (GitHub CLI) is required but not installed."
  exit 1
fi

pr="$1"
shift
repo=$(gh repo view --json nameWithOwner -q .nameWithOwner 2>/dev/null) || {
  echo "Error: could not determine repository. Run from inside a git repo with a GitHub remote."
  exit 1
}
owner="${repo%%/*}"
name="${repo##*/}"

# Fetch all review threads with first-comment metadata
threads=$(gh api graphql -f query="
{
  repository(owner: \"$owner\", name: \"$name\") {
    pullRequest(number: $pr) {
      reviewThreads(first: 100) {
        nodes {
          id
          isResolved
          comments(first: 1) {
            nodes {
              databaseId
              author { login }
            }
          }
        }
      }
    }
  }
}" --jq '.data.repository.pullRequest.reviewThreads.nodes')

for db_id in "$@"; do
  thread=$(echo "$threads" | jq --argjson id "$db_id" \
    '[.[] | select(
      .comments.nodes[0].databaseId == $id and
      (.comments.nodes[0].author.login | test("copilot"; "i"))
    )] | first // empty')

  if [ -z "$thread" ]; then
    echo "Skipped $db_id (not found or not a Copilot comment)"
    continue
  fi

  is_resolved=$(echo "$thread" | jq -r '.isResolved')
  if [ "$is_resolved" = "true" ]; then
    echo "Skipped $db_id (already resolved)"
    continue
  fi

  thread_id=$(echo "$thread" | jq -r '.id')
  gh api graphql -f query="
mutation {
  resolveReviewThread(input: { threadId: \"$thread_id\" }) {
    thread { isResolved }
  }
}" --jq '.data.resolveReviewThread.thread.isResolved' > /dev/null

  echo "Resolved thread $thread_id (comment $db_id)"
done
