package artist

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// prefixedArtistColumns returns artistColumns with each column prefixed by the given table alias.
func prefixedArtistColumns(table string) string {
	cols := strings.Split(artistColumns, ",")
	for i, col := range cols {
		col = strings.TrimSpace(col)
		cols[i] = table + "." + col
	}
	return strings.Join(cols, ", ")
}

// Alias represents an alternative name for an artist.
type Alias struct {
	ID        string    `json:"id"`
	ArtistID  string    `json:"artist_id"`
	Alias     string    `json:"alias"`
	Source    string    `json:"source,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// DuplicateGroup represents a set of artists that may be duplicates.
type DuplicateGroup struct {
	Artists []Artist `json:"artists"`
	Reason  string   `json:"reason"`
}

// AddAlias adds an alias for an artist.
func (s *Service) AddAlias(ctx context.Context, artistID, alias, source string) (*Alias, error) {
	if alias == "" {
		return nil, fmt.Errorf("alias is required")
	}

	// Check artist exists
	_, err := s.GetByID(ctx, artistID)
	if err != nil {
		return nil, fmt.Errorf("artist not found: %w", err)
	}

	a := &Alias{
		ID:        uuid.New().String(),
		ArtistID:  artistID,
		Alias:     alias,
		Source:    source,
		CreatedAt: time.Now().UTC(),
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO artist_aliases (id, artist_id, alias, source, created_at)
		VALUES (?, ?, ?, ?, ?)
	`, a.ID, a.ArtistID, a.Alias, a.Source, a.CreatedAt.Format(time.RFC3339))
	if err != nil {
		return nil, fmt.Errorf("inserting alias: %w", err)
	}
	return a, nil
}

// RemoveAlias removes an alias by ID.
func (s *Service) RemoveAlias(ctx context.Context, aliasID string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM artist_aliases WHERE id = ?`, aliasID)
	if err != nil {
		return fmt.Errorf("deleting alias: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("alias not found")
	}
	return nil
}

// ListAliases returns all aliases for an artist.
func (s *Service) ListAliases(ctx context.Context, artistID string) ([]Alias, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, artist_id, alias, COALESCE(source, ''), created_at
		FROM artist_aliases WHERE artist_id = ? ORDER BY alias
	`, artistID)
	if err != nil {
		return nil, fmt.Errorf("listing aliases: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var aliases []Alias
	for rows.Next() {
		var a Alias
		var createdAt string
		if err := rows.Scan(&a.ID, &a.ArtistID, &a.Alias, &a.Source, &createdAt); err != nil {
			return nil, fmt.Errorf("scanning alias: %w", err)
		}
		a.CreatedAt = parseTime(createdAt)
		aliases = append(aliases, a)
	}
	return aliases, rows.Err()
}

// SearchWithAliases searches artists by name or alias text.
func (s *Service) SearchWithAliases(ctx context.Context, query string) ([]Artist, error) {
	pattern := "%" + strings.ToLower(query) + "%"

	rows, err := s.db.QueryContext(ctx, `
		SELECT `+artistColumns+` FROM artists WHERE id IN ( `+ //nolint:gosec // G202: concatenation uses trusted static column list
		`SELECT artists.id FROM artists
		LEFT JOIN artist_aliases ON artists.id = artist_aliases.artist_id
		WHERE LOWER(artists.name) LIKE ? OR LOWER(artist_aliases.alias) LIKE ?
		) ORDER BY name`, pattern, pattern)
	if err != nil {
		return nil, fmt.Errorf("searching with aliases: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var artists []Artist
	for rows.Next() {
		a, err := scanArtist(rows)
		if err != nil {
			return nil, err
		}
		artists = append(artists, *a)
	}
	return artists, rows.Err()
}

// FindDuplicates returns groups of artists that appear to be duplicates.
// Detection is based on shared aliases, matching MBIDs, or similar names.
// Uses single queries with subqueries to avoid SQLite single-connection deadlocks.
func (s *Service) FindDuplicates(ctx context.Context) ([]DuplicateGroup, error) {
	var groups []DuplicateGroup

	// Find artists sharing MBIDs in a single query
	mbidGroups, err := s.findMBIDDuplicates(ctx)
	if err != nil {
		return nil, fmt.Errorf("finding MBID duplicates: %w", err)
	}
	groups = append(groups, mbidGroups...)

	// Find artists sharing aliases in a single query
	aliasGroups, err := s.findAliasDuplicates(ctx)
	if err != nil {
		return nil, fmt.Errorf("finding alias duplicates: %w", err)
	}
	groups = append(groups, aliasGroups...)

	return groups, nil
}

func (s *Service) findMBIDDuplicates(ctx context.Context) ([]DuplicateGroup, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+artistColumns+` FROM artists `+ //nolint:gosec // G202: concatenation uses trusted static column list
		`WHERE musicbrainz_id != '' AND is_excluded = 0
		AND musicbrainz_id IN (
			SELECT musicbrainz_id FROM artists
			WHERE musicbrainz_id != '' AND is_excluded = 0
			GROUP BY musicbrainz_id HAVING COUNT(*) > 1
		) ORDER BY musicbrainz_id, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck

	// Group artists by their MBID
	mbidMap := make(map[string][]Artist)
	var mbidOrder []string
	for rows.Next() {
		a, err := scanArtist(rows)
		if err != nil {
			return nil, err
		}
		if _, exists := mbidMap[a.MusicBrainzID]; !exists {
			mbidOrder = append(mbidOrder, a.MusicBrainzID)
		}
		mbidMap[a.MusicBrainzID] = append(mbidMap[a.MusicBrainzID], *a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var groups []DuplicateGroup
	for _, mbid := range mbidOrder {
		groups = append(groups, DuplicateGroup{
			Artists: mbidMap[mbid],
			Reason:  fmt.Sprintf("shared MusicBrainz ID: %s", mbid),
		})
	}
	return groups, nil
}

func (s *Service) findAliasDuplicates(ctx context.Context) ([]DuplicateGroup, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+prefixedArtistColumns("artists")+`, aa.alias FROM artists `+ //nolint:gosec // G202: concatenation uses trusted static column list
			`JOIN artist_aliases aa ON artists.id = aa.artist_id
		WHERE LOWER(aa.alias) IN (
			SELECT LOWER(alias) FROM artist_aliases
			GROUP BY LOWER(alias) HAVING COUNT(DISTINCT artist_id) > 1
		) ORDER BY LOWER(aa.alias), artists.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck

	// Group artists by their shared alias
	aliasMap := make(map[string][]Artist)
	aliasSeenArtist := make(map[string]map[string]bool) // alias -> artist ID -> seen
	var aliasOrder []string
	for rows.Next() {
		a, err := scanArtistWithExtra(rows, 1)
		if err != nil {
			return nil, err
		}
		lowerAlias := strings.ToLower(a.extra[0])
		if _, exists := aliasMap[lowerAlias]; !exists {
			aliasOrder = append(aliasOrder, lowerAlias)
			aliasSeenArtist[lowerAlias] = make(map[string]bool)
		}
		// Deduplicate: an artist might match multiple aliases
		if !aliasSeenArtist[lowerAlias][a.artist.ID] {
			aliasSeenArtist[lowerAlias][a.artist.ID] = true
			aliasMap[lowerAlias] = append(aliasMap[lowerAlias], a.artist)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var groups []DuplicateGroup
	for _, alias := range aliasOrder {
		if len(aliasMap[alias]) > 1 {
			groups = append(groups, DuplicateGroup{
				Artists: aliasMap[alias],
				Reason:  fmt.Sprintf("shared alias: %s", alias),
			})
		}
	}
	return groups, nil
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
	var metadataSources string
	var audiodbFetchedAt, discogsFetchedAt, wikidataFetchedAt, lastfmFetchedAt sql.NullString
	var lastScannedAt sql.NullString
	var nfo, thumb, fanart, logo, banner int
	var thumbLowRes, fanartLowRes, logoLowRes, bannerLowRes int
	var isExcluded, isClassical int
	var createdAt, updatedAt string

	args := []any{
		&a.ID, &a.Name, &a.SortName, &a.Type, &a.Gender, &a.Disambiguation,
		&a.MusicBrainzID, &a.AudioDBID, &a.DiscogsID, &a.WikidataID, &a.DeezerID,
		&genres, &styles, &moods,
		&a.YearsActive, &a.Born, &a.Formed, &a.Died, &a.Disbanded, &a.Biography,
		&a.Path, &nfo, &thumb, &fanart, &logo, &banner,
		&thumbLowRes, &fanartLowRes, &logoLowRes, &bannerLowRes,
		&a.HealthScore, &isExcluded, &a.ExclusionReason, &isClassical,
		&metadataSources,
		&audiodbFetchedAt, &discogsFetchedAt, &wikidataFetchedAt, &lastfmFetchedAt,
		&lastScannedAt, &createdAt, &updatedAt,
	}
	args = append(args, extraPtrs...)

	if err := row.Scan(args...); err != nil {
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
	a.ThumbLowRes = thumbLowRes == 1
	a.FanartLowRes = fanartLowRes == 1
	a.LogoLowRes = logoLowRes == 1
	a.BannerLowRes = bannerLowRes == 1
	a.IsExcluded = isExcluded == 1
	a.IsClassical = isClassical == 1
	a.MetadataSources = UnmarshalStringMap(metadataSources)
	if audiodbFetchedAt.Valid {
		t := parseTime(audiodbFetchedAt.String)
		a.AudioDBIDFetchedAt = &t
	}
	if discogsFetchedAt.Valid {
		t := parseTime(discogsFetchedAt.String)
		a.DiscogsIDFetchedAt = &t
	}
	if wikidataFetchedAt.Valid {
		t := parseTime(wikidataFetchedAt.String)
		a.WikidataIDFetchedAt = &t
	}
	if lastfmFetchedAt.Valid {
		t := parseTime(lastfmFetchedAt.String)
		a.LastFMFetchedAt = &t
	}
	if lastScannedAt.Valid {
		t := parseTime(lastScannedAt.String)
		a.LastScannedAt = &t
	}
	a.CreatedAt = parseTime(createdAt)
	a.UpdatedAt = parseTime(updatedAt)

	return &artistWithExtra{artist: a, extra: extra}, nil
}
