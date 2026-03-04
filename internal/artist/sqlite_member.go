package artist

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/sydlexius/stillwater/internal/dbutil"
)

type sqliteMemberRepo struct {
	db *sql.DB
}

func newSQLiteMemberRepo(db *sql.DB) *sqliteMemberRepo {
	return &sqliteMemberRepo{db: db}
}

func (r *sqliteMemberRepo) ListByArtistID(ctx context.Context, artistID string) ([]BandMember, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, artist_id, member_name, member_mbid, instruments, vocal_type,
			date_joined, date_left, is_original_member, sort_order, created_at, updated_at
		FROM band_members WHERE artist_id = ? ORDER BY sort_order, member_name
	`, artistID)
	if err != nil {
		return nil, fmt.Errorf("listing band members: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var members []BandMember
	for rows.Next() {
		m, err := scanMember(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning band member: %w", err)
		}
		members = append(members, *m)
	}
	return members, rows.Err()
}

func (r *sqliteMemberRepo) Create(ctx context.Context, m *BandMember) error {
	if m.ID == "" {
		m.ID = uuid.New().String()
	}
	now := time.Now().UTC()
	m.CreatedAt = now
	m.UpdatedAt = now

	_, err := r.db.ExecContext(ctx, `
		INSERT INTO band_members (
			id, artist_id, member_name, member_mbid, instruments, vocal_type,
			date_joined, date_left, is_original_member, sort_order, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		m.ID, m.ArtistID, m.MemberName, m.MemberMBID,
		MarshalStringSlice(m.Instruments), m.VocalType,
		m.DateJoined, m.DateLeft, dbutil.BoolToInt(m.IsOriginalMember), m.SortOrder,
		now.Format(time.RFC3339), now.Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("creating band member: %w", err)
	}
	return nil
}

func (r *sqliteMemberRepo) Delete(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM band_members WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("deleting band member: %w", err)
	}
	return nil
}

func (r *sqliteMemberRepo) DeleteByArtistID(ctx context.Context, artistID string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM band_members WHERE artist_id = ?`, artistID)
	if err != nil {
		return fmt.Errorf("deleting band members for artist: %w", err)
	}
	return nil
}

func (r *sqliteMemberRepo) Upsert(ctx context.Context, artistID string, members []BandMember) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx, `DELETE FROM band_members WHERE artist_id = ?`, artistID); err != nil {
		return fmt.Errorf("clearing existing members: %w", err)
	}

	now := time.Now().UTC()
	for i := range members {
		m := &members[i]
		if m.ID == "" {
			m.ID = uuid.New().String()
		}
		m.ArtistID = artistID
		m.CreatedAt = now
		m.UpdatedAt = now

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO band_members (
				id, artist_id, member_name, member_mbid, instruments, vocal_type,
				date_joined, date_left, is_original_member, sort_order, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`,
			m.ID, m.ArtistID, m.MemberName, m.MemberMBID,
			MarshalStringSlice(m.Instruments), m.VocalType,
			m.DateJoined, m.DateLeft, dbutil.BoolToInt(m.IsOriginalMember), m.SortOrder,
			now.Format(time.RFC3339), now.Format(time.RFC3339),
		); err != nil {
			return fmt.Errorf("inserting member %s: %w", m.MemberName, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing member upsert: %w", err)
	}
	return nil
}
