# Milestone 34 -- UX & Accessibility

## Goal

Improve keyboard navigation and accessibility, add browser notifications, fix login
form bugs, and preserve filter state across navigation.

## Acceptance Criteria

- [ ] All interactive elements keyboard-accessible with visible focus indicators (Phase 1)
- [ ] Power-user shortcuts available (j/k navigation, chord shortcuts, ? help overlay) (Phase 2)
- [ ] Browser notifications delivered via SSE for configurable event types
- [ ] Login form recognized by password managers (1Password, Bitwarden, etc.)
- [ ] Clipboard paste no longer doubles input in login fields
- [ ] Search filters and sort preserved on browser back navigation

## Dependency Map

```
#514 (1Password) --\
                    +--> both are login form fixes, can share a branch if small
#515 (paste)     --/

#503 (keyboard nav) -- independent
#513 (browser notifications) -- independent
#517 (filter persistence) -- independent
```

#514 and #515 are both login form bugs and may share a root cause.

## Checklist

### Issue #514 -- Login form 1Password autofill
- [ ] Audit login form HTML for `autocomplete`, `name`, `id` attributes
- [ ] Ensure `<form>` wrapper exists (even with HTMX submission)
- [ ] `autocomplete="username"` and `autocomplete="current-password"` attributes
- [ ] Test with 1Password, Bitwarden, and browser built-in password manager
- [ ] Tests

### Issue #515 -- Clipboard paste doubles input
- [ ] Investigate event handlers on login fields (input, paste, HTMX triggers)
- [ ] Fix duplicate event processing (preventDefault or remove manual insertion)
- [ ] Test with keyboard paste (Ctrl+V) and context menu paste
- [ ] Tests

### Issue #503 -- Keyboard navigation and accessibility
- [ ] Phase 1: tab order, focus indicators, ARIA roles, skip-to-content, modal keyboard support
- [ ] Phase 2: global shortcuts (/, ?, g+x), j/k list navigation, ? help overlay
- [ ] keyboard.js vendored (no external dependencies)
- [ ] Tests for shortcut registration

### Issue #513 -- Browser notifications via SSE
- [ ] SSE endpoint: `GET /api/v1/events/stream`
- [ ] Notification permission requested on opt-in
- [ ] Per-event toggles in Settings page
- [ ] Master enable/disable toggle
- [ ] "Test Notification" button
- [ ] No duplicate with in-page toasts
- [ ] notification.js vendored
- [ ] Tests

### Issue #517 -- Preserve search filters on back navigation
- [ ] Filter state stored in URL query parameters
- [ ] `history.replaceState()` updates URL as filters change
- [ ] Filters restored from URL on page load
- [ ] Works across Chrome, Firefox, Safari, Edge
- [ ] Tests

## UAT / Merge Order

Session 1 (login fixes -- small, quick):
1. PR for #514 + #515 (base: main) -- login form fixes (may combine if root cause shared)

Session 2 (filter persistence):
2. PR for #517 (base: main) -- URL-based filter state

Session 3 (accessibility):
3. PR for #503 Phase 1 (base: main) -- accessibility baseline
4. PR for #503 Phase 2 (base: main) -- power-user shortcuts

Session 4 (notifications):
5. PR for #513 (base: main) -- SSE + Web Notifications

## Notes

- #514/#515: `[mode: direct] [model: sonnet] [effort: medium]` -- login form bugs
- #503: `[mode: plan] [model: sonnet] [effort: medium]` -- phased approach
- #513: `[mode: plan] [model: sonnet] [effort: medium]` -- SSE now, Web Push later (Phase 2 future issue)
- #517: `[mode: plan] [model: sonnet] [effort: medium]`
- WCAG 2.1 Level AA is the target accessibility standard for #503 Phase 1
