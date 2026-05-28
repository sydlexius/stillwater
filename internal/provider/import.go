package provider

// import.go contains the tx-aware helpers used by the settingsio import
// orchestrator (#1693). They mirror the SQL of SetAPIKey / SetPriority /
// SetDisabledProviders but accept a DBExecutor so the orchestrator can run
// them inside its own transaction; a mid-import failure then rolls back the
// provider settings writes alongside every other section's writes.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// DBExecutor is the subset of *sql.DB used by the tx-aware provider settings
// import helpers. Both *sql.DB and *sql.Tx satisfy it.
type DBExecutor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// ImportSetAPIKeyTx writes an encrypted API key plus a fresh status delete
// through the supplied executor. Unlike SetAPIKey it does NOT open its own
// transaction -- the orchestrator's tx covers both writes.
func (s *SettingsService) ImportSetAPIKeyTx(ctx context.Context, db DBExecutor, name ProviderName, apiKey string) error {
	encrypted, err := s.encryptor.Encrypt(apiKey)
	if err != nil {
		return fmt.Errorf("encrypting API key for %s: %w", name, err)
	}
	key := apiKeySettingKey(name)
	if _, err := db.ExecContext(ctx,
		"INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = ?, updated_at = datetime('now')",
		key, encrypted, encrypted,
	); err != nil {
		return fmt.Errorf("storing API key for %s: %w", name, err)
	}
	// Clear stale status so the key shows as "untested" until re-verified.
	statusKey := keyStatusSettingKey(name)
	if _, err := db.ExecContext(ctx, "DELETE FROM settings WHERE key = ?", statusKey); err != nil {
		return fmt.Errorf("clearing key status for %s: %w", name, err)
	}
	return nil
}

// ImportSetPriorityTx mirrors SetPriority but writes through the supplied
// executor instead of s.db.
func (s *SettingsService) ImportSetPriorityTx(ctx context.Context, db DBExecutor, field string, providers []ProviderName) error {
	data, err := json.Marshal(providers)
	if err != nil {
		return fmt.Errorf("marshaling priority for %s: %w", field, err)
	}
	key := prioritySettingKey(field)
	_, err = db.ExecContext(ctx,
		"INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = ?, updated_at = datetime('now')",
		key, string(data), string(data),
	)
	if err != nil {
		return fmt.Errorf("storing priority for %s: %w", field, err)
	}
	return nil
}

// ImportSetDisabledProvidersTx mirrors SetDisabledProviders but writes through
// the supplied executor instead of s.db.
func (s *SettingsService) ImportSetDisabledProvidersTx(ctx context.Context, db DBExecutor, field string, disabled []ProviderName) error {
	key := priorityDisabledKey(field)
	if len(disabled) == 0 {
		_, err := db.ExecContext(ctx, "DELETE FROM settings WHERE key = ?", key)
		if err != nil {
			return fmt.Errorf("clearing disabled providers for %s: %w", field, err)
		}
		return nil
	}
	data, err := json.Marshal(disabled)
	if err != nil {
		return fmt.Errorf("marshaling disabled providers for %s: %w", field, err)
	}
	_, err = db.ExecContext(ctx,
		"INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = ?, updated_at = datetime('now')",
		key, string(data), string(data),
	)
	if err != nil {
		return fmt.Errorf("storing disabled providers for %s: %w", field, err)
	}
	return nil
}
