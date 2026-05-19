package artist

import (
	"database/sql"
	"strings"

	"github.com/sydlexius/stillwater/internal/dbutil"
)

// artistColumns is the ordered list of columns for SELECT queries.
// Provider IDs are stored in artist_provider_ids, image metadata in artist_images.
// Library membership lives in artist_libraries; Artist.LibraryID is hydrated
// post-scan via Service.hydratePrimaryLibrary so call sites that read the
// runtime-only field still see a value derived from the M:N table.
const artistColumns = `id, name, sort_name, type, gender, origin, disambiguation,
	genres, styles, moods,
	years_active, born, formed, died, disbanded, biography,
	path, nfo_exists,
	health_score, health_evaluated_at, dirty_since, rules_evaluated_at, is_excluded, exclusion_reason, is_classical,
	locked, lock_source, locked_at, locked_fields,
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
	a                 Artist
	genres            string
	styles            string
	moods             string
	metadataSources   string
	healthEvaluatedAt sql.NullString
	dirtySince        sql.NullString
	rulesEvaluatedAt  sql.NullString
	lastScannedAt     sql.NullString
	nfo               int
	isExcluded        int
	isClassical       int
	locked            int
	lockedAt          sql.NullString
	lockedFields      string
	createdAt         string
	updatedAt         string
}

// scanPtrs returns the ordered slice of pointers matching artistColumns.
func (s *scannedArtist) scanPtrs() []any {
	return []any{
		&s.a.ID, &s.a.Name, &s.a.SortName, &s.a.Type, &s.a.Gender, &s.a.Origin, &s.a.Disambiguation,
		&s.genres, &s.styles, &s.moods,
		&s.a.YearsActive, &s.a.Born, &s.a.Formed, &s.a.Died, &s.a.Disbanded, &s.a.Biography,
		&s.a.Path, &s.nfo,
		&s.a.HealthScore, &s.healthEvaluatedAt, &s.dirtySince, &s.rulesEvaluatedAt, &s.isExcluded, &s.a.ExclusionReason, &s.isClassical,
		&s.locked, &s.a.LockSource, &s.lockedAt, &s.lockedFields,
		&s.metadataSources,
		&s.lastScannedAt,
		&s.createdAt, &s.updatedAt,
	}
}

// apply converts intermediate scan values into the Artist struct fields.
func (s *scannedArtist) apply() {
	s.a.Genres = UnmarshalStringSlice(s.genres)
	s.a.Styles = UnmarshalStringSlice(s.styles)
	s.a.Moods = UnmarshalStringSlice(s.moods)
	s.a.NFOExists = s.nfo == 1
	if s.healthEvaluatedAt.Valid {
		t := dbutil.ParseTime(s.healthEvaluatedAt.String)
		s.a.HealthEvaluatedAt = &t
	}
	if s.dirtySince.Valid {
		t := dbutil.ParseTime(s.dirtySince.String)
		s.a.DirtySince = &t
	}
	if s.rulesEvaluatedAt.Valid {
		t := dbutil.ParseTime(s.rulesEvaluatedAt.String)
		s.a.RulesEvaluatedAt = &t
	}
	s.a.IsExcluded = s.isExcluded == 1
	s.a.IsClassical = s.isClassical == 1
	s.a.Locked = s.locked == 1
	if s.lockedAt.Valid {
		t := dbutil.ParseTime(s.lockedAt.String)
		s.a.LockedAt = &t
	}
	s.a.LockedFields = UnmarshalStringSlice(s.lockedFields)
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
// ListParams. The HTTP boundary (dbutil.ValidateSortKey via the api package
// helpers) rejects unknown sort keys with 400, and params.Validate() is a
// second-line defense that normalizes any remaining unexpected value to the
// "name" default. The switch on params.Sort and the fixed dir values below
// use only string literals, so static-analysis tools can verify no user
// input flows into the SQL string.
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

// filterPredicate maps an include/exclude state to a SQL fragment and its bound
// arguments. An empty fragment signals that the state is not applicable (e.g.
// an unrecognized state string); the caller skips such entries.
type filterPredicate func(state string) (fragment string, args []any)

// imageExistsClause builds a single NOT EXISTS / EXISTS sub-select for the
// artist_images table. Keeping the template here avoids repeating the table
// name and column list in every predicate definition.
func imageExistsClause(imageType string, exists bool) string {
	if exists {
		return "EXISTS (SELECT 1 FROM artist_images WHERE artist_id = artists.id AND image_type = '" + imageType + "' AND exists_flag = 1)"
	}
	return "NOT EXISTS (SELECT 1 FROM artist_images WHERE artist_id = artists.id AND image_type = '" + imageType + "' AND exists_flag = 1)"
}

// legacyImageTypes is the ordered set of image types used by the missing_images
// multi-EXISTS predicate. Order is stable so generated SQL is deterministic.
var legacyImageTypes = []string{"thumb", "fanart", "logo", "banner"}

// missingImagesPredicate returns a compound SQL fragment that matches artists
// missing ANY image type (include) or having ALL image types (exclude).
func missingImagesPredicate(state string) (string, []any) {
	parts := make([]string, len(legacyImageTypes))
	switch state {
	case "include":
		// Match artists missing ANY of the tracked image types.
		for i, t := range legacyImageTypes {
			parts[i] = imageExistsClause(t, false)
		}
		return "(" + strings.Join(parts, " OR ") + ")", nil
	case "exclude":
		// Match artists that have ALL of the tracked image types (drops the missing ones).
		for i, t := range legacyImageTypes {
			parts[i] = imageExistsClause(t, true)
		}
		return "(" + strings.Join(parts, " AND ") + ")", nil
	}
	return "", nil
}

// artistFilterPredicates maps flyout filter keys to their predicate functions.
// Keys here are the canonical names from ListParams.Filters. Each predicate
// receives the filter state ("include" or "exclude") and returns the SQL
// fragment and any bound arguments for that state. Predicates that produce no
// SQL for a given state return an empty fragment.
//
// Type filters (type_person, type_group, type_orchestra) and library filters
// (library_{id}) are NOT in this map; they aggregate across multiple keys and
// are handled separately in buildWhereClause.
var artistFilterPredicates = map[string]filterPredicate{
	"missing_meta": func(state string) (string, []any) {
		switch state {
		case "include":
			return "nfo_exists = 0", nil
		case "exclude":
			return "nfo_exists = 1", nil
		}
		return "", nil
	},
	"missing_images": missingImagesPredicate,
	"missing_mbid": func(state string) (string, []any) {
		// Check for a non-empty provider_id, not just row existence.
		// UpsertAll inserts a musicbrainz row even when no MBID was found
		// (provider_id == ""), so a bare EXISTS would misclassify those artists.
		switch state {
		case "include":
			return "NOT EXISTS (SELECT 1 FROM artist_provider_ids WHERE artist_id = artists.id AND provider = 'musicbrainz' AND provider_id IS NOT NULL AND provider_id <> '')", nil
		case "exclude":
			return "EXISTS (SELECT 1 FROM artist_provider_ids WHERE artist_id = artists.id AND provider = 'musicbrainz' AND provider_id IS NOT NULL AND provider_id <> '')", nil
		}
		return "", nil
	},
	"excluded": func(state string) (string, []any) {
		switch state {
		case "include":
			return "is_excluded = 1", nil
		case "exclude":
			return "is_excluded = 0", nil
		}
		return "", nil
	},
	"locked": func(state string) (string, []any) {
		switch state {
		case "include":
			return "locked = 1", nil
		case "exclude":
			return "locked = 0", nil
		}
		return "", nil
	},
}

// typeFilterKeys maps flyout type-filter keys to their database type name
// values. type_person maps to two values because MusicBrainz stores Person as
// "solo" while legacy imports may use "person".
var typeFilterKeys = map[string][]string{
	"type_person":    {"person", "solo"},
	"type_group":     {"group"},
	"type_orchestra": {"orchestra"},
}

// buildPlaceholders returns a comma-separated "?,?,?" string of length n.
func buildPlaceholders(n int) string {
	ph := strings.Repeat("?,", n)
	return ph[:len(ph)-1]
}

// legacyFilterSQL maps the legacy single-value params.Filter strings to fixed
// SQL fragments. These use only literal values (no user input is interpolated).
var legacyFilterSQL = map[string]string{
	"missing_nfo":    "nfo_exists = 0",
	"missing_thumb":  imageExistsClause("thumb", false),
	"missing_fanart": imageExistsClause("fanart", false),
	"missing_logo":   imageExistsClause("logo", false),
	"missing_banner": imageExistsClause("banner", false),
	"missing_mbid":   "NOT EXISTS (SELECT 1 FROM artist_provider_ids WHERE artist_id = artists.id AND provider = 'musicbrainz' AND provider_id IS NOT NULL AND provider_id <> '')",
	"excluded":       "is_excluded = 1",
	"not_excluded":   "is_excluded = 0",
	"locked":         "locked = 1",
	"not_locked":     "locked = 0",
	"compliant":      "health_score >= 100",
	"non_compliant":  "health_score < 100",
}

// buildWhereClause constructs WHERE conditions from list parameters.
//
//nolint:gocognit // SQL WHERE assembler: each filter field (IDs, search, locked, missing-image flags, status enum, library scope, source list) has bespoke placeholder shape and argument count; the if-ladder enforces the column-to-args correspondence the SQL parameter binding depends on.
func buildWhereClause(params ListParams) (string, []any) {
	var conditions []string
	var args []any

	// IDs filter (#1227): when present, restrict the result set to exactly
	// these artist IDs. Used by the bulk-selection "Show selected" affordance
	// so cross-page selections can be reviewed in one place. Validate() has
	// already capped the slice to MaxListIDs and dropped empty strings, so
	// we trust it here. Renders as `artists.id IN (?, ?, ?)` with one bound
	// parameter per ID.
	if len(params.IDs) > 0 {
		placeholders := make([]string, len(params.IDs))
		for i, id := range params.IDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		conditions = append(conditions, "artists.id IN ("+strings.Join(placeholders, ", ")+")")
	}

	if params.Search != "" {
		conditions = append(conditions, "name LIKE ?")
		args = append(args, "%"+params.Search+"%")
	}

	if params.LibraryID != "" {
		// Match via the M:N membership table. The legacy artists.library_id
		// column was dropped in migration 004; artist_libraries is the
		// authoritative membership record.
		conditions = append(conditions, `EXISTS (
			SELECT 1 FROM artist_libraries al
			WHERE al.artist_id = artists.id AND al.library_id = ?
		)`)
		args = append(args, params.LibraryID)
	}

	// Legacy single-filter param: maps a fixed set of filter names to SQL.
	if frag, ok := legacyFilterSQL[params.Filter]; ok {
		conditions = append(conditions, frag)
	}

	// Flyout multi-filter: each key dispatches through the predicate map.
	// Type and library keys are accumulated separately below.
	var typeIncludes, typeExcludes []string
	var libIncludes, libExcludes []string

	for key, state := range params.Filters {
		if typeNames, ok := typeFilterKeys[key]; ok {
			switch state {
			case "include":
				typeIncludes = append(typeIncludes, typeNames...)
			case "exclude":
				typeExcludes = append(typeExcludes, typeNames...)
			}
			continue
		}
		if strings.HasPrefix(key, "library_") {
			libID := key[len("library_"):]
			if libID == "" {
				continue
			}
			switch state {
			case "include":
				libIncludes = append(libIncludes, libID)
			case "exclude":
				libExcludes = append(libExcludes, libID)
			}
			continue
		}
		if pred, ok := artistFilterPredicates[key]; ok {
			if frag, pArgs := pred(state); frag != "" {
				conditions = append(conditions, frag)
				args = append(args, pArgs...)
			}
		}
	}

	// Aggregate type filters into a single IN / NOT IN clause so that
	// selecting multiple types produces OR logic (not impossible AND).
	if len(typeIncludes) > 0 {
		conditions = append(conditions, "type IN ("+buildPlaceholders(len(typeIncludes))+")")
		for _, t := range typeIncludes {
			args = append(args, t)
		}
	}
	if len(typeExcludes) > 0 {
		conditions = append(conditions, "type NOT IN ("+buildPlaceholders(len(typeExcludes))+")")
		for _, t := range typeExcludes {
			args = append(args, t)
		}
	}

	// Aggregate per-library filters into EXISTS / NOT EXISTS sub-selects.
	if len(libIncludes) > 0 {
		// Match artists with membership in ANY of the included libraries.
		conditions = append(conditions, `EXISTS (
			SELECT 1 FROM artist_libraries al
			WHERE al.artist_id = artists.id
			  AND al.library_id IN (`+buildPlaceholders(len(libIncludes))+`)
		)`)
		for _, id := range libIncludes {
			args = append(args, id)
		}
	}
	if len(libExcludes) > 0 {
		// Drop artists with membership in any excluded library. Artists
		// with no membership in the excluded set pass through.
		conditions = append(conditions, `NOT EXISTS (
			SELECT 1 FROM artist_libraries al
			WHERE al.artist_id = artists.id
			  AND al.library_id IN (`+buildPlaceholders(len(libExcludes))+`)
		)`)
		for _, id := range libExcludes {
			args = append(args, id)
		}
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
