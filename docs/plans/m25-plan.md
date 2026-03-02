# Milestone 25 -- Documentation & Wiki

## Goal

Audit all API endpoints registered in `router.go` against the OpenAPI spec
(`internal/api/openapi.yaml`) so every route is documented and every spec entry
corresponds to a live route. Reduces noise in Copilot PR reviews caused by
undocumented endpoints.

## Acceptance Criteria

- [x] Every route in `router.go` has a corresponding entry in `openapi.yaml`
- [x] Every entry in `openapi.yaml` corresponds to an active route
- [x] Request and response schemas match actual handler behavior

## Dependency Map

```
#298 (API spec audit) -- standalone, no dependencies
```

#124 (Developer Guide wiki page) is already closed.

## Checklist

### Issue #298 -- Audit all API endpoints against OpenAPI spec
- [x] Extract all route registrations from `router.go`
- [x] Compare against all `paths:` entries in `openapi.yaml`
- [x] Add missing operations (method + path + request/response schemas)
- [x] Remove spec entries for routes that no longer exist (none found)
- [x] Tests
- [ ] PR opened (#?)
- [ ] CI passing
- [ ] PR merged

## UAT / Merge Order

1. PR for #298 (base: main) -- spec-only change, no runtime impact

## Notes

- Labels: documentation, technical-debt, api
- No mode/model hints on #298 -- default to Sonnet direct (technical debt)
- ~134 routes registered vs ~54 documented at time of issue creation
- 2026-03-01: Audit complete. 136 API operations in router.go, all 136 now documented.
  66 new path entries added, 0 stale entries found. 8 new reusable schemas added
  (Library, LibraryOpResult, APIToken, Violation, LoggingConfig, MaintenanceStatus,
  and existing Error/Status schemas reused). Spec version bumped to 0.25.0.
  New tags: Docs, Libraries, Scraper, Notifications, Push.
