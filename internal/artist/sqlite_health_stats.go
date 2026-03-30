package artist

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"time"
)

// HealthStatsResult holds aggregate health metrics for the dashboard endpoint.
type HealthStatsResult struct {
	TotalArtists     int     `json:"total_artists"`
	CompliantArtists int     `json:"compliant_artists"`
	Score            float64 `json:"score"`
	MissingNFO       int     `json:"missing_nfo"`
	MissingThumb     int     `json:"missing_thumb"`
	MissingFanart    int     `json:"missing_fanart"`
	MissingMBID      int     `json:"missing_mbid"`
}

// UpdateHealthScore sets the health_score and health_evaluated_at columns for the given artist.
func (r *sqliteArtistRepo) UpdateHealthScore(ctx context.Context, id string, score float64) error {
	now := time.Now().UTC().Format(time.RFC3339)
	result, err := r.db.ExecContext(ctx,
		`UPDATE artists SET health_score = ?, health_evaluated_at = ?, updated_at = ? WHERE id = ?`,
		score, now, now, id)
	if err != nil {
		return fmt.Errorf("updating health score: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return nil
}

// ListUnevaluatedIDs returns the IDs of non-excluded artists that have never
// been evaluated (health_evaluated_at IS NULL). This is used by the bootstrap
// process to identify artists needing initial health score calculation.
func (r *sqliteArtistRepo) ListUnevaluatedIDs(ctx context.Context) ([]string, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id FROM artists WHERE is_excluded = 0 AND health_evaluated_at IS NULL`)
	if err != nil {
		return nil, fmt.Errorf("querying unevaluated artists: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scanning unevaluated artist id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating unevaluated artists: %w", err)
	}
	return ids, nil
}

// MarkDirty sets dirty_since to the current UTC time for the given artist.
// This signals that the artist's data has changed and rules must be re-evaluated.
func (r *sqliteArtistRepo) MarkDirty(ctx context.Context, id string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	result, err := r.db.ExecContext(ctx,
		`UPDATE artists SET dirty_since = ? WHERE id = ?`,
		now, id)
	if err != nil {
		return fmt.Errorf("marking artist dirty: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return nil
}

// MarkAllDirty sets dirty_since to the current UTC time for all non-excluded
// artists, forcing a full re-evaluation on the next rule run.
func (r *sqliteArtistRepo) MarkAllDirty(ctx context.Context) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := r.db.ExecContext(ctx,
		`UPDATE artists SET dirty_since = ? WHERE is_excluded = 0`,
		now)
	if err != nil {
		return fmt.Errorf("marking all artists dirty: %w", err)
	}
	return nil
}

// SetRulesEvaluatedAt records the current UTC time as rules_evaluated_at for
// the given artist, marking it clean until its data changes again.
func (r *sqliteArtistRepo) SetRulesEvaluatedAt(ctx context.Context, id string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	result, err := r.db.ExecContext(ctx,
		`UPDATE artists SET rules_evaluated_at = ? WHERE id = ?`,
		now, id)
	if err != nil {
		return fmt.Errorf("setting rules_evaluated_at: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return nil
}

// ListDirtyIDs returns the IDs of non-excluded artists that need rule
// re-evaluation: those that have never been evaluated (rules_evaluated_at IS
// NULL) or whose data changed after the last evaluation (dirty_since >
// rules_evaluated_at).
func (r *sqliteArtistRepo) ListDirtyIDs(ctx context.Context) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id FROM artists
		WHERE is_excluded = 0
		  AND (rules_evaluated_at IS NULL OR (dirty_since IS NOT NULL AND dirty_since > rules_evaluated_at))
		ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("querying dirty artists: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scanning dirty artist id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating dirty artists: %w", err)
	}
	return ids, nil
}

// When libraryID is non-empty, only artists in that library are included.
// This runs a single indexed SQL query with LEFT JOINs instead of loading
// every artist into memory.
func (r *sqliteArtistRepo) HealthStats(ctx context.Context, libraryID string) (HealthStatsResult, error) {
	const baseQuery = `
SELECT
    COUNT(*)                                                                AS total_artists,
    SUM(CASE WHEN a.health_score >= 100 THEN 1 ELSE 0 END)                 AS compliant_artists,
    COALESCE(AVG(a.health_score), 100)                                      AS score,
    SUM(CASE WHEN a.nfo_exists = 0 THEN 1 ELSE 0 END)                      AS missing_nfo,
    SUM(CASE WHEN COALESCE(thumb.exists_flag, 0) = 0 THEN 1 ELSE 0 END)    AS missing_thumb,
    SUM(CASE WHEN COALESCE(fanart.exists_flag, 0) = 0 THEN 1 ELSE 0 END)   AS missing_fanart,
    SUM(CASE WHEN COALESCE(mbid.provider_id, '') = '' THEN 1 ELSE 0 END)    AS missing_mbid
FROM artists a
LEFT JOIN artist_images thumb   ON thumb.artist_id = a.id   AND thumb.image_type = 'thumb'   AND thumb.slot_index = 0
LEFT JOIN artist_images fanart  ON fanart.artist_id = a.id  AND fanart.image_type = 'fanart'  AND fanart.slot_index = 0
LEFT JOIN artist_provider_ids mbid ON mbid.artist_id = a.id AND mbid.provider = 'musicbrainz'
WHERE a.is_excluded = 0`

	var query string
	var args []any

	if libraryID != "" {
		query = baseQuery + " AND a.library_id = ?"
		args = []any{libraryID}
	} else {
		query = baseQuery
	}

	var hs HealthStatsResult
	var compliant, missingNFO, missingThumb, missingFanart, missingMBID sql.NullInt64
	var score sql.NullFloat64

	err := r.db.QueryRowContext(ctx, query, args...).Scan(
		&hs.TotalArtists,
		&compliant,
		&score,
		&missingNFO,
		&missingThumb,
		&missingFanart,
		&missingMBID,
	)
	if err != nil {
		return hs, fmt.Errorf("querying health stats: %w", err)
	}

	hs.CompliantArtists = int(compliant.Int64)
	hs.MissingNFO = int(missingNFO.Int64)
	hs.MissingThumb = int(missingThumb.Int64)
	hs.MissingFanart = int(missingFanart.Int64)
	hs.MissingMBID = int(missingMBID.Int64)

	if score.Valid {
		hs.Score = math.Round(score.Float64*10) / 10
	} else {
		hs.Score = 100.0
	}

	return hs, nil
}
