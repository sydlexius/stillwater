package library

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/sydlexius/stillwater/internal/dbutil"
)

const libraryColumns = `id, name, path, type, source, connection_id, external_id, fs_watch, fs_poll_interval, shared_fs_status, shared_fs_evidence, shared_fs_peer_library_ids, created_at, updated_at`

// Service provides library data operations.
type Service struct {
	db *sql.DB
}

// NewService creates a library service.
func NewService(db *sql.DB) *Service {
	return &Service{db: db}
}

// Create inserts a new library.
func (s *Service) Create(ctx context.Context, lib *Library) error {
	if lib.Name == "" {
		return fmt.Errorf("library name is required")
	}
	if lib.Type != TypeRegular && lib.Type != TypeClassical {
		return fmt.Errorf("library type must be %q or %q", TypeRegular, TypeClassical)
	}
	if lib.Source == "" {
		lib.Source = SourceManual
	}
	if !isValidSource(lib.Source) {
		return fmt.Errorf("library source must be one of %q, %q, %q, %q", SourceManual, SourceEmby, SourceJellyfin, SourceLidarr)
	}
	if lib.Path != "" {
		cleaned, err := ValidatePath(lib.Path)
		if err != nil {
			return fmt.Errorf("validating library path format: %w", err)
		}
		if err := CheckPathExists(cleaned); err != nil {
			return fmt.Errorf("checking library path exists: %w", err)
		}
		lib.Path = cleaned
	}

	if lib.ID == "" {
		lib.ID = uuid.New().String()
	}
	if lib.FSPollInterval <= 0 || !IsValidPollInterval(lib.FSPollInterval) {
		lib.FSPollInterval = 60
	}
	now := time.Now().UTC()
	lib.CreatedAt = now
	lib.UpdatedAt = now

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO libraries (id, name, path, type, source, connection_id, external_id, fs_watch, fs_poll_interval, shared_fs_status, shared_fs_evidence, shared_fs_peer_library_ids, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		lib.ID, lib.Name, lib.Path, lib.Type,
		lib.Source, dbutil.NullableString(lib.ConnectionID), lib.ExternalID,
		lib.FSWatch, lib.FSPollInterval,
		lib.SharedFSStatus, lib.SharedFSEvidence, lib.SharedFSPeerLibraryIDs,
		now.Format(time.RFC3339), now.Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("creating library: %w", err)
	}
	return nil
}

// GetByID retrieves a library by primary key.
func (s *Service) GetByID(ctx context.Context, id string) (*Library, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+libraryColumns+` FROM libraries WHERE id = ?`, id)
	lib, err := scanLibrary(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("library not found: %s", id)
	}
	if err != nil {
		return nil, fmt.Errorf("getting library by id: %w", err)
	}
	return lib, nil
}

// GetByPath retrieves a library by filesystem path.
// Returns nil, nil when no library matches the path.
func (s *Service) GetByPath(ctx context.Context, path string) (*Library, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+libraryColumns+` FROM libraries WHERE path = ?`, path)
	lib, err := scanLibrary(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting library by path: %w", err)
	}
	return lib, nil
}

// GetByConnectionAndExternalID retrieves a library by connection ID and external ID.
// Returns nil, nil when no library matches.
func (s *Service) GetByConnectionAndExternalID(ctx context.Context, connectionID, externalID string) (*Library, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+libraryColumns+` FROM libraries WHERE connection_id = ? AND external_id = ?`,
		connectionID, externalID)
	lib, err := scanLibrary(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting library by connection+external_id: %w", err)
	}
	return lib, nil
}

// List returns all libraries ordered by name.
func (s *Service) List(ctx context.Context) ([]Library, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+libraryColumns+` FROM libraries ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("listing libraries: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var libs []Library
	for rows.Next() {
		lib, err := scanLibrary(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning library: %w", err)
		}
		libs = append(libs, *lib)
	}
	return libs, rows.Err()
}

// Update modifies an existing library.
func (s *Service) Update(ctx context.Context, lib *Library) error {
	if lib.Name == "" {
		return fmt.Errorf("library name is required")
	}
	if lib.Type != TypeRegular && lib.Type != TypeClassical {
		return fmt.Errorf("library type must be %q or %q", TypeRegular, TypeClassical)
	}
	if lib.Source == "" {
		lib.Source = SourceManual
	}
	if !isValidSource(lib.Source) {
		return fmt.Errorf("library source must be one of %q, %q, %q, %q", SourceManual, SourceEmby, SourceJellyfin, SourceLidarr)
	}
	if lib.Path != "" {
		cleaned, err := ValidatePath(lib.Path)
		if err != nil {
			return fmt.Errorf("validating library path format: %w", err)
		}
		if err := CheckPathExists(cleaned); err != nil {
			return fmt.Errorf("checking library path exists: %w", err)
		}
		lib.Path = cleaned
	}

	if lib.FSPollInterval <= 0 || !IsValidPollInterval(lib.FSPollInterval) {
		lib.FSPollInterval = 60
	}
	lib.UpdatedAt = time.Now().UTC()

	result, err := s.db.ExecContext(ctx, `
		UPDATE libraries SET name = ?, path = ?, type = ?, source = ?, connection_id = ?, external_id = ?, fs_watch = ?, fs_poll_interval = ?, shared_fs_status = ?, shared_fs_evidence = ?, shared_fs_peer_library_ids = ?, updated_at = ?
		WHERE id = ?
	`,
		lib.Name, lib.Path, lib.Type,
		lib.Source, dbutil.NullableString(lib.ConnectionID), lib.ExternalID,
		lib.FSWatch, lib.FSPollInterval,
		lib.SharedFSStatus, lib.SharedFSEvidence, lib.SharedFSPeerLibraryIDs,
		lib.UpdatedAt.Format(time.RFC3339),
		lib.ID,
	)
	if err != nil {
		return fmt.Errorf("updating library: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("library not found: %s", lib.ID)
	}
	return nil
}

// ClearConnectionID sets connection_id to NULL for all libraries referencing
// the given connection. Used before deleting a connection.
func (s *Service) ClearConnectionID(ctx context.Context, connectionID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE libraries SET connection_id = NULL, updated_at = ? WHERE connection_id = ?`,
		time.Now().UTC().Format(time.RFC3339), connectionID)
	if err != nil {
		return fmt.Errorf("clearing connection_id on libraries: %w", err)
	}
	return nil
}

// Delete removes a library by ID. Artists are preserved; their membership
// rows in artist_libraries cascade-delete via the FK. this is
// the "unlink only" path, where the user wants to disconnect the library
// but keep the artists (which may still be observed by other libraries).
//
// The legacy artists.library_id column is also cleared for any artist that
// pointed at the deleted library, so older code paths that still read the
// orphan column do not see a dangling FK. The column will be removed in
// the final phase.
func (s *Service) Delete(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("starting transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// Soft-deprecation cleanup for the orphan artists.library_id column.
	if _, err := tx.ExecContext(ctx,
		`UPDATE artists SET library_id = NULL WHERE library_id = ?`, id); err != nil {
		return fmt.Errorf("clearing artist references: %w", err)
	}

	result, err := tx.ExecContext(ctx, `DELETE FROM libraries WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("deleting library: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("library not found: %s", id)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing delete: %w", err)
	}
	return nil
}

// CountArtists returns the number of artists assigned to a library.
func (s *Service) CountArtists(ctx context.Context, libraryID string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artists WHERE library_id = ?`, libraryID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("counting artists for library: %w", err)
	}
	return count, nil
}

// collectUnlinkCandidates returns the union of artist IDs that hold a
// membership row in the about-to-be-deleted library and artist IDs whose
// legacy artists.library_id column points at it. Split out so the rows
// scopes can use defer (sqlclosecheck-friendly).
func collectUnlinkCandidates(ctx context.Context, tx *sql.Tx, libraryID string) ([]string, error) {
	candidates := []string{}
	seen := make(map[string]bool)

	memberRows, err := tx.QueryContext(ctx,
		`SELECT artist_id FROM artist_libraries WHERE library_id = ?`, libraryID)
	if err != nil {
		return nil, fmt.Errorf("listing memberships for unlink: %w", err)
	}
	defer memberRows.Close() //nolint:errcheck
	for memberRows.Next() {
		var aid string
		if err := memberRows.Scan(&aid); err != nil {
			return nil, fmt.Errorf("scanning membership row: %w", err)
		}
		if !seen[aid] {
			candidates = append(candidates, aid)
			seen[aid] = true
		}
	}
	if err := memberRows.Err(); err != nil {
		return nil, fmt.Errorf("iterating memberships: %w", err)
	}

	legacyRows, err := tx.QueryContext(ctx,
		`SELECT id FROM artists WHERE library_id = ?`, libraryID)
	if err != nil {
		return nil, fmt.Errorf("listing legacy library_id artists: %w", err)
	}
	defer legacyRows.Close() //nolint:errcheck
	for legacyRows.Next() {
		var aid string
		if err := legacyRows.Scan(&aid); err != nil {
			return nil, fmt.Errorf("scanning legacy artist row: %w", err)
		}
		if !seen[aid] {
			candidates = append(candidates, aid)
			seen[aid] = true
		}
	}
	if err := legacyRows.Err(); err != nil {
		return nil, fmt.Errorf("iterating legacy artist rows: %w", err)
	}
	return candidates, nil
}

// DeleteWithArtists removes a library and prunes artists that have no
// other home. Membership-based for the M:N model, with a fallback prune
// for legacy connection-orphan rows that predate artist_libraries:
//
// - Membership prune: for each artist with a membership
// in the deleted library, if zero memberships remain after the
// cascade AND zero platform mappings exist, drop the artist row.
// Artists with sibling-library memberships or live platform mappings
// elsewhere survive.
// - Connection-orphan prune (carryover): when the deleted
// library was the last library on its connection, also drop artists
// whose only platform mapping is on that connection and who had no
// library_id assignment. These are legacy data shapes that the
// migration backfill could not see (no library_id to copy from).
// The "last library on connection" guard avoids deleting data that a
// sibling library still legitimately references.
//
// Order matters: the membership snapshot is taken BEFORE the library
// delete fires the cascade.
func (s *Service) DeleteWithArtists(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("starting transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	var connectionID sql.NullString
	if err := tx.QueryRowContext(ctx,
		`SELECT connection_id FROM libraries WHERE id = ?`, id).Scan(&connectionID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("library not found: %s", id)
		}
		return fmt.Errorf("looking up library: %w", err)
	}

	candidateIDs, err := collectUnlinkCandidates(ctx, tx, id)
	if err != nil {
		return err
	}

	// Soft-deprecation cleanup: the orphan artists.library_id FK still
	// blocks the library delete on legacy / test rows that point at it.
	// Clear it before the delete; final phase removes the column.
	if _, err := tx.ExecContext(ctx,
		`UPDATE artists SET library_id = NULL WHERE library_id = ?`, id); err != nil {
		return fmt.Errorf("clearing legacy library_id refs: %w", err)
	}

	// Drop the library. CASCADE removes its artist_libraries rows.
	result, err := tx.ExecContext(ctx, `DELETE FROM libraries WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("deleting library: %w", err)
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return fmt.Errorf("library not found: %s", id)
	}

	// Decide whether the connection-orphan prune is safe to run.
	connOrphanPruneAllowed := false
	if connectionID.Valid && connectionID.String != "" {
		var siblings int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM libraries WHERE connection_id = ?`,
			connectionID.String).Scan(&siblings); err != nil {
			return fmt.Errorf("counting sibling libraries: %w", err)
		}
		connOrphanPruneAllowed = siblings == 0
	}

	// Candidate prune: an artist is a candidate iff it had explicit
	// presence in the deleted library (membership row OR legacy library_id
	// pointer). Keep it if it has any other home: a remaining membership
	// in some other library, OR a platform mapping on a connection other
	// than the deleted library's connection. Mappings on the SAME
	// connection do not count as "other home" because the user has just
	// unlinked the only library tying them to that connection (or, if a
	// sibling library remains, the sibling will still observe the artist
	// via its own membership row, which would have made the artist a
	// candidate via that sibling's row, not this one).
	for _, aid := range candidateIDs {
		var memberships int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM artist_libraries WHERE artist_id = ?`,
			aid).Scan(&memberships); err != nil {
			return fmt.Errorf("counting remaining memberships for %s: %w", aid, err)
		}
		if memberships > 0 {
			continue
		}
		var otherConnMappings int
		args := []any{aid}
		query := `SELECT COUNT(*) FROM artist_platform_ids WHERE artist_id = ?`
		if connectionID.Valid && connectionID.String != "" {
			query += ` AND connection_id != ?`
			args = append(args, connectionID.String)
		}
		if err := tx.QueryRowContext(ctx, query, args...).Scan(&otherConnMappings); err != nil {
			return fmt.Errorf("counting cross-connection mappings for %s: %w", aid, err)
		}
		if otherConnMappings > 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM artists WHERE id = ?`, aid); err != nil {
			return fmt.Errorf("pruning orphan artist %s: %w", aid, err)
		}
	}

	// Connection-orphan sweep for artists never in candidateIDs (no
	// membership row, no library_id pointer, but a mapping on the
	// just-unlinked connection). Legacy case.
	//
	// The COALESCE(a.library_id, '') = '' guard is required during partial
	// migration: the lines above only cleared library_id pointing at the
	// library being deleted, so an artist can still legitimately reference
	// some other surviving library through the legacy column. Without
	// this guard, a connection-library unlink would silently delete those
	// artists when their only artist_libraries row had not yet been
	// backfilled.
	if connOrphanPruneAllowed {
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM artists
			WHERE id IN (
				SELECT ap.artist_id
				FROM artist_platform_ids ap
				JOIN artists a ON a.id = ap.artist_id
				WHERE ap.connection_id = ?
				 AND COALESCE(a.library_id, '') = ''
				 AND ap.artist_id NOT IN (
					SELECT artist_id FROM artist_libraries
				 )
				 AND ap.artist_id NOT IN (
					SELECT artist_id FROM artist_platform_ids WHERE connection_id != ?
				 )
			)
		`, connectionID.String, connectionID.String); err != nil {
			return fmt.Errorf("sweeping connection-orphan artists: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing delete: %w", err)
	}
	return nil
}

// ListByConnectionID returns all libraries associated with a connection.
func (s *Service) ListByConnectionID(ctx context.Context, connectionID string) ([]Library, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+libraryColumns+` FROM libraries WHERE connection_id = ? ORDER BY name`, connectionID)
	if err != nil {
		return nil, fmt.Errorf("listing libraries by connection: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var libs []Library
	for rows.Next() {
		lib, err := scanLibrary(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning library: %w", err)
		}
		libs = append(libs, *lib)
	}
	return libs, rows.Err()
}

// CountArtistsByConnectionID returns the total number of artists across all
// libraries belonging to a connection.
func (s *Service) CountArtistsByConnectionID(ctx context.Context, connectionID string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artists WHERE library_id IN (SELECT id FROM libraries WHERE connection_id = ?)`,
		connectionID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("counting artists for connection: %w", err)
	}
	return count, nil
}

// scanLibrary scans a database row into a Library struct.
func scanLibrary(row interface{ Scan(...any) error }) (*Library, error) {
	var lib Library
	var connectionID sql.NullString
	var createdAt, updatedAt string

	err := row.Scan(
		&lib.ID, &lib.Name, &lib.Path, &lib.Type,
		&lib.Source, &connectionID, &lib.ExternalID,
		&lib.FSWatch, &lib.FSPollInterval,
		&lib.SharedFSStatus, &lib.SharedFSEvidence, &lib.SharedFSPeerLibraryIDs,
		&createdAt, &updatedAt,
	)
	if err != nil {
		return nil, err
	}

	if connectionID.Valid {
		lib.ConnectionID = connectionID.String
	}
	lib.CreatedAt = dbutil.ParseTime(createdAt)
	lib.UpdatedAt = dbutil.ParseTime(updatedAt)

	return &lib, nil
}

// SetSharedFSStatus updates the shared-filesystem status, evidence, and peer
// library IDs on a library. The status must be one of the SharedFS* constants
// or an empty string (to clear / reset to unknown).
func (s *Service) SetSharedFSStatus(ctx context.Context, id, status, evidence, peerIDs string) error {
	if !isValidSharedFSStatus(status) {
		return fmt.Errorf("invalid shared_fs_status %q", status)
	}
	now := time.Now().UTC()

	// When setting to "suspected", guard against downgrading a library that
	// was concurrently promoted to "confirmed" by another request. The WHERE
	// clause ensures the UPDATE is a no-op if the current status is already
	// stronger.
	query := `UPDATE libraries SET shared_fs_status = ?, shared_fs_evidence = ?, shared_fs_peer_library_ids = ?, updated_at = ? WHERE id = ?`
	if status == SharedFSSuspected {
		query = `UPDATE libraries SET shared_fs_status = ?, shared_fs_evidence = ?, shared_fs_peer_library_ids = ?, updated_at = ? WHERE id = ? AND shared_fs_status != 'confirmed'`
	}

	result, err := s.db.ExecContext(ctx, query,
		status, evidence, peerIDs, now.Format(time.RFC3339), id)
	if err != nil {
		return fmt.Errorf("setting shared_fs_status: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		if status != SharedFSSuspected {
			return fmt.Errorf("library not found: %s", id)
		}
		// Guarded update: rows=0 means either "already confirmed" (expected)
		// or "library not found" (bug). Distinguish with an existence check.
		var exists int
		existErr := s.db.QueryRowContext(ctx,
			`SELECT 1 FROM libraries WHERE id = ?`, id).Scan(&exists)
		if errors.Is(existErr, sql.ErrNoRows) {
			return fmt.Errorf("library not found: %s", id)
		}
		if existErr != nil {
			return fmt.Errorf("checking library existence after guarded shared_fs update: %w", existErr)
		}
		// Library exists but is already confirmed; no-op is correct.
	}
	return nil
}

// ListSharedFS returns all libraries with a suspected or confirmed shared-filesystem status.
func (s *Service) ListSharedFS(ctx context.Context) ([]Library, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+libraryColumns+` FROM libraries WHERE shared_fs_status IN (?, ?) ORDER BY name`,
		SharedFSSuspected, SharedFSConfirmed)
	if err != nil {
		return nil, fmt.Errorf("listing shared-filesystem libraries: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var libs []Library
	for rows.Next() {
		lib, err := scanLibrary(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning shared library: %w", err)
		}
		libs = append(libs, *lib)
	}
	return libs, rows.Err()
}

// isValidSharedFSStatus reports whether s is an allowed shared-filesystem status.
func isValidSharedFSStatus(s string) bool {
	switch s {
	case "", SharedFSNone, SharedFSSuspected, SharedFSConfirmed:
		return true
	default:
		return false
	}
}

// HasLocalLibrary reports whether at least one library has a non-empty filesystem
// path configured. Libraries without a path are API-only and cannot support
// filesystem-dependent rule checks (NFO existence, image file analysis, etc.).
func (s *Service) HasLocalLibrary(ctx context.Context) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM libraries WHERE path != '' AND path IS NOT NULL`).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("checking for local libraries: %w", err)
	}
	return count > 0, nil
}

// isValidSource reports whether s is one of the allowed library source values.
func isValidSource(s string) bool {
	switch s {
	case SourceManual, SourceEmby, SourceJellyfin, SourceLidarr:
		return true
	default:
		return false
	}
}
