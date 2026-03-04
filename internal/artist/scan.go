package artist

import (
	"database/sql"
	"strings"

	"github.com/sydlexius/stillwater/internal/dbutil"
)

// artistColumns is the ordered list of columns for SELECT queries.
const artistColumns = `id, name, sort_name, type, gender, disambiguation,
	musicbrainz_id, audiodb_id, discogs_id, wikidata_id, deezer_id, spotify_id,
	genres, styles, moods,
	years_active, born, formed, died, disbanded, biography,
	path, library_id, nfo_exists, thumb_exists, fanart_exists, fanart_count, logo_exists, banner_exists,
	thumb_low_res, fanart_low_res, logo_low_res, banner_low_res,
	thumb_placeholder, fanart_placeholder, logo_placeholder, banner_placeholder,
	health_score, is_excluded, exclusion_reason, is_classical, metadata_sources,
	audiodb_id_fetched_at, discogs_id_fetched_at, wikidata_id_fetched_at, lastfm_id_fetched_at,
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

// scanArtist scans a database row into an Artist struct.
func scanArtist(row interface{ Scan(...any) error }) (*Artist, error) {
	var a Artist
	var genres, styles, moods string
	var libraryID sql.NullString
	var metadataSources string
	var audiodbFetchedAt, discogsFetchedAt, wikidataFetchedAt, lastfmFetchedAt sql.NullString
	var lastScannedAt sql.NullString
	var nfo, thumb, fanart, fanartCount, logo, banner int
	var thumbLowRes, fanartLowRes, logoLowRes, bannerLowRes int
	var isExcluded, isClassical int
	var createdAt, updatedAt string

	err := row.Scan(
		&a.ID, &a.Name, &a.SortName, &a.Type, &a.Gender, &a.Disambiguation,
		&a.MusicBrainzID, &a.AudioDBID, &a.DiscogsID, &a.WikidataID, &a.DeezerID, &a.SpotifyID,
		&genres, &styles, &moods,
		&a.YearsActive, &a.Born, &a.Formed, &a.Died, &a.Disbanded, &a.Biography,
		&a.Path, &libraryID, &nfo, &thumb, &fanart, &fanartCount, &logo, &banner,
		&thumbLowRes, &fanartLowRes, &logoLowRes, &bannerLowRes,
		&a.ThumbPlaceholder, &a.FanartPlaceholder, &a.LogoPlaceholder, &a.BannerPlaceholder,
		&a.HealthScore, &isExcluded, &a.ExclusionReason, &isClassical,
		&metadataSources,
		&audiodbFetchedAt, &discogsFetchedAt, &wikidataFetchedAt, &lastfmFetchedAt,
		&lastScannedAt,
		&createdAt, &updatedAt,
	)
	if err != nil {
		return nil, err
	}

	if libraryID.Valid {
		a.LibraryID = libraryID.String
	}
	a.Genres = UnmarshalStringSlice(genres)
	a.Styles = UnmarshalStringSlice(styles)
	a.Moods = UnmarshalStringSlice(moods)
	a.NFOExists = nfo == 1
	a.ThumbExists = thumb == 1
	a.FanartExists = fanart == 1
	a.FanartCount = fanartCount
	a.LogoExists = logo == 1
	a.BannerExists = banner == 1
	a.ThumbLowRes = thumbLowRes == 1
	a.FanartLowRes = fanartLowRes == 1
	a.LogoLowRes = logoLowRes == 1
	a.BannerLowRes = bannerLowRes == 1
	a.IsExcluded = isExcluded == 1
	a.IsClassical = isClassical == 1
	a.MetadataSources = UnmarshalStringMap(metadataSources)
	a.CreatedAt = dbutil.ParseTime(createdAt)
	a.UpdatedAt = dbutil.ParseTime(updatedAt)

	if audiodbFetchedAt.Valid {
		t := dbutil.ParseTime(audiodbFetchedAt.String)
		a.AudioDBIDFetchedAt = &t
	}
	if discogsFetchedAt.Valid {
		t := dbutil.ParseTime(discogsFetchedAt.String)
		a.DiscogsIDFetchedAt = &t
	}
	if wikidataFetchedAt.Valid {
		t := dbutil.ParseTime(wikidataFetchedAt.String)
		a.WikidataIDFetchedAt = &t
	}
	if lastfmFetchedAt.Valid {
		t := dbutil.ParseTime(lastfmFetchedAt.String)
		a.LastFMFetchedAt = &t
	}
	if lastScannedAt.Valid {
		t := dbutil.ParseTime(lastScannedAt.String)
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

	var a Artist
	var genres, styles, moods string
	var libraryID sql.NullString
	var metadataSources string
	var audiodbFetchedAt, discogsFetchedAt, wikidataFetchedAt, lastfmFetchedAt sql.NullString
	var lastScannedAt sql.NullString
	var nfo, thumb, fanart, fanartCount, logo, banner int
	var thumbLowRes, fanartLowRes, logoLowRes, bannerLowRes int
	var isExcluded, isClassical int
	var createdAt, updatedAt string

	args := []any{
		&a.ID, &a.Name, &a.SortName, &a.Type, &a.Gender, &a.Disambiguation,
		&a.MusicBrainzID, &a.AudioDBID, &a.DiscogsID, &a.WikidataID, &a.DeezerID, &a.SpotifyID,
		&genres, &styles, &moods,
		&a.YearsActive, &a.Born, &a.Formed, &a.Died, &a.Disbanded, &a.Biography,
		&a.Path, &libraryID, &nfo, &thumb, &fanart, &fanartCount, &logo, &banner,
		&thumbLowRes, &fanartLowRes, &logoLowRes, &bannerLowRes,
		&a.ThumbPlaceholder, &a.FanartPlaceholder, &a.LogoPlaceholder, &a.BannerPlaceholder,
		&a.HealthScore, &isExcluded, &a.ExclusionReason, &isClassical,
		&metadataSources,
		&audiodbFetchedAt, &discogsFetchedAt, &wikidataFetchedAt, &lastfmFetchedAt,
		&lastScannedAt, &createdAt, &updatedAt,
	}
	args = append(args, extraPtrs...)

	if err := row.Scan(args...); err != nil {
		return nil, err
	}

	if libraryID.Valid {
		a.LibraryID = libraryID.String
	}
	a.Genres = UnmarshalStringSlice(genres)
	a.Styles = UnmarshalStringSlice(styles)
	a.Moods = UnmarshalStringSlice(moods)
	a.NFOExists = nfo == 1
	a.ThumbExists = thumb == 1
	a.FanartExists = fanart == 1
	a.FanartCount = fanartCount
	a.LogoExists = logo == 1
	a.BannerExists = banner == 1
	a.ThumbLowRes = thumbLowRes == 1
	a.FanartLowRes = fanartLowRes == 1
	a.LogoLowRes = logoLowRes == 1
	a.BannerLowRes = bannerLowRes == 1
	a.IsExcluded = isExcluded == 1
	a.IsClassical = isClassical == 1
	a.MetadataSources = UnmarshalStringMap(metadataSources)
	if audiodbFetchedAt.Valid {
		t := dbutil.ParseTime(audiodbFetchedAt.String)
		a.AudioDBIDFetchedAt = &t
	}
	if discogsFetchedAt.Valid {
		t := dbutil.ParseTime(discogsFetchedAt.String)
		a.DiscogsIDFetchedAt = &t
	}
	if wikidataFetchedAt.Valid {
		t := dbutil.ParseTime(wikidataFetchedAt.String)
		a.WikidataIDFetchedAt = &t
	}
	if lastfmFetchedAt.Valid {
		t := dbutil.ParseTime(lastfmFetchedAt.String)
		a.LastFMFetchedAt = &t
	}
	if lastScannedAt.Valid {
		t := dbutil.ParseTime(lastScannedAt.String)
		a.LastScannedAt = &t
	}
	a.CreatedAt = dbutil.ParseTime(createdAt)
	a.UpdatedAt = dbutil.ParseTime(updatedAt)

	return &artistWithExtra{artist: a, extra: extra}, nil
}

// validatedOrderClause returns a safe ORDER BY column expression from
// ListParams. It assumes params.Validate() was called upstream (which
// enforces the sort-column allowlist). This function only applies defaults
// and formats the clause.
func validatedOrderClause(params ListParams) string {
	col := params.Sort
	if col == "" {
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
		conditions = append(conditions, "thumb_exists = 0")
	case "missing_fanart":
		conditions = append(conditions, "fanart_exists = 0")
	case "missing_logo":
		conditions = append(conditions, "logo_exists = 0")
	case "missing_banner":
		conditions = append(conditions, "banner_exists = 0")
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
