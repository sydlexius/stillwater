package scraper

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"testing"

	_ "modernc.org/sqlite"
)

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}

	// Create the scraper_config table
	_, err = db.ExecContext(context.Background(), `
		CREATE TABLE scraper_config (
			id TEXT PRIMARY KEY,
			scope TEXT NOT NULL UNIQUE,
			config_json TEXT NOT NULL DEFAULT '{}',
			overrides_json TEXT NOT NULL DEFAULT '{}',
			created_at TEXT NOT NULL DEFAULT (datetime('now')),
			updated_at TEXT NOT NULL DEFAULT (datetime('now'))
		)
	`)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { db.Close() })
	return db
}

func newTestService(t *testing.T) *Service {
	t.Helper()
	db := setupTestDB(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return NewService(db, logger)
}

func TestSeedDefaults(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	// First seed should create the global config
	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatal(err)
	}

	cfg, err := svc.GetConfig(ctx, ScopeGlobal)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Scope != ScopeGlobal {
		t.Errorf("Scope = %q, want %q", cfg.Scope, ScopeGlobal)
	}
	if len(cfg.Fields) == 0 {
		t.Error("Fields should not be empty")
	}
	if len(cfg.FallbackChains) != 2 {
		t.Errorf("FallbackChains count = %d, want 2", len(cfg.FallbackChains))
	}

	// Second seed should be a no-op
	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestSaveAndGetConfig(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatal(err)
	}

	// Get global config
	cfg, err := svc.GetConfig(ctx, ScopeGlobal)
	if err != nil {
		t.Fatal(err)
	}

	// Save a connection override
	connCfg := DefaultConfig()
	connCfg.Scope = "conn-123"
	connCfg.Fields[0].Primary = "audiodb" // Override biography primary

	overrides := &Overrides{
		Fields: map[FieldName]bool{FieldBiography: true},
	}

	if err := svc.SaveConfig(ctx, "conn-123", connCfg, overrides); err != nil {
		t.Fatal(err)
	}

	// Get merged connection config
	merged, err := svc.GetConfig(ctx, "conn-123")
	if err != nil {
		t.Fatal(err)
	}

	// Biography should be overridden
	if got := merged.PrimaryFor(FieldBiography); got != "audiodb" {
		t.Errorf("merged PrimaryFor(biography) = %q, want %q", got, "audiodb")
	}

	// Genres should inherit from global
	if got := merged.PrimaryFor(FieldGenres); got != cfg.PrimaryFor(FieldGenres) {
		t.Errorf("merged PrimaryFor(genres) = %q, want %q (inherited)", got, cfg.PrimaryFor(FieldGenres))
	}
}

func TestGetRawConfig(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatal(err)
	}

	// Global config should have nil overrides
	cfg, overrides, err := svc.GetRawConfig(ctx, ScopeGlobal)
	if err != nil {
		t.Fatal(err)
	}
	if cfg == nil {
		t.Fatal("global config should not be nil")
	}
	if overrides != nil {
		t.Error("global overrides should be nil")
	}

	// Non-existent connection should return error
	_, _, err = svc.GetRawConfig(ctx, "nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent scope")
	}
}

func TestResetConfig(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatal(err)
	}

	// Save a connection config
	connCfg := DefaultConfig()
	connCfg.Scope = "conn-456"
	if err := svc.SaveConfig(ctx, "conn-456", connCfg, nil); err != nil {
		t.Fatal(err)
	}

	// Reset should delete it
	if err := svc.ResetConfig(ctx, "conn-456"); err != nil {
		t.Fatal(err)
	}

	// GetConfig for reset scope should fall back to global
	cfg, err := svc.GetConfig(ctx, "conn-456")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Scope != ScopeGlobal {
		t.Errorf("after reset, config Scope = %q, want %q", cfg.Scope, ScopeGlobal)
	}
}

func TestResetGlobalConfigFails(t *testing.T) {
	svc := newTestService(t)
	if err := svc.ResetConfig(context.Background(), ScopeGlobal); err == nil {
		t.Error("expected error when resetting global config")
	}
}

func TestMergeConfigs(t *testing.T) {
	global := DefaultConfig()
	global.ID = "global-id"
	global.Scope = ScopeGlobal

	conn := DefaultConfig()
	conn.ID = "conn-id"
	conn.Scope = "conn-1"
	// Override styles primary
	for i, f := range conn.Fields {
		if f.Field == FieldStyles {
			conn.Fields[i].Primary = "discogs"
		}
	}

	overrides := &Overrides{
		Fields: map[FieldName]bool{FieldStyles: true},
	}

	merged := mergeConfigs(global, conn, overrides)

	// Styles should be overridden
	if got := merged.PrimaryFor(FieldStyles); got != "discogs" {
		t.Errorf("merged PrimaryFor(styles) = %q, want %q", got, "discogs")
	}

	// Biography should inherit from global
	if got := merged.PrimaryFor(FieldBiography); got != global.PrimaryFor(FieldBiography) {
		t.Errorf("merged PrimaryFor(biography) = %q, want %q", got, global.PrimaryFor(FieldBiography))
	}

	// Scope should be from connection
	if merged.Scope != "conn-1" {
		t.Errorf("merged Scope = %q, want %q", merged.Scope, "conn-1")
	}
}
