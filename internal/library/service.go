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

// Delete removes a library by ID. Any artists referencing the library
// are dereferenced (library_id set to NULL) before the row is removed.
func (s *Service) Delete(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("starting transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// Clear artist references so the foreign key constraint is satisfied.
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

// DeleteWithArtists removes a library and all artists belonging to it in a
// single transaction. Band members are cleaned up by ON DELETE CASCADE.
func (s *Service) DeleteWithArtists(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("starting transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM artists WHERE library_id = ?`, id); err != nil {
		return fmt.Errorf("deleting library artists: %w", err)
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

// isValidSource reports whether s is one of the allowed library source values.
func isValidSource(s string) bool {
	switch s {
	case SourceManual, SourceEmby, SourceJellyfin, SourceLidarr:
		return true
	default:
		return false
	}
}
