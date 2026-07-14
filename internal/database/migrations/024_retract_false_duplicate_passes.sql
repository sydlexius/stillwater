-- +goose Up
-- Retracts the false "passed" rows the two duplicate rules recorded for
-- artists that have no local filesystem path (issue #2509).
--
-- Before the per-artist capability gate landed, both duplicate checkers opened
-- with:
--
--     if a.Path == "" { return nil }
--
-- A nil return means "no violation", which the pipeline persists as a
-- passed=1 rule_results row. So every path-less artist (an API-only import
-- from Emby or Jellyfin) was recorded as PASSING two rules that never looked
-- at a single one of its images. The gate stops new false rows from being
-- written and the runtime retraction removes them the next time an artist is
-- evaluated -- but rules_evaluated_at can keep an artist out of the dirty set
-- for a long time, and until then the artist rule-result breakdown, the
-- compliance grid and the per-rule pass-rate dashboards all keep reporting the
-- phantom pass. This migration clears them once, up front.
--
-- The predicate needs no capability logic in SQL, and deliberately does not
-- replicate it: under the old code a path-less artist could NEVER produce a
-- violation for these rules, so EVERY such row is a false pass by
-- construction. Deleting all of them is correct even for an artist that IS
-- capable under the new gate (it has two or more comparable stored hashes):
-- that artist's rows get repopulated honestly by its next evaluation, whereas
-- an incapable artist's stay absent, which is the honest state. A SQL
-- re-implementation of the "two or more comparable hashes" predicate would
-- duplicate logic that lives in Go and drift from it.

-- artists.path is NOT NULL, so "" is the only spelling of "no local directory"
-- (that is exactly what the old checkers tested: a.Path == "").
--
-- Deleting the rows is only half the job: the artists have to be RE-EVALUATED,
-- or the delete freezes their health score. artist.ListDirtyIDs is the dirty set
-- the default (incremental) rule pass walks, and an artist is dirty when
-- rules_evaluated_at IS NULL, or dirty_since is later than it, or an ENABLED
-- rule was updated after it. This migration satisfies none of those on its own:
-- it touches neither rules.updated_at nor artists.dirty_since. So a path-less
-- artist that IS capable under the new gate (no directory, but two or more
-- comparable stored hashes) would be left with the duplicate rules eligible, no
-- rule_results rows for them, and no way back into the dirty set --
-- Pipeline.offlineHealthScore counts the missing rows and refuses to rescore,
-- and the score stops updating until someone forces a full re-evaluation. That
-- is precisely the "health quietly stops updating" failure the capability gate
-- exists to prevent.
--
-- Clearing rules_evaluated_at puts an artist back on the never-evaluated branch
-- of ListDirtyIDs, so the next incremental pass re-walks it and repopulates its
-- rows honestly (or, for an incapable artist, leaves them absent, which is the
-- honest state). It runs BEFORE the DELETE because afterwards there is no row
-- left to identify the affected artists by. The cost is one extra full evaluation
-- for those artists and no more: the pass stamps rules_evaluated_at again on the
-- way out.
--
-- The UPDATE is deliberately NOT scoped to is_excluded = 0 AND locked = 0, even
-- though ListDirtyIDs requires both. Scoping it that way strands the artist.
--
-- ListDirtyIDs returns an artist when is_excluded = 0 AND locked = 0 AND one of
-- its freshness branches holds, the first of which is rules_evaluated_at IS NULL.
-- Setting the column NULL while the artist is still locked or excluded schedules
-- nothing today (the pipeline skips it by design), but it arms the first branch
-- so the artist is re-walked the moment it is unlocked or re-included.
--
-- Leaving the column intact instead is what would strand it: unlocking does NOT
-- stamp dirty_since. SetLock (internal/artist/sqlite_artist.go) updates only
-- locked, lock_source, locked_at and updated_at, so an unlocked artist whose rows
-- this migration deleted would satisfy no freshness branch, would never be
-- re-evaluated, and would hold a frozen health score indefinitely.

UPDATE artists
SET rules_evaluated_at = NULL
WHERE path = ''
  AND EXISTS (
    SELECT 1 FROM rule_results
    WHERE rule_results.artist_id = artists.id
      AND rule_results.rule_id IN ('image_duplicate', 'image_duplicate_exact')
  );

DELETE FROM rule_results
WHERE rule_id IN ('image_duplicate', 'image_duplicate_exact')
  AND artist_id IN (SELECT id FROM artists WHERE path = '');

-- +goose Down
-- Irreversible by design, and harmless: the deleted rows asserted a pass that
-- never happened. Re-creating them would re-introduce the bug. The next
-- evaluation of each artist writes whatever the rules genuinely find.
SELECT 1;
