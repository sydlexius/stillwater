package scraper

// import.go contains the tx-aware helpers used by the settingsio import
// orchestrator (#1693). They mirror the SQL of SaveConfig but accept a
// DBExecutor so the orchestrator can run them inside its own transaction; a
// mid-import failure then rolls back the scraper config writes alongside
// every other section's writes.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// DBExecutor is the subset of *sql.DB used by the tx-aware scraper import
// helpers. Both *sql.DB and *sql.Tx satisfy it.
type DBExecutor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// ImportSaveConfigTx is the tx-aware equivalent of SaveConfig. It performs
// the same upsert (resolving cfg.ID from any pre-existing row for the scope)
// but reads and writes through the supplied executor.
func (s *Service) ImportSaveConfigTx(ctx context.Context, db DBExecutor, scope string, cfg *ScraperConfig, overrides *Overrides) error {
	cfg.Scope = scope
	if cfg.ID == "" {
		var existingID string
		err := db.QueryRowContext(ctx,
			"SELECT id FROM scraper_config WHERE scope = ?", scope,
		).Scan(&existingID)
		if errors.Is(err, sql.ErrNoRows) {
			cfg.ID = uuid.New().String()
		} else if err != nil {
			return fmt.Errorf("checking existing config: %w", err)
		} else {
			cfg.ID = existingID
		}
	}
	return saveConfigRowTx(ctx, db, cfg, overrides)
}

// saveConfigRowTx mirrors saveConfigRow but writes through the supplied
// executor.
func saveConfigRowTx(ctx context.Context, db DBExecutor, cfg *ScraperConfig, overrides *Overrides) error {
	configJSON, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}

	overridesJSON := "{}"
	if overrides != nil {
		data, err := json.Marshal(overrides)
		if err != nil {
			return fmt.Errorf("marshaling overrides: %w", err)
		}
		overridesJSON = string(data)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	_, err = db.ExecContext(ctx, `
		INSERT INTO scraper_config (id, scope, config_json, overrides_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(scope) DO UPDATE SET
			config_json = ?,
			overrides_json = ?,
			updated_at = ?
	`, cfg.ID, cfg.Scope, string(configJSON), overridesJSON, now, now,
		string(configJSON), overridesJSON, now)
	if err != nil {
		return fmt.Errorf("saving config for scope %q: %w", cfg.Scope, err)
	}
	return nil
}
