# Milestone 24 -- Automation (v0.24.0)

## Goal

Add automation infrastructure: API tokens for external access, inbound webhooks from
Lidarr, scheduled rule evaluation, notification badges, and filesystem watching (deferred
to M24.5).

## Acceptance Criteria

- [ ] Notification badge on navbar showing active violation count with configurable severity filter
- [ ] Automation mode "notify" renamed to "manual" across DB, model, and UI
- [ ] Cron scheduler for rule evaluation with configurable interval
- [ ] API token CRUD with scoped permissions (read, write, webhook, admin)
- [ ] Dual auth (session + API token) in middleware with CSRF bypass for API tokens
- [ ] Inbound Lidarr webhook endpoint with async event processing
- [ ] Settings UI cards for all new features

## Dependency Map

```
#162 Phase 1 (rename + cron)     -- independent
#180 (badges)                    -- independent
#181 (API tokens)                -- independent (#178 already closed)
#182 (Lidarr webhooks)           -- blocked by #181
#162 Phase 3 (fsnotify watcher)  -- deferred to M24.5
```

## Checklist

### Issue #180 -- Notification counter badges
- [ ] Implementation
- [ ] PR opened
- [ ] CI passing
- [ ] PR merged

### Issue #162 Phase 1 -- Rename notify to manual + cron scheduler
- [ ] Implementation
- [ ] PR opened
- [ ] CI passing
- [ ] PR merged

### Issue #181 -- API token generation
- [ ] Implementation
- [ ] PR opened
- [ ] CI passing
- [ ] PR merged

### Issue #182 -- Inbound Lidarr webhooks
- [ ] Implementation
- [ ] PR opened
- [ ] CI passing
- [ ] PR merged

## UAT / Merge Order

1. PR 1: feat/notification-badges (base: main)
2. PR 2: feat/rename-notify-cron (base: main)
3. PR 3: feat/api-tokens (base: main)
4. PR 4: feat/lidarr-webhooks (base: main, after PR 3 merges)

PRs 1-3 can land in any order. PR 4 lands after PR 3.

## Notes

- Migration numbers assigned at implementation time based on merge order. Next available: 006.
- fsnotify filesystem watcher deferred to M24.5 (separate plan file).
