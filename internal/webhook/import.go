package webhook

// import.go contains the tx-aware helpers used by the settingsio import
// orchestrator (#1693). They mirror the SQL of Create/Update/GetByNameAndURL
// but accept a DBExecutor so the orchestrator can run them inside its own
// transaction; a mid-import failure then rolls back the webhook writes
// alongside every other section's writes.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// DBExecutor is the subset of *sql.DB used by the tx-aware webhook import
// helpers. Both *sql.DB and *sql.Tx satisfy it.
type DBExecutor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// ImportGetByNameAndURLTx is the tx-aware equivalent of GetByNameAndURL.
func (s *Service) ImportGetByNameAndURLTx(ctx context.Context, db DBExecutor, name, url string) (*Webhook, error) {
	row := db.QueryRowContext(ctx, `
		SELECT id, name, url, type, events, enabled, created_at, updated_at
		FROM webhooks WHERE name = ? AND url = ? LIMIT 1
	`, name, url)
	w, err := scanWebhook(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("looking up webhook by name+url: %w", err)
	}
	return w, nil
}

// ImportCreateTx inserts a new webhook via the supplied executor. Mirrors
// Create's SQL exactly.
func (s *Service) ImportCreateTx(ctx context.Context, db DBExecutor, w *Webhook) error {
	if w == nil {
		return fmt.Errorf("webhook is required")
	}
	if w.Name == "" {
		return fmt.Errorf("name is required")
	}
	if w.URL == "" {
		return fmt.Errorf("url is required")
	}
	if w.Type == "" {
		w.Type = TypeGeneric
	}

	now := time.Now().UTC()
	if w.ID == "" {
		w.ID = uuid.New().String()
	}
	w.CreatedAt = now
	w.UpdatedAt = now

	eventsJSON, err := json.Marshal(w.Events)
	if err != nil {
		return fmt.Errorf("marshaling events: %w", err)
	}

	_, err = db.ExecContext(ctx, `
		INSERT INTO webhooks (id, name, url, type, events, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, w.ID, w.Name, w.URL, w.Type, string(eventsJSON), w.Enabled, now.Format(time.RFC3339), now.Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("inserting webhook: %w", err)
	}
	return nil
}

// ImportUpdateTx updates an existing webhook via the supplied executor.
// Mirrors Update's SQL exactly.
func (s *Service) ImportUpdateTx(ctx context.Context, db DBExecutor, w *Webhook) error {
	if w == nil {
		return fmt.Errorf("webhook is required")
	}
	w.UpdatedAt = time.Now().UTC()

	eventsJSON, err := json.Marshal(w.Events)
	if err != nil {
		return fmt.Errorf("marshaling events: %w", err)
	}

	result, err := db.ExecContext(ctx, `
		UPDATE webhooks SET name = ?, url = ?, type = ?, events = ?, enabled = ?, updated_at = ?
		WHERE id = ?
	`, w.Name, w.URL, w.Type, string(eventsJSON), w.Enabled, w.UpdatedAt.Format(time.RFC3339), w.ID)
	if err != nil {
		return fmt.Errorf("updating webhook: %w", err)
	}

	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking updated webhook rows: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("webhook not found")
	}
	return nil
}
