package nfo

import (
	"context"
	"database/sql"
	"log/slog"
	"testing"

	_ "modernc.org/sqlite"
)

func setupSettingsTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	_, err = db.ExecContext(context.Background(), `
		CREATE TABLE settings (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL DEFAULT '',
			updated_at TEXT NOT NULL DEFAULT (datetime('now'))
		);
	`)
	if err != nil {
		t.Fatalf("creating table: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestNFOSettingsService_GetFieldMap_Defaults(t *testing.T) {
	db := setupSettingsTestDB(t)
	svc := NewNFOSettingsService(db, slog.Default())

	fm, err := svc.GetFieldMap(context.Background())
	if err != nil {
		t.Fatalf("GetFieldMap: %v", err)
	}

	if !fm.DefaultBehavior {
		t.Error("expected DefaultBehavior=true for empty settings")
	}
	if fm.MoodsAsStyles {
		t.Error("expected MoodsAsStyles=false for empty settings")
	}
	if len(fm.GenreSources) != 1 || fm.GenreSources[0] != "genres" {
		t.Errorf("GenreSources = %v, want [genres]", fm.GenreSources)
	}
	if fm.AdvancedRemap != nil {
		t.Error("expected AdvancedRemap=nil for empty settings")
	}
}

func TestNFOSettingsService_SetAndGetFieldMap(t *testing.T) {
	db := setupSettingsTestDB(t)
	svc := NewNFOSettingsService(db, slog.Default())
	ctx := context.Background()

	input := NFOFieldMap{
		DefaultBehavior: false,
		MoodsAsStyles:   true,
		GenreSources:    []string{"genres", "styles"},
		AdvancedRemap:   nil,
	}

	if err := svc.SetFieldMap(ctx, input); err != nil {
		t.Fatalf("SetFieldMap: %v", err)
	}

	got, err := svc.GetFieldMap(ctx)
	if err != nil {
		t.Fatalf("GetFieldMap: %v", err)
	}

	if got.DefaultBehavior != false {
		t.Error("DefaultBehavior should be false")
	}
	if got.MoodsAsStyles != true {
		t.Error("MoodsAsStyles should be true")
	}
	if len(got.GenreSources) != 2 || got.GenreSources[0] != "genres" || got.GenreSources[1] != "styles" {
		t.Errorf("GenreSources = %v, want [genres styles]", got.GenreSources)
	}
	if got.AdvancedRemap != nil {
		t.Error("AdvancedRemap should be nil")
	}
}

func TestNFOSettingsService_SetAndGetFieldMap_WithAdvancedRemap(t *testing.T) {
	db := setupSettingsTestDB(t)
	svc := NewNFOSettingsService(db, slog.Default())
	ctx := context.Background()

	input := NFOFieldMap{
		DefaultBehavior: false,
		AdvancedRemap: map[string][]string{
			"genre": {"styles"},
			"style": {"genres", "moods"},
			"mood":  {},
		},
	}

	if err := svc.SetFieldMap(ctx, input); err != nil {
		t.Fatalf("SetFieldMap: %v", err)
	}

	got, err := svc.GetFieldMap(ctx)
	if err != nil {
		t.Fatalf("GetFieldMap: %v", err)
	}

	if got.AdvancedRemap == nil {
		t.Fatal("AdvancedRemap should not be nil")
	}
	if len(got.AdvancedRemap["genre"]) != 1 || got.AdvancedRemap["genre"][0] != "styles" {
		t.Errorf("AdvancedRemap[genre] = %v, want [styles]", got.AdvancedRemap["genre"])
	}
	if len(got.AdvancedRemap["style"]) != 2 {
		t.Errorf("AdvancedRemap[style] = %v, want [genres moods]", got.AdvancedRemap["style"])
	}
	if len(got.AdvancedRemap["mood"]) != 0 {
		t.Errorf("AdvancedRemap[mood] = %v, want []", got.AdvancedRemap["mood"])
	}
}

func TestNFOSettingsService_OverwriteExisting(t *testing.T) {
	db := setupSettingsTestDB(t)
	svc := NewNFOSettingsService(db, slog.Default())
	ctx := context.Background()

	// Write initial settings
	initial := NFOFieldMap{
		DefaultBehavior: false,
		MoodsAsStyles:   true,
		GenreSources:    []string{"genres"},
	}
	if err := svc.SetFieldMap(ctx, initial); err != nil {
		t.Fatalf("SetFieldMap (initial): %v", err)
	}

	// Overwrite with new settings
	updated := NFOFieldMap{
		DefaultBehavior: true,
		MoodsAsStyles:   false,
		GenreSources:    []string{"genres", "moods"},
	}
	if err := svc.SetFieldMap(ctx, updated); err != nil {
		t.Fatalf("SetFieldMap (updated): %v", err)
	}

	got, err := svc.GetFieldMap(ctx)
	if err != nil {
		t.Fatalf("GetFieldMap: %v", err)
	}

	if !got.DefaultBehavior {
		t.Error("DefaultBehavior should be true after update")
	}
	if got.MoodsAsStyles {
		t.Error("MoodsAsStyles should be false after update")
	}
	if len(got.GenreSources) != 2 {
		t.Errorf("GenreSources = %v, want [genres moods]", got.GenreSources)
	}
}
