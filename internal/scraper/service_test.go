package scraper

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"slices"
	"testing"

	"github.com/sydlexius/stillwater/internal/provider"
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

// TestSeedDefaults_BackfillsFieldAddedAfterInstall covers the upgrade half of
// #2895. The scraper config is a JSON blob written once at install time, so a
// field added to DefaultConfig in a later release reaches new installs only --
// an existing install keeps the field list it was created with and never
// fetches the new field. Origin hit exactly that, which is why an upgraded
// instance would still not repair a bad origin even with the applier in place.
//
// The fixture writes a config with origin REMOVED, mimicking a row written by
// a pre-fix build, then asserts the startup path restores it.
func TestSeedDefaults_BackfillsFieldAddedAfterInstall(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatal(err)
	}

	stored, err := svc.GetConfig(ctx, ScopeGlobal)
	if err != nil {
		t.Fatal(err)
	}

	// Strip origin and persist, simulating a config written before the field
	// existed. Save through the service so the row is written the same way a
	// real older build would have written it.
	pruned := make([]FieldConfig, 0, len(stored.Fields))
	for _, f := range stored.Fields {
		if f.Field != FieldOrigin {
			pruned = append(pruned, f)
		}
	}
	if len(pruned) == len(stored.Fields) {
		t.Fatalf("precondition failed: seeded config had no %q field to remove", FieldOrigin)
	}
	stored.Fields = pruned
	if err := svc.SaveConfig(ctx, ScopeGlobal, stored, nil); err != nil {
		t.Fatal(err)
	}

	// SeedDefaults is the startup path an upgraded install runs through.
	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatal(err)
	}

	reloaded, err := svc.GetConfig(ctx, ScopeGlobal)
	if err != nil {
		t.Fatal(err)
	}

	var got *FieldConfig
	for i := range reloaded.Fields {
		if reloaded.Fields[i].Field == FieldOrigin {
			got = &reloaded.Fields[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("%q missing after reload: an upgraded install will never fetch it", FieldOrigin)
	}
	if !got.Enabled {
		t.Errorf("backfilled %q Enabled = false, want true", FieldOrigin)
	}
	if got.Category != CategoryMetadata {
		t.Errorf("backfilled %q Category = %q, want %q", FieldOrigin, got.Category, CategoryMetadata)
	}
}

// TestSeedDefaults_BackfillPreservesOperatorChoices is the counterweight: the
// backfill must only ADD. An operator who disabled a field or repointed it at
// a different provider must keep that setting across the upgrade, otherwise
// the self-heal silently reverts their configuration on every read.
func TestSeedDefaults_BackfillPreservesOperatorChoices(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatal(err)
	}

	stored, err := svc.GetConfig(ctx, ScopeGlobal)
	if err != nil {
		t.Fatal(err)
	}
	for i := range stored.Fields {
		if stored.Fields[i].Field == FieldOrigin {
			stored.Fields[i].Enabled = false
			stored.Fields[i].Primary = provider.NameAudioDB
		}
	}
	if err := svc.SaveConfig(ctx, ScopeGlobal, stored, nil); err != nil {
		t.Fatal(err)
	}

	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatal(err)
	}

	reloaded, err := svc.GetConfig(ctx, ScopeGlobal)
	if err != nil {
		t.Fatal(err)
	}

	count := 0
	for _, f := range reloaded.Fields {
		if f.Field != FieldOrigin {
			continue
		}
		count++
		if f.Enabled {
			t.Error("operator disabled origin but backfill re-enabled it")
		}
		if f.Primary != provider.NameAudioDB {
			t.Errorf("operator set primary %q but backfill reverted it to %q", provider.NameAudioDB, f.Primary)
		}
	}
	if count != 1 {
		t.Errorf("origin appears %d times after reload, want exactly 1: the backfill duplicated an existing field", count)
	}
}

// TestSeedDefaults_BackfillDoesNotResurrectDeletedFields covers the hostile-review
// finding on the #2895 backfill.
//
// PUT /api/v1/scraper/config accepts an arbitrary Fields array and does not
// require completeness, so "disable this field" and "remove this field" are
// both reachable through the same API. Judging the backfill on the Fields list
// alone makes those indistinguishable from a field introduced by a later
// release, and every boot silently restores the removed entries -- re-enabled,
// because DefaultConfig has Enabled: true.
func TestSeedDefaults_BackfillDoesNotResurrectDeletedFields(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatal(err)
	}
	// A first boot on an existing install seeds the known-field roster.
	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatal(err)
	}

	stored, err := svc.GetConfig(ctx, ScopeGlobal)
	if err != nil {
		t.Fatal(err)
	}

	// The operator removes every image field rather than disabling them.
	kept := make([]FieldConfig, 0, len(stored.Fields))
	removed := 0
	for _, f := range stored.Fields {
		if CategoryFor(f.Field) == CategoryImages {
			removed++
			continue
		}
		kept = append(kept, f)
	}
	if removed == 0 {
		t.Fatal("precondition failed: seeded config had no image fields to remove")
	}
	stored.Fields = kept
	if err := svc.SaveConfig(ctx, ScopeGlobal, stored, nil); err != nil {
		t.Fatal(err)
	}

	// Restart.
	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatal(err)
	}

	reloaded, err := svc.GetConfig(ctx, ScopeGlobal)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range reloaded.Fields {
		if CategoryFor(f.Field) == CategoryImages {
			t.Errorf("field %q was deleted by the operator but came back after restart (enabled=%v)",
				f.Field, f.Enabled)
		}
	}

	// Precondition: the backfill still works for a genuinely new field. Drop
	// origin from BOTH the field list and the known roster, which is what a
	// config written before the field existed looks like.
	reloaded.Fields = slices.DeleteFunc(reloaded.Fields, func(f FieldConfig) bool {
		return f.Field == FieldOrigin
	})
	reloaded.KnownFields = slices.DeleteFunc(reloaded.KnownFields, func(f FieldName) bool {
		return f == FieldOrigin
	})
	if err := svc.SaveConfig(ctx, ScopeGlobal, reloaded, nil); err != nil {
		t.Fatal(err)
	}
	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := svc.GetConfig(ctx, ScopeGlobal)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(after.Fields, func(f FieldConfig) bool { return f.Field == FieldOrigin }) {
		t.Error("a field absent from both Fields and KnownFields must still be added; the deletion guard has disabled the backfill entirely")
	}
}
