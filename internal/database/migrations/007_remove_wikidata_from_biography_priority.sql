-- +goose Up
-- Issue #1029: remove Wikidata from the default biography priority list.
-- Wikidata stores structured facts (formed, members, type, gender, origin
-- ...) but its mapArtist never populates Biography, so including it in the
-- biography priority wasted a fetch iteration and surfaced misleading
-- "attempted" telemetry. New installs no longer include it via
-- DefaultPriorities(); this migration scrubs existing installs.
--
-- The UPDATE rebuilds the JSON array by selecting every element that is
-- not 'wikidata'. The EXISTS guard makes the statement a no-op (RowsAffected
-- = 0) when 'wikidata' is already absent, so re-runs are safe. Exact-element
-- matching via json_each avoids any partial-string false positives a REPLACE
-- would have allowed.

UPDATE settings
SET value = (
    SELECT json_group_array(j.value)
    FROM json_each(settings.value) j
    WHERE j.value != 'wikidata'
)
WHERE key = 'provider.priority.biography'
  AND EXISTS (
    SELECT 1 FROM json_each(settings.value) WHERE value = 'wikidata'
  );

-- +goose Down
-- Restoration is intentionally a no-op. Wikidata cannot return biographies,
-- so adding it back has no behavioral effect; preserving "what the user had"
-- across a re-up would require a separate ledger this migration does not
-- carry.
SELECT 1;
