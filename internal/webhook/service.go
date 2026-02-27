package webhook

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Service manages webhook CRUD operations.
type Service struct {
	db *sql.DB
}

// NewService creates a webhook service.
func NewService(db *sql.DB) *Service {
	return &Service{db: db}
}

// Create inserts a new webhook.
func (s *Service) Create(ctx context.Context, w *Webhook) error {
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
	w.ID = uuid.New().String()
	w.CreatedAt = now
	w.UpdatedAt = now

	eventsJSON, err := json.Marshal(w.Events)
	if err != nil {
		return fmt.Errorf("marshaling events: %w", err)
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO webhooks (id, name, url, type, events, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, w.ID, w.Name, w.URL, w.Type, string(eventsJSON), w.Enabled, now.Format(time.RFC3339), now.Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("inserting webhook: %w", err)
	}
	return nil
}

// GetByID returns a webhook by ID.
func (s *Service) GetByID(ctx context.Context, id string) (*Webhook, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, name, url, type, events, enabled, created_at, updated_at
		FROM webhooks WHERE id = ?
	`, id)
	return scanWebhook(row)
}

// List returns all webhooks ordered by name.
func (s *Service) List(ctx context.Context) ([]Webhook, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, url, type, events, enabled, created_at, updated_at
		FROM webhooks ORDER BY name
	`)
	if err != nil {
		return nil, fmt.Errorf("listing webhooks: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var webhooks []Webhook
	for rows.Next() {
		w, err := scanWebhookRow(rows)
		if err != nil {
			return nil, err
		}
		webhooks = append(webhooks, *w)
	}
	return webhooks, rows.Err()
}

// ListByEvent returns all enabled webhooks subscribed to the given event type.
func (s *Service) ListByEvent(ctx context.Context, eventType string) ([]Webhook, error) {
	all, err := s.List(ctx)
	if err != nil {
		return nil, err
	}

	var matched []Webhook
	for _, w := range all {
		if !w.Enabled {
			continue
		}
		for _, e := range w.Events {
			if e == eventType {
				matched = append(matched, w)
				break
			}
		}
	}
	return matched, nil
}

// Update modifies an existing webhook.
func (s *Service) Update(ctx context.Context, w *Webhook) error {
	w.UpdatedAt = time.Now().UTC()

	eventsJSON, err := json.Marshal(w.Events)
	if err != nil {
		return fmt.Errorf("marshaling events: %w", err)
	}

	result, err := s.db.ExecContext(ctx, `
		UPDATE webhooks SET name = ?, url = ?, type = ?, events = ?, enabled = ?, updated_at = ?
		WHERE id = ?
	`, w.Name, w.URL, w.Type, string(eventsJSON), w.Enabled, w.UpdatedAt.Format(time.RFC3339), w.ID)
	if err != nil {
		return fmt.Errorf("updating webhook: %w", err)
	}

	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("webhook not found")
	}
	return nil
}

// GetByNameAndURL returns a webhook matching the given name and URL, or nil if not found.
func (s *Service) GetByNameAndURL(ctx context.Context, name, url string) (*Webhook, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, name, url, type, events, enabled, created_at, updated_at
		FROM webhooks WHERE name = ? AND url = ? LIMIT 1
	`, name, url)
	w, err := scanWebhook(row)
	if err != nil {
		return nil, nil //nolint:nilerr // not found is expected
	}
	return w, nil
}

// Delete removes a webhook by ID.
func (s *Service) Delete(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM webhooks WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("deleting webhook: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("webhook not found")
	}
	return nil
}

// scanner interface for both *sql.Row and *sql.Rows
type scanner interface {
	Scan(dest ...any) error
}

func scanWebhookFromScanner(s scanner) (*Webhook, error) {
	var w Webhook
	var eventsJSON, createdAt, updatedAt string
	var enabled int

	if err := s.Scan(&w.ID, &w.Name, &w.URL, &w.Type, &eventsJSON, &enabled, &createdAt, &updatedAt); err != nil {
		return nil, fmt.Errorf("scanning webhook: %w", err)
	}

	w.Enabled = enabled != 0
	if err := json.Unmarshal([]byte(eventsJSON), &w.Events); err != nil {
		w.Events = []string{}
	}
	w.CreatedAt = parseTime(createdAt)
	w.UpdatedAt = parseTime(updatedAt)

	return &w, nil
}

func scanWebhook(row *sql.Row) (*Webhook, error) {
	return scanWebhookFromScanner(row)
}

func scanWebhookRow(rows *sql.Rows) (*Webhook, error) {
	return scanWebhookFromScanner(rows)
}

func parseTime(s string) time.Time {
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}
