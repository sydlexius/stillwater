-- +goose Up
-- Issue #2810: the ledger that records whether a stored MusicBrainz ID was
-- ever CHECKED against MusicBrainz, and what the check found.
--
-- Nothing in Stillwater has ever validated a stored MBID beyond its UUID
-- shape. A shape check passes for any well-formed id, including one belonging
-- to a completely different act that happens to share the artist's name, so a
-- wrong id is today indistinguishable from a right one and propagates into
-- every fetch, NFO write and platform push as fact.
--
-- This migration adds the persistence only. The resolver that talks to
-- MusicBrainz and the sweep that drives it land separately; this table is what
-- they write into, and it is deliberately useful on its own -- an empty table
-- is an honest "nothing has been checked yet", which is exactly the state the
-- product is in today and has never been able to say.
--
-- ONE ROW PER ARTIST, AND artist_id IS THE PRIMARY KEY. The ledger holds the
-- LATEST verdict for an artist, not a history of verdicts, so the writer is an
-- upsert and re-running the sweep must never accumulate rows. Making artist_id
-- the primary key rather than adding a surrogate id plus a UNIQUE constraint
-- makes that structurally impossible to get wrong, and follows rule_results
-- (001_initial_schema.sql), which is keyed the same way for the same reason.
-- The per-write audit trail already exists elsewhere (metadata_changes), so
-- there is nothing to lose by collapsing here.
--
-- THE OUTCOMES, AND WHAT EACH ONE MEANS OPERATIONALLY.
--
--   validated      the id resolved to an artist whose name and release
--                  catalogue match the local one. Positive evidence, not an
--                  absence of evidence.
--
--   failed         the id resolved to something, and that something is not
--                  this artist. This is the case the issue exists to find.
--
--   not_checkable  Stillwater could not reach a verdict. NOT a pass. An id
--                  that does not resolve at all may be stale-but-correct (the
--                  artist was merged upstream in MusicBrainz), and a provider
--                  outage says nothing about the id whatsoever. Collapsing
--                  this into "validated" would be the "unknown rendered as
--                  clean" failure this whole area keeps producing.
--
-- WHY A NON-VALIDATED ROW MUST CARRY A REASON. The acceptance criteria state
-- that every checked artist gets validated, failed WITH A REASON, or
-- not-checkable WITH THE REASON. The CHECK constraint below enforces exactly
-- that and nothing more: a failed or not_checkable row with a blank reason is
-- rejected by the database, so no writer can record a verdict an operator
-- cannot act on. The reason-to-outcome PAIRING (which reasons belong to which
-- outcome) is deliberately NOT constrained here -- classification is the
-- resolver's job, and freezing its taxonomy in a schema check would make a
-- correct future refinement require a migration.
--
-- THE EVIDENCE COLUMNS. resolved_name and catalogue_match_percent are what let
-- an operator judge the verdict instead of trusting it. The motivating
-- production case is precisely why the catalogue number is stored: the name
-- matched EXACTLY (100% by any string measure) while the remote artist had no
-- music releases at all, so the name alone would have accepted it. The
-- catalogue overlap is the discriminator, and it is recorded so the operator
-- sees the number the verdict rests on.
--
-- catalogue_match_percent IS NULLABLE, AND THAT IS LOAD-BEARING. Zero percent
-- is a real, meaningful measurement -- it is the value the motivating case
-- produces, the strongest possible evidence of a wrong id. "We never got far
-- enough to measure" is a different fact entirely, and a NOT NULL DEFAULT 0
-- would render the two identical and turn the most damning evidence into the
-- default state of every unmeasured row. NULL means not measured.

-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS mbid_validation (
    -- The artist whose stored id was checked. Primary key: one verdict per
    -- artist, replaced in place on re-check. CASCADE so a deleted artist
    -- cannot leave a verdict behind pointing at nothing.
    artist_id               TEXT NOT NULL PRIMARY KEY
                                 REFERENCES artists(id) ON DELETE CASCADE,

    -- The MusicBrainz id that was actually checked, recorded rather than
    -- re-read at display time. If the artist's id changes after the check,
    -- the verdict describes the OLD id and this column is what says so.
    mbid                    TEXT NOT NULL,

    outcome                 TEXT NOT NULL
                                 CHECK (outcome IN ('validated', 'failed', 'not_checkable')),

    -- Machine-readable reason, empty only for a validated row. The enumerated
    -- values are the taxonomy the resolver classifies into.
    reason                  TEXT NOT NULL DEFAULT ''
                                 CHECK (reason IN (
                                     '',
                                     'resolves_to_different_artist',
                                     'name_mismatch',
                                     'catalogue_mismatch',
                                     'mbid_not_found',
                                     'provider_unavailable',
                                     'no_local_albums'
                                 )),

    -- Optional human-readable elaboration for the operator. Never parsed.
    detail                  TEXT NOT NULL DEFAULT '',

    -- Evidence: the artist name MusicBrainz returned for this id. Empty when
    -- the id did not resolve.
    resolved_name           TEXT NOT NULL DEFAULT '',

    -- Evidence: how much of the local album catalogue matched the remote
    -- release groups, 0-100. NULL means not measured -- see the header.
    catalogue_match_percent REAL,

    checked_at              TEXT NOT NULL
                                 DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),

    -- Every non-validated verdict carries a reason; a validated one carries
    -- none. See the header for why this is enforced and the pairing is not.
    CHECK (
        (outcome =  'validated' AND reason =  '') OR
        (outcome <> 'validated' AND reason <> '')
    )
);
-- +goose StatementEnd

-- Outcome is the operator's entry point: "show me the failures" is the whole
-- point of the ledger, and that filter must not scan the artist catalogue.
-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS idx_mbid_validation_outcome
    ON mbid_validation(outcome, checked_at DESC);
-- +goose StatementEnd

-- checked_at alone serves the unfiltered "what was checked recently" ordering
-- and, later, the sweep's "what is stalest" selection. The outcome index above
-- cannot serve either, because it does not lead with checked_at.
-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS idx_mbid_validation_checked_at
    ON mbid_validation(checked_at DESC);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_mbid_validation_checked_at;
-- +goose StatementEnd

-- +goose StatementBegin
DROP INDEX IF EXISTS idx_mbid_validation_outcome;
-- +goose StatementEnd

-- Dropping the table discards every verdict. That is correct for a rollback:
-- the rows are derived data, wholly reproducible by re-running the sweep, and
-- nothing else references them. No orphan-cleanup entry is added anywhere for
-- this table either -- see the migration note in sqlite_mbid_validation.go for
-- why the FK cascade alone is sufficient here.
-- +goose StatementBegin
DROP TABLE IF EXISTS mbid_validation;
-- +goose StatementEnd
