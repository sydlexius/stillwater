-- +goose Up
-- Issue #2306: guarantee artist.nfo generation on existing installs.
--
-- The nfo_exists rule now ships enabled + auto by default (defaultRules), but
-- that seed only applies to fresh installs -- existing installs keep whatever
-- state their rules row already holds, so an install where the rule was left
-- (or turned) off would silently never generate missing artist.nfo files,
-- breaking Stillwater's core NFO-management contract.
--
-- This reconciles existing installs: force nfo_exists on + auto, EXCEPT when the
-- active platform profile is Plex (nfo_enabled=0), which does not use .nfo files
-- (Plex builtin is seeded nfo_enabled=0 in 001_initial_schema.sql). The carve-out
-- keys off the data (active profile's nfo_enabled), never the profile name.
UPDATE rules
SET enabled = 1,
    automation_mode = 'auto'
WHERE id = 'nfo_exists'
  AND NOT EXISTS (
    SELECT 1 FROM platform_profiles WHERE is_active = 1 AND nfo_enabled = 0
  );

-- +goose Down
-- Irreversible data reconciliation: the prior per-install enabled/automation_mode
-- state is not recorded, and reverting nfo_exists to disabled would wrongly break
-- NFO generation for installs that legitimately want it on. No-op by design.
