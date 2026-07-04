-- +goose Up
-- Issue #2219: server-side persistence for ignored near-duplicate groups.
--
-- Before #2219 the duplicates "Ignore" affordance was CLIENT-ONLY: a per-browser
-- localStorage key (ui.confirm.duplicate.<sorted-member-ids>). That state never
-- reached the server, so the sidebar count pill kept counting ignored groups and
-- the choice did not follow the user across browsers or devices. This migration
-- moves the ledger server-side; the client now POSTs the ignore instead.
--
-- signature is the canonical group identity: the group's member artist IDs
-- sorted ascending and joined with '|' (matching the detector's member set and
-- the old client key scheme exactly). It is an EXACT-match key by design: if the
-- detector later regroups (a member is added, removed, or merged away) the new
-- group computes a DIFFERENT signature and RESURFACES as a fresh suspected
-- duplicate rather than staying silently suppressed. The UNIQUE index on
-- signature makes re-ignoring the same group idempotent (INSERT ... ON CONFLICT
-- DO NOTHING).
--
-- group_key and reason are non-authoritative display context captured at ignore
-- time for the manage-ignored view (a later PR); they never participate in the
-- match.
CREATE TABLE IF NOT EXISTS ignored_duplicate_groups (
    id         TEXT NOT NULL PRIMARY KEY,
    signature  TEXT NOT NULL UNIQUE,
    group_key  TEXT NOT NULL DEFAULT '',
    reason     TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

-- +goose Down
DROP TABLE IF EXISTS ignored_duplicate_groups;
