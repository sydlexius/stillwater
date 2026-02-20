package artist

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// artistColumns is the ordered list of columns for SELECT queries.
const artistColumns = `id, name, sort_name, type, gender, disambiguation,
	musicbrainz_id, audiodb_id, discogs_id, wikidata_id,
	genres, styles, moods,
	years_active, born, formed, died, disbanded, biography,
	path, nfo_exists, thumb_exists, fanart_exists, logo_exists, banner_exists,
	health_score, is_excluded, exclusion_reason, is_classical,
	last_scanned_at, created_at, updated_at`

// Service provides artist and band member data operations.
type Service struct {
	db *sql.DB
}

// NewService creates an artist service.
func NewService(db *sql.DB) *Service {
	return &Service{db: db}
}

// Create inserts a new artist.
func (s *Service) Create(ctx context.Context, a *Artist) error {
	if a.ID == "" {
		a.ID = uuid.New().String()
	}
	now := time.Now().UTC()
	a.CreatedAt = now
	a.UpdatedAt = now

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO artists (
			id, name, sort_name, type, gender, disambiguation,
			musicbrainz_id, audiodb_id, discogs_id, wikidata_id,
			genres, styles, moods,
			years_active, born, formed, died, disbanded, biography,
			path, nfo_exists, thumb_exists, fanart_exists, logo_exists, banner_exists,
			health_score, is_excluded, exclusion_reason, is_classical,
			last_scanned_at, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		a.ID, a.Name, a.SortName, a.Type, a.Gender, a.Disambiguation,
		a.MusicBrainzID, a.AudioDBID, a.DiscogsID, a.WikidataID,
		MarshalStringSlice(a.Genres), MarshalStringSlice(a.Styles), MarshalStringSlice(a.Moods),
		a.YearsActive, a.Born, a.Formed, a.Died, a.Disbanded, a.Biography,
		a.Path, boolToInt(a.NFOExists), boolToInt(a.ThumbExists),
		boolToInt(a.FanartExists), boolToInt(a.LogoExists), boolToInt(a.BannerExists),
		a.HealthScore, boolToInt(a.IsExcluded), a.ExclusionReason, boolToInt(a.IsClassical),
		formatNullableTime(a.LastScannedAt),
		now.Format(time.RFC3339), now.Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("creating artist: %w", err)
	}
	return nil
}

// GetByID retrieves an artist by primary key.
func (s *Service) GetByID(ctx context.Context, id string) (*Artist, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+artistColumns+` FROM artists WHERE id = ?`, id)
	a, err := scanArtist(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("artist not found: %s", id)
	}
	if err != nil {
		return nil, fmt.Errorf("getting artist by id: %w", err)
	}
	return a, nil
}

// GetByMBID retrieves an artist by MusicBrainz ID.
func (s *Service) GetByMBID(ctx context.Context, mbid string) (*Artist, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+artistColumns+` FROM artists WHERE musicbrainz_id = ?`, mbid)
	a, err := scanArtist(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting artist by mbid: %w", err)
	}
	return a, nil
}

// GetByProviderID retrieves an artist by a provider-specific ID.
// Supported providers: "musicbrainz", "audiodb", "discogs", "wikidata".
func (s *Service) GetByProviderID(ctx context.Context, provider, id string) (*Artist, error) {
	var col string
	switch provider {
	case "musicbrainz":
		col = "musicbrainz_id"
	case "audiodb":
		col = "audiodb_id"
	case "discogs":
		col = "discogs_id"
	case "wikidata":
		col = "wikidata_id"
	default:
		return nil, fmt.Errorf("unknown provider: %s", provider)
	}

	row := s.db.QueryRowContext(ctx,
		`SELECT `+artistColumns+` FROM artists WHERE `+col+` = ?`, id) //nolint:gosec // G202: col is from validated switch, not user input
	a, err := scanArtist(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting artist by %s: %w", provider, err)
	}
	return a, nil
}

// GetByPath retrieves an artist by filesystem path.
func (s *Service) GetByPath(ctx context.Context, path string) (*Artist, error) {
	row := s.db.QueryRowContext(ctx,
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

// List returns a paginated list of artists and the total count.
func (s *Service) List(ctx context.Context, params ListParams) ([]Artist, int, error) {
	params.Validate()

	where, args := buildWhereClause(params)

	// Count total matching rows
	var total int
	countQuery := "SELECT COUNT(*) FROM artists" + where
	if err := s.db.QueryRowContext(ctx, countQuery, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("counting artists: %w", err)
	}

	// Fetch page
	orderCol := params.Sort
	if params.Order == "desc" {
		orderCol += " DESC"
	} else {
		orderCol += " ASC"
	}

	offset := (params.Page - 1) * params.PageSize
	query := `SELECT ` + artistColumns + ` FROM artists` + where + //nolint:gosec // G202: orderCol is from validated params, not user input
		` ORDER BY ` + orderCol +
		` LIMIT ? OFFSET ?`
	args = append(args, params.PageSize, offset)

	rows, err := s.db.QueryContext(ctx, query, args...)
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

// Update modifies an existing artist.
func (s *Service) Update(ctx context.Context, a *Artist) error {
	a.UpdatedAt = time.Now().UTC()

	_, err := s.db.ExecContext(ctx, `
		UPDATE artists SET
			name = ?, sort_name = ?, type = ?, gender = ?, disambiguation = ?,
			musicbrainz_id = ?, audiodb_id = ?, discogs_id = ?, wikidata_id = ?,
			genres = ?, styles = ?, moods = ?,
			years_active = ?, born = ?, formed = ?, died = ?, disbanded = ?, biography = ?,
			path = ?, nfo_exists = ?, thumb_exists = ?, fanart_exists = ?, logo_exists = ?, banner_exists = ?,
			health_score = ?, is_excluded = ?, exclusion_reason = ?, is_classical = ?,
			last_scanned_at = ?, updated_at = ?
		WHERE id = ?
	`,
		a.Name, a.SortName, a.Type, a.Gender, a.Disambiguation,
		a.MusicBrainzID, a.AudioDBID, a.DiscogsID, a.WikidataID,
		MarshalStringSlice(a.Genres), MarshalStringSlice(a.Styles), MarshalStringSlice(a.Moods),
		a.YearsActive, a.Born, a.Formed, a.Died, a.Disbanded, a.Biography,
		a.Path, boolToInt(a.NFOExists), boolToInt(a.ThumbExists),
		boolToInt(a.FanartExists), boolToInt(a.LogoExists), boolToInt(a.BannerExists),
		a.HealthScore, boolToInt(a.IsExcluded), a.ExclusionReason, boolToInt(a.IsClassical),
		formatNullableTime(a.LastScannedAt),
		a.UpdatedAt.Format(time.RFC3339),
		a.ID,
	)
	if err != nil {
		return fmt.Errorf("updating artist: %w", err)
	}
	return nil
}

// Delete removes an artist by ID. Cascade deletes related rows.
func (s *Service) Delete(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM artists WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("deleting artist: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("artist not found: %s", id)
	}
	return nil
}

// Search finds artists by name substring match.
func (s *Service) Search(ctx context.Context, query string) ([]Artist, error) {
	pattern := "%" + query + "%"
	rows, err := s.db.QueryContext(ctx,
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

// ListMembersByArtistID returns all band members for an artist, ordered by sort_order.
func (s *Service) ListMembersByArtistID(ctx context.Context, artistID string) ([]BandMember, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, artist_id, member_name, member_mbid, instruments, vocal_type,
			date_joined, date_left, is_original_member, sort_order, created_at, updated_at
		FROM band_members WHERE artist_id = ? ORDER BY sort_order, member_name
	`, artistID)
	if err != nil {
		return nil, fmt.Errorf("listing band members: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var members []BandMember
	for rows.Next() {
		m, err := scanMember(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning band member: %w", err)
		}
		members = append(members, *m)
	}
	return members, rows.Err()
}

// CreateMember inserts a new band member.
func (s *Service) CreateMember(ctx context.Context, m *BandMember) error {
	if m.ID == "" {
		m.ID = uuid.New().String()
	}
	now := time.Now().UTC()
	m.CreatedAt = now
	m.UpdatedAt = now

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO band_members (
			id, artist_id, member_name, member_mbid, instruments, vocal_type,
			date_joined, date_left, is_original_member, sort_order, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		m.ID, m.ArtistID, m.MemberName, m.MemberMBID,
		MarshalStringSlice(m.Instruments), m.VocalType,
		m.DateJoined, m.DateLeft, boolToInt(m.IsOriginalMember), m.SortOrder,
		now.Format(time.RFC3339), now.Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("creating band member: %w", err)
	}
	return nil
}

// DeleteMember removes a band member by ID.
func (s *Service) DeleteMember(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM band_members WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("deleting band member: %w", err)
	}
	return nil
}

// DeleteMembersByArtistID removes all band members for an artist.
func (s *Service) DeleteMembersByArtistID(ctx context.Context, artistID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM band_members WHERE artist_id = ?`, artistID)
	if err != nil {
		return fmt.Errorf("deleting band members for artist: %w", err)
	}
	return nil
}

// UpsertMembers replaces all band members for an artist with the given list.
func (s *Service) UpsertMembers(ctx context.Context, artistID string, members []BandMember) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx, `DELETE FROM band_members WHERE artist_id = ?`, artistID); err != nil {
		return fmt.Errorf("clearing existing members: %w", err)
	}

	now := time.Now().UTC()
	for i := range members {
		m := &members[i]
		if m.ID == "" {
			m.ID = uuid.New().String()
		}
		m.ArtistID = artistID
		m.CreatedAt = now
		m.UpdatedAt = now

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO band_members (
				id, artist_id, member_name, member_mbid, instruments, vocal_type,
				date_joined, date_left, is_original_member, sort_order, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`,
			m.ID, m.ArtistID, m.MemberName, m.MemberMBID,
			MarshalStringSlice(m.Instruments), m.VocalType,
			m.DateJoined, m.DateLeft, boolToInt(m.IsOriginalMember), m.SortOrder,
			now.Format(time.RFC3339), now.Format(time.RFC3339),
		); err != nil {
			return fmt.Errorf("inserting member %s: %w", m.MemberName, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing member upsert: %w", err)
	}
	return nil
}

// scanArtist scans a database row into an Artist struct.
func scanArtist(row interface{ Scan(...any) error }) (*Artist, error) {
	var a Artist
	var genres, styles, moods string
	var lastScannedAt sql.NullString
	var nfo, thumb, fanart, logo, banner int
	var isExcluded, isClassical int
	var createdAt, updatedAt string

	err := row.Scan(
		&a.ID, &a.Name, &a.SortName, &a.Type, &a.Gender, &a.Disambiguation,
		&a.MusicBrainzID, &a.AudioDBID, &a.DiscogsID, &a.WikidataID,
		&genres, &styles, &moods,
		&a.YearsActive, &a.Born, &a.Formed, &a.Died, &a.Disbanded, &a.Biography,
		&a.Path, &nfo, &thumb, &fanart, &logo, &banner,
		&a.HealthScore, &isExcluded, &a.ExclusionReason, &isClassical,
		&lastScannedAt,
		&createdAt, &updatedAt,
	)
	if err != nil {
		return nil, err
	}

	a.Genres = UnmarshalStringSlice(genres)
	a.Styles = UnmarshalStringSlice(styles)
	a.Moods = UnmarshalStringSlice(moods)
	a.NFOExists = nfo == 1
	a.ThumbExists = thumb == 1
	a.FanartExists = fanart == 1
	a.LogoExists = logo == 1
	a.BannerExists = banner == 1
	a.IsExcluded = isExcluded == 1
	a.IsClassical = isClassical == 1
	a.CreatedAt = parseTime(createdAt)
	a.UpdatedAt = parseTime(updatedAt)

	if lastScannedAt.Valid {
		t := parseTime(lastScannedAt.String)
		a.LastScannedAt = &t
	}

	return &a, nil
}

// scanMember scans a database row into a BandMember struct.
func scanMember(row interface{ Scan(...any) error }) (*BandMember, error) {
	var m BandMember
	var instruments string
	var isOriginal int
	var createdAt, updatedAt string

	err := row.Scan(
		&m.ID, &m.ArtistID, &m.MemberName, &m.MemberMBID,
		&instruments, &m.VocalType,
		&m.DateJoined, &m.DateLeft, &isOriginal, &m.SortOrder,
		&createdAt, &updatedAt,
	)
	if err != nil {
		return nil, err
	}

	m.Instruments = UnmarshalStringSlice(instruments)
	m.IsOriginalMember = isOriginal == 1
	m.CreatedAt = parseTime(createdAt)
	m.UpdatedAt = parseTime(updatedAt)

	return &m, nil
}

// buildWhereClause constructs WHERE conditions from list parameters.
func buildWhereClause(params ListParams) (string, []any) {
	var conditions []string
	var args []any

	if params.Search != "" {
		conditions = append(conditions, "name LIKE ?")
		args = append(args, "%"+params.Search+"%")
	}

	switch params.Filter {
	case "missing_nfo":
		conditions = append(conditions, "nfo_exists = 0")
	case "missing_thumb":
		conditions = append(conditions, "thumb_exists = 0")
	case "missing_fanart":
		conditions = append(conditions, "fanart_exists = 0")
	case "missing_mbid":
		conditions = append(conditions, "(musicbrainz_id IS NULL OR musicbrainz_id = '')")
	case "excluded":
		conditions = append(conditions, "is_excluded = 1")
	case "not_excluded":
		conditions = append(conditions, "is_excluded = 0")
	case "compliant":
		conditions = append(conditions, "health_score >= 100")
	case "non_compliant":
		conditions = append(conditions, "health_score < 100")
	}

	if len(conditions) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conditions, " AND "), args
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func formatNullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.Format(time.RFC3339)
}

// parseTime parses a time string, handling both RFC3339 and SQLite datetime formats.
func parseTime(s string) time.Time {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	if t, err := time.Parse("2006-01-02 15:04:05", s); err == nil {
		return t
	}
	return time.Time{}
}
