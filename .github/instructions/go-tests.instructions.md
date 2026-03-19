---
applyTo: "**/*_test.go"
---

# Test Code Review

## Data races in test handlers (always check)

Variables written inside an `httptest.NewRecorder` handler goroutine (or any closure
passed to a test server) and read in the test goroutine after `Do()` returns must be
synchronized via channel or mutex. Unprotected shared variables are data races.

When you find one instance, search for ALL instances in the same file and related
test files. Flag every occurrence, not just the first.

## Multipart writer errors

Test helpers that use `multipart.NewWriter` (`CreatePart`, `WriteField`, `w.Close()`)
must check errors and call `t.Fatal`/`t.Fatalf` on failure. Ignored write errors
produce malformed requests and misleading test failures.

## Response body reads

`io.ReadAll(r.Body)` in test handlers must check the error before using the result.

## Test quality

- Prefer table-driven tests with `t.Run` subtests
- Assert specific values, not just `err == nil`
- Test error paths, not just success paths
- Engine/rule tests should assert relative properties (e.g., "violations > 0")
  rather than exact counts that break when new rules are added
