-- +goose Up
-- Issue #1221: realign legacy 'auto' automation_mode rows for the
-- directory_name_mismatch and extraneous_images rules to 'manual'. Both
-- rules can rewrite filesystem state when in auto mode; the auto default
-- silently reverted user-initiated on-disk renames (and, for
-- extraneous_images, deleted user-managed image files). Defaulting them to
-- manual gates the destructive action behind an explicit user toggle.
--
-- The column DEFAULT in 001 remains 'auto' (historical, not edited per
-- the forward-only goose policy documented in 002). SeedDefaults always
-- specifies the value explicitly when seeding rules on a fresh install,
-- so the column DEFAULT is unreachable from code paths -- this UPDATE
-- only affects DBs that were initialized before the in-code default was
-- changed to 'manual'.
--
-- Operators who deliberately set either rule to 'auto' before this
-- migration ran will need to re-toggle after upgrading. This is the
-- recommended option from the issue: the data-loss risk justifies an
-- explicit opt-in over preserving the previous selection.
UPDATE rules
SET automation_mode = 'manual',
    updated_at = datetime('now')
WHERE id IN ('directory_name_mismatch', 'extraneous_images')
  AND automation_mode = 'auto';

-- +goose Down
-- No-op: we cannot reliably distinguish "user explicitly chose auto
-- post-migration" from "row was migrated up by this script." Operators
-- who need to roll back can flip the values manually.
SELECT 1;
