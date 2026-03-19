---
applyTo: ".github/workflows/*.yml"
---

# CI / GitHub Actions Review

- `actions/setup-go` steps should use `go-version-file: go.mod` rather than a
  hardcoded `go-version` string. Hardcoded versions drift from `go.mod` and cause
  CI breakage.
- When an endpoint's HTTP status code changes, verify that `scripts/smoke.sh` and
  any integration tests are updated to expect the new code.
- All GitHub Actions must be pinned to commit SHAs (not version tags) for supply
  chain security. The original version tag should be kept as an inline comment.
