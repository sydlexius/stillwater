package artist

import (
	"context"
	"database/sql"
	"fmt"
)

// LibraryMembership records that an artist is observed by a particular
// library. An artist can have many memberships across filesystem and
// connection libraries
type LibraryMembership struct {
	ArtistID  string `json:"artist_id"`
	LibraryID string `json:"library_id"`
	// Source is the discovery channel: 'filesystem', 'emby', 'jellyfin',
	// or 'manual'. Tracked for debugging and future UI affordances.
	Source  string `json:"source"`
	AddedAt string `json:"added_at"`
}

// MembershipRepository manages the M:N relationship between artists and
// libraries via the artist_libraries join table. The Repository interface
// provides legacy library_id-scoped lookups during the multi-phase rollout;
// MembershipRepository is the new authoritative surface.
type MembershipRepository interface {
	// Add inserts (or upserts on conflict) a membership row. Idempotent: a
	// repeat Add with the same (artistID, libraryID) is a no-op (the source
	// and added_at of the existing row are preserved).
	Add(ctx context.Context, artistID, libraryID, source string) error

	// Remove deletes a membership row. No-op if the membership does not
	// exist; returns nil error in that case.
	Remove(ctx context.Context, artistID, libraryID string) error

	// ListForArtist returns every library membership the artist holds.
	ListForArtist(ctx context.Context, artistID string) ([]LibraryMembership, error)

	// CountForArtist returns the number of libraries this artist belongs
	// to. Used by the unlink path to decide whether to prune the artist
	// (zero remaining memberships AND zero platform mappings -> prune).
	CountForArtist(ctx context.Context, artistID string) (int, error)
}

type sqliteMembershipRepo struct {
	db *sql.DB
}

func newSQLiteMembershipRepo(db *sql.DB) *sqliteMembershipRepo {
	return &sqliteMembershipRepo{db: db}
}

func (r *sqliteMembershipRepo) Add(ctx context.Context, artistID, libraryID, source string) error {
	// INSERT OR IGNORE preserves the original added_at on idempotent re-adds,
	// which is the right semantic: "this artist was first seen in this
	// library at <original time>" should not move forward on every populate.
	_, err := r.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO artist_libraries (artist_id, library_id, source, added_at)
		VALUES (?, ?, ?, datetime('now'))
	`, artistID, libraryID, source)
	if err != nil {
		return fmt.Errorf("adding artist library membership: %w", err)
	}
	return nil
}

func (r *sqliteMembershipRepo) Remove(ctx context.Context, artistID, libraryID string) error {
	_, err := r.db.ExecContext(ctx,
		`DELETE FROM artist_libraries WHERE artist_id = ? AND library_id = ?`,
		artistID, libraryID)
	if err != nil {
		return fmt.Errorf("removing artist library membership: %w", err)
	}
	return nil
}

func (r *sqliteMembershipRepo) ListForArtist(ctx context.Context, artistID string) ([]LibraryMembership, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT artist_id, library_id, source, added_at
		FROM artist_libraries
		WHERE artist_id = ?
		ORDER BY added_at, library_id
	`, artistID)
	if err != nil {
		return nil, fmt.Errorf("listing memberships for artist %s: %w", artistID, err)
	}
	defer rows.Close() //nolint:errcheck

	var out []LibraryMembership
	for rows.Next() {
		var m LibraryMembership
		if err := rows.Scan(&m.ArtistID, &m.LibraryID, &m.Source, &m.AddedAt); err != nil {
			return nil, fmt.Errorf("scanning membership row: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating membership rows: %w", err)
	}
	return out, nil
}

func (r *sqliteMembershipRepo) CountForArtist(ctx context.Context, artistID string) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artist_libraries WHERE artist_id = ?`,
		artistID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("counting memberships for artist %s: %w", artistID, err)
	}
	return n, nil
}
