# Milestone 19 -- Provider & Connection UX

## Goal
Test provider API keys and connection credentials before persisting them. Show inline errors on failure with a "Save anyway" override. Persist test status so the UI reflects whether keys/connections are verified.

## Acceptance Criteria
- [x] Provider API keys are tested before saving (for testable providers)
- [x] Test failure shows inline error with "Save anyway" button
- [x] "Save anyway" persists the key with untested status
- [x] Successful test persists "ok" status, shown as green dot
- [x] Standalone "Test" button persists status ("ok" or "invalid")
- [x] Connection credentials are tested before saving
- [x] Connection test failure shows inline error with "Save anyway"
- [x] OOBE wizard has same test-before-save behavior for both providers and connections
- [x] Context-based key override allows testing unsaved keys
- [x] Key status is cleared when a new key is saved or deleted

## Dependency Map
#207 (provider key test-before-save) -- independent
#208 Phase 1 (connection test-before-save) -- independent

## Checklist
### Issue #207 -- Provider API key test-before-save
- [x] Context-based key override in settings.go
- [x] Persistent key status tracking (SetKeyStatus, GetKeyStatus)
- [x] SetAPIKey/DeleteAPIKey clear stale status
- [x] ListProviderKeyStatuses uses persisted status
- [x] handleSetProviderKey test-before-save logic
- [x] handleTestProvider persists status + re-renders card
- [x] ProviderTestSaveFailure template (settings + OOBE)
- [x] Unit tests for context override and key status
- [x] Tests passing
- [x] PR opened
- [ ] CI passing
- [ ] PR merged

### Issue #208 Phase 1 -- Connection test-before-save
- [x] testConnectionDirect helper
- [x] handleCreateConnection test-before-save logic
- [x] handleCreateConnectionSuccess for HTMX/JSON responses
- [x] ConnectionTestSaveFailure template (settings + OOBE)
- [x] Settings form targets conn-result div
- [x] OOBE form targets ob-conn-result div
- [x] Tests passing
- [x] PR opened
- [ ] CI passing
- [ ] PR merged

## UAT / Merge Order
1. Single PR for both #207 and #208 (no code overlap, same UX pattern)

## Notes
- 2026-02-26: Both issues implemented in a single session, single branch
- Context override pattern (WithAPIKeyOverride) uses unexported struct key to avoid collisions
- Connection test uses testConnectionDirect which constructs clients directly (no DB lookup needed)
- OOBE connection form keeps the hx-on::after-request callback for onConnectionSaved; test failures return 422 so event.detail.successful is false and the callback does not fire
- Settings connection form switches from hx-swap="none" + after-request reload to hx-target + HX-Refresh on success
