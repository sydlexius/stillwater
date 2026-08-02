package scraper

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sydlexius/stillwater/internal/dbutil"
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
		// An install that already has a config still needs fields added by
		// later releases; see backfillMissingFields.
		return s.backfillGlobalFields(ctx)
	}

	cfg := DefaultConfig()
	cfg.ID = uuid.New().String()
	return s.saveConfigRow(ctx, cfg, nil)
}

// backfillGlobalFields adds any field present in DefaultConfig but absent from
// the stored global config, then persists the result.
//
// The scraper config is a JSON blob written once at install time, so a field
// added to DefaultConfig in a later release reaches NEW installs only: an
// existing install keeps the field list it was created with and the new field
// is simply never fetched. That is how "origin" ended up unreachable on the
// production scrape path despite being a first-class artist column, having its
// own rule, and appearing in the provider priority settings (#2895).
//
// This runs at startup rather than on every config read. Reading is the wrong
// place: a caller that deliberately supplies a narrow config would have fields
// silently added underneath it, and the rewrite would be invisible. Doing it
// once at seed time makes the upgrade explicit and leaves read paths honest.
//
// It only ADDS. A field the operator disabled or repointed at another provider
// keeps their setting, because an entry that already exists is never touched.
func (s *Service) backfillGlobalFields(ctx context.Context) error {
	cfg, err := s.loadGlobalConfig(ctx)
	if err != nil {
		return fmt.Errorf("loading global config for backfill: %w", err)
	}

	// Compare the field roster before and after. A boot that adds nothing but
	// seeds KnownFields for the first time still has to persist, or the roster
	// is recomputed from scratch every startup and a deletion is never durable.
	rosterBefore := len(cfg.KnownFields)
	added, seeded := backfillMissingFields(cfg)
	if len(added) == 0 && len(cfg.KnownFields) == rosterBefore {
		return nil
	}

	if err := s.saveConfigRow(ctx, cfg, nil); err != nil {
		return fmt.Errorf("saving backfilled global config: %w", err)
	}
	if len(added) > 0 {
		// The message differs by case on purpose. On the seeding boot the added
		// set can include a field the operator DELETED before the roster
		// existed, because that deletion left no trace to read -- so claiming
		// everything here is newly introduced would be false. See
		// backfillMissingFields for why that is unrecoverable.
		msg := "added scraper fields introduced since this install was created"
		if seeded {
			msg = "added scraper fields on first upgrade; any field removed before this release is restored once and stays removed if deleted again"
		}
		names := make([]string, 0, len(added))
		for _, f := range added {
			names = append(names, string(f))
		}
		s.logger.Info(msg, "fields", strings.Join(names, ","), "count", len(added), "scope", ScopeGlobal)
	}
	return nil
}

// GetConfig returns the effective scraper configuration for a scope.
// For the global scope, returns the global config directly.
// For a connection scope, returns the global config with connection overrides merged in.
func (s *Service) GetConfig(ctx context.Context, scope string) (*ScraperConfig, error) {
	if scope == ScopeGlobal {
		return s.loadGlobalConfig(ctx)
	}

	global, err := s.loadGlobalConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading global config: %w", err)
	}

	conn, overrides, err := s.loadConfigWithOverrides(ctx, scope)
	if err != nil {
		// Missing scoped config falls back to global; any other error (DB
		// failure, JSON decode error, context cancellation) must propagate so
		// the caller does not silently receive stale/effective config.
		if errors.Is(err, sql.ErrNoRows) {
			return global, nil
		}
		return nil, fmt.Errorf("loading scoped config for %q: %w", scope, err)
	}

	return mergeConfigs(global, conn, overrides), nil
}

// GetRawConfig returns the unmerged configuration and overrides for a scope.
// For the global scope, overrides will be nil.
func (s *Service) GetRawConfig(ctx context.Context, scope string) (*ScraperConfig, *Overrides, error) {
	if scope == ScopeGlobal {
		cfg, err := s.loadGlobalConfig(ctx)
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
		if errors.Is(err, sql.ErrNoRows) {
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

// loadGlobalConfig reads the global scraper config row. Scoped rows carry
// overrides and are read by loadConfigWithOverrides instead.
func (s *Service) loadGlobalConfig(ctx context.Context) (*ScraperConfig, error) {
	const scope = ScopeGlobal

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
	cfg.CreatedAt = dbutil.ParseTime(createdAt)
	cfg.UpdatedAt = dbutil.ParseTime(updatedAt)
	return cfg, nil
}

// backfillMissingFields adds any field present in DefaultConfig but absent from
// a stored config, using the default's primary provider and enabled state.
//
// The config is persisted as a JSON blob written once at install time, so a
// field added to DefaultConfig later reaches new installs only -- an existing
// install keeps the field list it was created with and the new field is simply
// never fetched. That is how "origin" was unreachable on the production scrape
// path despite being a first-class artist column (#2895).
//
// It only ADDS, and returns the names of the fields it appended plus whether
// this call SEEDED the roster (that is, the stored config predated
// KnownFields). A field the operator disabled or repointed keeps their setting,
// because an entry that already exists is never touched.
//
// A field the operator DELETED stays deleted -- with ONE boundary case. The
// decision is made against cfg.KnownFields, which records every field this
// install has been offered, so "removed on purpose" is normally
// distinguishable from "introduced after this install was created"; judging on
// the Fields list alone would resurrect a deletion on every boot.
//
// THE BOUNDARY CASE: a config written before KnownFields existed carries no
// roster, so it is seeded from the fields that config currently holds. A
// deletion made BEFORE that upgrade left no trace anywhere, and is therefore
// indistinguishable from a field that never existed -- so on that one boot it
// is re-added, enabled. There is no way to recover the operator's pre-upgrade
// intent; the roster only makes deletions durable from the seeding boot
// onward. The seeded return value exists so the caller can say so honestly in
// the log rather than claiming every added field is new.
//
// Callers persist the result; see backfillGlobalFields for why that happens at
// startup rather than on every read.
func backfillMissingFields(cfg *ScraperConfig) (added []FieldName, seeded bool) {
	if cfg == nil {
		return nil, false
	}

	known := make(map[FieldName]bool, len(cfg.KnownFields)+len(cfg.Fields))
	for _, f := range cfg.KnownFields {
		known[f] = true
	}
	// A config written before KnownFields existed has none. Seed it from the
	// fields the install currently carries so its present selection counts as
	// already-seen; anything genuinely newer is still absent and gets added.
	seeded = len(cfg.KnownFields) == 0
	if seeded {
		for _, f := range cfg.Fields {
			known[f.Field] = true
		}
	}

	present := make(map[FieldName]bool, len(cfg.Fields))
	for _, f := range cfg.Fields {
		present[f.Field] = true
	}

	for _, def := range DefaultConfig().Fields {
		if !present[def.Field] && !known[def.Field] {
			cfg.Fields = append(cfg.Fields, def)
			added = append(added, def.Field)
		}
		known[def.Field] = true
	}

	// Rewrite the roster from the union so it covers both the fields in use and
	// the ones deliberately dropped.
	cfg.KnownFields = cfg.KnownFields[:0]
	for _, def := range AllFieldNames() {
		if known[def] {
			cfg.KnownFields = append(cfg.KnownFields, def)
		}
	}
	return added, seeded
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
	cfg.CreatedAt = dbutil.ParseTime(createdAt)
	cfg.UpdatedAt = dbutil.ParseTime(updatedAt)

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
