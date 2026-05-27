package platform

// import.go contains the tx-aware helpers used by the settingsio import
// orchestrator (#1693). They mirror the SQL of Create/Update/GetByName but
// accept a DBExecutor so the orchestrator can run them inside its own
// transaction; a mid-import failure then rolls back the platform profile
// writes alongside every other section's writes.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/sydlexius/stillwater/internal/dbutil"
)

// DBExecutor is the subset of *sql.DB used by the tx-aware platform import
// helpers. Both *sql.DB and *sql.Tx satisfy it.
type DBExecutor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// ImportGetByNameTx is the tx-aware equivalent of GetByName.
func (s *Service) ImportGetByNameTx(ctx context.Context, db DBExecutor, name string) (*Profile, error) {
	row := db.QueryRowContext(ctx, `
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

// ImportCreateTx inserts a new profile via the supplied executor. Mirrors
// Create's SQL exactly.
func (s *Service) ImportCreateTx(ctx context.Context, db DBExecutor, p *Profile) error {
	if p == nil {
		return fmt.Errorf("platform profile is required")
	}
	if p.ID == "" {
		p.ID = uuid.New().String()
	}
	now := time.Now().UTC()
	p.CreatedAt = now
	p.UpdatedAt = now

	_, err := db.ExecContext(ctx, `
		INSERT INTO platform_profiles (id, name, is_builtin, is_active, nfo_enabled, nfo_format, image_naming, use_symlinks, created_at, updated_at)
		VALUES (?, ?, 0, ?, ?, ?, ?, ?, ?, ?)
	`,
		p.ID, p.Name, dbutil.BoolToInt(p.IsActive), dbutil.BoolToInt(p.NFOEnabled), p.NFOFormat,
		MarshalImageNaming(p.ImageNaming), dbutil.BoolToInt(p.UseSymlinks),
		now.Format(time.RFC3339), now.Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("creating platform profile: %w", err)
	}
	return nil
}

// ImportUpdateTx updates an existing profile via the supplied executor.
// Mirrors Update's SQL exactly.
func (s *Service) ImportUpdateTx(ctx context.Context, db DBExecutor, p *Profile) error {
	if p == nil {
		return fmt.Errorf("platform profile is required")
	}
	p.UpdatedAt = time.Now().UTC()

	result, err := db.ExecContext(ctx, `
		UPDATE platform_profiles SET
			name = ?, nfo_enabled = ?, nfo_format = ?, image_naming = ?, use_symlinks = ?, updated_at = ?
		WHERE id = ?
	`,
		p.Name, dbutil.BoolToInt(p.NFOEnabled), p.NFOFormat,
		MarshalImageNaming(p.ImageNaming), dbutil.BoolToInt(p.UseSymlinks),
		p.UpdatedAt.Format(time.RFC3339),
		p.ID,
	)
	if err != nil {
		return fmt.Errorf("updating platform profile: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking updated platform profile rows: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("platform profile not found: %s", p.ID)
	}
	return nil
}
