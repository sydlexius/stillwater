# Milestone 21 -- Library Operations

## Goal

Add async library operations (populate + scan) with progress feedback, and close out two already-resolved issues (#195, #209).

## Acceptance Criteria

- [ ] Populate Artists runs async (202 Accepted, goroutine, status polling)
- [ ] Populate button shows spinner and disables during execution
- [ ] Toast on populate completion: "Populated N artists from LibraryName"
- [ ] Scan Library button added for libraries with a connection
- [ ] Scan runs async with spinner and disable
- [ ] Toast on scan completion: "Scan complete: N artists updated in LibraryName"
- [ ] Double-click returns 409 Conflict
- [ ] Success toast variant (green styling)

## Dependency Map

```
#195 (verify & close) -- no code changes
#209 (wontfix) -- no code changes
#196 (async operations) -- sole implementation PR
```

## Checklist

### Issue #195 -- Library selector dropdown
- [x] Implementation (already done)
- [ ] Manual verification
- [ ] Issue closed

### Issue #209 -- UNC/CIFS path support
- [ ] Close as wontfix with explanation

### Issue #196 -- Async library operations
- [ ] LibraryOpResult type and Router fields
- [ ] Refactor populate to async (202 + goroutine)
- [ ] New scan endpoint and handler
- [ ] Status polling endpoint
- [ ] Platform type extensions (ImageTags, ArtistImage)
- [ ] Success toast function in layout.templ
- [ ] Settings UI: async buttons with spinner + polling
- [ ] Tests
- [ ] PR opened
- [ ] CI passing
- [ ] PR merged

## UAT / Merge Order

1. PR for #196 (base: main) -- single PR for all async operation work

## Notes

- 2026-02-26: #195 confirmed already implemented
- 2026-02-26: #209 closed as wontfix
