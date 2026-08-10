package artist

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// SQLite implementation of the MusicBrainz ID re-validation ledger (#2810).
// The vocabulary and the model live in mbid_validation.go; the interface is in
// repository.go.
//
// # WHY NO ORPHAN-CLEANUP ENTRY WAS ADDED FOR THIS TABLE
//
// Migrations 009 and 019 sweep artist-scoped child tables by hand. Neither
// needs a mbid_validation entry, and adding one would be dead code, for two
// separate reasons -- either alone is sufficient.
//
// First, both are ONE-TIME REPAIRS of damage that predates this table. 009
// (009_orphan_artist_cleanup.sql:2) collects orphan ARTIST rows that
// accumulated before Service.Delete learned to prune them inline; 019
// (019_general_orphan_cleanup.sql:2-11) collects orphan CHILD rows left behind
// by the #2272 FK-enforcement defect, where PRAGMA foreign_keys reverted OFF
// on recycled pool connections so the CASCADE silently no-opped. Both run once
// against history. mbid_validation is created by migration 028, AFTER both,
// and after the #2272 fix, so it can never hold a row either sweep could see.
//
// Second, the mechanism those sweeps compensated for is fixed. The runtime
// pool is opened by database.OpenRuntime, whose DSN carries foreign_keys(1)
// (internal/database/database.go:42-43), so every connection the pool hands
// out -- including recycled and freshly reopened ones -- enforces foreign
// keys. database.VerifyForeignKeys re-checks that on a pool connection at
// startup WITHOUT mutating it (database.go:132-158), so a driver or DSN
// regression fails loudly at boot rather than silently orphaning rows.
// database.TestRuntimeFKCascadeSurvivesConnectionChurn
// (internal/database/fk_cascade_test.go:30) forces connection churn and
// asserts the cascade still fires.
//
// So the ON DELETE CASCADE on mbid_validation.artist_id is sufficient, and a
// hand-written sweep entry would be a second, redundant mechanism that could
// only drift from the first. TestMBIDValidation_CascadeDeleteWithArtist pins
// the cascade directly.
//
// One nuance worth stating because it looks like a hole and is not: goose runs
// migrations on a database.Open handle, which is FK-OFF (database.go:23-25),
// so a future migration that deletes artist rows would NOT cascade into this
// table and would have to delete from it explicitly, exactly as 009 does for
// its own child list. That is a rule for writing future migrations, not a gap
// in runtime behavior.

type sqliteMBIDValidationRepo struct {
	db *sql.DB
}

// newSQLiteMBIDValidationRepo returns the SQLite-backed ledger repository.
func newSQLiteMBIDValidationRepo(db *sql.DB) MBIDValidationRepository {
	return &sqliteMBIDValidationRepo{db: db}
}

// mbidValidationColumns is the shared SELECT list, defined once so the row
// scanner and every query cannot drift apart in column order.
const mbidValidationColumns = `artist_id, mbid, outcome, reason, detail,
	resolved_name, catalogue_match_percent, checked_at`

// Upsert writes one artist's verdict, replacing any prior one.
//
// The ON CONFLICT target is artist_id, the table's primary key, which is what
// makes this idempotent: a sweep that runs twice updates the same row twice
// instead of accumulating a second verdict for the same artist. Every column
// is overwritten on conflict, deliberately -- a re-check supersedes the old
// verdict wholesale, and carrying forward a stale ResolvedName or match
// percent alongside a fresh outcome would present evidence that does not
// belong to the verdict shown beside it.
func (r *sqliteMBIDValidationRepo) Upsert(ctx context.Context, v *MBIDValidation) error {
	if v == nil {
		return errors.New("upserting mbid validation: nil record")
	}
	// Fail at the call site with a named error rather than as an opaque
	// SQLite constraint violation. The schema's CHECK constraints remain the
	// authority; this is only a better error message.
	if err := v.Validate(); err != nil {
		return err
	}

	checkedAt := v.CheckedAt
	if checkedAt.IsZero() {
		checkedAt = time.Now()
	}

	const q = `
		INSERT INTO mbid_validation (` + mbidValidationColumns + `)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(artist_id) DO UPDATE SET
			mbid                    = excluded.mbid,
			outcome                 = excluded.outcome,
			reason                  = excluded.reason,
			detail                  = excluded.detail,
			resolved_name           = excluded.resolved_name,
			catalogue_match_percent = excluded.catalogue_match_percent,
			checked_at              = excluded.checked_at`

	// CatalogueMatchPercent is bound as a nullable: nil becomes SQL NULL,
	// meaning "not measured". A measured 0 is a real and damning value and
	// must not collapse into the same cell -- see the field's comment.
	var pct any
	if v.CatalogueMatchPercent != nil {
		pct = *v.CatalogueMatchPercent
	}

	if _, err := r.db.ExecContext(ctx, q,
		v.ArtistID,
		v.MBID,
		v.Outcome,
		v.Reason,
		v.Detail,
		v.ResolvedName,
		pct,
		checkedAt.UTC().Format(time.RFC3339),
	); err != nil {
		return fmt.Errorf("upserting mbid validation for artist %s: %w", v.ArtistID, err)
	}
	return nil
}

// mbidValidationWhere builds the optional outcome narrowing. The value is
// bound, never interpolated, and Validate has already coerced anything
// unrecognized to "" (every outcome) before this is reached.
func mbidValidationWhere(f MBIDValidationFilter) (string, []any) {
	if f.Outcome == "" {
		return "", nil
	}
	return "WHERE outcome = ?", []any{f.Outcome}
}

// List returns a page of ledger rows, newest check first.
//
// artist_id is the ORDER BY tiebreaker so a page boundary is deterministic:
// without it, two rows sharing a checked_at could appear twice or not at all
// across consecutive pages. checked_at is stored RFC3339, so a plain TEXT sort
// is chronological.
func (r *sqliteMBIDValidationRepo) List(ctx context.Context, filter MBIDValidationFilter) ([]MBIDValidation, error) {
	filter.Validate()

	where, args := mbidValidationWhere(filter)
	//nolint:gosec // G202: every concatenated fragment is server-built. where is
	// one of two compile-time literals and emits only a "?" placeholder (its
	// value is bound below); the column list and the order clause are constants.
	// No caller-supplied text reaches the query string.
	q := `SELECT ` + mbidValidationColumns + `
		FROM mbid_validation
		` + where + `
		ORDER BY checked_at DESC, artist_id ASC
		LIMIT ? OFFSET ?`

	queryArgs := make([]any, 0, len(args)+2)
	queryArgs = append(queryArgs, args...)
	queryArgs = append(queryArgs, filter.Limit, filter.Offset)

	rows, err := r.db.QueryContext(ctx, q, queryArgs...)
	if err != nil {
		return nil, fmt.Errorf("querying mbid validation rows: %w", err)
	}
	defer rows.Close() //nolint:errcheck // Close error not actionable on cleanup

	out := make([]MBIDValidation, 0)
	for rows.Next() {
		v, scanErr := scanMBIDValidation(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, *v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating mbid validation rows: %w", err)
	}
	return out, nil
}

// Count returns how many rows match filter.Outcome. Limit and Offset are
// ignored: the count describes the whole result set so a caller can paginate,
// and a count that shrank with the page would be useless for that.
func (r *sqliteMBIDValidationRepo) Count(ctx context.Context, filter MBIDValidationFilter) (int, error) {
	filter.Validate()

	where, args := mbidValidationWhere(filter)
	q := `SELECT COUNT(*) FROM mbid_validation ` + where

	var total int
	if err := r.db.QueryRowContext(ctx, q, args...).Scan(&total); err != nil {
		return 0, fmt.Errorf("counting mbid validation rows: %w", err)
	}
	return total, nil
}

// GetByArtistID returns one artist's verdict.
//
// A missing row yields ErrMBIDValidationNotFound rather than a zero-valued
// record, because "never checked" is a genuinely different fact from any
// verdict and a caller must be able to tell them apart. Returning a zero
// MBIDValidation would read as an empty outcome that nothing in the vocabulary
// defines.
func (r *sqliteMBIDValidationRepo) GetByArtistID(ctx context.Context, artistID string) (*MBIDValidation, error) {
	const q = `SELECT ` + mbidValidationColumns + `
		FROM mbid_validation
		WHERE artist_id = ?`

	v, err := scanMBIDValidation(r.db.QueryRowContext(ctx, q, artistID))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: %s", ErrMBIDValidationNotFound, artistID)
		}
		return nil, err
	}
	return v, nil
}

// mbidValidationScanner is satisfied by both *sql.Row and *sql.Rows, so the
// single-row getter and the list loop share one scanner and cannot disagree
// about column order or NULL handling.
type mbidValidationScanner interface {
	Scan(dest ...any) error
}

// scanMBIDValidation reads one row in mbidValidationColumns order.
//
// It returns sql.ErrNoRows unwrapped when a *sql.Row is empty, so
// GetByArtistID can recognize it; every other failure is wrapped.
func scanMBIDValidation(s mbidValidationScanner) (*MBIDValidation, error) {
	var v MBIDValidation
	// NULL catalogue_match_percent means "not measured" and must stay
	// distinguishable from a measured 0.
	var pct sql.NullFloat64
	var checkedAt string

	if err := s.Scan(
		&v.ArtistID, &v.MBID, &v.Outcome, &v.Reason, &v.Detail,
		&v.ResolvedName, &pct, &checkedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("scanning mbid validation row: %w", err)
	}

	if pct.Valid {
		p := pct.Float64
		v.CatalogueMatchPercent = &p
	}
	v.CheckedAt = parseMBIDValidationTimestamp(v.ArtistID, checkedAt)
	return &v, nil
}

// parseMBIDValidationTimestamp parses a checked_at value.
//
// Upsert always writes RFC3339, but the column's schema DEFAULT
// (strftime('%Y-%m-%dT%H:%M:%SZ','now')) produces the same shape without an
// offset, and a row inserted by hand during an incident could be anything. A
// second attempt at SQLite's "YYYY-MM-DD HH:MM:SS" form covers that, and an
// unparsable value is reported rather than swallowed.
func parseMBIDValidationTimestamp(artistID, raw string) time.Time {
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.UTC()
	}
	if t, err := time.Parse(time.DateTime, raw); err == nil {
		return t.UTC()
	}
	// Deliberately the zero time, not time.Now(): a zero timestamp reads as
	// "unknown" to any caller, whereas a synthesized "now" would present a
	// fabricated check time as fact, which is the failure mode this whole
	// ledger exists to avoid.
	slog.Warn("unparsable checked_at in mbid_validation",
		"artist_id", artistID,
		"raw_value", raw,
	)
	return time.Time{}
}
