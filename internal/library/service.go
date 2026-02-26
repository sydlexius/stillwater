package library

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

const libraryColumns = `id, name, path, type, source, connection_id, external_id, created_at, updated_at`

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

	if lib.ID == "" {
		lib.ID = uuid.New().String()
	}
	now := time.Now().UTC()
	lib.CreatedAt = now
	lib.UpdatedAt = now

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO libraries (id, name, path, type, source, connection_id, external_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		lib.ID, lib.Name, lib.Path, lib.Type,
		lib.Source, nullableString(lib.ConnectionID), lib.ExternalID,
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

	lib.UpdatedAt = time.Now().UTC()

	result, err := s.db.ExecContext(ctx, `
		UPDATE libraries SET name = ?, path = ?, type = ?, source = ?, connection_id = ?, external_id = ?, updated_at = ?
		WHERE id = ?
	`,
		lib.Name, lib.Path, lib.Type,
		lib.Source, nullableString(lib.ConnectionID), lib.ExternalID,
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
		&createdAt, &updatedAt,
	)
	if err != nil {
		return nil, err
	}

	if connectionID.Valid {
		lib.ConnectionID = connectionID.String
	}
	lib.CreatedAt = parseTime(createdAt)
	lib.UpdatedAt = parseTime(updatedAt)

	return &lib, nil
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
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

// parseTime parses a time string, handling both RFC3339 and SQLite datetime formats.
func parseTime(s string) time.Time {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	if t, err := time.Parse("2006-01-02 15:04:05", s); err == nil {
		return t
	}
	return time.Time{}
}
