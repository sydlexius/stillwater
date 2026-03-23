package artist

import (
	"database/sql"
	"strings"

	"github.com/sydlexius/stillwater/internal/dbutil"
)

// artistColumns is the ordered list of columns for SELECT queries.
// Provider IDs are stored in artist_provider_ids, image metadata in artist_images.
const artistColumns = `id, name, sort_name, type, gender, disambiguation,
	genres, styles, moods,
	years_active, born, formed, died, disbanded, biography,
	path, library_id, nfo_exists,
	health_score, is_excluded, exclusion_reason, is_classical,
	locked, lock_source, locked_at,
	metadata_sources,
	last_scanned_at, created_at, updated_at`

// prefixedArtistColumns returns artistColumns with each column prefixed by the given table alias.
func prefixedArtistColumns(table string) string {
	cols := strings.Split(artistColumns, ",")
	for i, col := range cols {
		col = strings.TrimSpace(col)
		cols[i] = table + "." + col
	}
	return strings.Join(cols, ", ")
}

// scannedArtist holds intermediate scan targets for the artist columns.
type scannedArtist struct {
	a               Artist
	genres          string
	styles          string
	moods           string
	libraryID       sql.NullString
	metadataSources string
	lastScannedAt   sql.NullString
	nfo             int
	isExcluded      int
	isClassical     int
	locked          int
	lockedAt        sql.NullString
	createdAt       string
	updatedAt       string
}

// scanPtrs returns the ordered slice of pointers matching artistColumns.
func (s *scannedArtist) scanPtrs() []any {
	return []any{
		&s.a.ID, &s.a.Name, &s.a.SortName, &s.a.Type, &s.a.Gender, &s.a.Disambiguation,
		&s.genres, &s.styles, &s.moods,
		&s.a.YearsActive, &s.a.Born, &s.a.Formed, &s.a.Died, &s.a.Disbanded, &s.a.Biography,
		&s.a.Path, &s.libraryID, &s.nfo,
		&s.a.HealthScore, &s.isExcluded, &s.a.ExclusionReason, &s.isClassical,
		&s.locked, &s.a.LockSource, &s.lockedAt,
		&s.metadataSources,
		&s.lastScannedAt,
		&s.createdAt, &s.updatedAt,
	}
}

// apply converts intermediate scan values into the Artist struct fields.
func (s *scannedArtist) apply() {
	if s.libraryID.Valid {
		s.a.LibraryID = s.libraryID.String
	}
	s.a.Genres = UnmarshalStringSlice(s.genres)
	s.a.Styles = UnmarshalStringSlice(s.styles)
	s.a.Moods = UnmarshalStringSlice(s.moods)
	s.a.NFOExists = s.nfo == 1
	s.a.IsExcluded = s.isExcluded == 1
	s.a.IsClassical = s.isClassical == 1
	s.a.Locked = s.locked == 1
	if s.lockedAt.Valid {
		t := dbutil.ParseTime(s.lockedAt.String)
		s.a.LockedAt = &t
	}
	s.a.MetadataSources = UnmarshalStringMap(s.metadataSources)
	if s.lastScannedAt.Valid {
		t := dbutil.ParseTime(s.lastScannedAt.String)
		s.a.LastScannedAt = &t
	}
	s.a.CreatedAt = dbutil.ParseTime(s.createdAt)
	s.a.UpdatedAt = dbutil.ParseTime(s.updatedAt)
}

// scanArtist scans a database row into an Artist struct.
func scanArtist(row interface{ Scan(...any) error }) (*Artist, error) {
	var s scannedArtist
	if err := row.Scan(s.scanPtrs()...); err != nil {
		return nil, err
	}
	s.apply()
	return &s.a, nil
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
	m.CreatedAt = dbutil.ParseTime(createdAt)
	m.UpdatedAt = dbutil.ParseTime(updatedAt)

	return &m, nil
}

// artistWithExtra wraps an Artist with extra scanned string columns.
type artistWithExtra struct {
	artist Artist
	extra  []string
}

// scanArtistWithExtra scans the standard artist columns plus n additional string columns.
func scanArtistWithExtra(row interface{ Scan(...any) error }, n int) (*artistWithExtra, error) {
	extra := make([]string, n)
	extraPtrs := make([]any, n)
	for i := range extra {
		extraPtrs[i] = &extra[i]
	}

	var s scannedArtist
	args := s.scanPtrs()
	args = append(args, extraPtrs...)

	if err := row.Scan(args...); err != nil {
		return nil, err
	}
	s.apply()

	return &artistWithExtra{artist: s.a, extra: extra}, nil
}

// validatedOrderClause returns a safe ORDER BY column expression from
// ListParams. It assumes params.Validate() was called upstream to normalize
// and allowlist the Sort and Order fields. The switch on params.Sort and the
// fixed dir values below use only string literals, so static-analysis tools
// can verify no user input flows into the SQL string.
func validatedOrderClause(params ListParams) string {
	var col string
	switch params.Sort {
	case "sort_name":
		col = "sort_name"
	case "health_score":
		col = "health_score"
	case "updated_at":
		col = "updated_at"
	case "created_at":
		col = "created_at"
	default:
		col = "name"
	}
	dir := "ASC"
	if params.Order == "desc" {
		dir = "DESC"
	}
	return col + " " + dir
}

// buildWhereClause constructs WHERE conditions from list parameters.
func buildWhereClause(params ListParams) (string, []any) {
	var conditions []string
	var args []any

	if params.Search != "" {
		conditions = append(conditions, "name LIKE ?")
		args = append(args, "%"+params.Search+"%")
	}

	if params.LibraryID != "" {
		conditions = append(conditions, "library_id = ?")
		args = append(args, params.LibraryID)
	}

	switch params.Filter {
	case "missing_nfo":
		conditions = append(conditions, "nfo_exists = 0")
	case "missing_thumb":
		conditions = append(conditions, "NOT EXISTS (SELECT 1 FROM artist_images WHERE artist_id = artists.id AND image_type = 'thumb' AND exists_flag = 1)")
	case "missing_fanart":
		conditions = append(conditions, "NOT EXISTS (SELECT 1 FROM artist_images WHERE artist_id = artists.id AND image_type = 'fanart' AND exists_flag = 1)")
	case "missing_logo":
		conditions = append(conditions, "NOT EXISTS (SELECT 1 FROM artist_images WHERE artist_id = artists.id AND image_type = 'logo' AND exists_flag = 1)")
	case "missing_banner":
		conditions = append(conditions, "NOT EXISTS (SELECT 1 FROM artist_images WHERE artist_id = artists.id AND image_type = 'banner' AND exists_flag = 1)")
	case "missing_mbid":
		conditions = append(conditions, "NOT EXISTS (SELECT 1 FROM artist_provider_ids WHERE artist_id = artists.id AND provider = 'musicbrainz')")
	case "excluded":
		conditions = append(conditions, "is_excluded = 1")
	case "not_excluded":
		conditions = append(conditions, "is_excluded = 0")
	case "locked":
		conditions = append(conditions, "locked = 1")
	case "not_locked":
		conditions = append(conditions, "locked = 0")
	case "compliant":
		conditions = append(conditions, "health_score >= 100")
	case "non_compliant":
		conditions = append(conditions, "health_score < 100")
	}

	if params.HealthScoreMin > 0 {
		conditions = append(conditions, "health_score >= ?")
		args = append(args, params.HealthScoreMin)
	}
	if params.HealthScoreMax > 0 && params.HealthScoreMax <= 100 {
		conditions = append(conditions, "health_score <= ?")
		args = append(args, params.HealthScoreMax)
	}

	if len(conditions) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conditions, " AND "), args
}
