package artist

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sydlexius/stillwater/internal/dbutil"
)

type sqliteAliasRepo struct {
	db *sql.DB
}

func newSQLiteAliasRepo(db *sql.DB) *sqliteAliasRepo {
	return &sqliteAliasRepo{db: db}
}

func (r *sqliteAliasRepo) Create(ctx context.Context, a *Alias) error {
	if a.ID == "" {
		a.ID = uuid.New().String()
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now().UTC()
	}

	_, err := r.db.ExecContext(ctx, `
		INSERT INTO artist_aliases (id, artist_id, alias, source, created_at)
		VALUES (?, ?, ?, ?, ?)
	`, a.ID, a.ArtistID, a.Alias, a.Source, a.CreatedAt.Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("inserting alias: %w", err)
	}
	return nil
}

func (r *sqliteAliasRepo) Delete(ctx context.Context, aliasID string) error {
	result, err := r.db.ExecContext(ctx, `DELETE FROM artist_aliases WHERE id = ?`, aliasID)
	if err != nil {
		return fmt.Errorf("deleting alias: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("alias not found")
	}
	return nil
}

func (r *sqliteAliasRepo) ListByArtistID(ctx context.Context, artistID string) ([]Alias, error) {
	rows, err := r.db.QueryContext(ctx, `
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
		a.CreatedAt = dbutil.ParseTime(createdAt)
		aliases = append(aliases, a)
	}
	return aliases, rows.Err()
}

func (r *sqliteAliasRepo) SearchWithAliases(ctx context.Context, query string) ([]Artist, error) {
	pattern := "%" + strings.ToLower(query) + "%"

	rows, err := r.db.QueryContext(ctx, `
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
			return nil, fmt.Errorf("scanning search-with-aliases result: %w", err)
		}
		artists = append(artists, *a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating search-with-aliases rows: %w", err)
	}
	return artists, nil
}

func (r *sqliteAliasRepo) FindMBIDDuplicates(ctx context.Context) ([]DuplicateGroup, error) {
	rows, err := r.db.QueryContext(ctx, `
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

func (r *sqliteAliasRepo) FindAliasDuplicates(ctx context.Context) ([]DuplicateGroup, error) {
	rows, err := r.db.QueryContext(ctx,
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

	aliasMap := make(map[string][]Artist)
	aliasSeenArtist := make(map[string]map[string]bool)
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
