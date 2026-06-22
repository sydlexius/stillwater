package connection

// import.go contains the tx-aware helpers used by the settingsio import
// orchestrator (#1693). They mirror the SQL of Create/Update/GetByTypeAndURL
// but accept a DBExecutor so the orchestrator can run them inside its own
// transaction; a mid-import failure then rolls back the connection writes
// alongside every other section's writes.
//
// Public Create/Update/GetByTypeAndURL signatures are unchanged so non-import
// callers see no surface drift.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/sydlexius/stillwater/internal/dbutil"
)

// DBExecutor is the subset of *sql.DB used by the tx-aware connection import
// helpers. Both *sql.DB and *sql.Tx satisfy it. The interface is local to
// this package to avoid pulling in a settingsio dependency.
type DBExecutor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// ImportGetByTypeAndURLTx is the tx-aware equivalent of GetByTypeAndURL,
// scoped to the import orchestrator so the lookup observes uncommitted
// writes inside the same transaction.
func (s *Service) ImportGetByTypeAndURLTx(ctx context.Context, db DBExecutor, connType, url string) (*Connection, error) {
	row := db.QueryRowContext(ctx, `
		SELECT id, name, type, url, encrypted_api_key, enabled, status, status_message, last_checked_at, created_at, updated_at, feature_library_import, feature_nfo_write, feature_image_write, feature_metadata_push, feature_trigger_refresh, feature_manage_server_files, verify_path_after_update, platform_user_id, platform_server_id, pre_stillwater_config_json
		FROM connections WHERE type = ? AND url = ? ORDER BY created_at DESC LIMIT 1
	`, connType, url)
	c, err := s.scanConnection(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if errors.Is(err, errDecrypt) {
		slog.Warn("treating undecryptable connection as not found for type+url lookup (import tx)",
			"error", err, "type", connType, "url", url)
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting connection by type+url: %w", err)
	}
	return c, nil
}

// ImportCreateTx inserts a new connection via the supplied executor. Mirrors
// Create's SQL exactly so the rows are bit-identical to a non-import insert.
func (s *Service) ImportCreateTx(ctx context.Context, db DBExecutor, c *Connection) error {
	if c == nil {
		return fmt.Errorf("connection is required")
	}
	if err := c.Validate(); err != nil {
		return fmt.Errorf("validating connection: %w", err)
	}
	if c.ID == "" {
		c.ID = uuid.New().String()
	}
	now := time.Now().UTC()
	c.CreatedAt = now
	c.UpdatedAt = now
	if c.Status == "" {
		c.Status = "unknown"
	}
	encKey, err := s.encryptor.Encrypt(c.APIKey)
	if err != nil {
		return fmt.Errorf("encrypting api key: %w", err)
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO connections (id, name, type, url, encrypted_api_key, enabled, status, status_message, last_checked_at, created_at, updated_at, feature_library_import, feature_nfo_write, feature_image_write, feature_metadata_push, feature_trigger_refresh, feature_manage_server_files, verify_path_after_update, platform_user_id, platform_server_id, pre_stillwater_config_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		c.ID, c.Name, c.Type, c.URL, encKey,
		dbutil.BoolToInt(c.Enabled), c.Status, c.StatusMessage,
		dbutil.FormatNullableTime(c.LastCheckedAt),
		now.Format(time.RFC3339), now.Format(time.RFC3339),
		dbutil.BoolToInt(c.GetFeatureLibraryImport()), dbutil.BoolToInt(c.GetFeatureNFOWrite()), dbutil.BoolToInt(c.GetFeatureImageWrite()),
		dbutil.BoolToInt(c.GetFeatureMetadataPush()), dbutil.BoolToInt(c.GetFeatureTriggerRefresh()),
		dbutil.BoolToInt(c.FeatureManageServerFiles),
		dbutil.BoolToInt(c.GetVerifyPathAfterUpdate()),
		c.GetPlatformUserID(), c.GetPlatformServerID(),
		c.PreStillwaterConfigJSON,
	)
	if err != nil {
		return fmt.Errorf("creating connection: %w", err)
	}
	return nil
}

// ImportUpdateTx updates an existing connection via the supplied executor.
// Mirrors Update's SQL exactly.
func (s *Service) ImportUpdateTx(ctx context.Context, db DBExecutor, c *Connection) error {
	if c == nil {
		return fmt.Errorf("connection is required")
	}
	if err := c.Validate(); err != nil {
		return fmt.Errorf("validating connection: %w", err)
	}
	c.UpdatedAt = time.Now().UTC()
	encKey, err := s.encryptor.Encrypt(c.APIKey)
	if err != nil {
		return fmt.Errorf("encrypting api key: %w", err)
	}
	result, err := db.ExecContext(ctx, `
		UPDATE connections SET
			name = ?, type = ?, url = ?, encrypted_api_key = ?, enabled = ?,
			status = ?, status_message = ?, updated_at = ?,
			feature_library_import = ?, feature_nfo_write = ?, feature_image_write = ?,
			feature_metadata_push = ?, feature_trigger_refresh = ?,
			feature_manage_server_files = ?,
			verify_path_after_update = ?,
			platform_user_id = ?, platform_server_id = ?,
			pre_stillwater_config_json = ?
		WHERE id = ?
	`,
		c.Name, c.Type, c.URL, encKey, dbutil.BoolToInt(c.Enabled),
		c.Status, c.StatusMessage,
		c.UpdatedAt.Format(time.RFC3339),
		dbutil.BoolToInt(c.GetFeatureLibraryImport()), dbutil.BoolToInt(c.GetFeatureNFOWrite()), dbutil.BoolToInt(c.GetFeatureImageWrite()),
		dbutil.BoolToInt(c.GetFeatureMetadataPush()), dbutil.BoolToInt(c.GetFeatureTriggerRefresh()),
		dbutil.BoolToInt(c.FeatureManageServerFiles),
		dbutil.BoolToInt(c.GetVerifyPathAfterUpdate()),
		c.GetPlatformUserID(), c.GetPlatformServerID(),
		c.PreStillwaterConfigJSON,
		c.ID,
	)
	if err != nil {
		return fmt.Errorf("updating connection: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking updated connection rows: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("connection not found: %s", c.ID)
	}
	return nil
}
