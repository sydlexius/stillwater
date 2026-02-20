package artist

import (
	"context"
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

	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT `+prefixedArtistColumns("artists")+` `+ //nolint:gosec // G202: concatenation uses trusted static column list
			`FROM artists
		LEFT JOIN artist_aliases ON artists.id = artist_aliases.artist_id
		WHERE LOWER(artists.name) LIKE ? OR LOWER(artist_aliases.alias) LIKE ?
		ORDER BY artists.name`, pattern, pattern)
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
func (s *Service) FindDuplicates(ctx context.Context) ([]DuplicateGroup, error) {
	var groups []DuplicateGroup

	// Collect shared MBIDs first, then close rows before sub-queries (SQLite limitation)
	mbids, err := s.collectStrings(ctx, `
		SELECT musicbrainz_id FROM artists
		WHERE musicbrainz_id != '' AND is_excluded = 0
		GROUP BY musicbrainz_id HAVING COUNT(*) > 1
	`)
	if err != nil {
		return nil, fmt.Errorf("finding MBID duplicates: %w", err)
	}

	for _, mbid := range mbids {
		artists, err := s.listByMBID(ctx, mbid)
		if err != nil {
			return nil, err
		}
		groups = append(groups, DuplicateGroup{
			Artists: artists,
			Reason:  fmt.Sprintf("shared MusicBrainz ID: %s", mbid),
		})
	}

	// Collect shared aliases, then close rows before sub-queries
	aliases, err := s.collectStrings(ctx, `
		SELECT LOWER(alias) FROM artist_aliases
		GROUP BY LOWER(alias) HAVING COUNT(DISTINCT artist_id) > 1
	`)
	if err != nil {
		return nil, fmt.Errorf("finding alias duplicates: %w", err)
	}

	for _, alias := range aliases {
		artists, err := s.listByAlias(ctx, alias)
		if err != nil {
			return nil, err
		}
		groups = append(groups, DuplicateGroup{
			Artists: artists,
			Reason:  fmt.Sprintf("shared alias: %s", alias),
		})
	}

	return groups, nil
}

// collectStrings runs a query that returns a single string column and collects all values.
func (s *Service) collectStrings(ctx context.Context, query string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck

	var result []string
	for rows.Next() {
		var val string
		if err := rows.Scan(&val); err != nil {
			return nil, err
		}
		result = append(result, val)
	}
	return result, rows.Err()
}

func (s *Service) listByMBID(ctx context.Context, mbid string) ([]Artist, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+artistColumns+` FROM artists WHERE musicbrainz_id = ? ORDER BY name
	`, mbid)
	if err != nil {
		return nil, err
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

func (s *Service) listByAlias(ctx context.Context, alias string) ([]Artist, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT `+prefixedArtistColumns("artists")+` `+ //nolint:gosec // G202: concatenation uses trusted static column list
			`FROM artists
		JOIN artist_aliases ON artists.id = artist_aliases.artist_id
		WHERE LOWER(artist_aliases.alias) = ?
		ORDER BY artists.name`, strings.ToLower(alias))
	if err != nil {
		return nil, err
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
