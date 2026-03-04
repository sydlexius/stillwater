package artist

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type sqliteProviderIDRepo struct {
	db *sql.DB
}

func newSQLiteProviderIDRepo(db *sql.DB) *sqliteProviderIDRepo {
	return &sqliteProviderIDRepo{db: db}
}

func (r *sqliteProviderIDRepo) GetByProviderID(ctx context.Context, provider, id string) (*Artist, error) {
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
	case "deezer":
		col = "deezer_id"
	case "spotify":
		col = "spotify_id"
	default:
		return nil, fmt.Errorf("unknown provider: %s", provider)
	}

	row := r.db.QueryRowContext(ctx,
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

func (r *sqliteProviderIDRepo) UpdateProviderFetchedAt(ctx context.Context, artistID, prov string) error {
	col, ok := map[string]string{
		"audiodb":  "audiodb_id_fetched_at",
		"discogs":  "discogs_id_fetched_at",
		"wikidata": "wikidata_id_fetched_at",
		"lastfm":   "lastfm_id_fetched_at",
	}[prov]
	if !ok {
		return fmt.Errorf("unknown provider for fetched_at: %s", prov)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := r.db.ExecContext(ctx,
		"UPDATE artists SET "+col+" = ?, updated_at = ? WHERE id = ?", //nolint:gosec // col is from validated map
		now, now, artistID,
	)
	if err != nil {
		return fmt.Errorf("updating %s fetched_at for artist %s: %w", prov, artistID, err)
	}
	return nil
}
