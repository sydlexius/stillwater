---
description: "Verify OpenAPI spec consistency against handler implementations"
allowed-tools: ["Bash", "Glob", "Grep", "Read", "Agent"]
---

# OpenAPI Consistency Check

Scan the current diff (or the full codebase if no diff) for OpenAPI spec drift. This
catches the class of issues that Copilot flags most often in this project -- response
fields added to handlers that are not reflected in the spec, and spec descriptions that
no longer match what the code actually does.

## Steps

### 1. Determine scope

Compute the PR-wide diff base so all commits on the branch are included:

```bash
base=$(git merge-base main HEAD)
git diff --name-only "$base"..HEAD
```

If the diff includes `internal/api/handlers_*.go` or `internal/api/openapi.yaml`, run
both checks. If only handler files changed (no openapi.yaml), flag that as a finding
immediately -- the spec was not updated.

### 2. Extract response field names from changed handlers

For each changed handler file, grep for fields returned in `map[string]any` or struct
literals that are marshalled as JSON responses:

```bash
base=$(git merge-base main HEAD)
git diff "$base"..HEAD -- internal/api/handlers_*.go \
  | grep '^+' \
  | grep -E '"[a-zA-Z0-9_]+":'
```

Collect every field name that appears in `+` lines (newly added or changed).

### 3. Verify each field exists in openapi.yaml

For each field name found in step 2, check that `internal/api/openapi.yaml` has a
schema entry for that field in the relevant endpoint's response schema:

```bash
grep -n "field_name" internal/api/openapi.yaml
```

**Flag as CRITICAL** if a field appears in a handler response but has no matching entry
in the OpenAPI spec.

### 4. Verify description accuracy

For each field that IS in the spec, read its `description:` value and compare it to what
the code does:

- If the description says "Empty when [specific condition]", check whether the code also
  returns a non-empty value in other conditions not listed.
- The preferred form is "Empty only when there are no [items]" -- an invariant about the
  empty case, not an enumeration of when it is non-empty.

**Flag as IMPORTANT** if a description lists conditions but the code has additional paths
that make the field non-empty that are not listed.

### 5. Check for $ref schema mismatches

If a changed endpoint uses `$ref: "#/components/schemas/SomeName"` in its response, read
that schema definition and verify it includes every field the handler actually returns.

**Flag as CRITICAL** if the handler returns fields not in the referenced schema.

### 6. Check error path warning completeness

For each function in the diff that returns `[]string` (warnings) or appends to a
warnings slice, read the full function body and verify:

- Every branch that returns early (before the primary operation) also appends a warning
  if the function's purpose is to surface failures to clients.
- No warning string uses `%v` or `%s` with a raw `error` variable from a DB or
  internal service call.

**Flag as IMPORTANT** if an early-return error path does not emit a warning.
**Flag as CRITICAL** if a warning string includes a raw internal error message.

### 7. Check generated file staleness

```bash
base=$(git merge-base main HEAD)
templ_changed=$(git diff --name-only "$base"..HEAD -- '*.templ')
generated_changed=$(git diff --name-only "$base"..HEAD -- '*_templ.go')
```

**Flag as CRITICAL** if `$templ_changed` is non-empty and `$generated_changed` is empty.

## Output format

```
## OpenAPI Consistency Report

### CRITICAL (must fix before push)
- [field/endpoint]: description of issue

### IMPORTANT (should fix before push)
- [field/endpoint]: description of issue

### OK
- List what was checked and passed
```

If no issues found, output:
```
All checks passed. OpenAPI spec is consistent with handler implementations.
```
