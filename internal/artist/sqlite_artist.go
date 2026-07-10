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

// fieldColumnMap maps API field names to database column names for fields
// stored directly in the artists table.
var fieldColumnMap = map[string]string{
	"biography":      "biography",
	"genres":         "genres",
	"styles":         "styles",
	"moods":          "moods",
	"formed":         "formed",
	"born":           "born",
	"disbanded":      "disbanded",
	"died":           "died",
	"years_active":   "years_active",
	"type":           "type",
	"gender":         "gender",
	"origin":         "origin",
	"name":           "name",
	"sort_name":      "sort_name",
	"disambiguation": "disambiguation",
}

// providerFieldMap maps API field names that correspond to provider IDs stored
// in the artist_provider_ids normalized table.
var providerFieldMap = map[string]string{
	"musicbrainz_id": "musicbrainz",
	"audiodb_id":     "audiodb",
	"discogs_id":     "discogs",
	"wikidata_id":    "wikidata",
	"deezer_id":      "deezer",
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

// DB satisfies the dbProvider interface used by Service.hydratePrimaryLibrary
// (and its batch sibling) so wrapped repositories that embed *sqliteArtistRepo
// continue to expose the underlying handle without a concrete type assertion.
func (r *sqliteArtistRepo) DB() *sql.DB {
	return r.db
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
			id, name, sort_name, type, gender, origin, disambiguation,
			genres, styles, moods,
			years_active, born, formed, died, disbanded, biography,
			path, nfo_exists,
			health_score, is_excluded, exclusion_reason, is_classical,
			locked, lock_source, locked_at, locked_fields,
			metadata_sources,
			last_scanned_at, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		a.ID, a.Name, a.SortName, a.Type, a.Gender, a.Origin, a.Disambiguation,
		MarshalStringSlice(a.Genres), MarshalStringSlice(a.Styles), MarshalStringSlice(a.Moods),
		a.YearsActive, a.Born, a.Formed, a.Died, a.Disbanded, a.Biography,
		a.Path, dbutil.BoolToInt(a.NFOExists),
		a.HealthScore, dbutil.BoolToInt(a.IsExcluded), a.ExclusionReason, dbutil.BoolToInt(a.IsClassical),
		dbutil.BoolToInt(a.Locked), a.LockSource, dbutil.FormatNullableTime(a.LockedAt),
		MarshalStringSlice(a.LockedFields),
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

// GetByName performs case-insensitive exact-name lookup with no library
// scope. Used by connection populate paths to dedupe across all
// libraries; replaces the older GetByNameAndLibrary call site.
func (r *sqliteArtistRepo) GetByName(ctx context.Context, name string) (*Artist, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT `+artistColumns+` FROM artists WHERE LOWER(name) = LOWER(?) LIMIT 1`, name)
	a, err := scanArtist(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting artist by name: %w", err)
	}
	return a, nil
}

// FindByMBIDOrNameUnscoped tries MBID first then case-insensitive name,
// returning the first match without any library scope. This is the dedupe
// helper for the unscoped populate path.
func (r *sqliteArtistRepo) FindByMBIDOrNameUnscoped(ctx context.Context, mbid, name string) (*Artist, error) {
	if mbid != "" {
		a, err := r.GetByMBID(ctx, mbid)
		if err != nil {
			return nil, fmt.Errorf("finding by mbid (unscoped): %w", err)
		}
		if a != nil {
			return a, nil
		}
	}
	if name == "" {
		return nil, nil
	}
	return r.GetByName(ctx, name)
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
	defer rows.Close() //nolint:errcheck // Close error not actionable on cleanup

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

// Count returns the number of artists matching the given filter parameters
// using a lightweight SELECT COUNT(*) query. It reuses buildWhereClause for
// consistent filtering with List.
func (r *sqliteArtistRepo) Count(ctx context.Context, params CountParams) (int, error) {
	lp := params.toListParams()
	lp.Validate()

	where, args := buildWhereClause(lp)
	var total int
	if err := r.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM artists"+where, args...).Scan(&total); err != nil {
		return 0, fmt.Errorf("counting artists: %w", err)
	}
	return total, nil
}

// ListIDs returns the IDs of all artists that match the given filters,
// ordered by sort_name, id for a stable sequence. A separate COUNT(*) query
// provides the true total (so the UI can show "500 matching" even when the
// ID list is capped). When there are more than MaxListIDs matches, only the
// first MaxListIDs IDs are returned and capped is true.
func (r *sqliteArtistRepo) ListIDs(ctx context.Context, params CountParams) ([]string, int, error) {
	// Convert to ListParams so buildWhereClause can be reused. Page/PageSize
	// values are irrelevant because this query does not paginate.
	lp := params.toListParams()
	lp.Validate()

	where, args := buildWhereClause(lp)

	// Fetch the true total count first.
	var total int
	if err := r.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM artists"+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("counting artists for ListIDs: %w", err)
	}

	// Request MaxListIDs IDs ordered stably by sort_name, then id as the
	// tiebreaker. The ORDER BY mirrors the standard artist list sort so
	// selection order feels consistent with what the user sees on-screen.
	//nolint:gosec // ORDER BY clause is hard-coded, not user-supplied.
	query := "SELECT id FROM artists" + where + " ORDER BY sort_name, id LIMIT ?"
	args = append(args, MaxListIDs)

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("listing artist IDs: %w", err)
	}
	defer rows.Close() //nolint:errcheck // Close error not actionable on cleanup

	ids := make([]string, 0, MaxListIDs)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, 0, fmt.Errorf("scanning artist ID: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterating artist ID rows: %w", err)
	}

	return ids, total, nil
}

func (r *sqliteArtistRepo) Update(ctx context.Context, a *Artist) error {
	a.UpdatedAt = time.Now().UTC()

	// dirty_since and rules_evaluated_at are intentionally excluded from this
	// UPDATE: they are owned by MarkDirty/MarkRulesEvaluated and racing with
	// concurrent event-driven dirty marks would silently lose mutations.
	// The Artist struct still carries these fields for read-side consumers,
	// but write-side ownership lives in the targeted helpers.
	_, err := r.db.ExecContext(ctx, `
		UPDATE artists SET
			name = ?, sort_name = ?, type = ?, gender = ?, origin = ?, disambiguation = ?,
			genres = ?, styles = ?, moods = ?,
			years_active = ?, born = ?, formed = ?, died = ?, disbanded = ?, biography = ?,
			path = ?, nfo_exists = ?,
			health_score = ?, health_evaluated_at = ?, is_excluded = ?, exclusion_reason = ?, is_classical = ?,
			locked = ?, lock_source = ?, locked_at = ?, locked_fields = ?,
			metadata_sources = ?,
			last_scanned_at = ?, updated_at = ?
		WHERE id = ?
	`,
		a.Name, a.SortName, a.Type, a.Gender, a.Origin, a.Disambiguation,
		MarshalStringSlice(a.Genres), MarshalStringSlice(a.Styles), MarshalStringSlice(a.Moods),
		a.YearsActive, a.Born, a.Formed, a.Died, a.Disbanded, a.Biography,
		a.Path, dbutil.BoolToInt(a.NFOExists),
		a.HealthScore, dbutil.FormatNullableTime(a.HealthEvaluatedAt), dbutil.BoolToInt(a.IsExcluded), a.ExclusionReason, dbutil.BoolToInt(a.IsClassical),
		dbutil.BoolToInt(a.Locked), a.LockSource, dbutil.FormatNullableTime(a.LockedAt),
		MarshalStringSlice(a.LockedFields),
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
		"UPDATE artists SET "+col+" = ?, updated_at = ? WHERE id = ?",
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
		"UPDATE artists SET "+col+" = ?, updated_at = ? WHERE id = ?",
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

// ListRefsByLibrary returns lightweight (id, name, path) records for every
// artist whose membership includes libraryID and whose path is non-empty.
// Single-query equivalent of paginating the full List output when callers
// only need basic identifying fields (e.g. the scanner's per-library
// removal sweep, #1409). The returned slice's order is the natural row
// order from SQLite; callers MUST NOT rely on a particular sort.
func (r *sqliteArtistRepo) ListRefsByLibrary(ctx context.Context, libraryID string) ([]ArtistRef, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT a.id, a.name, a.path FROM artists a
		JOIN artist_libraries al ON al.artist_id = a.id
		WHERE al.library_id = ? AND a.path != ''`,
		libraryID)
	if err != nil {
		return nil, fmt.Errorf("listing artist refs for library %s: %w", libraryID, err)
	}
	defer rows.Close() //nolint:errcheck // Close error not actionable on cleanup

	var result []ArtistRef
	for rows.Next() {
		var ref ArtistRef
		if err := rows.Scan(&ref.ID, &ref.Name, &ref.Path); err != nil {
			return nil, fmt.Errorf("scanning artist ref: %w", err)
		}
		result = append(result, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating artist refs for library %s: %w", libraryID, err)
	}
	return result, nil
}

// ListMBIDPaths returns one (MBID, path) record per artist that has both a
// non-empty MusicBrainz ID and a non-empty path. The MBID lives in
// artist_provider_ids (provider='musicbrainz'), so the join filters on that
// provider and its provider_id, and the artists.path column supplies the host
// path. Rows missing either side are excluded so callers never observe a blank
// key. Order is not guaranteed.
func (r *sqliteArtistRepo) ListMBIDPaths(ctx context.Context) ([]MBIDPath, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT p.provider_id, a.path FROM artists a
		JOIN artist_provider_ids p ON p.artist_id = a.id
		WHERE p.provider = 'musicbrainz' AND p.provider_id != '' AND a.path != ''`)
	if err != nil {
		return nil, fmt.Errorf("listing artist MBID paths: %w", err)
	}
	defer rows.Close() //nolint:errcheck // Close error not actionable on cleanup

	var result []MBIDPath
	for rows.Next() {
		var mp MBIDPath
		if err := rows.Scan(&mp.MBID, &mp.Path); err != nil {
			return nil, fmt.Errorf("scanning artist MBID path: %w", err)
		}
		result = append(result, mp)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating artist MBID paths: %w", err)
	}
	return result, nil
}

// ListByIDs returns the artist rows matching ids in a single query. Empty
// input yields an empty slice with no DB hit. The IN-clause is built by
// joining "?" literals so static-analysis tools can confirm no user input
// flows into the SQL string; bound parameters carry every value. Order
// of the returned rows is SQLite's natural order -- callers needing a
// specific order should reconstruct it from a map keyed by ID.
func (r *sqliteArtistRepo) ListByIDs(ctx context.Context, ids []string) ([]Artist, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	//nolint:gosec // G202: placeholders is a literal "?,?,..." string built by joining "?" literals; no user input.
	query := `SELECT ` + artistColumns + ` FROM artists WHERE id IN (` + strings.Join(placeholders, ",") + `)`
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("listing artists by IDs: %w", err)
	}
	defer rows.Close() //nolint:errcheck // Close error not actionable on cleanup

	result := make([]Artist, 0, len(ids))
	for rows.Next() {
		a, err := scanArtist(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning artist row: %w", err)
		}
		result = append(result, *a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating artist rows: %w", err)
	}
	return result, nil
}

// ListByLibrary returns the full artist rows whose membership includes
// libraryID. Single query equivalent of paginating List with LibraryID set.
// Used by the scanner's per-directory pre-load (#1411) so processDirectory
// can read existing artists out of an in-memory map keyed by path instead
// of issuing N GetByPath round-trips. Membership is resolved through
// artist_libraries; rows with no membership are excluded.
func (r *sqliteArtistRepo) ListByLibrary(ctx context.Context, libraryID string) ([]Artist, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+artistColumns+` FROM artists
		WHERE EXISTS (
			SELECT 1 FROM artist_libraries al
			WHERE al.artist_id = artists.id AND al.library_id = ?
		)`,
		libraryID)
	if err != nil {
		return nil, fmt.Errorf("listing artists for library %s: %w", libraryID, err)
	}
	defer rows.Close() //nolint:errcheck // Close error not actionable on cleanup

	var result []Artist
	for rows.Next() {
		a, err := scanArtist(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning artist row: %w", err)
		}
		result = append(result, *a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating artist rows for library %s: %w", libraryID, err)
	}
	return result, nil
}

// ListPathsByLibrary returns a map of artist ID to filesystem path for all
// artists in the given library that have a non-empty path. Uses artist ID
// as the key (not name) to avoid collisions when multiple artists share
// the same name. Membership is resolved through artist_libraries (the
// authoritative M:N table) since the legacy artists.library_id column
// was dropped in migration 004.
func (r *sqliteArtistRepo) ListPathsByLibrary(ctx context.Context, libraryID string) (map[string]string, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT a.id, a.path FROM artists a
		JOIN artist_libraries al ON al.artist_id = a.id
		WHERE al.library_id = ? AND a.path != ''`,
		libraryID)
	if err != nil {
		return nil, fmt.Errorf("listing artist paths for library %s: %w", libraryID, err)
	}
	defer rows.Close() //nolint:errcheck // Close error not actionable on cleanup

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
	escaped := dbutil.EscapeLike(query)
	pattern := "%" + escaped + "%"
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+artistColumns+` FROM artists WHERE name LIKE ? ESCAPE '\' ORDER BY name LIMIT 20`, pattern)
	if err != nil {
		return nil, fmt.Errorf("searching artists: %w", err)
	}
	defer rows.Close() //nolint:errcheck // Close error not actionable on cleanup

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

// SetLockedFields replaces the set of locked field names for an artist.
// Field names are normalized to lowercase unique tokens before being persisted
// as a JSON array. Pass an empty slice to clear all field locks.
func (r *sqliteArtistRepo) SetLockedFields(ctx context.Context, id string, fields []string) error {
	normalized := normalizeLockedFields(fields)
	now := time.Now().UTC().Format(time.RFC3339)
	result, err := r.db.ExecContext(ctx,
		`UPDATE artists SET locked_fields = ?, updated_at = ? WHERE id = ?`,
		MarshalStringSlice(normalized), now, id,
	)
	if err != nil {
		return fmt.Errorf("setting locked fields for artist %s: %w", id, err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("reading locked_fields rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return nil
}

// normalizeLockedFields trims, lowercases, and deduplicates locked field
// entries while preserving their first-seen order. Empty tokens are dropped.
func normalizeLockedFields(fields []string) []string {
	seen := make(map[string]struct{}, len(fields))
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.TrimSpace(strings.ToLower(f))
		if f == "" {
			continue
		}
		if _, ok := seen[f]; ok {
			continue
		}
		seen[f] = struct{}{}
		out = append(out, f)
	}
	return out
}

// ErrAlreadyLocked is returned when trying to lock an already-locked artist.
var ErrAlreadyLocked = errors.New("artist is already locked")

// ErrNotLocked is returned when trying to unlock an artist that is not locked.
var ErrNotLocked = errors.New("artist is not locked")

func (r *sqliteArtistRepo) SetLock(ctx context.Context, id string, locked bool, source string) error {
	var lockedAt any
	if locked {
		lockedAt = time.Now().UTC().Format(time.RFC3339)
	}

	// Use a WHERE precondition on locked state to prevent TOCTOU races.
	// If the artist is already in the target state, 0 rows are affected.
	wantPrior := 1 // default: must be locked to unlock
	if locked {
		wantPrior = 0 // must be unlocked to lock
	}

	result, err := r.db.ExecContext(ctx, `
		UPDATE artists SET locked = ?, lock_source = ?, locked_at = ?, updated_at = ?
		WHERE id = ? AND locked = ?
	`, dbutil.BoolToInt(locked), source, lockedAt, time.Now().UTC().Format(time.RFC3339), id, wantPrior)
	if err != nil {
		return fmt.Errorf("setting artist lock: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("reading lock transition rows affected: %w", err)
	}
	if n == 0 {
		// Distinguish "not found" from "already in target state".
		var exists int
		if err := r.db.QueryRowContext(ctx, `SELECT 1 FROM artists WHERE id = ?`, id).Scan(&exists); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: %s", ErrNotFound, id)
			}
			return fmt.Errorf("checking artist existence for lock transition: %w", err)
		}
		if locked {
			return ErrAlreadyLocked
		}
		return ErrNotLocked
	}
	return nil
}
