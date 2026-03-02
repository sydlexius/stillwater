# Milestone 25 -- Documentation & Wiki

## Goal

Audit all API endpoints registered in `router.go` against the OpenAPI spec
(`internal/api/openapi.yaml`) so every route is documented and every spec entry
corresponds to a live route. Reduces noise in Copilot PR reviews caused by
undocumented endpoints.

## Acceptance Criteria

- [ ] Every route in `router.go` has a corresponding entry in `openapi.yaml`
- [ ] Every entry in `openapi.yaml` corresponds to an active route
- [ ] Request and response schemas match actual handler behavior

## Dependency Map

```
#298 (API spec audit) -- standalone, no dependencies
```

#124 (Developer Guide wiki page) is already closed.

## Checklist

### Issue #298 -- Audit all API endpoints against OpenAPI spec
- [ ] Extract all route registrations from `router.go`
- [ ] Compare against all `paths:` entries in `openapi.yaml`
- [ ] Add missing operations (method + path + request/response schemas)
- [ ] Remove spec entries for routes that no longer exist
- [ ] Tests
- [ ] PR opened (#?)
- [ ] CI passing
- [ ] PR merged

## UAT / Merge Order

1. PR for #298 (base: main) -- spec-only change, no runtime impact

## Notes

- Labels: documentation, technical-debt, api
- No mode/model hints on #298 -- default to Sonnet direct (technical debt)
- ~134 routes registered vs ~54 documented at time of issue creation
