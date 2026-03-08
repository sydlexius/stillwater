---
description: "Run all pre-push checks then squash and push -- the full pre-PR gate"
argument-hint: "[optional: short description of what this PR does]"
allowed-tools: ["Bash", "Glob", "Grep", "Read", "Agent", "Task"]
---

# PR Preparation Gate

Run every pre-push check in order. Gate on failures. Squash and push only when clean.

**Optional context:** "$ARGUMENTS"

---

## Step 1 -- Orient

```bash
base=$(git merge-base main HEAD)
git branch --show-current
git log main..HEAD --oneline
git diff "$base"..HEAD --stat
```

Report:
- Current branch name
- Number of commits ahead of main
- Files changed (summary)

If on `main`, stop immediately: "You are on main. Create a feature branch first."

---

## Step 2 -- Tests

```bash
go test ./... 2>&1
```

If any test fails: print the failures, stop, and say:
"Fix failing tests before proceeding. Do not push broken code."

If tests pass: note it and continue.

---

## Step 3 -- OpenAPI consistency check

Follow the logic in `.claude/commands/check-openapi.md` against the PR-wide diff:

```bash
base=$(git merge-base main HEAD)
git diff "$base"..HEAD --name-only
```

Do not use `git diff main` directly -- that can include unrelated commits that landed
on main after this branch was cut.

Report findings using the same CRITICAL / IMPORTANT / OK format defined in that file.

If any CRITICAL finding: stop. List what must be fixed.

---

## Step 4 -- Local code review

Launch the following agents against the PR-wide diff (`git diff "$(git merge-base main HEAD)"..HEAD`), in parallel if
possible:

- `pr-review-toolkit:code-reviewer` -- general quality and CLAUDE.md compliance
- `pr-review-toolkit:silent-failure-hunter` -- error paths that swallow failures

If test files changed, also launch:
- `pr-review-toolkit:pr-test-analyzer` -- test coverage gaps

If new types were added, also launch:
- `pr-review-toolkit:type-design-analyzer`

Collect all findings and consolidate into a single report:

```
## Local Review Summary

### Critical (must fix)
- [agent]: issue [file:line]

### Important (should fix)
- [agent]: issue [file:line]

### Suggestions (optional)
- [agent]: suggestion [file:line]
```

If any Critical findings: stop. Say "Fix all critical issues before pushing."

If Important findings: present them and ask:
"There are important (non-blocking) findings. Fix them now, or proceed anyway? (fix/proceed)"

Wait for the user's answer before continuing.

---

## Step 5 -- Generated file check

```bash
base=$(git merge-base main HEAD)
templ_changed=$(git diff --name-only "$base"..HEAD -- '*.templ')
generated_changed=$(git diff --name-only "$base"..HEAD -- '*_templ.go')

if [ -n "$templ_changed" ] && [ -z "$generated_changed" ]; then
  echo "ERROR: .templ files changed but *_templ.go files did not."
  echo "Run 'templ generate' and stage the generated files before pushing."
  exit 1
fi
```

If the check exits with an error, stop and show the message.

---

## Step 6 -- Squash

Count commits ahead of main:

```bash
git log main..HEAD --oneline
```

If there is more than one commit:

Say: "Squashing [N] commits into clean commit(s) before push. This is required so
Copilot sees the full changeset at once rather than discovering issues incrementally."

Run interactive rebase to let the user squash:

```bash
git rebase -i main
```

After the rebase completes, verify the branch is still ahead of main:

```bash
git log main..HEAD --oneline
```

If the rebase failed or the branch is no longer ahead of main, stop and explain.

If there is already only one commit, say: "Already a single commit -- no squash needed."

---

## Step 7 -- Push

```bash
git push origin $(git branch --show-current) 2>&1
```

If the branch has no upstream yet, use `-u`:

```bash
git push -u origin $(git branch --show-current) 2>&1
```

Report the push result. If it fails (non-fast-forward, auth error, etc.), stop and
explain -- do not retry automatically.

---

## Step 8 -- PR creation offer

After a successful push, check if a PR already exists:

```bash
gh pr view 2>&1
```

If no PR exists, offer to create one:
"Push succeeded. Create the PR now? (yes/no)"

If yes, run:
```bash
gh pr create --title "<branch-description>" --body "$(cat <<'EOF'
## Summary
<bullet points from $ARGUMENTS or inferred from commit message>

## Test plan
- [ ] `go test ./...` passes
- [ ] OpenAPI spec verified against implementation
- [ ] Local review toolkit passed

Generated with [Claude Code](https://claude.com/claude-code)
EOF
)"
```

Fill in the summary from `$ARGUMENTS` if provided, or from the squashed commit message
if not.

If a PR already exists, print its URL and say "PR already open -- Copilot will review
the push automatically."
