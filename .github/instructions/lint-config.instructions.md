---
applyTo: ".golangci.yml"
---

# Lint Configuration Review

The lint surface is intentionally curated, not maximal. Rules below describe the
cadence and conventions established during Milestone 49 (PRs #1428-#1432, #1452).

## Cadence: one linter per PR

When enabling a new linter, the PR must contain both the enable and the full
cleanup sweep in a single change. Splitting them produces a window where CI is
red on `main` for unrelated work.

- Run `golangci-lint cache clean && golangci-lint run --max-issues-per-linter 0
  --max-same-issues 0 ./...` before sizing the sweep. The default cap of 50
  issues per linter hides batches and produces under-scoped PRs.
- For each violation, decide: real fix, justified `//nolint:foo // reason`
  suppression, or config-level exclusion in `linters.exclusions.rules`. Avoid
  config exclusions for paths that are not categorically exempt; per-site
  `//nolint` with a reason is preferred because it survives refactors.

## `//nolint` directive convention

`nolintlint` is enabled with `require-explanation: true`. Every `//nolint:foo`
directive must carry a `// reason` comment after the directive:

```go
defer f.Close() //nolint:errcheck // Close error not actionable on cleanup
```

The reason text is the friction that turns "I'll just nolint this" into either
a real fix or a reviewable justification. Do not paper over the requirement
with placeholder text like `// silence linter`.

## Test-file exclusions are load-bearing

`linters.exclusions.rules` excludes `gosec`, `errcheck`, and `noctx` from
`_test.go` files. This is intentional, not aspirational; rewriting tests to pass
these linters generates ceremony without value. The corresponding review-time
rules live in `go-tests.instructions.md` and are the only enforcement for those
classes in test code.

## Custom function registration

Linters that walk call sites by function name require explicit registration
when handlers wrap a stdlib helper. The current example is `musttag`:

```yaml
musttag:
  functions:
    - name: github.com/sydlexius/stillwater/internal/api.writeJSON
      tag: json
      arg-pos: 2
```

When introducing a new response-wrapper helper (anything that hides
`encoding/json` from the call site), register it here. Without registration,
`musttag` only validates direct `json.Marshal` callers and silently misses every
handler response that flows through the wrapper.

## CI version pin

`.github/workflows/ci.yml` pins the `golangci-lint` action to a specific
version. Bumping the pin requires a probe PR: enable the new version against a
clean cache and verify no transient or analyzer-regression findings appear
before merging. The `gosec` taint analyzer in particular has produced phantom
findings in past releases that did not reproduce locally; PR #1452 bumped the
pin in that scenario.
