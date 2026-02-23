package connection

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/sydlexius/stillwater/internal/encryption"
)

// errDecrypt is a sentinel indicating an API key decryption failure.
var errDecrypt = errors.New("decrypt")

// Service provides connection data operations.
type Service struct {
	db        *sql.DB
	encryptor *encryption.Encryptor
}

// NewService creates a connection service.
func NewService(db *sql.DB, enc *encryption.Encryptor) *Service {
	return &Service{db: db, encryptor: enc}
}

// Create inserts a new connection. The APIKey is encrypted before storage.
func (s *Service) Create(ctx context.Context, c *Connection) error {
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

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO connections (id, name, type, url, encrypted_api_key, enabled, status, status_message, last_checked_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		c.ID, c.Name, c.Type, c.URL, encKey,
		boolToInt(c.Enabled), c.Status, c.StatusMessage,
		formatNullableTime(c.LastCheckedAt),
		now.Format(time.RFC3339), now.Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("creating connection: %w", err)
	}
	return nil
}

// GetByID retrieves a connection by ID with API key decrypted.
func (s *Service) GetByID(ctx context.Context, id string) (*Connection, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, name, type, url, encrypted_api_key, enabled, status, status_message, last_checked_at, created_at, updated_at
		FROM connections WHERE id = ?
	`, id)
	c, err := s.scanConnection(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("connection not found: %s", id)
	}
	if err != nil {
		return nil, fmt.Errorf("getting connection: %w", err)
	}
	return c, nil
}

// List returns all connections with API keys decrypted.
func (s *Service) List(ctx context.Context) ([]Connection, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, type, url, encrypted_api_key, enabled, status, status_message, last_checked_at, created_at, updated_at
		FROM connections ORDER BY name
	`)
	if err != nil {
		return nil, fmt.Errorf("listing connections: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var connections []Connection
	for rows.Next() {
		c, scanErr := s.scanConnection(rows)
		if scanErr != nil {
			if errors.Is(scanErr, errDecrypt) {
				slog.Warn("skipping connection with undecryptable key", "error", scanErr)
				continue
			}
			return nil, fmt.Errorf("scanning connection row: %w", scanErr)
		}
		connections = append(connections, *c)
	}
	return connections, rows.Err()
}

// ListByType returns connections filtered by type.
func (s *Service) ListByType(ctx context.Context, connType string) ([]Connection, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, type, url, encrypted_api_key, enabled, status, status_message, last_checked_at, created_at, updated_at
		FROM connections WHERE type = ? ORDER BY name
	`, connType)
	if err != nil {
		return nil, fmt.Errorf("listing connections by type: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var connections []Connection
	for rows.Next() {
		c, scanErr := s.scanConnection(rows)
		if scanErr != nil {
			if errors.Is(scanErr, errDecrypt) {
				slog.Warn("skipping connection with undecryptable key", "error", scanErr)
				continue
			}
			return nil, fmt.Errorf("scanning connection row: %w", scanErr)
		}
		connections = append(connections, *c)
	}
	return connections, rows.Err()
}

// GetByTypeAndURL returns the most recently created connection matching type and URL, or nil if none.
func (s *Service) GetByTypeAndURL(ctx context.Context, connType, url string) (*Connection, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, name, type, url, encrypted_api_key, enabled, status, status_message, last_checked_at, created_at, updated_at
		FROM connections WHERE type = ? AND url = ? ORDER BY created_at DESC LIMIT 1
	`, connType, url)
	c, err := s.scanConnection(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if errors.Is(err, errDecrypt) {
		slog.Warn("treating undecryptable connection as not found for type+url lookup",
			"error", err, "type", connType, "url", url)
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting connection by type+url: %w", err)
	}
	return c, nil
}

// DeduplicateByTypeURL removes duplicate connection rows, keeping only the most
// recent row per type+url combination. Returns the number of rows removed.
func (s *Service) DeduplicateByTypeURL(ctx context.Context) (int64, error) {
	result, err := s.db.ExecContext(ctx, `
		DELETE FROM connections WHERE id NOT IN (
			SELECT id FROM (
				SELECT id, ROW_NUMBER() OVER (PARTITION BY type, url ORDER BY created_at DESC) AS rn
				FROM connections
			) WHERE rn = 1
		)
	`)
	if err != nil {
		return 0, fmt.Errorf("deduplicating connections: %w", err)
	}
	return result.RowsAffected()
}

// Update modifies an existing connection. If APIKey is non-empty, it re-encrypts.
func (s *Service) Update(ctx context.Context, c *Connection) error {
	c.UpdatedAt = time.Now().UTC()

	encKey, err := s.encryptor.Encrypt(c.APIKey)
	if err != nil {
		return fmt.Errorf("encrypting api key: %w", err)
	}

	result, err := s.db.ExecContext(ctx, `
		UPDATE connections SET
			name = ?, type = ?, url = ?, encrypted_api_key = ?, enabled = ?,
			status = ?, status_message = ?, updated_at = ?
		WHERE id = ?
	`,
		c.Name, c.Type, c.URL, encKey, boolToInt(c.Enabled),
		c.Status, c.StatusMessage,
		c.UpdatedAt.Format(time.RFC3339),
		c.ID,
	)
	if err != nil {
		return fmt.Errorf("updating connection: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("connection not found: %s", c.ID)
	}
	return nil
}

// Delete removes a connection by ID.
func (s *Service) Delete(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM connections WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("deleting connection: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("connection not found: %s", id)
	}
	return nil
}

// UpdateStatus sets the status, status message, and last checked time.
func (s *Service) UpdateStatus(ctx context.Context, id, status, statusMessage string) error {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `
		UPDATE connections SET status = ?, status_message = ?, last_checked_at = ?, updated_at = ?
		WHERE id = ?
	`, status, statusMessage, now.Format(time.RFC3339), now.Format(time.RFC3339), id)
	if err != nil {
		return fmt.Errorf("updating connection status: %w", err)
	}
	return nil
}

func (s *Service) scanConnection(row interface{ Scan(...any) error }) (*Connection, error) {
	var c Connection
	var encKey string
	var enabled int
	var lastCheckedAt sql.NullString
	var createdAt, updatedAt string

	err := row.Scan(
		&c.ID, &c.Name, &c.Type, &c.URL, &encKey,
		&enabled, &c.Status, &c.StatusMessage,
		&lastCheckedAt,
		&createdAt, &updatedAt,
	)
	if err != nil {
		return nil, err
	}

	if encKey != "" {
		apiKey, decErr := s.encryptor.Decrypt(encKey)
		if decErr != nil {
			return nil, fmt.Errorf("%w: api key for connection %s: %w", errDecrypt, c.ID, decErr)
		}
		c.APIKey = apiKey
	}
	c.Enabled = enabled == 1
	c.CreatedAt = parseTime(createdAt)
	c.UpdatedAt = parseTime(updatedAt)

	if lastCheckedAt.Valid {
		t := parseTime(lastCheckedAt.String)
		c.LastCheckedAt = &t
	}

	return &c, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func formatNullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.Format(time.RFC3339)
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
