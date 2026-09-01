-- +goose Up
-- Issue #2897: strip provider/field pairs whose adapter cannot populate the
-- field from the stored provider priority lists.
--
-- Migration 001 seeds provider.priority.* rows on first run, and
-- SettingsService.GetPriorities reads the stored row and only APPENDS defaults
-- that are missing -- it never removes a provider the stored row names. So
-- correcting DefaultPriorities() alone reaches new installs only; every
-- install that has run 001 keeps querying providers that structurally cannot
-- answer, spending a rate-limited request per refresh on a guaranteed-empty
-- response. This is the same gap migration 007 closed for the single
-- biography/wikidata pair (#1029/#1577); the pairs below are the nine found by
-- checking every default chain against what each adapter actually assigns.
--
--   genres       -> discogs    (assigns Styles from master releases, never Genres)
--   members      -> wikidata   (mapArtist assigns only Formed/Disbanded/Origin/Genres)
--   born         -> wikidata   (as above)
--   died         -> wikidata   (as above)
--   type         -> wikidata   (as above)
--   type         -> discogs    (never assigns Type)
--   gender       -> wikidata   (as above)
--   disbanded    -> wikipedia  (infobox parser assigns Born/Died, never Disbanded)
--   years_active -> audiodb    (never assigns YearsActive)
--   biography    -> musicbrainz (never assigns Biography)
--
-- The biography/musicbrainz pair is the tenth, and it is NOT one of the nine
-- found in the default chains: DefaultPriorities() has never listed MusicBrainz
-- for biography, but migration 001 seeds it FIRST in the stored row and
-- migration 007 removed only wikidata from that row. #1029/#1577 covered it
-- with a fieldProviderExclusions entry instead of a migration, and that entry
-- is consulted by the LEGACY orchestrator and the settings template but NOT by
-- the live scraper executor -- so on the path that actually runs, the request
-- is still spent. Stripping the row is what makes it true on every path.
--
-- Each statement rebuilds the JSON array from the elements that are NOT being
-- stripped, so the operator's chosen ORDER of the surviving providers is
-- preserved. The EXISTS guard makes a statement a no-op (RowsAffected = 0)
-- when the pair is already absent, so re-running is safe and an operator who
-- already removed the provider by hand is not rewritten. Exact-element
-- matching via json_each avoids the partial-string false positives a REPLACE
-- would allow.
--
-- Only the priority list itself is rewritten. A provider left behind in the
-- companion provider.priority.<field>.disabled row is inert: EnabledProviders
-- intersects the disabled set with the chain, so naming a provider that is no
-- longer in the chain has no effect.

UPDATE settings
SET value = (SELECT json_group_array(j.value) FROM json_each(settings.value) j WHERE j.value != 'discogs')
WHERE key = 'provider.priority.genres'
  AND EXISTS (SELECT 1 FROM json_each(settings.value) WHERE value = 'discogs');

UPDATE settings
SET value = (SELECT json_group_array(j.value) FROM json_each(settings.value) j WHERE j.value != 'wikidata')
WHERE key = 'provider.priority.members'
  AND EXISTS (SELECT 1 FROM json_each(settings.value) WHERE value = 'wikidata');

UPDATE settings
SET value = (SELECT json_group_array(j.value) FROM json_each(settings.value) j WHERE j.value != 'wikidata')
WHERE key = 'provider.priority.born'
  AND EXISTS (SELECT 1 FROM json_each(settings.value) WHERE value = 'wikidata');

UPDATE settings
SET value = (SELECT json_group_array(j.value) FROM json_each(settings.value) j WHERE j.value != 'wikidata')
WHERE key = 'provider.priority.died'
  AND EXISTS (SELECT 1 FROM json_each(settings.value) WHERE value = 'wikidata');

UPDATE settings
SET value = (SELECT json_group_array(j.value) FROM json_each(settings.value) j WHERE j.value != 'wikidata')
WHERE key = 'provider.priority.gender'
  AND EXISTS (SELECT 1 FROM json_each(settings.value) WHERE value = 'wikidata');

UPDATE settings
SET value = (SELECT json_group_array(j.value) FROM json_each(settings.value) j WHERE j.value NOT IN ('wikidata', 'discogs'))
WHERE key = 'provider.priority.type'
  AND EXISTS (SELECT 1 FROM json_each(settings.value) WHERE value IN ('wikidata', 'discogs'));

UPDATE settings
SET value = (SELECT json_group_array(j.value) FROM json_each(settings.value) j WHERE j.value != 'wikipedia')
WHERE key = 'provider.priority.disbanded'
  AND EXISTS (SELECT 1 FROM json_each(settings.value) WHERE value = 'wikipedia');

UPDATE settings
SET value = (SELECT json_group_array(j.value) FROM json_each(settings.value) j WHERE j.value != 'audiodb')
WHERE key = 'provider.priority.years_active'
  AND EXISTS (SELECT 1 FROM json_each(settings.value) WHERE value = 'audiodb');

UPDATE settings
SET value = (SELECT json_group_array(j.value) FROM json_each(settings.value) j WHERE j.value != 'musicbrainz')
WHERE key = 'provider.priority.biography'
  AND EXISTS (SELECT 1 FROM json_each(settings.value) WHERE value = 'musicbrainz');

-- +goose Down
-- Restoration is intentionally a no-op, matching migration 007. None of the
-- stripped providers can populate the field it was listed for, so adding them
-- back has no behavioral effect beyond restoring the wasted request; and
-- preserving "what the operator had" across a re-up would require a separate
-- ledger this migration does not carry.
SELECT 1;
