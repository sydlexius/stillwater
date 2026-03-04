package artist

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/sydlexius/stillwater/internal/dbutil"
)

type sqliteImageRepo struct {
	db *sql.DB
}

func newSQLiteImageRepo(db *sql.DB) *sqliteImageRepo {
	return &sqliteImageRepo{db: db}
}

func (r *sqliteImageRepo) GetForArtist(ctx context.Context, artistID string) ([]ArtistImage, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, artist_id, image_type, slot_index, exists_flag, low_res, placeholder,
			width, height, phash, file_format, source
		FROM artist_images WHERE artist_id = ? ORDER BY image_type, slot_index`,
		artistID)
	if err != nil {
		return nil, fmt.Errorf("getting images for artist %s: %w", artistID, err)
	}
	defer rows.Close() //nolint:errcheck

	return scanImageRows(rows)
}

func (r *sqliteImageRepo) GetForArtists(ctx context.Context, artistIDs []string) (map[string][]ArtistImage, error) {
	if len(artistIDs) == 0 {
		return nil, nil
	}

	placeholders := make([]string, len(artistIDs))
	args := make([]any, len(artistIDs))
	for i, id := range artistIDs {
		placeholders[i] = "?"
		args[i] = id
	}

	query := `SELECT id, artist_id, image_type, slot_index, exists_flag, low_res, placeholder, ` + //nolint:gosec // G202: placeholders are "?" literals
		`width, height, phash, file_format, source ` +
		`FROM artist_images ` +
		`WHERE artist_id IN (` + strings.Join(placeholders, ",") + `) ` +
		`ORDER BY artist_id, image_type, slot_index`
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("batch getting images: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	result := make(map[string][]ArtistImage, len(artistIDs))
	for rows.Next() {
		var img ArtistImage
		var existsFlag, lowRes int
		if err := rows.Scan(
			&img.ID, &img.ArtistID, &img.ImageType, &img.SlotIndex,
			&existsFlag, &lowRes, &img.Placeholder,
			&img.Width, &img.Height, &img.PHash, &img.FileFormat, &img.Source,
		); err != nil {
			return nil, fmt.Errorf("scanning image row: %w", err)
		}
		img.Exists = existsFlag == 1
		img.LowRes = lowRes == 1
		result[img.ArtistID] = append(result[img.ArtistID], img)
	}
	return result, rows.Err()
}

func (r *sqliteImageRepo) Upsert(ctx context.Context, img *ArtistImage) error {
	if img.ID == "" {
		img.ID = uuid.New().String()
	}

	_, err := r.db.ExecContext(ctx, `
		INSERT INTO artist_images (id, artist_id, image_type, slot_index, exists_flag, low_res, placeholder,
			width, height, phash, file_format, source)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(artist_id, image_type, slot_index) DO UPDATE SET
			id = excluded.id,
			exists_flag = excluded.exists_flag,
			low_res = excluded.low_res,
			placeholder = excluded.placeholder,
			width = excluded.width,
			height = excluded.height,
			phash = excluded.phash,
			file_format = excluded.file_format,
			source = excluded.source`,
		img.ID, img.ArtistID, img.ImageType, img.SlotIndex,
		dbutil.BoolToInt(img.Exists), dbutil.BoolToInt(img.LowRes), img.Placeholder,
		img.Width, img.Height, img.PHash, img.FileFormat, img.Source,
	)
	if err != nil {
		return fmt.Errorf("upserting image %s/%s: %w", img.ArtistID, img.ImageType, err)
	}
	return nil
}

func (r *sqliteImageRepo) UpsertAll(ctx context.Context, artistID string, images []ArtistImage) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx, `DELETE FROM artist_images WHERE artist_id = ?`, artistID); err != nil {
		return fmt.Errorf("deleting old images: %w", err)
	}

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO artist_images (id, artist_id, image_type, slot_index, exists_flag, low_res, placeholder,
			width, height, phash, file_format, source)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("preparing insert: %w", err)
	}
	defer stmt.Close() //nolint:errcheck

	for _, img := range images {
		id := img.ID
		if id == "" {
			id = uuid.New().String()
		}
		if _, err := stmt.ExecContext(ctx,
			id, artistID, img.ImageType, img.SlotIndex,
			dbutil.BoolToInt(img.Exists), dbutil.BoolToInt(img.LowRes), img.Placeholder,
			img.Width, img.Height, img.PHash, img.FileFormat, img.Source,
		); err != nil {
			return fmt.Errorf("inserting image %s: %w", img.ImageType, err)
		}
	}

	return tx.Commit()
}

func (r *sqliteImageRepo) DeleteByArtistID(ctx context.Context, artistID string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM artist_images WHERE artist_id = ?`, artistID)
	if err != nil {
		return fmt.Errorf("deleting images for artist %s: %w", artistID, err)
	}
	return nil
}

func scanImageRows(rows *sql.Rows) ([]ArtistImage, error) {
	var images []ArtistImage
	for rows.Next() {
		var img ArtistImage
		var existsFlag, lowRes int
		if err := rows.Scan(
			&img.ID, &img.ArtistID, &img.ImageType, &img.SlotIndex,
			&existsFlag, &lowRes, &img.Placeholder,
			&img.Width, &img.Height, &img.PHash, &img.FileFormat, &img.Source,
		); err != nil {
			return nil, fmt.Errorf("scanning image: %w", err)
		}
		img.Exists = existsFlag == 1
		img.LowRes = lowRes == 1
		images = append(images, img)
	}
	return images, rows.Err()
}
