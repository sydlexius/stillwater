## Summary

- What changed and why (focus on the why; 1-3 bullets).
- Note any user-visible behavior change.
- Call out anything reviewers should look at first.

## Linked issue

Closes #

(Use `Part of #N` for a slice of a larger issue that is not yet fully resolved.)

## Pre-flight checklist

- [ ] Pre-push gate green locally (`bash scripts/pre-push-gate.sh`), or run via `/prep-pr` / `/pr-review-toolkit:review-pr` which invokes it.
- [ ] Code review pass complete (`/prep-pr` or `/pr-review-toolkit:review-pr`); critical and important findings fixed before pushing.
- [ ] Commits squashed into clean, logical commits before the first push (Copilot reviews the diff at PR open).
- [ ] At least one label set on `gh pr create --label ...` (the CI label gate fails without one).
- [ ] Docs shipped in this PR for any user-visible change, or `docs: not-required` set for a test, CI, or refactor PR with no user-visible behavioral change. Never add `needs-docs-review` yourself: the Docs Drift action sets it when code changed without docs, and clears it once the docs land.
- [ ] Screenshot attached for any UI change.
- [ ] UAT performed for user-visible changes (run the binary, not just the test suite).
- [ ] OpenAPI spec updated if any request or response shape changed (`make check-openapi`).
- [ ] `templ generate` re-run and `*_templ.go` committed if any `.templ` file changed.

## Test plan

- [ ] New or changed functionality in this PR is covered by tests.
- [ ] `go test -race ./...` passes locally.
- [ ] `bash scripts/pre-push-gate.sh` passes locally.
- [ ] Manual UAT steps (list the specific flows exercised):
  - [ ]
  - [ ]
- [ ] Reviewer follow-ups (anything you want a second pair of eyes on):
  - [ ]
