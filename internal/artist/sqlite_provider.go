package artist

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/dbutil"
)

type sqliteProviderIDRepo struct {
	db *sql.DB
}

func newSQLiteProviderIDRepo(db *sql.DB) *sqliteProviderIDRepo {
	return &sqliteProviderIDRepo{db: db}
}

func (r *sqliteProviderIDRepo) GetByProviderID(ctx context.Context, provider, id string) (*Artist, error) {
	validProviders := map[string]bool{
		"musicbrainz": true, "audiodb": true, "discogs": true,
		"wikidata": true, "deezer": true, "spotify": true,
	}
	if !validProviders[provider] {
		return nil, fmt.Errorf("unknown provider: %s", provider)
	}

	row := r.db.QueryRowContext(ctx,
		`SELECT `+artistColumns+` FROM artists
		WHERE id = (SELECT artist_id FROM artist_provider_ids WHERE provider = ? AND provider_id = ? LIMIT 1)`,
		provider, id)
	a, err := scanArtist(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting artist by %s: %w", provider, err)
	}
	return a, nil
}

func (r *sqliteProviderIDRepo) GetForArtist(ctx context.Context, artistID string) ([]ProviderID, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT provider, provider_id, fetched_at FROM artist_provider_ids WHERE artist_id = ? ORDER BY provider`,
		artistID)
	if err != nil {
		return nil, fmt.Errorf("getting provider IDs for artist %s: %w", artistID, err)
	}
	defer rows.Close() //nolint:errcheck

	var ids []ProviderID
	for rows.Next() {
		var p ProviderID
		var fetchedAt sql.NullString
		if err := rows.Scan(&p.Provider, &p.ProviderID, &fetchedAt); err != nil {
			return nil, fmt.Errorf("scanning provider ID: %w", err)
		}
		if fetchedAt.Valid {
			t := dbutil.ParseTime(fetchedAt.String)
			p.FetchedAt = &t
		}
		ids = append(ids, p)
	}
	return ids, rows.Err()
}

func (r *sqliteProviderIDRepo) GetForArtists(ctx context.Context, artistIDs []string) (map[string][]ProviderID, error) {
	if len(artistIDs) == 0 {
		return nil, nil
	}

	placeholders := make([]string, len(artistIDs))
	args := make([]any, len(artistIDs))
	for i, id := range artistIDs {
		placeholders[i] = "?"
		args[i] = id
	}

	query := `SELECT artist_id, provider, provider_id, fetched_at ` + //nolint:gosec // G202: placeholders are "?" literals
		`FROM artist_provider_ids ` +
		`WHERE artist_id IN (` + strings.Join(placeholders, ",") + `) ` +
		`ORDER BY artist_id, provider`
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("batch getting provider IDs: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	result := make(map[string][]ProviderID, len(artistIDs))
	for rows.Next() {
		var artistID string
		var p ProviderID
		var fetchedAt sql.NullString
		if err := rows.Scan(&artistID, &p.Provider, &p.ProviderID, &fetchedAt); err != nil {
			return nil, fmt.Errorf("scanning provider ID row: %w", err)
		}
		if fetchedAt.Valid {
			t := dbutil.ParseTime(fetchedAt.String)
			p.FetchedAt = &t
		}
		result[artistID] = append(result[artistID], p)
	}
	return result, rows.Err()
}

func (r *sqliteProviderIDRepo) UpsertAll(ctx context.Context, artistID string, ids []ProviderID) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx, `DELETE FROM artist_provider_ids WHERE artist_id = ?`, artistID); err != nil {
		return fmt.Errorf("deleting old provider IDs: %w", err)
	}

	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO artist_provider_ids (artist_id, provider, provider_id, fetched_at) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("preparing insert: %w", err)
	}
	defer stmt.Close() //nolint:errcheck

	for _, p := range ids {
		if p.Provider == "" {
			continue
		}
		// Skip rows that have neither an ID nor a fetched_at timestamp.
		// Providers like lastfm/audiodb/discogs/wikidata may store only
		// fetched_at (empty provider_id) to record lookup attempts.
		if p.ProviderID == "" && p.FetchedAt == nil {
			continue
		}
		if _, err := stmt.ExecContext(ctx, artistID, p.Provider, p.ProviderID, dbutil.FormatNullableTime(p.FetchedAt)); err != nil {
			return fmt.Errorf("inserting provider ID %s: %w", p.Provider, err)
		}
	}

	return tx.Commit()
}

func (r *sqliteProviderIDRepo) DeleteAll(ctx context.Context, artistID string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM artist_provider_ids WHERE artist_id = ?`, artistID)
	if err != nil {
		return fmt.Errorf("deleting all provider IDs for artist %s: %w", artistID, err)
	}
	return nil
}

func (r *sqliteProviderIDRepo) UpdateProviderFetchedAt(ctx context.Context, artistID, prov string) error {
	validProviders := map[string]bool{
		"audiodb": true, "discogs": true, "wikidata": true, "lastfm": true,
	}
	if !validProviders[prov] {
		return fmt.Errorf("unknown provider for fetched_at: %s", prov)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO artist_provider_ids (artist_id, provider, provider_id, fetched_at)
		VALUES (?, ?, '', ?)
		ON CONFLICT(artist_id, provider) DO UPDATE SET fetched_at = ?`,
		artistID, prov, now, now)
	if err != nil {
		return fmt.Errorf("updating %s fetched_at for artist %s: %w", prov, artistID, err)
	}

	// Also bump artist updated_at
	_, err = r.db.ExecContext(ctx,
		`UPDATE artists SET updated_at = ? WHERE id = ?`, now, artistID)
	if err != nil {
		return fmt.Errorf("updating artist updated_at: %w", err)
	}
	return nil
}
