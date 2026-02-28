package platform

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Service provides platform profile data operations.
type Service struct {
	db *sql.DB
}

// NewService creates a platform service.
func NewService(db *sql.DB) *Service {
	return &Service{db: db}
}

// List returns all platform profiles ordered by name.
func (s *Service) List(ctx context.Context) ([]Profile, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, is_builtin, is_active, nfo_enabled, nfo_format,
			image_naming, use_symlinks, created_at, updated_at
		FROM platform_profiles ORDER BY name
	`)
	if err != nil {
		return nil, fmt.Errorf("listing platform profiles: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var profiles []Profile
	for rows.Next() {
		p, err := scanProfile(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning platform profile: %w", err)
		}
		profiles = append(profiles, *p)
	}
	return profiles, rows.Err()
}

// GetByID retrieves a profile by ID.
func (s *Service) GetByID(ctx context.Context, id string) (*Profile, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, name, is_builtin, is_active, nfo_enabled, nfo_format,
			image_naming, use_symlinks, created_at, updated_at
		FROM platform_profiles WHERE id = ?
	`, id)
	p, err := scanProfile(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("platform profile not found: %s", id)
	}
	if err != nil {
		return nil, fmt.Errorf("getting platform profile: %w", err)
	}
	return p, nil
}

// GetActive returns the currently active profile.
func (s *Service) GetActive(ctx context.Context) (*Profile, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, name, is_builtin, is_active, nfo_enabled, nfo_format,
			image_naming, use_symlinks, created_at, updated_at
		FROM platform_profiles WHERE is_active = 1 LIMIT 1
	`)
	p, err := scanProfile(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting active profile: %w", err)
	}
	return p, nil
}

// GetByName returns a profile matching the given name, or nil if not found.
func (s *Service) GetByName(ctx context.Context, name string) (*Profile, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, name, is_builtin, is_active, nfo_enabled, nfo_format,
			image_naming, use_symlinks, created_at, updated_at
		FROM platform_profiles WHERE name = ? LIMIT 1
	`, name)
	p, err := scanProfile(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting profile by name: %w", err)
	}
	return p, nil
}

// SetActive makes the given profile the active one (deactivates all others).
func (s *Service) SetActive(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx, `UPDATE platform_profiles SET is_active = 0`); err != nil {
		return fmt.Errorf("deactivating profiles: %w", err)
	}

	result, err := tx.ExecContext(ctx, `UPDATE platform_profiles SET is_active = 1 WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("activating profile: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("platform profile not found: %s", id)
	}

	return tx.Commit()
}

// Create inserts a new custom profile.
func (s *Service) Create(ctx context.Context, p *Profile) error {
	if p.ID == "" {
		p.ID = uuid.New().String()
	}
	now := time.Now().UTC()
	p.CreatedAt = now
	p.UpdatedAt = now

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO platform_profiles (id, name, is_builtin, is_active, nfo_enabled, nfo_format, image_naming, use_symlinks, created_at, updated_at)
		VALUES (?, ?, 0, ?, ?, ?, ?, ?, ?, ?)
	`,
		p.ID, p.Name, boolToInt(p.IsActive), boolToInt(p.NFOEnabled), p.NFOFormat,
		MarshalImageNaming(p.ImageNaming), boolToInt(p.UseSymlinks),
		now.Format(time.RFC3339), now.Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("creating platform profile: %w", err)
	}
	return nil
}

// Update modifies an existing profile.
func (s *Service) Update(ctx context.Context, p *Profile) error {
	p.UpdatedAt = time.Now().UTC()

	_, err := s.db.ExecContext(ctx, `
		UPDATE platform_profiles SET
			name = ?, nfo_enabled = ?, nfo_format = ?, image_naming = ?, use_symlinks = ?, updated_at = ?
		WHERE id = ?
	`,
		p.Name, boolToInt(p.NFOEnabled), p.NFOFormat,
		MarshalImageNaming(p.ImageNaming), boolToInt(p.UseSymlinks),
		p.UpdatedAt.Format(time.RFC3339),
		p.ID,
	)
	if err != nil {
		return fmt.Errorf("updating platform profile: %w", err)
	}
	return nil
}

// Delete removes a profile. Built-in profiles cannot be deleted.
func (s *Service) Delete(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx,
		`DELETE FROM platform_profiles WHERE id = ? AND is_builtin = 0`, id)
	if err != nil {
		return fmt.Errorf("deleting platform profile: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("profile not found or is a built-in profile: %s", id)
	}
	return nil
}

func scanProfile(row interface{ Scan(...any) error }) (*Profile, error) {
	var p Profile
	var isBuiltin, isActive, nfoEnabled, useSymlinks int
	var imageNaming string
	var createdAt, updatedAt string

	err := row.Scan(
		&p.ID, &p.Name, &isBuiltin, &isActive, &nfoEnabled, &p.NFOFormat,
		&imageNaming, &useSymlinks, &createdAt, &updatedAt,
	)
	if err != nil {
		return nil, err
	}

	p.IsBuiltin = isBuiltin == 1
	p.IsActive = isActive == 1
	p.NFOEnabled = nfoEnabled == 1
	p.UseSymlinks = useSymlinks == 1
	p.ImageNaming = UnmarshalImageNaming(imageNaming)
	p.CreatedAt = parseTime(createdAt)
	p.UpdatedAt = parseTime(updatedAt)

	return &p, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func parseTime(s string) time.Time {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	if t, err := time.Parse("2006-01-02 15:04:05", s); err == nil {
		return t
	}
	return time.Time{}
}
