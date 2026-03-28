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
			width, height, phash, file_format, source, last_written_at
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
		`width, height, phash, file_format, source, last_written_at ` +
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
			&img.LastWrittenAt,
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
			width, height, phash, file_format, source, last_written_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(artist_id, image_type, slot_index) DO UPDATE SET
			id = excluded.id,
			exists_flag = excluded.exists_flag,
			low_res = excluded.low_res,
			placeholder = excluded.placeholder,
			width = excluded.width,
			height = excluded.height,
			phash = excluded.phash,
			file_format = excluded.file_format,
			source = excluded.source,
			last_written_at = excluded.last_written_at`,
		img.ID, img.ArtistID, img.ImageType, img.SlotIndex,
		dbutil.BoolToInt(img.Exists), dbutil.BoolToInt(img.LowRes), img.Placeholder,
		img.Width, img.Height, img.PHash, img.FileFormat, img.Source, img.LastWrittenAt,
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

	// Build a set of (image_type, slot_index) keys present in the incoming data
	// so we can mark absent slots as not-existing afterward.
	type slotKey struct {
		imageType string
		slotIndex int
	}
	incoming := make(map[slotKey]struct{}, len(images))

	// Upsert each incoming image row. ON CONFLICT updates only display fields,
	// leaving provenance columns (phash, source, file_format, last_written_at)
	// untouched so that UpdateProvenance data survives.
	upsertStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO artist_images (id, artist_id, image_type, slot_index, exists_flag, low_res, placeholder,
			width, height, phash, file_format, source, last_written_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, '', '', '', '')
		ON CONFLICT(artist_id, image_type, slot_index) DO UPDATE SET
			exists_flag = excluded.exists_flag,
			low_res     = excluded.low_res,
			placeholder = excluded.placeholder,
			width  = CASE WHEN excluded.width  > 0 THEN excluded.width  ELSE artist_images.width  END,
			height = CASE WHEN excluded.height > 0 THEN excluded.height ELSE artist_images.height END`)
	if err != nil {
		return fmt.Errorf("preparing upsert: %w", err)
	}
	defer upsertStmt.Close() //nolint:errcheck

	for _, img := range images {
		id := img.ID
		if id == "" {
			id = uuid.New().String()
		}
		incoming[slotKey{img.ImageType, img.SlotIndex}] = struct{}{}
		if _, err := upsertStmt.ExecContext(ctx,
			id, artistID, img.ImageType, img.SlotIndex,
			dbutil.BoolToInt(img.Exists), dbutil.BoolToInt(img.LowRes), img.Placeholder,
			img.Width, img.Height,
		); err != nil {
			return fmt.Errorf("upserting image %s/%d: %w", img.ImageType, img.SlotIndex, err)
		}
	}

	// Delete rows for slots that are no longer in the incoming set (e.g., an
	// image type was removed). We fetch existing rows and compare against the
	// incoming set, then delete stale rows individually rather than using a
	// broad DELETE that could race with UpdateProvenance.
	existing, err := tx.QueryContext(ctx,
		`SELECT image_type, slot_index FROM artist_images WHERE artist_id = ?`, artistID)
	if err != nil {
		return fmt.Errorf("querying existing image slots: %w", err)
	}
	defer existing.Close() //nolint:errcheck
	var toRemove []slotKey
	for existing.Next() {
		var k slotKey
		if err := existing.Scan(&k.imageType, &k.slotIndex); err != nil {
			return fmt.Errorf("scanning existing image slot: %w", err)
		}
		if _, ok := incoming[k]; !ok {
			toRemove = append(toRemove, k)
		}
	}
	if err := existing.Err(); err != nil {
		return fmt.Errorf("iterating existing image slots: %w", err)
	}

	if len(toRemove) > 0 {
		delStmt, err := tx.PrepareContext(ctx,
			`DELETE FROM artist_images WHERE artist_id = ? AND image_type = ? AND slot_index = ?`)
		if err != nil {
			return fmt.Errorf("preparing delete for removed slots: %w", err)
		}
		defer delStmt.Close() //nolint:errcheck
		for _, k := range toRemove {
			if _, err := delStmt.ExecContext(ctx, artistID, k.imageType, k.slotIndex); err != nil {
				return fmt.Errorf("deleting removed slot %s/%d: %w", k.imageType, k.slotIndex, err)
			}
		}
	}

	return tx.Commit()
}

// UpdateProvenance updates only the provenance-related fields (phash, source,
// file_format, last_written_at) on an existing artist_images row, identified by
// artist_id + image_type + slot_index. This is a targeted update that does not
// touch display fields (exists_flag, low_res, placeholder, dimensions).
// Returns an error if no matching row exists.
func (r *sqliteImageRepo) UpdateProvenance(ctx context.Context, artistID, imageType string, slotIndex int, phash, source, fileFormat, lastWrittenAt string) error {
	result, err := r.db.ExecContext(ctx, `
		UPDATE artist_images
		SET phash = ?, source = ?, file_format = ?, last_written_at = ?
		WHERE artist_id = ? AND image_type = ? AND slot_index = ?`,
		phash, source, fileFormat, lastWrittenAt,
		artistID, imageType, slotIndex,
	)
	if err != nil {
		return fmt.Errorf("updating image provenance %s/%s/%d: %w", artistID, imageType, slotIndex, err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking rows affected for provenance %s/%s/%d: %w", artistID, imageType, slotIndex, err)
	}
	if n == 0 {
		return fmt.Errorf("no image row found for %s/%s/%d", artistID, imageType, slotIndex)
	}
	return nil
}

func (r *sqliteImageRepo) DeleteByArtistID(ctx context.Context, artistID string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM artist_images WHERE artist_id = ?`, artistID)
	if err != nil {
		return fmt.Errorf("deleting images for artist %s: %w", artistID, err)
	}
	return nil
}

// NewestWriteTimesByArtist returns a map of artist_id to their most recent
// last_written_at timestamp string for all artists in the given library.
// Only artists with at least one non-empty last_written_at are included.
func (r *sqliteImageRepo) NewestWriteTimesByArtist(ctx context.Context, libraryID string) (map[string]string, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT a.id, MAX(ai.last_written_at)
		FROM artist_images ai
		JOIN artists a ON ai.artist_id = a.id
		WHERE a.library_id = ? AND ai.last_written_at != ''
		GROUP BY a.id`,
		libraryID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying newest write times by artist for library %s: %w", libraryID, err)
	}
	defer rows.Close() //nolint:errcheck

	result := make(map[string]string)
	for rows.Next() {
		var artistID, maxWriteTime string
		if err := rows.Scan(&artistID, &maxWriteTime); err != nil {
			return nil, fmt.Errorf("scanning newest write time row: %w", err)
		}
		result[artistID] = maxWriteTime
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating newest write time rows: %w", err)
	}
	return result, nil
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
			&img.LastWrittenAt,
		); err != nil {
			return nil, fmt.Errorf("scanning image: %w", err)
		}
		img.Exists = existsFlag == 1
		img.LowRes = lowRes == 1
		images = append(images, img)
	}
	return images, rows.Err()
}
