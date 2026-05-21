package provider

import (
	"context"
	"database/sql"
	"reflect"
	"strings"
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

	// MusicBrainz: free tier, no help URL, rate limit set
	if mb.AccessTier != TierFree {
		t.Errorf("expected MusicBrainz access tier %q, got %q", TierFree, mb.AccessTier)
	}
	if mb.HelpURL != "" {
		t.Errorf("expected MusicBrainz to have no help URL, got %q", mb.HelpURL)
	}
	if mb.RateLimit == nil {
		t.Error("expected MusicBrainz to have rate limit info")
	} else if mb.RateLimit.RequestsPerSecond != 1 {
		t.Errorf("expected MusicBrainz rate limit 1 req/s, got %v", mb.RateLimit.RequestsPerSecond)
	}

	// Fanart.tv: has key (index 2; Wikipedia is at index 1)
	fanart := statuses[2]
	if fanart.Name != NameFanartTV {
		t.Errorf("expected third provider to be fanarttv, got %s", fanart.Name)
	}
	if !fanart.HasKey {
		t.Error("Fanart.tv should have key")
	}
	if fanart.Status != "untested" {
		t.Errorf("expected status 'untested', got %s", fanart.Status)
	}
	if fanart.AccessTier != TierFreeKey {
		t.Errorf("expected Fanart.tv access tier %q, got %q", TierFreeKey, fanart.AccessTier)
	}
	if fanart.HelpURL == "" {
		t.Error("expected Fanart.tv to have a help URL")
	}
	if fanart.RateLimit == nil {
		t.Error("expected Fanart.tv to have rate limit info")
	}

	// Discogs: no key configured (index 4; Wikipedia shifted indices by 1)
	discogs := statuses[4]
	if discogs.Name != NameDiscogs {
		t.Errorf("expected fifth provider to be discogs, got %s", discogs.Name)
	}
	if discogs.HasKey {
		t.Error("Discogs should not have key")
	}
	if discogs.Status != "unconfigured" {
		t.Errorf("expected status 'unconfigured', got %s", discogs.Status)
	}
	if discogs.AccessTier != TierFreeKey {
		t.Errorf("expected Discogs access tier %q, got %q", TierFreeKey, discogs.AccessTier)
	}
	if discogs.HelpURL == "" {
		t.Error("expected Discogs to have a help URL")
	}
	if discogs.RateLimit == nil {
		t.Error("expected Discogs to have rate limit info")
	}
}

// findStatus returns the ProviderKeyStatus for the given name, or fails the test.
func findStatus(t *testing.T, statuses []ProviderKeyStatus, name ProviderName) ProviderKeyStatus {
	t.Helper()
	for _, s := range statuses {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("provider %s not found in statuses", name)
	return ProviderKeyStatus{}
}

func TestOptionalKeyField(t *testing.T) {
	db := setupTestDB(t)
	enc := setupTestEncryptor(t)
	svc := NewSettingsService(db, enc)
	ctx := context.Background()

	statuses, err := svc.ListProviderKeyStatuses(ctx)
	if err != nil {
		t.Fatalf("ListProviderKeyStatuses: %v", err)
	}

	// AudioDB should have OptionalKey=true
	audiodb := findStatus(t, statuses, NameAudioDB)
	if !audiodb.OptionalKey {
		t.Error("expected AudioDB to have OptionalKey=true")
	}
	if audiodb.RequiresKey {
		t.Error("expected AudioDB RequiresKey=false")
	}
	// Without a key: status should be not_required
	if audiodb.Status != "not_required" {
		t.Errorf("expected AudioDB status 'not_required' without key, got %s", audiodb.Status)
	}

	// Set a premium key for AudioDB
	if err := svc.SetAPIKey(ctx, NameAudioDB, "premium-key-123"); err != nil {
		t.Fatalf("SetAPIKey: %v", err)
	}
	statuses, err = svc.ListProviderKeyStatuses(ctx)
	if err != nil {
		t.Fatalf("ListProviderKeyStatuses after key set: %v", err)
	}
	audiodb = findStatus(t, statuses, NameAudioDB)
	if !audiodb.HasKey {
		t.Error("expected AudioDB HasKey=true after setting key")
	}
	if audiodb.Status != "untested" {
		t.Errorf("expected AudioDB status 'untested' with key, got %s", audiodb.Status)
	}

	// Other no-key providers should NOT have OptionalKey
	for _, s := range statuses {
		if s.Name == NameMusicBrainz || s.Name == NameWikidata || s.Name == NameDeezer {
			if s.OptionalKey {
				t.Errorf("expected %s OptionalKey=false, got true", s.Name)
			}
		}
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
	if bio.Providers[0] != NameWikipedia {
		t.Errorf("expected first biography provider to be wikipedia, got %s", bio.Providers[0])
	}

	// Override biography with a subset of providers (simulating a user reorder).
	// The stored list is [lastfm, audiodb]; GetPriorities should reconcile by
	// appending any default providers absent from the stored list so
	// newly-added defaults surface without a manual reset.
	newOrder := []ProviderName{NameLastFM, NameAudioDB}
	if err := svc.SetPriority(ctx, "biography", newOrder); err != nil {
		t.Fatalf("SetPriority: %v", err)
	}

	// Read back: user-ordered providers first, then reconciled defaults appended.
	priorities, err = svc.GetPriorities(ctx)
	if err != nil {
		t.Fatalf("GetPriorities after set: %v", err)
	}
	biographyDefault := DefaultPriorities()
	var defaultBioCount int
	for _, d := range biographyDefault {
		if d.Field == "biography" {
			defaultBioCount = len(d.Providers)
			break
		}
	}
	for _, p := range priorities {
		if p.Field == "biography" {
			if len(p.Providers) != defaultBioCount {
				t.Fatalf("expected %d providers after reconciliation, got %d", defaultBioCount, len(p.Providers))
			}
			if p.Providers[0] != NameLastFM {
				t.Errorf("expected first provider to be lastfm, got %s", p.Providers[0])
			}
			break
		}
	}
}

func TestWebSearchEnabledRoundTrip(t *testing.T) {
	db := setupTestDB(t)
	enc := setupTestEncryptor(t)
	svc := NewSettingsService(db, enc)
	ctx := context.Background()

	// Initially disabled
	enabled, err := svc.IsWebSearchEnabled(ctx, NameDuckDuckGo)
	if err != nil {
		t.Fatalf("IsWebSearchEnabled: %v", err)
	}
	if enabled {
		t.Error("expected disabled by default")
	}

	// Enable
	if err := svc.SetWebSearchEnabled(ctx, NameDuckDuckGo, true); err != nil {
		t.Fatalf("SetWebSearchEnabled(true): %v", err)
	}
	enabled, err = svc.IsWebSearchEnabled(ctx, NameDuckDuckGo)
	if err != nil {
		t.Fatalf("IsWebSearchEnabled after enable: %v", err)
	}
	if !enabled {
		t.Error("expected enabled after set")
	}

	// Disable again
	if err := svc.SetWebSearchEnabled(ctx, NameDuckDuckGo, false); err != nil {
		t.Fatalf("SetWebSearchEnabled(false): %v", err)
	}
	enabled, err = svc.IsWebSearchEnabled(ctx, NameDuckDuckGo)
	if err != nil {
		t.Fatalf("IsWebSearchEnabled after disable: %v", err)
	}
	if enabled {
		t.Error("expected disabled after set false")
	}
}

func TestListWebSearchStatuses(t *testing.T) {
	db := setupTestDB(t)
	enc := setupTestEncryptor(t)
	svc := NewSettingsService(db, enc)
	ctx := context.Background()

	statuses, err := svc.ListWebSearchStatuses(ctx)
	if err != nil {
		t.Fatalf("ListWebSearchStatuses: %v", err)
	}
	if len(statuses) != len(AllWebSearchProviderNames()) {
		t.Fatalf("expected %d statuses, got %d", len(AllWebSearchProviderNames()), len(statuses))
	}

	ddg := statuses[0]
	if ddg.Name != NameDuckDuckGo {
		t.Errorf("expected duckduckgo, got %s", ddg.Name)
	}
	if ddg.DisplayName != "DuckDuckGo" {
		t.Errorf("expected display name DuckDuckGo, got %s", ddg.DisplayName)
	}
	if ddg.Enabled {
		t.Error("expected disabled by default")
	}

	// Enable and re-check
	if err := svc.SetWebSearchEnabled(ctx, NameDuckDuckGo, true); err != nil {
		t.Fatalf("SetWebSearchEnabled: %v", err)
	}
	statuses, err = svc.ListWebSearchStatuses(ctx)
	if err != nil {
		t.Fatalf("ListWebSearchStatuses after enable: %v", err)
	}
	if !statuses[0].Enabled {
		t.Error("expected enabled after set")
	}
}

func TestDisabledProvidersRoundTrip(t *testing.T) {
	db := setupTestDB(t)
	enc := setupTestEncryptor(t)
	svc := NewSettingsService(db, enc)
	ctx := context.Background()

	// Initially no disabled providers.
	priorities, err := svc.GetPriorities(ctx)
	if err != nil {
		t.Fatalf("GetPriorities: %v", err)
	}
	for _, p := range priorities {
		if p.Field == "biography" {
			if len(p.Disabled) != 0 {
				t.Errorf("expected no disabled providers initially, got %v", p.Disabled)
			}
			break
		}
	}

	// Disable two providers for biography.
	disabled := []ProviderName{NameMusicBrainz, NameWikidata}
	if err := svc.SetDisabledProviders(ctx, "biography", disabled); err != nil {
		t.Fatalf("SetDisabledProviders: %v", err)
	}

	// Read back and verify.
	priorities, err = svc.GetPriorities(ctx)
	if err != nil {
		t.Fatalf("GetPriorities after disable: %v", err)
	}
	for _, p := range priorities {
		if p.Field == "biography" {
			if len(p.Disabled) != 2 {
				t.Fatalf("expected 2 disabled providers, got %d", len(p.Disabled))
			}
			if p.Disabled[0] != NameMusicBrainz || p.Disabled[1] != NameWikidata {
				t.Errorf("unexpected disabled list: %v", p.Disabled)
			}
			break
		}
	}

	// Clear disabled list.
	if err := svc.SetDisabledProviders(ctx, "biography", nil); err != nil {
		t.Fatalf("SetDisabledProviders(nil): %v", err)
	}
	priorities, err = svc.GetPriorities(ctx)
	if err != nil {
		t.Fatalf("GetPriorities after clear: %v", err)
	}
	for _, p := range priorities {
		if p.Field == "biography" {
			if len(p.Disabled) != 0 {
				t.Errorf("expected empty disabled list after clear, got %v", p.Disabled)
			}
			break
		}
	}
}

func TestEnabledProviders(t *testing.T) {
	fp := FieldPriority{
		Field:     "biography",
		Providers: []ProviderName{NameMusicBrainz, NameAudioDB, NameDiscogs, NameWikidata},
		Disabled:  []ProviderName{NameAudioDB, NameWikidata},
	}

	enabled := fp.EnabledProviders()
	if len(enabled) != 2 {
		t.Fatalf("expected 2 enabled providers, got %d", len(enabled))
	}
	if enabled[0] != NameMusicBrainz {
		t.Errorf("expected first enabled to be musicbrainz, got %s", enabled[0])
	}
	if enabled[1] != NameDiscogs {
		t.Errorf("expected second enabled to be discogs, got %s", enabled[1])
	}
}

func TestEnabledProvidersNoDisabled(t *testing.T) {
	fp := FieldPriority{
		Field:     "genres",
		Providers: []ProviderName{NameMusicBrainz, NameAudioDB},
	}

	enabled := fp.EnabledProviders()
	if len(enabled) != 2 {
		t.Fatalf("expected 2 enabled providers, got %d", len(enabled))
	}
	if enabled[0] != NameMusicBrainz || enabled[1] != NameAudioDB {
		t.Errorf("expected original order, got %v", enabled)
	}
}

func TestAPIKeyContextOverride(t *testing.T) {
	db := setupTestDB(t)
	enc := setupTestEncryptor(t)
	svc := NewSettingsService(db, enc)
	ctx := context.Background()

	// Store a key in the database.
	if err := svc.SetAPIKey(ctx, NameFanartTV, "db-key"); err != nil {
		t.Fatalf("SetAPIKey: %v", err)
	}

	// Without override: reads from DB.
	key, err := svc.GetAPIKey(ctx, NameFanartTV)
	if err != nil {
		t.Fatalf("GetAPIKey: %v", err)
	}
	if key != "db-key" {
		t.Errorf("expected 'db-key', got %q", key)
	}

	// With override: returns the injected value.
	overrideCtx := WithAPIKeyOverride(ctx, NameFanartTV, "override-key")
	key, err = svc.GetAPIKey(overrideCtx, NameFanartTV)
	if err != nil {
		t.Fatalf("GetAPIKey with override: %v", err)
	}
	if key != "override-key" {
		t.Errorf("expected 'override-key', got %q", key)
	}

	// Override does not affect other providers.
	key, err = svc.GetAPIKey(overrideCtx, NameDiscogs)
	if err != nil {
		t.Fatalf("GetAPIKey other provider: %v", err)
	}
	if key != "" {
		t.Errorf("expected empty key for Discogs, got %q", key)
	}
}

func TestKeyStatusRoundTrip(t *testing.T) {
	db := setupTestDB(t)
	enc := setupTestEncryptor(t)
	svc := NewSettingsService(db, enc)
	ctx := context.Background()

	// Initially no status.
	status, err := svc.GetKeyStatus(ctx, NameFanartTV)
	if err != nil {
		t.Fatalf("GetKeyStatus: %v", err)
	}
	if status != "" {
		t.Errorf("expected empty status, got %q", status)
	}

	// Set status "ok".
	if err := svc.SetKeyStatus(ctx, NameFanartTV, "ok"); err != nil {
		t.Fatalf("SetKeyStatus ok: %v", err)
	}
	status, err = svc.GetKeyStatus(ctx, NameFanartTV)
	if err != nil {
		t.Fatalf("GetKeyStatus after set: %v", err)
	}
	if status != "ok" {
		t.Errorf("expected 'ok', got %q", status)
	}

	// Set status "invalid".
	if err := svc.SetKeyStatus(ctx, NameFanartTV, "invalid"); err != nil {
		t.Fatalf("SetKeyStatus invalid: %v", err)
	}
	status, err = svc.GetKeyStatus(ctx, NameFanartTV)
	if err != nil {
		t.Fatalf("GetKeyStatus after invalid: %v", err)
	}
	if status != "invalid" {
		t.Errorf("expected 'invalid', got %q", status)
	}

	// Clear status.
	if err := svc.SetKeyStatus(ctx, NameFanartTV, ""); err != nil {
		t.Fatalf("SetKeyStatus clear: %v", err)
	}
	status, err = svc.GetKeyStatus(ctx, NameFanartTV)
	if err != nil {
		t.Fatalf("GetKeyStatus after clear: %v", err)
	}
	if status != "" {
		t.Errorf("expected empty status after clear, got %q", status)
	}
}

func TestSetAPIKeyClearsStatus(t *testing.T) {
	db := setupTestDB(t)
	enc := setupTestEncryptor(t)
	svc := NewSettingsService(db, enc)
	ctx := context.Background()

	// Set a key and mark it as "ok".
	if err := svc.SetAPIKey(ctx, NameFanartTV, "key-1"); err != nil {
		t.Fatalf("SetAPIKey: %v", err)
	}
	if err := svc.SetKeyStatus(ctx, NameFanartTV, "ok"); err != nil {
		t.Fatalf("SetKeyStatus: %v", err)
	}

	// Update the key: status should be cleared.
	if err := svc.SetAPIKey(ctx, NameFanartTV, "key-2"); err != nil {
		t.Fatalf("SetAPIKey update: %v", err)
	}
	status, err := svc.GetKeyStatus(ctx, NameFanartTV)
	if err != nil {
		t.Fatalf("GetKeyStatus: %v", err)
	}
	if status != "" {
		t.Errorf("expected cleared status after key update, got %q", status)
	}
}

func TestDeleteAPIKeyClearsStatus(t *testing.T) {
	db := setupTestDB(t)
	enc := setupTestEncryptor(t)
	svc := NewSettingsService(db, enc)
	ctx := context.Background()

	if err := svc.SetAPIKey(ctx, NameFanartTV, "key-1"); err != nil {
		t.Fatalf("SetAPIKey: %v", err)
	}
	if err := svc.SetKeyStatus(ctx, NameFanartTV, "ok"); err != nil {
		t.Fatalf("SetKeyStatus: %v", err)
	}
	if err := svc.DeleteAPIKey(ctx, NameFanartTV); err != nil {
		t.Fatalf("DeleteAPIKey: %v", err)
	}

	status, err := svc.GetKeyStatus(ctx, NameFanartTV)
	if err != nil {
		t.Fatalf("GetKeyStatus: %v", err)
	}
	if status != "" {
		t.Errorf("expected cleared status after delete, got %q", status)
	}
}

func TestListProviderKeyStatusesWithPersistedStatus(t *testing.T) {
	db := setupTestDB(t)
	enc := setupTestEncryptor(t)
	svc := NewSettingsService(db, enc)
	ctx := context.Background()

	// Set a key and mark it tested.
	if err := svc.SetAPIKey(ctx, NameFanartTV, "key-1"); err != nil {
		t.Fatalf("SetAPIKey: %v", err)
	}
	if err := svc.SetKeyStatus(ctx, NameFanartTV, "ok"); err != nil {
		t.Fatalf("SetKeyStatus: %v", err)
	}

	statuses, err := svc.ListProviderKeyStatuses(ctx)
	if err != nil {
		t.Fatalf("ListProviderKeyStatuses: %v", err)
	}
	fanart := findStatus(t, statuses, NameFanartTV)
	if fanart.Status != "ok" {
		t.Errorf("expected status 'ok', got %q", fanart.Status)
	}

	// Mark as invalid.
	if err := svc.SetKeyStatus(ctx, NameFanartTV, "invalid"); err != nil {
		t.Fatalf("SetKeyStatus invalid: %v", err)
	}
	statuses, err = svc.ListProviderKeyStatuses(ctx)
	if err != nil {
		t.Fatalf("ListProviderKeyStatuses: %v", err)
	}
	fanart = findStatus(t, statuses, NameFanartTV)
	if fanart.Status != "invalid" {
		t.Errorf("expected status 'invalid', got %q", fanart.Status)
	}

	// Clear status: should revert to "untested".
	if err := svc.SetKeyStatus(ctx, NameFanartTV, ""); err != nil {
		t.Fatalf("SetKeyStatus clear: %v", err)
	}
	statuses, err = svc.ListProviderKeyStatuses(ctx)
	if err != nil {
		t.Fatalf("ListProviderKeyStatuses: %v", err)
	}
	fanart = findStatus(t, statuses, NameFanartTV)
	if fanart.Status != "untested" {
		t.Errorf("expected status 'untested' after clear, got %q", fanart.Status)
	}
}

func TestBaseURLRoundTrip(t *testing.T) {
	db := setupTestDB(t)
	enc := setupTestEncryptor(t)
	svc := NewSettingsService(db, enc)
	ctx := context.Background()

	// Initially empty.
	url, err := svc.GetBaseURL(ctx, NameMusicBrainz)
	if err != nil {
		t.Fatalf("GetBaseURL: %v", err)
	}
	if url != "" {
		t.Errorf("expected empty base URL, got %q", url)
	}

	// Set a custom URL.
	if err := svc.SetBaseURL(ctx, NameMusicBrainz, "http://192.168.1.50:5000/ws/2"); err != nil {
		t.Fatalf("SetBaseURL: %v", err)
	}

	url, err = svc.GetBaseURL(ctx, NameMusicBrainz)
	if err != nil {
		t.Fatalf("GetBaseURL after set: %v", err)
	}
	if url != "http://192.168.1.50:5000/ws/2" {
		t.Errorf("expected custom URL, got %q", url)
	}

	// Delete the URL.
	if err := svc.DeleteBaseURL(ctx, NameMusicBrainz); err != nil {
		t.Fatalf("DeleteBaseURL: %v", err)
	}

	url, err = svc.GetBaseURL(ctx, NameMusicBrainz)
	if err != nil {
		t.Fatalf("GetBaseURL after delete: %v", err)
	}
	if url != "" {
		t.Errorf("expected empty URL after delete, got %q", url)
	}
}

func TestRateLimitRoundTrip(t *testing.T) {
	db := setupTestDB(t)
	enc := setupTestEncryptor(t)
	svc := NewSettingsService(db, enc)
	ctx := context.Background()

	// Initially zero.
	limit, err := svc.GetRateLimit(ctx, NameMusicBrainz)
	if err != nil {
		t.Fatalf("GetRateLimit: %v", err)
	}
	if limit != 0 {
		t.Errorf("expected 0 rate limit, got %v", limit)
	}

	// Set a custom limit.
	if err := svc.SetRateLimit(ctx, NameMusicBrainz, 10); err != nil {
		t.Fatalf("SetRateLimit: %v", err)
	}

	limit, err = svc.GetRateLimit(ctx, NameMusicBrainz)
	if err != nil {
		t.Fatalf("GetRateLimit after set: %v", err)
	}
	if limit != 10 {
		t.Errorf("expected 10, got %v", limit)
	}

	// Delete the limit.
	if err := svc.DeleteRateLimit(ctx, NameMusicBrainz); err != nil {
		t.Fatalf("DeleteRateLimit: %v", err)
	}

	limit, err = svc.GetRateLimit(ctx, NameMusicBrainz)
	if err != nil {
		t.Fatalf("GetRateLimit after delete: %v", err)
	}
	if limit != 0 {
		t.Errorf("expected 0 after delete, got %v", limit)
	}
}

func TestGetMirrorConfig(t *testing.T) {
	db := setupTestDB(t)
	enc := setupTestEncryptor(t)
	svc := NewSettingsService(db, enc)
	ctx := context.Background()

	// No config initially.
	mc, err := svc.GetMirrorConfig(ctx, NameMusicBrainz)
	if err != nil {
		t.Fatalf("GetMirrorConfig: %v", err)
	}
	if mc != nil {
		t.Errorf("expected nil mirror config, got %+v", mc)
	}

	// Set base URL and rate limit.
	if err := svc.SetBaseURL(ctx, NameMusicBrainz, "http://mirror:5000/ws/2"); err != nil {
		t.Fatalf("SetBaseURL: %v", err)
	}
	if err := svc.SetRateLimit(ctx, NameMusicBrainz, 20); err != nil {
		t.Fatalf("SetRateLimit: %v", err)
	}

	mc, err = svc.GetMirrorConfig(ctx, NameMusicBrainz)
	if err != nil {
		t.Fatalf("GetMirrorConfig after set: %v", err)
	}
	if mc == nil {
		t.Fatal("expected non-nil mirror config")
	}
	if mc.BaseURL != "http://mirror:5000/ws/2" {
		t.Errorf("expected base URL, got %q", mc.BaseURL)
	}
	if mc.RateLimit != 20 {
		t.Errorf("expected rate limit 20, got %v", mc.RateLimit)
	}
}

func TestMirrorInProviderKeyStatuses(t *testing.T) {
	db := setupTestDB(t)
	enc := setupTestEncryptor(t)
	svc := NewSettingsService(db, enc)
	ctx := context.Background()

	// Set mirror config for MusicBrainz.
	if err := svc.SetBaseURL(ctx, NameMusicBrainz, "http://mirror:5000/ws/2"); err != nil {
		t.Fatalf("SetBaseURL: %v", err)
	}
	if err := svc.SetRateLimit(ctx, NameMusicBrainz, 15); err != nil {
		t.Fatalf("SetRateLimit: %v", err)
	}

	statuses, err := svc.ListProviderKeyStatuses(ctx)
	if err != nil {
		t.Fatalf("ListProviderKeyStatuses: %v", err)
	}

	mb := findStatus(t, statuses, NameMusicBrainz)
	if !mb.SupportsBaseURL {
		t.Error("expected MusicBrainz SupportsBaseURL=true")
	}
	if mb.Mirror == nil {
		t.Fatal("expected non-nil Mirror on MusicBrainz")
	}
	if mb.Mirror.BaseURL != "http://mirror:5000/ws/2" {
		t.Errorf("expected mirror base URL, got %q", mb.Mirror.BaseURL)
	}
	if mb.Mirror.RateLimit != 15 {
		t.Errorf("expected mirror rate limit 15, got %v", mb.Mirror.RateLimit)
	}

	// Other providers should not have mirror config.
	fanart := findStatus(t, statuses, NameFanartTV)
	if fanart.SupportsBaseURL {
		t.Error("expected Fanart.tv SupportsBaseURL=false")
	}
	if fanart.Mirror != nil {
		t.Error("expected nil Mirror on Fanart.tv")
	}
}

func TestAnyWebSearchEnabled(t *testing.T) {
	db := setupTestDB(t)
	enc := setupTestEncryptor(t)
	svc := NewSettingsService(db, enc)
	ctx := context.Background()

	any, err := svc.AnyWebSearchEnabled(ctx)
	if err != nil {
		t.Fatalf("AnyWebSearchEnabled: %v", err)
	}
	if any {
		t.Error("expected false when none enabled")
	}

	if err := svc.SetWebSearchEnabled(ctx, NameDuckDuckGo, true); err != nil {
		t.Fatalf("SetWebSearchEnabled: %v", err)
	}
	any, err = svc.AnyWebSearchEnabled(ctx)
	if err != nil {
		t.Fatalf("AnyWebSearchEnabled after enable: %v", err)
	}
	if !any {
		t.Error("expected true when duckduckgo enabled")
	}
}

func TestNameSimilarityThresholdDefault(t *testing.T) {
	db := setupTestDB(t)
	enc := setupTestEncryptor(t)
	svc := NewSettingsService(db, enc)
	ctx := context.Background()

	// When no setting is stored, should return the default.
	threshold, err := svc.GetNameSimilarityThreshold(ctx)
	if err != nil {
		t.Fatalf("GetNameSimilarityThreshold: %v", err)
	}
	if threshold != DefaultNameSimilarityThreshold {
		t.Errorf("expected default %d, got %d", DefaultNameSimilarityThreshold, threshold)
	}
}

func TestNameSimilarityThresholdRoundTrip(t *testing.T) {
	db := setupTestDB(t)
	enc := setupTestEncryptor(t)
	svc := NewSettingsService(db, enc)
	ctx := context.Background()

	// Set a custom threshold.
	if err := svc.SetNameSimilarityThreshold(ctx, 80); err != nil {
		t.Fatalf("SetNameSimilarityThreshold: %v", err)
	}
	threshold, err := svc.GetNameSimilarityThreshold(ctx)
	if err != nil {
		t.Fatalf("GetNameSimilarityThreshold: %v", err)
	}
	if threshold != 80 {
		t.Errorf("expected 80, got %d", threshold)
	}

	// Update to 0 (disables validation).
	if err := svc.SetNameSimilarityThreshold(ctx, 0); err != nil {
		t.Fatalf("SetNameSimilarityThreshold: %v", err)
	}
	threshold, err = svc.GetNameSimilarityThreshold(ctx)
	if err != nil {
		t.Fatalf("GetNameSimilarityThreshold: %v", err)
	}
	if threshold != 0 {
		t.Errorf("expected 0, got %d", threshold)
	}
}

func TestNameSimilarityThresholdValidation(t *testing.T) {
	db := setupTestDB(t)
	enc := setupTestEncryptor(t)
	svc := NewSettingsService(db, enc)
	ctx := context.Background()

	// Values outside 0-100 should be rejected.
	if err := svc.SetNameSimilarityThreshold(ctx, -1); err == nil {
		t.Error("expected error for negative threshold")
	}
	if err := svc.SetNameSimilarityThreshold(ctx, 101); err == nil {
		t.Error("expected error for threshold > 100")
	}

	// The stored value should still be the default since the invalid sets failed.
	threshold, err := svc.GetNameSimilarityThreshold(ctx)
	if err != nil {
		t.Fatalf("GetNameSimilarityThreshold: %v", err)
	}
	if threshold != DefaultNameSimilarityThreshold {
		t.Errorf("expected default %d after invalid sets, got %d", DefaultNameSimilarityThreshold, threshold)
	}
}

// TestResetPriorities seeds custom priority + disabled rows for two fields,
// calls ResetPriorities, and verifies the stored rows are gone (so
// GetPriorities falls back to the built-in DefaultPriorities).
func TestResetPriorities(t *testing.T) {
	db := setupTestDB(t)
	enc := setupTestEncryptor(t)
	svc := NewSettingsService(db, enc)
	ctx := context.Background()

	// Seed two fields with custom priority + disabled overrides.
	if err := svc.SetPriority(ctx, "biography", []ProviderName{NameLastFM, NameWikipedia}); err != nil {
		t.Fatalf("seeding biography priority: %v", err)
	}
	if err := svc.SetDisabledProviders(ctx, "biography", []ProviderName{NameLastFM}); err != nil {
		t.Fatalf("seeding biography disabled: %v", err)
	}
	if err := svc.SetPriority(ctx, "genres", []ProviderName{NameAudioDB}); err != nil {
		t.Fatalf("seeding genres priority: %v", err)
	}

	// Sanity-check: at least three provider.priority.% rows now exist.
	var beforeCount int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM settings WHERE key LIKE 'provider.priority.%'").Scan(&beforeCount); err != nil {
		t.Fatalf("counting seeded rows: %v", err)
	}
	if beforeCount < 3 {
		t.Fatalf("expected at least 3 seeded rows, got %d", beforeCount)
	}

	if err := svc.ResetPriorities(ctx); err != nil {
		t.Fatalf("ResetPriorities: %v", err)
	}

	var afterCount int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM settings WHERE key LIKE 'provider.priority.%'").Scan(&afterCount); err != nil {
		t.Fatalf("counting rows post-reset: %v", err)
	}
	if afterCount != 0 {
		t.Errorf("expected 0 provider.priority.* rows after reset, got %d", afterCount)
	}

	// Service contract: after reset, GetPriorities must equal DefaultPriorities
	// (same length, same Field per index, and an empty Disabled set since the
	// reset clears all overrides).
	got, err := svc.GetPriorities(ctx)
	if err != nil {
		t.Fatalf("GetPriorities after reset: %v", err)
	}
	defaults := DefaultPriorities()
	if len(got) != len(defaults) {
		t.Fatalf("expected %d priority entries after reset, got %d", len(defaults), len(got))
	}
	for i, d := range defaults {
		if got[i].Field != d.Field {
			t.Errorf("priority[%d]: expected Field %q, got %q", i, d.Field, got[i].Field)
		}
		if len(got[i].Disabled) != 0 {
			t.Errorf("priority[%d] (%s): expected empty Disabled, got %v", i, d.Field, got[i].Disabled)
		}
	}
}

// TestResetPrioritiesDBError covers the wrapped-error path by closing the
// underlying database before invoking ResetPriorities.
func TestResetPrioritiesDBError(t *testing.T) {
	db := setupTestDB(t)
	enc := setupTestEncryptor(t)
	svc := NewSettingsService(db, enc)

	if err := db.Close(); err != nil {
		t.Fatalf("closing db: %v", err)
	}

	err := svc.ResetPriorities(context.Background())
	if err == nil {
		t.Fatal("expected error after db close, got nil")
	}
	// Assert the error carries the documented "resetting priorities" wrap so
	// callers (and humans reading logs) can attribute the failure rather than
	// see a bare driver message.
	if !strings.Contains(err.Error(), "resetting priorities") {
		t.Fatalf("expected wrapped reset context, got: %v", err)
	}
}

// TestResetPrioritiesPreservesEnabledWebSearch verifies that ResetPriorities
// re-applies currently-enabled web search providers to image-field priority
// rows after the bulk delete, so enabled web search providers do not silently
// disappear from active priority lists.
func TestResetPrioritiesPreservesEnabledWebSearch(t *testing.T) {
	db := setupTestDB(t)
	enc := setupTestEncryptor(t)
	svc := NewSettingsService(db, enc)
	ctx := context.Background()

	// Pick a real web search provider name and enable it.
	wsNames := AllWebSearchProviderNames()
	if len(wsNames) == 0 {
		t.Skip("no web search providers registered")
	}
	wsName := wsNames[0]
	if err := svc.SetWebSearchEnabled(ctx, wsName, true); err != nil {
		t.Fatalf("enabling web search provider %s: %v", wsName, err)
	}

	// Seed a customized image-field priority so the reset has something to clear.
	if err := svc.SetPriority(ctx, "thumb", []ProviderName{NameAudioDB}); err != nil {
		t.Fatalf("seeding thumb priority: %v", err)
	}

	if err := svc.ResetPriorities(ctx); err != nil {
		t.Fatalf("ResetPriorities: %v", err)
	}

	// After reset, GetPriorities for image fields must contain the enabled
	// web search provider so it actually participates in fetches.
	priorities, err := svc.GetPriorities(ctx)
	if err != nil {
		t.Fatalf("GetPriorities: %v", err)
	}
	imageFields := map[string]bool{"thumb": true, "fanart": true, "logo": true, "banner": true}
	seen := map[string]bool{}
	for _, p := range priorities {
		if !imageFields[p.Field] {
			continue
		}
		seen[p.Field] = true
		found := false
		for _, name := range p.Providers {
			if name == wsName {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("image field %s missing enabled web search provider %s after reset; got %v", p.Field, wsName, p.Providers)
		}
	}
	// If an expected image field is absent from GetPriorities entirely, the
	// loop above silently passes; assert each one was observed so missing
	// fields fail the test.
	for field := range imageFields {
		if !seen[field] {
			t.Errorf("expected image field %s in GetPriorities after reset, but it was missing", field)
		}
	}

	// And the websearch.enabled flag itself must remain true (reset only
	// touches provider.priority.* rows).
	stillEnabled, err := svc.IsWebSearchEnabled(ctx, wsName)
	if err != nil {
		t.Fatalf("IsWebSearchEnabled after reset: %v", err)
	}
	if !stillEnabled {
		t.Errorf("web search provider %s should remain enabled after reset", wsName)
	}
}

// TestDefaultPriorities_BiographyExcludesWikidata pins #1029: Wikidata's
// mapArtist never populates Biography, so it must not appear in the default
// biography priority list. Other fields where Wikidata is a real data source
// (members, formed, born, ...) keep it -- this test only locks down the
// biography slot. Paired with migration 007 which scrubs existing installs.
func TestDefaultPriorities_BiographyExcludesWikidata(t *testing.T) {
	defaults := DefaultPriorities()

	var bio FieldPriority
	for _, p := range defaults {
		if p.Field == "biography" {
			bio = p
			break
		}
	}
	if len(bio.Providers) == 0 {
		t.Fatal("biography field missing from DefaultPriorities()")
	}
	for _, p := range bio.Providers {
		if p == NameWikidata {
			t.Errorf("biography default contains %s; Wikidata cannot return biographies (see #1029)", NameWikidata)
		}
	}

	// Pin the exact contents so a future refactor that renames or drops one
	// of the remaining biography providers fails loudly instead of silently
	// reordering the default chain.
	wantBio := []ProviderName{NameWikipedia, NameLastFM, NameAudioDB, NameDiscogs, NameGenius}
	if !reflect.DeepEqual(bio.Providers, wantBio) {
		t.Errorf("biography default = %v, want %v", bio.Providers, wantBio)
	}

	// Sanity: Wikidata must still appear in at least one fact-shaped field so
	// this test fails loudly if a later refactor strips it everywhere.
	factFields := map[string]bool{
		"members": false, "formed": false, "born": false, "died": false,
		"disbanded": false, "type": false, "gender": false, "origin": false,
	}
	for _, p := range defaults {
		if _, ok := factFields[p.Field]; !ok {
			continue
		}
		for _, prov := range p.Providers {
			if prov == NameWikidata {
				factFields[p.Field] = true
			}
		}
	}
	for field, present := range factFields {
		if !present {
			t.Errorf("Wikidata missing from %s default priority; #1029 only removes it from biography", field)
		}
	}
}

// TestSetFieldVerbosity_Persists verifies that a verbosity value is stored and
// retrieved correctly, and that the default is returned when nothing is stored.
func TestSetFieldVerbosity_Persists(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	enc := setupTestEncryptor(t)
	svc := NewSettingsService(db, enc)
	ctx := context.Background()

	// Before any value is stored, GetFieldVerbosity returns the catalogue default.
	got, err := svc.GetFieldVerbosity(ctx, NameWikipedia, "biography")
	if err != nil {
		t.Fatalf("GetFieldVerbosity (default): %v", err)
	}
	if got != VerbosityIntro {
		t.Errorf("default verbosity = %q, want %q", got, VerbosityIntro)
	}

	// Store VerbosityFull and read it back.
	if err := svc.SetFieldVerbosity(ctx, NameWikipedia, "biography", VerbosityFull); err != nil {
		t.Fatalf("SetFieldVerbosity: %v", err)
	}
	got, err = svc.GetFieldVerbosity(ctx, NameWikipedia, "biography")
	if err != nil {
		t.Fatalf("GetFieldVerbosity (after set): %v", err)
	}
	if got != VerbosityFull {
		t.Errorf("stored verbosity = %q, want %q", got, VerbosityFull)
	}

	// Update back to intro.
	if err := svc.SetFieldVerbosity(ctx, NameWikipedia, "biography", VerbosityIntro); err != nil {
		t.Fatalf("SetFieldVerbosity (reset): %v", err)
	}
	got, err = svc.GetFieldVerbosity(ctx, NameWikipedia, "biography")
	if err != nil {
		t.Fatalf("GetFieldVerbosity (after reset): %v", err)
	}
	if got != VerbosityIntro {
		t.Errorf("reset verbosity = %q, want %q", got, VerbosityIntro)
	}
}

// TestSetFieldVerbosity_InvalidValue verifies that SetFieldVerbosity rejects
// unknown verbosity values without modifying the stored setting.
func TestSetFieldVerbosity_InvalidValue(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	enc := setupTestEncryptor(t)
	svc := NewSettingsService(db, enc)
	ctx := context.Background()

	err := svc.SetFieldVerbosity(ctx, NameWikipedia, "biography", "medium")
	if err == nil {
		t.Error("expected error for invalid verbosity value, got nil")
	}
}

// TestSetFieldVerbosity_InvalidField verifies that SetFieldVerbosity rejects
// unknown field names.
func TestSetFieldVerbosity_InvalidField(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	enc := setupTestEncryptor(t)
	svc := NewSettingsService(db, enc)
	ctx := context.Background()

	err := svc.SetFieldVerbosity(ctx, NameWikipedia, "nonexistent_field", VerbosityFull)
	if err == nil {
		t.Error("expected error for unknown field, got nil")
	}
}

// TestGetFieldVerbosity_NoOptionsProvider verifies that GetFieldVerbosity
// returns an empty string for a provider that has no verbosity catalogue.
func TestGetFieldVerbosity_NoOptionsProvider(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	enc := setupTestEncryptor(t)
	svc := NewSettingsService(db, enc)
	ctx := context.Background()

	// MusicBrainz has no verbosity options -- should return empty string.
	got, err := svc.GetFieldVerbosity(ctx, NameMusicBrainz, "biography")
	if err != nil {
		t.Fatalf("GetFieldVerbosity for provider without options: %v", err)
	}
	if got != "" {
		t.Errorf("verbosity for provider without options = %q, want empty string", got)
	}
}

// TestGetFieldVerbosity_CorruptStoredValue verifies a stored value that is not a
// valid catalogue option is treated as unset: the catalogue default is returned.
func TestGetFieldVerbosity_CorruptStoredValue(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	enc := setupTestEncryptor(t)
	svc := NewSettingsService(db, enc)
	ctx := context.Background()

	// Write a bogus value directly, bypassing SetFieldVerbosity's validation.
	if _, err := db.ExecContext(ctx,
		"INSERT INTO settings (key, value) VALUES (?, ?)",
		"provider.wikipedia.field_verbosity.biography", "bogus"); err != nil {
		t.Fatalf("seeding corrupt value: %v", err)
	}

	got, err := svc.GetFieldVerbosity(ctx, NameWikipedia, "biography")
	if err != nil {
		t.Fatalf("GetFieldVerbosity: %v", err)
	}
	if got != VerbosityIntro {
		t.Errorf("verbosity for a corrupt stored value = %q, want the default %q", got, VerbosityIntro)
	}
}
