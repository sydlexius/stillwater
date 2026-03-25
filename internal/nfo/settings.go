package nfo

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
)

// Settings table keys for NFO output configuration.
const (
	SettingDefaultBehavior = "nfo.output.default_behavior"
	SettingMoodsAsStyles   = "nfo.output.moods_as_styles"
	SettingGenreSources    = "nfo.output.genre_sources"
	SettingAdvancedRemap   = "nfo.output.advanced_remap"
)

// NFOSettingsService reads and writes NFO output configuration from the
// key-value settings table.
type NFOSettingsService struct {
	db     *sql.DB
	logger *slog.Logger
}

// NewNFOSettingsService creates an NFO settings service.
func NewNFOSettingsService(db *sql.DB, logger *slog.Logger) *NFOSettingsService {
	return &NFOSettingsService{db: db, logger: logger}
}

// GetFieldMap reads all NFO output settings from the database and composes
// an NFOFieldMap. Missing keys are treated as defaults.
func (s *NFOSettingsService) GetFieldMap(ctx context.Context) (NFOFieldMap, error) {
	fm := DefaultFieldMap()

	rows, err := s.db.QueryContext(ctx,
		`SELECT key, value FROM settings WHERE key LIKE 'nfo.output.%'`) //nolint:gosec // G201: static prefix, no user input
	if err != nil {
		return fm, fmt.Errorf("querying nfo output settings: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return fm, fmt.Errorf("scanning nfo output setting: %w", err)
		}

		switch k {
		case SettingDefaultBehavior:
			fm.DefaultBehavior = v == "true" || v == "1"
		case SettingMoodsAsStyles:
			fm.MoodsAsStyles = v == "true" || v == "1"
		case SettingGenreSources:
			var sources []string
			if unmarshalErr := json.Unmarshal([]byte(v), &sources); unmarshalErr != nil {
				s.logger.Warn("corrupt nfo.output.genre_sources value, using default",
					slog.String("raw_value", v),
					slog.String("error", unmarshalErr.Error()))
			} else {
				fm.GenreSources = sources
			}
		case SettingAdvancedRemap:
			if v == "" || v == "null" {
				fm.AdvancedRemap = nil
			} else {
				var remap map[string][]string
				if unmarshalErr := json.Unmarshal([]byte(v), &remap); unmarshalErr != nil {
					s.logger.Warn("corrupt nfo.output.advanced_remap value, using default",
						slog.String("raw_value", v),
						slog.String("error", unmarshalErr.Error()))
				} else {
					fm.AdvancedRemap = remap
				}
			}
		}
	}
	if err := rows.Err(); err != nil {
		return fm, fmt.Errorf("iterating nfo output settings: %w", err)
	}

	return fm, nil
}

// SetFieldMap writes the NFOFieldMap as individual key-value pairs in the
// settings table. All four upserts run within a single transaction to ensure
// atomicity.
func (s *NFOSettingsService) SetFieldMap(ctx context.Context, fm NFOFieldMap) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning nfo settings transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback is a no-op after commit

	upsertSQL := `INSERT INTO settings (key, value, updated_at) VALUES (?, ?, datetime('now'))
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`

	// DefaultBehavior
	defaultVal := "false"
	if fm.DefaultBehavior {
		defaultVal = "true"
	}
	if _, err := tx.ExecContext(ctx, upsertSQL, SettingDefaultBehavior, defaultVal); err != nil {
		return fmt.Errorf("upserting %s: %w", SettingDefaultBehavior, err)
	}

	// MoodsAsStyles
	moodsVal := "false"
	if fm.MoodsAsStyles {
		moodsVal = "true"
	}
	if _, err := tx.ExecContext(ctx, upsertSQL, SettingMoodsAsStyles, moodsVal); err != nil {
		return fmt.Errorf("upserting %s: %w", SettingMoodsAsStyles, err)
	}

	// GenreSources
	genreSourcesJSON, err := json.Marshal(fm.GenreSources)
	if err != nil {
		return fmt.Errorf("marshaling genre_sources: %w", err)
	}
	if _, err := tx.ExecContext(ctx, upsertSQL, SettingGenreSources, string(genreSourcesJSON)); err != nil {
		return fmt.Errorf("upserting %s: %w", SettingGenreSources, err)
	}

	// AdvancedRemap
	var remapVal string
	if fm.AdvancedRemap == nil {
		remapVal = "null"
	} else {
		remapJSON, err := json.Marshal(fm.AdvancedRemap)
		if err != nil {
			return fmt.Errorf("marshaling advanced_remap: %w", err)
		}
		remapVal = string(remapJSON)
	}
	if _, err := tx.ExecContext(ctx, upsertSQL, SettingAdvancedRemap, remapVal); err != nil {
		return fmt.Errorf("upserting %s: %w", SettingAdvancedRemap, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing nfo settings: %w", err)
	}
	return nil
}
