package provider

import (
	"context"
	"database/sql"
	"testing"

	"github.com/sydlexius/stillwater/internal/encryption"
	_ "modernc.org/sqlite"
)

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	_, err = db.ExecContext(context.Background(), `
		CREATE TABLE IF NOT EXISTS settings (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL,
			updated_at TEXT NOT NULL DEFAULT (datetime('now'))
		)
	`)
	if err != nil {
		t.Fatalf("creating settings table: %v", err)
	}
	return db
}

func setupTestEncryptor(t *testing.T) *encryption.Encryptor {
	t.Helper()
	enc, _, err := encryption.NewEncryptor("")
	if err != nil {
		t.Fatalf("creating encryptor: %v", err)
	}
	return enc
}

func TestAPIKeyRoundTrip(t *testing.T) {
	db := setupTestDB(t)
	enc := setupTestEncryptor(t)
	svc := NewSettingsService(db, enc)
	ctx := context.Background()

	// Initially empty
	key, err := svc.GetAPIKey(ctx, NameFanartTV)
	if err != nil {
		t.Fatalf("GetAPIKey: %v", err)
	}
	if key != "" {
		t.Errorf("expected empty key, got %s", key)
	}

	// Set a key
	if err := svc.SetAPIKey(ctx, NameFanartTV, "my-secret-key-123"); err != nil {
		t.Fatalf("SetAPIKey: %v", err)
	}

	// Read it back
	key, err = svc.GetAPIKey(ctx, NameFanartTV)
	if err != nil {
		t.Fatalf("GetAPIKey after set: %v", err)
	}
	if key != "my-secret-key-123" {
		t.Errorf("expected 'my-secret-key-123', got %s", key)
	}

	// Verify it is encrypted in the database
	var raw string
	err = db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", "provider.fanarttv.api_key").Scan(&raw)
	if err != nil {
		t.Fatalf("reading raw value: %v", err)
	}
	if raw == "my-secret-key-123" {
		t.Error("API key stored in plaintext, expected encrypted")
	}

	// Update the key
	if err := svc.SetAPIKey(ctx, NameFanartTV, "updated-key-456"); err != nil {
		t.Fatalf("SetAPIKey update: %v", err)
	}
	key, err = svc.GetAPIKey(ctx, NameFanartTV)
	if err != nil {
		t.Fatalf("GetAPIKey after update: %v", err)
	}
	if key != "updated-key-456" {
		t.Errorf("expected 'updated-key-456', got %s", key)
	}
}

func TestDeleteAPIKey(t *testing.T) {
	db := setupTestDB(t)
	enc := setupTestEncryptor(t)
	svc := NewSettingsService(db, enc)
	ctx := context.Background()

	if err := svc.SetAPIKey(ctx, NameDiscogs, "token-abc"); err != nil {
		t.Fatalf("SetAPIKey: %v", err)
	}

	if err := svc.DeleteAPIKey(ctx, NameDiscogs); err != nil {
		t.Fatalf("DeleteAPIKey: %v", err)
	}

	key, err := svc.GetAPIKey(ctx, NameDiscogs)
	if err != nil {
		t.Fatalf("GetAPIKey after delete: %v", err)
	}
	if key != "" {
		t.Errorf("expected empty key after delete, got %s", key)
	}
}

func TestHasAPIKey(t *testing.T) {
	db := setupTestDB(t)
	enc := setupTestEncryptor(t)
	svc := NewSettingsService(db, enc)
	ctx := context.Background()

	has, err := svc.HasAPIKey(ctx, NameFanartTV)
	if err != nil {
		t.Fatalf("HasAPIKey: %v", err)
	}
	if has {
		t.Error("expected no key initially")
	}

	if err := svc.SetAPIKey(ctx, NameFanartTV, "key"); err != nil {
		t.Fatalf("SetAPIKey: %v", err)
	}

	has, err = svc.HasAPIKey(ctx, NameFanartTV)
	if err != nil {
		t.Fatalf("HasAPIKey: %v", err)
	}
	if !has {
		t.Error("expected key to exist after set")
	}
}

func TestListProviderKeyStatuses(t *testing.T) {
	db := setupTestDB(t)
	enc := setupTestEncryptor(t)
	svc := NewSettingsService(db, enc)
	ctx := context.Background()

	// Set a key for Fanart.tv
	if err := svc.SetAPIKey(ctx, NameFanartTV, "key"); err != nil {
		t.Fatalf("SetAPIKey: %v", err)
	}

	statuses, err := svc.ListProviderKeyStatuses(ctx)
	if err != nil {
		t.Fatalf("ListProviderKeyStatuses: %v", err)
	}

	if len(statuses) != len(AllProviderNames()) {
		t.Fatalf("expected %d statuses, got %d", len(AllProviderNames()), len(statuses))
	}

	// MusicBrainz: no key required
	mb := statuses[0]
	if mb.Name != NameMusicBrainz {
		t.Errorf("expected first provider to be musicbrainz, got %s", mb.Name)
	}
	if mb.RequiresKey {
		t.Error("MusicBrainz should not require key")
	}
	if mb.Status != "not_required" {
		t.Errorf("expected status 'not_required', got %s", mb.Status)
	}

	// Fanart.tv: has key
	fanart := statuses[1]
	if fanart.Name != NameFanartTV {
		t.Errorf("expected second provider to be fanarttv, got %s", fanart.Name)
	}
	if !fanart.HasKey {
		t.Error("Fanart.tv should have key")
	}
	if fanart.Status != "untested" {
		t.Errorf("expected status 'untested', got %s", fanart.Status)
	}

	// Discogs: no key configured
	discogs := statuses[3]
	if discogs.Name != NameDiscogs {
		t.Errorf("expected fourth provider to be discogs, got %s", discogs.Name)
	}
	if discogs.HasKey {
		t.Error("Discogs should not have key")
	}
	if discogs.Status != "unconfigured" {
		t.Errorf("expected status 'unconfigured', got %s", discogs.Status)
	}
}

func TestPriorityRoundTrip(t *testing.T) {
	db := setupTestDB(t)
	enc := setupTestEncryptor(t)
	svc := NewSettingsService(db, enc)
	ctx := context.Background()

	// Get defaults
	priorities, err := svc.GetPriorities(ctx)
	if err != nil {
		t.Fatalf("GetPriorities: %v", err)
	}
	if len(priorities) == 0 {
		t.Fatal("expected non-empty defaults")
	}

	// Check biography default
	var bio FieldPriority
	for _, p := range priorities {
		if p.Field == "biography" {
			bio = p
			break
		}
	}
	if len(bio.Providers) == 0 {
		t.Fatal("expected biography to have default providers")
	}
	if bio.Providers[0] != NameMusicBrainz {
		t.Errorf("expected first biography provider to be musicbrainz, got %s", bio.Providers[0])
	}

	// Override biography
	newOrder := []ProviderName{NameLastFM, NameMusicBrainz, NameAudioDB}
	if err := svc.SetPriority(ctx, "biography", newOrder); err != nil {
		t.Fatalf("SetPriority: %v", err)
	}

	// Read back
	priorities, err = svc.GetPriorities(ctx)
	if err != nil {
		t.Fatalf("GetPriorities after set: %v", err)
	}
	for _, p := range priorities {
		if p.Field == "biography" {
			if len(p.Providers) != 3 {
				t.Fatalf("expected 3 providers, got %d", len(p.Providers))
			}
			if p.Providers[0] != NameLastFM {
				t.Errorf("expected first provider to be lastfm, got %s", p.Providers[0])
			}
			break
		}
	}
}
