## Summary

- What changed and why (focus on the why; 1-3 bullets).
- Note anything reviewers should look at first.

## Linked issue

Closes #

(Use `Part of #N` for a slice of a larger issue that is not yet fully resolved.)

## Pre-flight checklist

- [ ] Pre-push gate green locally (`bash scripts/pre-push-gate.sh`), or run via `/prep-pr` / `/pr-review-toolkit:review-pr` which invokes it.
- [ ] Code review pass complete (`/prep-pr` or `/pr-review-toolkit:review-pr`); critical and important findings fixed before pushing.
- [ ] Commits squashed into clean, logical commits before the first push.
- [ ] At least one label set on `gh pr create --label ...` (the CI label gate fails without one).
- [ ] Docs label decision made: `docs: not-required` for chore/CI/refactor PRs with no user-visible behavioral change.

## Test plan

- [ ] `go test -race ./...` passes locally.
- [ ] `bash scripts/pre-push-gate.sh` passes locally.
- [ ] Reviewer follow-ups (anything you want a second pair of eyes on):
  - [ ]
