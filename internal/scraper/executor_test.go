package scraper

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"testing"

	"github.com/sydlexius/stillwater/internal/encryption"
	"github.com/sydlexius/stillwater/internal/provider"
	_ "modernc.org/sqlite"
)

// mockProvider implements provider.Provider for executor tests.
type mockProvider struct {
	name     provider.ProviderName
	authReq  bool
	getArtFn func(ctx context.Context, id string) (*provider.ArtistMetadata, error)
	getImgFn func(ctx context.Context, id string) ([]provider.ImageResult, error)
}

func (m *mockProvider) Name() provider.ProviderName { return m.name }
func (m *mockProvider) RequiresAuth() bool          { return m.authReq }
func (m *mockProvider) SearchArtist(_ context.Context, _ string) ([]provider.ArtistSearchResult, error) {
	return nil, nil
}
func (m *mockProvider) GetArtist(ctx context.Context, id string) (*provider.ArtistMetadata, error) {
	if m.getArtFn != nil {
		return m.getArtFn(ctx, id)
	}
	return nil, nil
}
func (m *mockProvider) GetImages(ctx context.Context, id string) ([]provider.ImageResult, error) {
	if m.getImgFn != nil {
		return m.getImgFn(ctx, id)
	}
	return nil, nil
}

func setupExecutorTest(t *testing.T) (*provider.Registry, *provider.SettingsService, *Service) {
	t.Helper()

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	// Create both tables needed by the executor: scraper_config and settings.
	_, err = db.ExecContext(context.Background(), `
		CREATE TABLE scraper_config (
			id TEXT PRIMARY KEY,
			scope TEXT NOT NULL UNIQUE,
			config_json TEXT NOT NULL DEFAULT '{}',
			overrides_json TEXT NOT NULL DEFAULT '{}',
			created_at TEXT NOT NULL DEFAULT (datetime('now')),
			updated_at TEXT NOT NULL DEFAULT (datetime('now'))
		);
		CREATE TABLE IF NOT EXISTS settings (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL,
			updated_at TEXT NOT NULL DEFAULT (datetime('now'))
		);
	`)
	if err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	svc := NewService(db, logger)
	if err := svc.SeedDefaults(context.Background()); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}

	enc, _, _ := encryption.NewEncryptor("")
	settings := provider.NewSettingsService(db, enc)
	registry := provider.NewRegistry()

	return registry, settings, svc
}

// TestExecutorErrNotFoundMarksFieldAttempted verifies that when all providers
// return ErrNotFound for a field, the field still appears in AttemptedFields.
// This is critical for refresh-overwrite semantics: "provider said not found"
// means stale data should be cleared, unlike "provider unreachable" which
// preserves existing data.
func TestExecutorErrNotFoundMarksFieldAttempted(t *testing.T) {
	registry, settings, svc := setupExecutorTest(t)

	// Register AudioDB which returns ErrNotFound (no data for this artist).
	registry.Register(&mockProvider{
		name:    provider.NameAudioDB,
		authReq: false,
		getArtFn: func(_ context.Context, id string) (*provider.ArtistMetadata, error) {
			return nil, &provider.ErrNotFound{Provider: provider.NameAudioDB, ID: id}
		},
	})

	// Use a minimal config: only one field (styles), primary = AudioDB,
	// fallback chain has only AudioDB.
	ctx := context.Background()
	cfg := &ScraperConfig{
		Scope: ScopeGlobal,
		Fields: []FieldConfig{
			{Field: FieldStyles, Primary: provider.NameAudioDB, Enabled: true, Category: CategoryMetadata},
		},
		FallbackChains: []FallbackChain{
			{Category: CategoryMetadata, Providers: []provider.ProviderName{provider.NameAudioDB}},
		},
	}
	if err := svc.SaveConfig(ctx, ScopeGlobal, cfg, nil); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	exec := NewExecutor(svc, registry, settings, logger)

	result, err := exec.ScrapeAll(ctx, "mbid-1234", "Test Artist", ScopeGlobal, nil)
	if err != nil {
		t.Fatalf("ScrapeAll: %v", err)
	}

	// "styles" should be in AttemptedFields because AudioDB was reached.
	found := false
	for _, f := range result.AttemptedFields {
		if f == string(FieldStyles) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected 'styles' in AttemptedFields after ErrNotFound, got %v", result.AttemptedFields)
	}

	// AudioDB should be in AttemptedProviders.
	provFound := false
	for _, p := range result.AttemptedProviders {
		if p == provider.NameAudioDB {
			provFound = true
			break
		}
	}
	if !provFound {
		t.Errorf("expected AudioDB in AttemptedProviders after ErrNotFound, got %v", result.AttemptedProviders)
	}
}

// TestExecutorNetworkErrorDoesNotMarkFieldAttempted verifies that a real
// network error (not ErrNotFound) does NOT mark the field as attempted.
func TestExecutorNetworkErrorDoesNotMarkFieldAttempted(t *testing.T) {
	registry, settings, svc := setupExecutorTest(t)

	// Register AudioDB which returns a network error.
	registry.Register(&mockProvider{
		name:    provider.NameAudioDB,
		authReq: false,
		getArtFn: func(_ context.Context, _ string) (*provider.ArtistMetadata, error) {
			return nil, fmt.Errorf("connection refused")
		},
	})

	ctx := context.Background()
	cfg := &ScraperConfig{
		Scope: ScopeGlobal,
		Fields: []FieldConfig{
			{Field: FieldStyles, Primary: provider.NameAudioDB, Enabled: true, Category: CategoryMetadata},
		},
		FallbackChains: []FallbackChain{
			{Category: CategoryMetadata, Providers: []provider.ProviderName{provider.NameAudioDB}},
		},
	}
	if err := svc.SaveConfig(ctx, ScopeGlobal, cfg, nil); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	exec := NewExecutor(svc, registry, settings, logger)

	result, err := exec.ScrapeAll(ctx, "mbid-1234", "Test Artist", ScopeGlobal, nil)
	if err != nil {
		t.Fatalf("ScrapeAll: %v", err)
	}

	// "styles" should NOT be in AttemptedFields because the provider failed.
	for _, f := range result.AttemptedFields {
		if f == string(FieldStyles) {
			t.Errorf("expected 'styles' NOT in AttemptedFields after network error, got %v", result.AttemptedFields)
			break
		}
	}
}
