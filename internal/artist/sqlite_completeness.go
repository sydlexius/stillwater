package artist

import (
	"context"
	"database/sql"
	"fmt"
)

type sqliteCompletenessRepo struct {
	db *sql.DB
}

func newSQLiteCompletenessRepo(db *sql.DB) *sqliteCompletenessRepo {
	return &sqliteCompletenessRepo{db: db}
}

// GetCompletenessRows returns one row per non-excluded artist with the raw
// boolean flags required to compute per-field coverage. The artist type is
// included so the caller can apply type-aware applicability rules in Go.
func (r *sqliteCompletenessRepo) GetCompletenessRows(ctx context.Context, libraryID string) ([]CompletenessRow, error) {
	// Each image-type sub-select returns 1 when at least one active image
	// of that type exists for the artist, 0 otherwise.
	const baseQuery = `
SELECT
    a.id,
    a.name,
    a.type,
    COALESCE(a.library_id, '')  AS library_id,
    a.biography,
    a.genres,
    a.styles,
    a.moods,
    a.years_active,
    a.born,
    a.formed,
    a.died,
    a.disbanded,
    a.nfo_exists,
    CASE WHEN EXISTS (
        SELECT 1 FROM artist_provider_ids
        WHERE artist_id = a.id AND provider = 'musicbrainz' AND provider_id <> ''
    ) THEN 1 ELSE 0 END AS has_mbid,
    CASE WHEN EXISTS (
        SELECT 1 FROM artist_images
        WHERE artist_id = a.id AND image_type = 'thumb' AND exists_flag = 1
    ) THEN 1 ELSE 0 END AS has_thumb,
    CASE WHEN EXISTS (
        SELECT 1 FROM artist_images
        WHERE artist_id = a.id AND image_type = 'fanart' AND exists_flag = 1
    ) THEN 1 ELSE 0 END AS has_fanart,
    CASE WHEN EXISTS (
        SELECT 1 FROM artist_images
        WHERE artist_id = a.id AND image_type = 'logo' AND exists_flag = 1
    ) THEN 1 ELSE 0 END AS has_logo,
    CASE WHEN EXISTS (
        SELECT 1 FROM artist_images
        WHERE artist_id = a.id AND image_type = 'banner' AND exists_flag = 1
    ) THEN 1 ELSE 0 END AS has_banner
FROM artists a
WHERE a.is_excluded = 0`

	var query string
	var args []any

	if libraryID != "" {
		query = baseQuery + " AND a.library_id = ?"
		args = []any{libraryID}
	} else {
		query = baseQuery
	}

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying completeness rows: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var results []CompletenessRow
	for rows.Next() {
		var cr CompletenessRow
		var nfoInt, hasMBIDInt, hasThumbInt, hasFanartInt, hasLogoInt, hasBannerInt int

		if err := rows.Scan(
			&cr.ID, &cr.Name, &cr.Type, &cr.LibraryID,
			&cr.Biography, &cr.Genres, &cr.Styles, &cr.Moods, &cr.YearsActive,
			&cr.Born, &cr.Formed, &cr.Died, &cr.Disbanded,
			&nfoInt, &hasMBIDInt,
			&hasThumbInt, &hasFanartInt, &hasLogoInt, &hasBannerInt,
		); err != nil {
			return nil, fmt.Errorf("scanning completeness row: %w", err)
		}

		cr.NFOExists = nfoInt == 1
		cr.HasMBID = hasMBIDInt == 1
		cr.HasThumb = hasThumbInt == 1
		cr.HasFanart = hasFanartInt == 1
		cr.HasLogo = hasLogoInt == 1
		cr.HasBanner = hasBannerInt == 1

		results = append(results, cr)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating completeness rows: %w", err)
	}

	return results, nil
}

// GetLowestCompleteness returns up to limit non-excluded artists sorted by
// health_score ascending so callers can show the least-complete artists.
func (r *sqliteCompletenessRepo) GetLowestCompleteness(ctx context.Context, libraryID string, limit int) ([]LowestCompletenessArtist, error) {
	if limit <= 0 {
		limit = 10
	}

	const baseQuery = `
SELECT id, name, COALESCE(library_id, '') AS library_id, health_score
FROM artists
WHERE is_excluded = 0`

	var query string
	var args []any

	if libraryID != "" {
		query = baseQuery + " AND library_id = ? ORDER BY health_score ASC LIMIT ?"
		args = []any{libraryID, limit}
	} else {
		query = baseQuery + " ORDER BY health_score ASC LIMIT ?"
		args = []any{limit}
	}

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying lowest completeness: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var results []LowestCompletenessArtist
	for rows.Next() {
		var a LowestCompletenessArtist
		if err := rows.Scan(&a.ID, &a.Name, &a.LibraryID, &a.HealthScore); err != nil {
			return nil, fmt.Errorf("scanning lowest completeness row: %w", err)
		}
		results = append(results, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating lowest completeness rows: %w", err)
	}

	return results, nil
}
