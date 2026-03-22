package artist

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sydlexius/stillwater/internal/dbutil"
)

// fieldColumnMap maps API field names to database column names.
var fieldColumnMap = map[string]string{
	"biography":    "biography",
	"genres":       "genres",
	"styles":       "styles",
	"moods":        "moods",
	"formed":       "formed",
	"born":         "born",
	"disbanded":    "disbanded",
	"died":         "died",
	"years_active": "years_active",
	"type":         "type",
	"gender":       "gender",
}

// sliceFields are fields that store JSON arrays in the database.
var sliceFields = map[string]bool{
	"genres": true,
	"styles": true,
	"moods":  true,
}

// splitTags splits a comma-separated string into trimmed non-empty values.
func splitTags(s string) []string {
	var tags []string
	for _, t := range strings.Split(s, ",") {
		t = strings.TrimSpace(t)
		if t != "" {
			tags = append(tags, t)
		}
	}
	return tags
}

type sqliteArtistRepo struct {
	db *sql.DB
}

func newSQLiteArtistRepo(db *sql.DB) *sqliteArtistRepo {
	return &sqliteArtistRepo{db: db}
}

func (r *sqliteArtistRepo) Create(ctx context.Context, a *Artist) error {
	if a.ID == "" {
		a.ID = uuid.New().String()
	}
	now := time.Now().UTC()
	a.CreatedAt = now
	a.UpdatedAt = now

	_, err := r.db.ExecContext(ctx, `
		INSERT INTO artists (
			id, name, sort_name, type, gender, disambiguation,
			genres, styles, moods,
			years_active, born, formed, died, disbanded, biography,
			path, library_id, nfo_exists,
			health_score, is_excluded, exclusion_reason, is_classical, metadata_sources,
			last_scanned_at, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		a.ID, a.Name, a.SortName, a.Type, a.Gender, a.Disambiguation,
		MarshalStringSlice(a.Genres), MarshalStringSlice(a.Styles), MarshalStringSlice(a.Moods),
		a.YearsActive, a.Born, a.Formed, a.Died, a.Disbanded, a.Biography,
		a.Path, dbutil.NullableString(a.LibraryID), dbutil.BoolToInt(a.NFOExists),
		a.HealthScore, dbutil.BoolToInt(a.IsExcluded), a.ExclusionReason, dbutil.BoolToInt(a.IsClassical),
		MarshalStringMap(a.MetadataSources),
		dbutil.FormatNullableTime(a.LastScannedAt),
		now.Format(time.RFC3339), now.Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("creating artist: %w", err)
	}
	return nil
}

func (r *sqliteArtistRepo) GetByID(ctx context.Context, id string) (*Artist, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+artistColumns+` FROM artists WHERE id = ?`, id)
	a, err := scanArtist(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("getting artist by id: %w", err)
	}
	return a, nil
}

func (r *sqliteArtistRepo) GetByMBID(ctx context.Context, mbid string) (*Artist, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT `+artistColumns+` FROM artists
		WHERE id = (SELECT artist_id FROM artist_provider_ids WHERE provider = 'musicbrainz' AND provider_id = ? LIMIT 1)`, mbid)
	a, err := scanArtist(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting artist by mbid: %w", err)
	}
	return a, nil
}

func (r *sqliteArtistRepo) GetByMBIDAndLibrary(ctx context.Context, mbid, libraryID string) (*Artist, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT `+artistColumns+` FROM artists
		WHERE id IN (SELECT artist_id FROM artist_provider_ids WHERE provider = 'musicbrainz' AND provider_id = ?)
		AND library_id = ?`, mbid, libraryID)
	a, err := scanArtist(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting artist by mbid+library: %w", err)
	}
	return a, nil
}

func (r *sqliteArtistRepo) GetByNameAndLibrary(ctx context.Context, name, libraryID string) (*Artist, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT `+artistColumns+` FROM artists WHERE name = ? AND library_id = ?`, name, libraryID)
	a, err := scanArtist(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting artist by name+library: %w", err)
	}
	return a, nil
}

func (r *sqliteArtistRepo) FindByMBIDOrName(ctx context.Context, mbid, name, libraryID string) (*Artist, error) {
	// Try MBID match first (most reliable).
	if mbid != "" {
		row := r.db.QueryRowContext(ctx,
			`SELECT `+artistColumns+` FROM artists
			WHERE id IN (SELECT artist_id FROM artist_provider_ids WHERE provider = 'musicbrainz' AND provider_id = ?)
			AND library_id = ?`, mbid, libraryID)
		a, err := scanArtist(row)
		if err == nil {
			return a, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("finding artist by mbid+library: %w", err)
		}
		// No MBID match -- fall through to name match.
	}

	// Fall back to case-insensitive name match.
	row := r.db.QueryRowContext(ctx,
		`SELECT `+artistColumns+` FROM artists WHERE LOWER(name) = LOWER(?) AND library_id = ?`, name, libraryID)
	a, err := scanArtist(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("finding artist by name+library: %w", err)
	}
	return a, nil
}

func (r *sqliteArtistRepo) GetByPath(ctx context.Context, path string) (*Artist, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT `+artistColumns+` FROM artists WHERE path = ?`, path)
	a, err := scanArtist(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting artist by path: %w", err)
	}
	return a, nil
}

func (r *sqliteArtistRepo) List(ctx context.Context, params ListParams) ([]Artist, int, error) {
	params.Validate()

	where, args := buildWhereClause(params)

	var total int
	countQuery := "SELECT COUNT(*) FROM artists" + where
	if err := r.db.QueryRowContext(ctx, countQuery, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("counting artists: %w", err)
	}

	offset := (params.Page - 1) * params.PageSize
	query := `SELECT ` + artistColumns + ` FROM artists` + where + //nolint:gosec // validatedOrderClause uses allowlist; safe
		` ORDER BY ` + validatedOrderClause(params) +
		` LIMIT ? OFFSET ?`
	args = append(args, params.PageSize, offset)

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("listing artists: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var artists []Artist
	for rows.Next() {
		a, err := scanArtist(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("scanning artist row: %w", err)
		}
		artists = append(artists, *a)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterating artist rows: %w", err)
	}

	return artists, total, nil
}

func (r *sqliteArtistRepo) Update(ctx context.Context, a *Artist) error {
	a.UpdatedAt = time.Now().UTC()

	_, err := r.db.ExecContext(ctx, `
		UPDATE artists SET
			name = ?, sort_name = ?, type = ?, gender = ?, disambiguation = ?,
			genres = ?, styles = ?, moods = ?,
			years_active = ?, born = ?, formed = ?, died = ?, disbanded = ?, biography = ?,
			path = ?, library_id = ?, nfo_exists = ?,
			health_score = ?, is_excluded = ?, exclusion_reason = ?, is_classical = ?,
			metadata_sources = ?,
			last_scanned_at = ?, updated_at = ?
		WHERE id = ?
	`,
		a.Name, a.SortName, a.Type, a.Gender, a.Disambiguation,
		MarshalStringSlice(a.Genres), MarshalStringSlice(a.Styles), MarshalStringSlice(a.Moods),
		a.YearsActive, a.Born, a.Formed, a.Died, a.Disbanded, a.Biography,
		a.Path, dbutil.NullableString(a.LibraryID), dbutil.BoolToInt(a.NFOExists),
		a.HealthScore, dbutil.BoolToInt(a.IsExcluded), a.ExclusionReason, dbutil.BoolToInt(a.IsClassical),
		MarshalStringMap(a.MetadataSources),
		dbutil.FormatNullableTime(a.LastScannedAt),
		a.UpdatedAt.Format(time.RFC3339),
		a.ID,
	)
	if err != nil {
		return fmt.Errorf("updating artist: %w", err)
	}
	return nil
}

func (r *sqliteArtistRepo) UpdateField(ctx context.Context, id, field, value string) error {
	col, ok := fieldColumnMap[field]
	if !ok {
		return fmt.Errorf("unknown field: %s", field)
	}

	dbValue := value
	if sliceFields[field] {
		dbValue = MarshalStringSlice(splitTags(value))
	}

	now := time.Now().UTC().Format(time.RFC3339)
	_, err := r.db.ExecContext(ctx,
		"UPDATE artists SET "+col+" = ?, updated_at = ? WHERE id = ?", //nolint:gosec // col is from validated map
		dbValue, now, id,
	)
	if err != nil {
		return fmt.Errorf("updating field %s: %w", field, err)
	}
	return nil
}

func (r *sqliteArtistRepo) ClearField(ctx context.Context, id, field string) error {
	col, ok := fieldColumnMap[field]
	if !ok {
		return fmt.Errorf("unknown field: %s", field)
	}

	zeroValue := ""
	if sliceFields[field] {
		zeroValue = "[]"
	}

	now := time.Now().UTC().Format(time.RFC3339)
	_, err := r.db.ExecContext(ctx,
		"UPDATE artists SET "+col+" = ?, updated_at = ? WHERE id = ?", //nolint:gosec // col is from validated map
		zeroValue, now, id,
	)
	if err != nil {
		return fmt.Errorf("clearing field %s: %w", field, err)
	}
	return nil
}

func (r *sqliteArtistRepo) Delete(ctx context.Context, id string) error {
	result, err := r.db.ExecContext(ctx, `DELETE FROM artists WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("deleting artist: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("artist not found: %s", id)
	}
	return nil
}

// ListPathsByLibrary returns a map of artist ID to filesystem path for all
// artists in the given library that have a non-empty path. Uses artist ID
// as the key (not name) to avoid collisions when multiple artists share
// the same name.
func (r *sqliteArtistRepo) ListPathsByLibrary(ctx context.Context, libraryID string) (map[string]string, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, path FROM artists WHERE library_id = ? AND path != ''`,
		libraryID)
	if err != nil {
		return nil, fmt.Errorf("listing artist paths for library %s: %w", libraryID, err)
	}
	defer rows.Close() //nolint:errcheck

	result := make(map[string]string)
	for rows.Next() {
		var id, path string
		if err := rows.Scan(&id, &path); err != nil {
			return nil, fmt.Errorf("scanning artist path: %w", err)
		}
		result[id] = path
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating artist paths for library %s: %w", libraryID, err)
	}
	return result, nil
}

func (r *sqliteArtistRepo) Search(ctx context.Context, query string) ([]Artist, error) {
	pattern := "%" + query + "%"
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+artistColumns+` FROM artists WHERE name LIKE ? ORDER BY name LIMIT 20`, pattern)
	if err != nil {
		return nil, fmt.Errorf("searching artists: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var artists []Artist
	for rows.Next() {
		a, err := scanArtist(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning search result: %w", err)
		}
		artists = append(artists, *a)
	}
	return artists, rows.Err()
}
