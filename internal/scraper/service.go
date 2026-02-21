package scraper

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
)

// Service provides CRUD operations for scraper configuration.
type Service struct {
	db     *sql.DB
	logger *slog.Logger
}

// NewService creates a new scraper configuration service.
func NewService(db *sql.DB, logger *slog.Logger) *Service {
	return &Service{
		db:     db,
		logger: logger.With(slog.String("component", "scraper-service")),
	}
}

// SeedDefaults inserts the default global scraper configuration if none exists.
func (s *Service) SeedDefaults(ctx context.Context) error {
	var count int
	err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM scraper_config WHERE scope = ?", ScopeGlobal,
	).Scan(&count)
	if err != nil {
		return fmt.Errorf("checking for existing global config: %w", err)
	}
	if count > 0 {
		return nil
	}

	cfg := DefaultConfig()
	cfg.ID = uuid.New().String()
	return s.saveConfigRow(ctx, cfg, nil)
}

// GetConfig returns the effective scraper configuration for a scope.
// For the global scope, returns the global config directly.
// For a connection scope, returns the global config with connection overrides merged in.
func (s *Service) GetConfig(ctx context.Context, scope string) (*ScraperConfig, error) {
	if scope == ScopeGlobal {
		return s.loadConfig(ctx, ScopeGlobal)
	}

	global, err := s.loadConfig(ctx, ScopeGlobal)
	if err != nil {
		return nil, fmt.Errorf("loading global config: %w", err)
	}

	conn, overrides, err := s.loadConfigWithOverrides(ctx, scope)
	if err != nil {
		// No connection config means full inheritance from global
		return global, nil //nolint:nilerr
	}

	return mergeConfigs(global, conn, overrides), nil
}

// GetRawConfig returns the unmerged configuration and overrides for a scope.
// For the global scope, overrides will be nil.
func (s *Service) GetRawConfig(ctx context.Context, scope string) (*ScraperConfig, *Overrides, error) {
	if scope == ScopeGlobal {
		cfg, err := s.loadConfig(ctx, ScopeGlobal)
		return cfg, nil, err
	}
	cfg, overrides, err := s.loadConfigWithOverrides(ctx, scope)
	if err != nil {
		return nil, nil, err
	}
	return cfg, overrides, nil
}

// SaveConfig creates or updates the scraper configuration for a scope.
// For the global scope, overrides should be nil.
func (s *Service) SaveConfig(ctx context.Context, scope string, cfg *ScraperConfig, overrides *Overrides) error {
	cfg.Scope = scope
	if cfg.ID == "" {
		// Check if a row already exists for this scope
		var existingID string
		err := s.db.QueryRowContext(ctx,
			"SELECT id FROM scraper_config WHERE scope = ?", scope,
		).Scan(&existingID)
		if err == sql.ErrNoRows {
			cfg.ID = uuid.New().String()
		} else if err != nil {
			return fmt.Errorf("checking existing config: %w", err)
		} else {
			cfg.ID = existingID
		}
	}
	return s.saveConfigRow(ctx, cfg, overrides)
}

// ResetConfig deletes the configuration for a non-global scope,
// causing it to revert to inheriting the global config.
func (s *Service) ResetConfig(ctx context.Context, scope string) error {
	if scope == ScopeGlobal {
		return fmt.Errorf("cannot reset global config; use SaveConfig with defaults instead")
	}
	_, err := s.db.ExecContext(ctx,
		"DELETE FROM scraper_config WHERE scope = ?", scope,
	)
	if err != nil {
		return fmt.Errorf("deleting config for scope %q: %w", scope, err)
	}
	return nil
}

// loadConfig reads a single scraper config row by scope.
func (s *Service) loadConfig(ctx context.Context, scope string) (*ScraperConfig, error) {
	var id, configJSON, createdAt, updatedAt string
	err := s.db.QueryRowContext(ctx,
		"SELECT id, config_json, created_at, updated_at FROM scraper_config WHERE scope = ?",
		scope,
	).Scan(&id, &configJSON, &createdAt, &updatedAt)
	if err != nil {
		return nil, fmt.Errorf("loading config for scope %q: %w", scope, err)
	}

	cfg := &ScraperConfig{ID: id, Scope: scope}
	if err := json.Unmarshal([]byte(configJSON), cfg); err != nil {
		return nil, fmt.Errorf("unmarshaling config for scope %q: %w", scope, err)
	}
	cfg.ID = id
	cfg.Scope = scope
	cfg.CreatedAt = parseTime(createdAt)
	cfg.UpdatedAt = parseTime(updatedAt)
	return cfg, nil
}

// loadConfigWithOverrides reads a config row along with its overrides.
func (s *Service) loadConfigWithOverrides(ctx context.Context, scope string) (*ScraperConfig, *Overrides, error) {
	var id, configJSON, overridesJSON, createdAt, updatedAt string
	err := s.db.QueryRowContext(ctx,
		"SELECT id, config_json, overrides_json, created_at, updated_at FROM scraper_config WHERE scope = ?",
		scope,
	).Scan(&id, &configJSON, &overridesJSON, &createdAt, &updatedAt)
	if err != nil {
		return nil, nil, fmt.Errorf("loading config for scope %q: %w", scope, err)
	}

	cfg := &ScraperConfig{ID: id, Scope: scope}
	if err := json.Unmarshal([]byte(configJSON), cfg); err != nil {
		return nil, nil, fmt.Errorf("unmarshaling config for scope %q: %w", scope, err)
	}
	cfg.ID = id
	cfg.Scope = scope
	cfg.CreatedAt = parseTime(createdAt)
	cfg.UpdatedAt = parseTime(updatedAt)

	var overrides Overrides
	if overridesJSON != "" && overridesJSON != "{}" {
		if err := json.Unmarshal([]byte(overridesJSON), &overrides); err != nil {
			return nil, nil, fmt.Errorf("unmarshaling overrides for scope %q: %w", scope, err)
		}
	}

	return cfg, &overrides, nil
}

// saveConfigRow persists a scraper config row.
func (s *Service) saveConfigRow(ctx context.Context, cfg *ScraperConfig, overrides *Overrides) error {
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
	_, err = s.db.ExecContext(ctx, `
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

// mergeConfigs produces an effective config by applying connection overrides
// on top of the global config.
func mergeConfigs(global, conn *ScraperConfig, overrides *Overrides) *ScraperConfig {
	merged := &ScraperConfig{
		ID:        conn.ID,
		Scope:     conn.Scope,
		CreatedAt: conn.CreatedAt,
		UpdatedAt: conn.UpdatedAt,
	}

	// Build a lookup of connection field configs by field name
	connFields := make(map[FieldName]FieldConfig, len(conn.Fields))
	for _, f := range conn.Fields {
		connFields[f.Field] = f
	}

	// Merge fields: use connection value if overridden, otherwise global
	for _, gf := range global.Fields {
		if overrides != nil && overrides.Fields[gf.Field] {
			if cf, ok := connFields[gf.Field]; ok {
				merged.Fields = append(merged.Fields, cf)
				continue
			}
		}
		merged.Fields = append(merged.Fields, gf)
	}

	// Build connection chain lookup
	connChains := make(map[FieldCategory]FallbackChain, len(conn.FallbackChains))
	for _, ch := range conn.FallbackChains {
		connChains[ch.Category] = ch
	}

	// Merge fallback chains
	for _, gch := range global.FallbackChains {
		if overrides != nil && overrides.FallbackChains[gch.Category] {
			if cch, ok := connChains[gch.Category]; ok {
				merged.FallbackChains = append(merged.FallbackChains, cch)
				continue
			}
		}
		merged.FallbackChains = append(merged.FallbackChains, gch)
	}

	return merged
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
