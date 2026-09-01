-- +goose Up
-- Issue #2897: strip provider/field pairs whose adapter cannot populate the
-- field from the stored provider priority lists.
--
-- Migration 001 seeds provider.priority.* rows on first run, and
-- SettingsService.GetPriorities reads the stored row and only APPENDS defaults
-- that are missing -- it never removes a provider the stored row names. So
-- correcting DefaultPriorities() alone reaches new installs only; every
-- install that has run 001 keeps routing to providers that structurally
-- cannot answer for the field.
--
-- This does NOT save a request: both full-refresh paths cache one GetArtist
-- call per provider for the whole refresh, and every stripped provider below
-- still appears in at least one surviving chain (wikidata in
-- formed/disbanded/origin, discogs in styles/biography/thumb, and so on), so
-- it is fetched regardless of whether this migration runs. The actual benefit
-- is a settings UI and per-field comparison panel that only ever lists
-- providers that can genuinely answer, instead of structurally-empty slots
-- that look like real options. This is the same gap migration 007 closed for
-- the single biography/wikidata pair (#1029/#1577); the pairs below are the
-- eight found by checking every default chain against what each adapter
-- actually assigns, plus the biography/musicbrainz pair carried over from
-- #1029/#1577.
--
--   genres       -> discogs    (assigns Styles from master releases, never Genres)
--   members      -> wikidata   (mapArtist assigns only Formed/Disbanded/Origin/Genres)
--   born         -> wikidata   (as above)
--   died         -> wikidata   (as above)
--   type         -> wikidata   (as above)
--   type         -> discogs    (never assigns Type)
--   gender       -> wikidata   (as above)
--   disbanded    -> wikipedia  (infobox parser assigns Born/Died, never Disbanded)
--   biography    -> musicbrainz (never assigns Biography)
--
-- years_active/audiodb is deliberately NOT in this list. audiodb.mapArtist
-- never assigns YearsActive literally, so it is dead on both full-refresh
-- paths (FetchMetadata's scalarFieldAccessors and the executor's
-- FieldYearsActive applier both read meta.YearsActive with no fallback). But
-- the per-field comparison path (FetchFieldFromProviders ->
-- extractFieldForComparison, internal/provider/orchestrator.go) synthesizes a
-- years_active candidate from Born/Died via SynthesizeYearsActive when the
-- literal is empty, and audiodb.mapArtist does set Born/Died for solo
-- artists -- so on that path AudioDB is a live, user-facing answer. Removing
-- it would silently drop a real candidate from the artist-detail
-- compare-providers panel.
--
-- The biography/musicbrainz pair is NOT one of the default-chain findings
-- above: DefaultPriorities() has never listed MusicBrainz for biography, but
-- migration 001 seeds it FIRST in the stored row and migration 007 removed
-- only wikidata from that row. #1029/#1577 covered it with a
-- fieldProviderExclusions entry instead of a migration, and that entry is
-- consulted by the LEGACY orchestrator and the settings template but NOT by
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
