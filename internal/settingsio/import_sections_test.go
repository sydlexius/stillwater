package settingsio

// import_sections_test.go contains focused unit tests for the per-section
// import helpers. Each test exercises a specific section in isolation,
// using a real SQLite DB (copied from the pre-migrated template) so schema
// constraints are enforced exactly as in production.

import (
	"context"
	"testing"
	"time"

	"log/slog"

	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/platform"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/internal/rule"
	"github.com/sydlexius/stillwater/internal/scraper"
	"github.com/sydlexius/stillwater/internal/webhook"
)

// --- importSettings ---

// TestImportSettings_UpsertAndCount verifies that importSettings upserts
// every key in the map and increments result.Settings once per key.
func TestImportSettings_UpsertAndCount(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)
	svc := NewService(db, provSettings, connSvc, platSvc, whSvc)

	settings := map[string]string{
		"section.key_a": "val_a",
		"section.key_b": "val_b",
	}
	result := &ImportResult{}
	if err := svc.importSettings(ctx, db, settings, result); err != nil {
		t.Fatalf("importSettings: %v", err)
	}
	if result.Settings != 2 {
		t.Errorf("Settings count: got %d, want 2", result.Settings)
	}

	for k, want := range settings {
		var got string
		if err := db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, k).Scan(&got); err != nil {
			t.Fatalf("reading %s: %v", k, err)
		}
		if got != want {
			t.Errorf("%s: got %q, want %q", k, got, want)
		}
	}
}

// TestImportSettings_IdempotentUpsert confirms that calling importSettings
// twice with the same map updates the value and does not add duplicate rows.
func TestImportSettings_IdempotentUpsert(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)
	svc := NewService(db, provSettings, connSvc, platSvc, whSvc)

	settings := map[string]string{"idem.key": "first"}
	if err := svc.importSettings(ctx, db, settings, &ImportResult{}); err != nil {
		t.Fatalf("first importSettings: %v", err)
	}
	settings["idem.key"] = "second"
	if err := svc.importSettings(ctx, db, settings, &ImportResult{}); err != nil {
		t.Fatalf("second importSettings: %v", err)
	}

	var got string
	if err := db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'idem.key'`).Scan(&got); err != nil {
		t.Fatalf("reading idem.key: %v", err)
	}
	if got != "second" {
		t.Errorf("upsert: got %q, want second", got)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM settings WHERE key = 'idem.key'`).Scan(&count); err != nil {
		t.Fatalf("counting idem.key rows: %v", err)
	}
	if count != 1 {
		t.Errorf("row count after two upserts: got %d, want 1", count)
	}
}

// TestImportSettings_EmptyMapIsNoOp confirms that an empty settings map
// leaves result.Settings at zero and does not fail.
func TestImportSettings_EmptyMapIsNoOp(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)
	svc := NewService(db, provSettings, connSvc, platSvc, whSvc)

	result := &ImportResult{}
	if err := svc.importSettings(ctx, db, map[string]string{}, result); err != nil {
		t.Fatalf("importSettings with empty map: %v", err)
	}
	if result.Settings != 0 {
		t.Errorf("Settings count: got %d, want 0", result.Settings)
	}
}

// --- importProviderKeys ---

// TestImportProviderKeys_CountsAndPersists verifies that importProviderKeys
// writes the provided key and increments result.ProviderKeys.
func TestImportProviderKeys_CountsAndPersists(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)
	svc := NewService(db, provSettings, connSvc, platSvc, whSvc)

	keys := map[string]string{string(provider.NameFanartTV): "fanart-test-key"}
	result := &ImportResult{}
	if err := svc.importProviderKeys(ctx, db, keys, result); err != nil {
		t.Fatalf("importProviderKeys: %v", err)
	}
	if result.ProviderKeys != 1 {
		t.Errorf("ProviderKeys count: got %d, want 1", result.ProviderKeys)
	}
	got, err := provSettings.GetAPIKey(ctx, provider.NameFanartTV)
	if err != nil {
		t.Fatalf("reading provider key: %v", err)
	}
	if got != "fanart-test-key" {
		t.Errorf("provider key: got %q, want fanart-test-key", got)
	}
}

// --- importConnections ---

// TestImportConnections_CreateAndUpdate verifies that importConnections
// creates a new connection when none matches (type, url), and updates the
// existing connection when one does.
func TestImportConnections_CreateAndUpdate(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)
	svc := NewService(db, provSettings, connSvc, platSvc, whSvc)

	// First call: insert.
	conns := []ConnectionExport{
		{Name: "Emby A", Type: "emby", URL: "http://emby.local:8096", APIKey: "key1", Enabled: true},
	}
	result := &ImportResult{}
	if err := svc.importConnections(ctx, db, conns, result, true, true); err != nil {
		t.Fatalf("importConnections (insert): %v", err)
	}
	if result.Connections != 1 {
		t.Errorf("Connections count after insert: got %d, want 1", result.Connections)
	}

	// Second call with same (type, url) but updated name and key: update.
	conns[0].Name = "Emby A Renamed"
	conns[0].APIKey = "key2"
	result2 := &ImportResult{}
	if err := svc.importConnections(ctx, db, conns, result2, true, true); err != nil {
		t.Fatalf("importConnections (update): %v", err)
	}
	if result2.Connections != 1 {
		t.Errorf("Connections count after update: got %d, want 1", result2.Connections)
	}

	all, err := connSvc.List(ctx)
	if err != nil {
		t.Fatalf("listing connections: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("expected 1 connection, got %d", len(all))
	}
	if all[0].Name != "Emby A Renamed" {
		t.Errorf("connection name: got %q, want Emby A Renamed", all[0].Name)
	}
}

// TestImportConnections_PreV14EnvelopePreservesV14Fields proves that when a
// legacy (pre-1.4) envelope updates an existing target connection, the four
// v1.4-only fields the envelope cannot carry are NOT overwritten with their
// zero values. Without the gate, a 1.3 backup would silently disable
// FeatureMetadataPush/FeatureTriggerRefresh/FeatureManageServerFiles and
// clear PreStillwaterConfigJSON the operator had set after upgrading.
func TestImportConnections_PreV14EnvelopePreservesV14Fields(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)
	svc := NewService(db, provSettings, connSvc, platSvc, whSvc)

	// Seed the target with a connection that has all four v1.4 fields set
	// to non-zero values, simulating an operator who upgraded to a v1.4
	// binary and then enabled the new toggles locally.
	seed := &connection.Connection{
		Name:                     "Emby A",
		Type:                     "emby",
		URL:                      "http://emby.local:8096",
		APIKey:                   "key1",
		Enabled:                  true,
		FeatureMetadataPush:      true,
		FeatureTriggerRefresh:    true,
		FeatureManageServerFiles: true,
		PreStillwaterConfigJSON:  `{"some":"snapshot"}`,
	}
	if err := connSvc.Create(ctx, seed); err != nil {
		t.Fatalf("seeding target connection: %v", err)
	}

	// Pre-1.4 envelope: the four v1.4 fields are absent in the wire format
	// and decode to zero values on the import side.
	conns := []ConnectionExport{{
		Name: "Emby A", Type: "emby", URL: "http://emby.local:8096",
		APIKey: "key2", Enabled: true,
		// v1.4-only fields explicitly left zero to model a pre-1.4 payload.
	}}
	if err := svc.importConnections(ctx, db, conns, &ImportResult{}, false, false); err != nil {
		t.Fatalf("importConnections (legacy envelope): %v", err)
	}

	all, err := connSvc.List(ctx)
	if err != nil {
		t.Fatalf("listing connections: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("expected 1 connection, got %d", len(all))
	}
	got := all[0]
	if got.APIKey != "key2" {
		t.Errorf("APIKey: got %q, want key2 (legacy import must still rotate keys)", got.APIKey)
	}
	if !got.FeatureMetadataPush {
		t.Error("FeatureMetadataPush was cleared by a legacy import; target value must be preserved")
	}
	if !got.FeatureTriggerRefresh {
		t.Error("FeatureTriggerRefresh was cleared by a legacy import; target value must be preserved")
	}
	if !got.FeatureManageServerFiles {
		t.Error("FeatureManageServerFiles was cleared by a legacy import; target value must be preserved")
	}
	if got.PreStillwaterConfigJSON != `{"some":"snapshot"}` {
		t.Errorf("PreStillwaterConfigJSON was overwritten by a legacy import; got %q", got.PreStillwaterConfigJSON)
	}
}

// TestEnvelopeCarriesConnectionV14Fields locks the gate's allow-set so a
// future envelope-version bump trips this test if the maintainer forgets to
// list it (or to refactor the helper to take a structured capability set).
func TestEnvelopeCarriesConnectionV14Fields(t *testing.T) {
	cases := []struct {
		version string
		want    bool
	}{
		{"1.0", false},
		{"1.1", false},
		{"1.2", false},
		{"1.3", false},
		{"1.4", true},
		{"1.5", true},
		{"", false},
		{"99.0", false},
	}
	for _, tc := range cases {
		if got := envelopeCarriesConnectionV14Fields(tc.version); got != tc.want {
			t.Errorf("envelopeCarriesConnectionV14Fields(%q) = %v, want %v", tc.version, got, tc.want)
		}
	}
}

// TestEnvelopeCarriesConnectionV15Fields locks the v1.5 allow-set. Same
// motivation as the v1.4 test: a future version bump that forgets to add
// itself here would silently disable VerifyPathAfterUpdate on legacy
// imports because the field would decode to its zero value.
func TestEnvelopeCarriesConnectionV15Fields(t *testing.T) {
	cases := []struct {
		version string
		want    bool
	}{
		{"1.0", false},
		{"1.1", false},
		{"1.2", false},
		{"1.3", false},
		{"1.4", false},
		{"1.5", true},
		{"", false},
		{"99.0", false},
	}
	for _, tc := range cases {
		if got := envelopeCarriesConnectionV15Fields(tc.version); got != tc.want {
			t.Errorf("envelopeCarriesConnectionV15Fields(%q) = %v, want %v", tc.version, got, tc.want)
		}
	}
}

// TestImportConnections_PreV15EnvelopePreservesV15Fields proves that a
// pre-1.5 envelope updating an existing target connection does NOT clear
// VerifyPathAfterUpdate. Without the gate, a 1.4 backup would silently
// disable the Lidarr path-verification opt-in after restore.
func TestImportConnections_PreV15EnvelopePreservesV15Fields(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)
	svc := NewService(db, provSettings, connSvc, platSvc, whSvc)

	// Seed the target with VerifyPathAfterUpdate=true, simulating an
	// operator who upgraded to a v1.5 binary and then enabled the toggle.
	seed := &connection.Connection{
		Name:                  "Lidarr A",
		Type:                  "lidarr",
		URL:                   "http://lidarr.local:8686",
		APIKey:                "key1",
		Enabled:               true,
		VerifyPathAfterUpdate: true,
	}
	if err := connSvc.Create(ctx, seed); err != nil {
		t.Fatalf("seeding target connection: %v", err)
	}

	// Pre-1.5 envelope: the v1.5 field is absent and decodes to zero.
	conns := []ConnectionExport{{
		Name: "Lidarr A", Type: "lidarr", URL: "http://lidarr.local:8686",
		APIKey: "key2", Enabled: true,
		// VerifyPathAfterUpdate explicitly left zero to model a pre-1.5 payload.
	}}
	if err := svc.importConnections(ctx, db, conns, &ImportResult{}, true, false); err != nil {
		t.Fatalf("importConnections (legacy envelope): %v", err)
	}

	all, err := connSvc.List(ctx)
	if err != nil {
		t.Fatalf("listing connections: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("expected 1 connection, got %d", len(all))
	}
	got := all[0]
	if got.APIKey != "key2" {
		t.Errorf("APIKey: got %q, want key2 (legacy import must still rotate keys)", got.APIKey)
	}
	if !got.VerifyPathAfterUpdate {
		t.Error("VerifyPathAfterUpdate was cleared by a legacy import; target value must be preserved")
	}
}

// --- importPlatformProfiles ---

// TestImportPlatformProfiles_CreateAndUpdate confirms the upsert-by-name
// logic: a new name produces a new row, and the same name updates the existing
// row without duplicating it.
func TestImportPlatformProfiles_CreateAndUpdate(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)
	svc := NewService(db, provSettings, connSvc, platSvc, whSvc)

	profiles := []platform.Profile{
		{Name: "Test Profile", NFOEnabled: true, NFOFormat: "kodi"},
	}
	result := &ImportResult{}
	if err := svc.importPlatformProfiles(ctx, db, profiles, result); err != nil {
		t.Fatalf("importPlatformProfiles (create): %v", err)
	}
	if result.Profiles != 1 {
		t.Errorf("Profiles count after create: got %d, want 1", result.Profiles)
	}

	// Re-import the same profile with a changed NFOFormat: must update, not insert.
	profiles[0].NFOFormat = "emby"
	result2 := &ImportResult{}
	if err := svc.importPlatformProfiles(ctx, db, profiles, result2); err != nil {
		t.Fatalf("importPlatformProfiles (update): %v", err)
	}

	all, err := platSvc.List(ctx)
	if err != nil {
		t.Fatalf("listing profiles: %v", err)
	}
	var found *platform.Profile
	foundCount := 0
	for i := range all {
		if all[i].Name == "Test Profile" {
			foundCount++
			found = &all[i]
		}
	}
	if foundCount != 1 {
		t.Fatalf("expected exactly 1 Test Profile row, got %d", foundCount)
	}
	if found == nil {
		t.Fatal("Test Profile not found after import")
	}
	if found.NFOFormat != "emby" {
		t.Errorf("NFOFormat: got %q, want emby", found.NFOFormat)
	}
}

// --- importWebhooks ---

// TestImportWebhooks_CreateAndUpdate verifies upsert-by-(name,url) semantics.
func TestImportWebhooks_CreateAndUpdate(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)
	svc := NewService(db, provSettings, connSvc, platSvc, whSvc)

	hooks := []webhook.Webhook{
		{Name: "Hook A", URL: "https://hook.example/a", Type: "generic", Events: []string{"artist.new"}, Enabled: true},
	}
	result := &ImportResult{}
	if err := svc.importWebhooks(ctx, db, hooks, result); err != nil {
		t.Fatalf("importWebhooks (create): %v", err)
	}
	if result.Webhooks != 1 {
		t.Errorf("Webhooks count after create: got %d, want 1", result.Webhooks)
	}

	// Re-import with disabled=true: must update, not duplicate.
	hooks[0].Enabled = false
	result2 := &ImportResult{}
	if err := svc.importWebhooks(ctx, db, hooks, result2); err != nil {
		t.Fatalf("importWebhooks (update): %v", err)
	}

	all, err := whSvc.List(ctx)
	if err != nil {
		t.Fatalf("listing webhooks: %v", err)
	}
	// Locate "Hook A" -- the DB may already have other webhooks.
	var found *webhook.Webhook
	foundCount := 0
	for i := range all {
		if all[i].Name == "Hook A" {
			foundCount++
			found = &all[i]
		}
	}
	if foundCount != 1 {
		t.Fatalf("expected exactly 1 Hook A row, got %d", foundCount)
	}
	if found == nil {
		t.Fatal("Hook A not found after import")
	}
	if found.Enabled {
		t.Error("expected Enabled=false after update, got true")
	}
}

// --- importRules ---

// TestImportRules_SkipsNilService confirms that importRules is a no-op when
// the rule service is not attached (WithRuleService was not called).
func TestImportRules_SkipsNilService(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)
	svc := NewService(db, provSettings, connSvc, platSvc, whSvc) // no WithRuleService

	rules := []RuleExport{
		{ID: "thumb_exists", Enabled: false, AutomationMode: "auto"},
	}
	result := &ImportResult{}
	if err := svc.importRules(ctx, db, rules, result); err != nil {
		t.Fatalf("importRules with nil service: %v", err)
	}
	if result.Rules != 0 {
		t.Errorf("Rules count: got %d, want 0 (service nil)", result.Rules)
	}
}

// TestImportRules_SkipsEmptyID verifies that a rule export with an empty ID
// is skipped without error.
func TestImportRules_SkipsEmptyID(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)
	ruleSvc := rule.NewService(db)
	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}
	svc := NewService(db, provSettings, connSvc, platSvc, whSvc).WithRuleService(ruleSvc)

	result := &ImportResult{}
	if err := svc.importRules(ctx, db, []RuleExport{{ID: "", Enabled: true, AutomationMode: "auto"}}, result); err != nil {
		t.Fatalf("importRules with empty ID: %v", err)
	}
	if result.Rules != 0 {
		t.Errorf("Rules count: got %d, want 0 (empty ID skipped)", result.Rules)
	}
}

// TestImportRules_SkipsUnknownID verifies that an ID not present on this
// instance (e.g. from a newer binary) is silently skipped.
func TestImportRules_SkipsUnknownID(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)
	ruleSvc := rule.NewService(db)
	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}
	svc := NewService(db, provSettings, connSvc, platSvc, whSvc).WithRuleService(ruleSvc)

	result := &ImportResult{}
	if err := svc.importRules(ctx, db, []RuleExport{{ID: "future_unknown_rule", Enabled: true, AutomationMode: "auto"}}, result); err != nil {
		t.Fatalf("importRules with unknown ID: %v", err)
	}
	if result.Rules != 0 {
		t.Errorf("Rules count: got %d, want 0 (unknown ID skipped)", result.Rules)
	}
}

// TestImportRules_SkipsInvalidAutomationMode verifies that an entry with an
// unrecognized automation_mode is skipped and the DB is not mutated.
func TestImportRules_SkipsInvalidAutomationMode(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)
	ruleSvc := rule.NewService(db)
	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}
	svc := NewService(db, provSettings, connSvc, platSvc, whSvc).WithRuleService(ruleSvc)

	// Read the current mode so we can assert it is not changed.
	before, err := ruleSvc.GetByID(ctx, rule.RuleThumbExists)
	if err != nil {
		t.Fatalf("getting rule before: %v", err)
	}
	originalMode := before.AutomationMode

	result := &ImportResult{}
	if err := svc.importRules(ctx, db, []RuleExport{
		{ID: rule.RuleThumbExists, Enabled: true, AutomationMode: "invalid_value"},
	}, result); err != nil {
		t.Fatalf("importRules with invalid mode: %v", err)
	}
	if result.Rules != 0 {
		t.Errorf("Rules count: got %d, want 0 (invalid mode skipped)", result.Rules)
	}

	after, err := ruleSvc.GetByID(ctx, rule.RuleThumbExists)
	if err != nil {
		t.Fatalf("getting rule after: %v", err)
	}
	if after.AutomationMode != originalMode {
		t.Errorf("automation_mode mutated to %q despite invalid value; original was %q", after.AutomationMode, originalMode)
	}
}

// TestImportRules_UpdatesValidRule confirms that a rule with a valid
// automation_mode is applied to the DB and result.Rules is incremented.
func TestImportRules_UpdatesValidRule(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)
	ruleSvc := rule.NewService(db)
	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}
	svc := NewService(db, provSettings, connSvc, platSvc, whSvc).WithRuleService(ruleSvc)

	result := &ImportResult{}
	if err := svc.importRules(ctx, db, []RuleExport{
		{ID: rule.RuleThumbExists, Enabled: false, AutomationMode: rule.AutomationModeAuto},
	}, result); err != nil {
		t.Fatalf("importRules: %v", err)
	}
	if result.Rules != 1 {
		t.Errorf("Rules count: got %d, want 1", result.Rules)
	}

	updated, err := ruleSvc.GetByID(ctx, rule.RuleThumbExists)
	if err != nil {
		t.Fatalf("getting updated rule: %v", err)
	}
	if updated.Enabled {
		t.Error("expected Enabled=false after import")
	}
	if updated.AutomationMode != rule.AutomationModeAuto {
		t.Errorf("AutomationMode: got %q, want auto", updated.AutomationMode)
	}
}

// --- importScraperPreferences ---

// TestImportScraperPreferences_SkipsNilService confirms no-op when the
// scraper service is not attached.
func TestImportScraperPreferences_SkipsNilService(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)
	svc := NewService(db, provSettings, connSvc, platSvc, whSvc) // no WithScraperService

	result := &ImportResult{}
	if err := svc.importScraperPreferences(ctx, db, []ScraperConfigExport{
		{Scope: "global"},
	}, result); err != nil {
		t.Fatalf("importScraperPreferences with nil service: %v", err)
	}
	if result.ScraperConfigs != 0 {
		t.Errorf("ScraperConfigs count: got %d, want 0 (service nil)", result.ScraperConfigs)
	}
}

// TestImportScraperPreferences_SkipsEmptyScope verifies that a config with an
// empty scope is skipped without error.
func TestImportScraperPreferences_SkipsEmptyScope(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)
	scraperSvc := scraper.NewService(db, slog.Default())
	svc := NewService(db, provSettings, connSvc, platSvc, whSvc).WithScraperService(scraperSvc)

	result := &ImportResult{}
	if err := svc.importScraperPreferences(ctx, db, []ScraperConfigExport{{Scope: ""}}, result); err != nil {
		t.Fatalf("importScraperPreferences with empty scope: %v", err)
	}
	if result.ScraperConfigs != 0 {
		t.Errorf("ScraperConfigs count: got %d, want 0 (empty scope skipped)", result.ScraperConfigs)
	}
}

// TestImportScraperPreferences_UpsertAndCount verifies that a non-empty scope
// is written and result.ScraperConfigs is incremented.
func TestImportScraperPreferences_UpsertAndCount(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)
	scraperSvc := scraper.NewService(db, slog.Default())
	if err := scraperSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding scraper defaults: %v", err)
	}
	svc := NewService(db, provSettings, connSvc, platSvc, whSvc).WithScraperService(scraperSvc)

	configs := []ScraperConfigExport{
		{Scope: "global", Config: scraper.ScraperConfig{}},
		{Scope: "custom-scope", Config: scraper.ScraperConfig{}},
	}
	result := &ImportResult{}
	if err := svc.importScraperPreferences(ctx, db, configs, result); err != nil {
		t.Fatalf("importScraperPreferences: %v", err)
	}
	if result.ScraperConfigs != 2 {
		t.Errorf("ScraperConfigs count: got %d, want 2", result.ScraperConfigs)
	}
}

// --- importProviderPriorities ---

// TestImportProviderPriorities_CountsAndPersists verifies that priorities are
// written and result.Priorities is incremented for each entry.
func TestImportProviderPriorities_CountsAndPersists(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)
	svc := NewService(db, provSettings, connSvc, platSvc, whSvc)

	priorities := []PriorityExport{
		{
			Field:     "biography",
			Providers: []provider.ProviderName{provider.NameMusicBrainz, provider.NameWikipedia},
		},
	}
	result := &ImportResult{}
	if err := svc.importProviderPriorities(ctx, db, priorities, result); err != nil {
		t.Fatalf("importProviderPriorities: %v", err)
	}
	if result.Priorities != 1 {
		t.Errorf("Priorities count: got %d, want 1", result.Priorities)
	}
	fps, err := provSettings.GetPriorities(ctx)
	if err != nil {
		t.Fatalf("GetPriorities: %v", err)
	}
	foundBio := false
	for _, fp := range fps {
		if fp.Field != "biography" {
			continue
		}
		foundBio = true
		// GetPriorities appends defaults not in the stored list, so check that
		// the imported providers appear first in the correct order.
		if len(fp.Providers) < 2 ||
			fp.Providers[0] != provider.NameMusicBrainz ||
			fp.Providers[1] != provider.NameWikipedia {
			t.Errorf("Providers prefix: got %v, want [%s %s] first", fp.Providers, provider.NameMusicBrainz, provider.NameWikipedia)
		}
		break
	}
	if !foundBio {
		t.Fatal("biography priorities not found after import")
	}
}

// TestImportProviderPriorities_DisabledPersisted verifies that a non-empty
// Disabled list is written alongside the priority order.
func TestImportProviderPriorities_DisabledPersisted(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)
	svc := NewService(db, provSettings, connSvc, platSvc, whSvc)

	priorities := []PriorityExport{
		{
			Field:     "biography",
			Providers: []provider.ProviderName{provider.NameMusicBrainz, provider.NameWikipedia},
			Disabled:  []provider.ProviderName{provider.NameWikipedia},
		},
	}
	result := &ImportResult{}
	if err := svc.importProviderPriorities(ctx, db, priorities, result); err != nil {
		t.Fatalf("importProviderPriorities: %v", err)
	}

	fps, err := provSettings.GetPriorities(ctx)
	if err != nil {
		t.Fatalf("GetPriorities: %v", err)
	}
	var got []provider.ProviderName
	for _, fp := range fps {
		if fp.Field == "biography" {
			got = fp.Disabled
			break
		}
	}
	if len(got) != 1 || got[0] != provider.NameWikipedia {
		t.Errorf("Disabled: got %v, want [%s]", got, provider.NameWikipedia)
	}
}

// TestImportProviderPriorities_EmptyDisabledClears verifies that importing an
// entry with no Disabled providers clears any previously-stored disabled list,
// ensuring idempotent restore behavior.
func TestImportProviderPriorities_EmptyDisabledClears(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)
	svc := NewService(db, provSettings, connSvc, platSvc, whSvc)

	// Seed a disabled entry directly so we can verify it gets cleared.
	if err := provSettings.SetDisabledProviders(ctx, "biography", []provider.ProviderName{provider.NameWikipedia}); err != nil {
		t.Fatalf("SetDisabledProviders seed: %v", err)
	}

	priorities := []PriorityExport{
		{
			Field:     "biography",
			Providers: []provider.ProviderName{provider.NameMusicBrainz, provider.NameWikipedia},
			// Disabled is intentionally nil -- import should clear the seeded value.
		},
	}
	result := &ImportResult{}
	if err := svc.importProviderPriorities(ctx, db, priorities, result); err != nil {
		t.Fatalf("importProviderPriorities: %v", err)
	}

	fps, err := provSettings.GetPriorities(ctx)
	if err != nil {
		t.Fatalf("GetPriorities: %v", err)
	}
	foundBio := false
	for _, fp := range fps {
		if fp.Field != "biography" {
			continue
		}
		foundBio = true
		if len(fp.Disabled) > 0 {
			t.Errorf("expected disabled list cleared, got %v", fp.Disabled)
		}
		break
	}
	if !foundBio {
		t.Fatal("biography priorities not found after import")
	}
}

// --- error-path coverage (closed-DB) ---

// TestImportSettings_DBError verifies that a DB failure propagates as an error.
func TestImportSettings_DBError(t *testing.T) {
	db := setupTestDB(t)
	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)
	svc := NewService(db, provSettings, connSvc, platSvc, whSvc)
	_ = db.Close()
	err := svc.importSettings(t.Context(), db, map[string]string{"k": "v"}, &ImportResult{})
	if err == nil {
		t.Fatal("expected error with closed DB, got nil")
	}
}

// TestImportProviderKeys_DBError verifies that a provider key write failure propagates.
func TestImportProviderKeys_DBError(t *testing.T) {
	db := setupTestDB(t)
	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)
	svc := NewService(db, provSettings, connSvc, platSvc, whSvc)
	_ = db.Close()
	err := svc.importProviderKeys(t.Context(), db, map[string]string{string(provider.NameFanartTV): "key"}, &ImportResult{})
	if err == nil {
		t.Fatal("expected error with closed DB, got nil")
	}
}

// TestImportConnections_DBError verifies that a connection lookup failure propagates.
func TestImportConnections_DBError(t *testing.T) {
	db := setupTestDB(t)
	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)
	svc := NewService(db, provSettings, connSvc, platSvc, whSvc)
	_ = db.Close()
	conns := []ConnectionExport{{Name: "x", Type: "emby", URL: "http://localhost:8096"}}
	err := svc.importConnections(t.Context(), db, conns, &ImportResult{}, true, true)
	if err == nil {
		t.Fatal("expected error with closed DB, got nil")
	}
}

// TestImportPlatformProfiles_DBError verifies that a profile lookup failure propagates.
func TestImportPlatformProfiles_DBError(t *testing.T) {
	db := setupTestDB(t)
	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)
	svc := NewService(db, provSettings, connSvc, platSvc, whSvc)
	_ = db.Close()
	profiles := []platform.Profile{{Name: "Test"}}
	err := svc.importPlatformProfiles(t.Context(), db, profiles, &ImportResult{})
	if err == nil {
		t.Fatal("expected error with closed DB, got nil")
	}
}

// TestImportWebhooks_DBError verifies that a webhook lookup failure propagates.
func TestImportWebhooks_DBError(t *testing.T) {
	db := setupTestDB(t)
	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)
	svc := NewService(db, provSettings, connSvc, platSvc, whSvc)
	_ = db.Close()
	hooks := []webhook.Webhook{{Name: "h", URL: "https://example.com/h", Type: "generic"}}
	err := svc.importWebhooks(t.Context(), db, hooks, &ImportResult{})
	if err == nil {
		t.Fatal("expected error with closed DB, got nil")
	}
}

// TestImportProviderPriorities_DBError verifies that a priority write failure propagates.
func TestImportProviderPriorities_DBError(t *testing.T) {
	db := setupTestDB(t)
	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)
	svc := NewService(db, provSettings, connSvc, platSvc, whSvc)
	_ = db.Close()
	priorities := []PriorityExport{{Field: "biography", Providers: []provider.ProviderName{provider.NameMusicBrainz}}}
	err := svc.importProviderPriorities(t.Context(), db, priorities, &ImportResult{})
	if err == nil {
		t.Fatal("expected error with closed DB, got nil")
	}
}

// TestImportRules_DBError verifies that a non-ErrNotFound DB error propagates
// instead of being silently swallowed as an unknown-rule skip.
func TestImportRules_DBError(t *testing.T) {
	db := setupTestDB(t)
	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)
	ruleSvc := rule.NewService(db)
	if err := ruleSvc.SeedDefaults(t.Context()); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}
	svc := NewService(db, provSettings, connSvc, platSvc, whSvc).WithRuleService(ruleSvc)
	_ = db.Close()
	// Use a known rule ID so the GetByID path is exercised (unknown IDs are
	// silently skipped; a closed-DB error is not ErrNotFound and must surface).
	rules := []RuleExport{{ID: rule.RuleThumbExists, Enabled: false, AutomationMode: rule.AutomationModeAuto}}
	err := svc.importRules(t.Context(), db, rules, &ImportResult{})
	if err == nil {
		t.Fatal("expected error with closed DB, got nil")
	}
}

// TestImportScraperPreferences_DBError verifies that a SaveConfig failure propagates.
func TestImportScraperPreferences_DBError(t *testing.T) {
	db := setupTestDB(t)
	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)
	scraperSvc := scraper.NewService(db, slog.Default())
	svc := NewService(db, provSettings, connSvc, platSvc, whSvc).WithScraperService(scraperSvc)
	_ = db.Close()
	configs := []ScraperConfigExport{{Scope: "global", Config: scraper.ScraperConfig{}}}
	err := svc.importScraperPreferences(t.Context(), db, configs, &ImportResult{})
	if err == nil {
		t.Fatal("expected error with closed DB, got nil")
	}
}

// --- API token orphan / FK-skip unit tests (section-level) ---

// TestImportAPITokens_OrphanTokenSkipped tests that a token whose owner is
// absent on the target is skipped (not inserted) and APITokensSkipped is
// incremented when admin-fallback is off.
func TestImportAPITokens_OrphanTokenSkipped(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)
	svc := NewService(db, provSettings, connSvc, platSvc, whSvc)

	// No users seeded -- "ghost" does not exist.
	tokens := []APITokenExport{
		{Name: "Orphan", TokenHash: "orphan-hash", Scopes: "read,write", Username: "ghost", Status: "active"},
	}
	result := &ImportResult{}
	if err := svc.importAPITokens(ctx, db, tokens, result, ImportOptions{}); err != nil {
		t.Fatalf("importAPITokens: %v", err)
	}
	if result.APITokensSkipped != 1 {
		t.Errorf("APITokensSkipped: got %d, want 1", result.APITokensSkipped)
	}
	if result.APITokens != 0 {
		t.Errorf("APITokens: got %d, want 0", result.APITokens)
	}

	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM api_tokens WHERE token_hash = 'orphan-hash'`).Scan(&count); err != nil {
		t.Fatalf("counting orphan token: %v", err)
	}
	if count != 0 {
		t.Errorf("orphan token was inserted despite missing owner: count=%d", count)
	}
}

// TestImportAPITokens_EmptyHashSkipped verifies that a token with an empty
// hash is skipped and APITokensSkipped is incremented.
func TestImportAPITokens_EmptyHashSkipped(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)
	svc := NewService(db, provSettings, connSvc, platSvc, whSvc)

	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := db.ExecContext(ctx,
		`INSERT INTO users (id, username, role, auth_provider, is_active, created_at, updated_at) VALUES ('u1', 'alice', 'administrator', 'local', 1, ?, ?)`,
		now, now); err != nil {
		t.Fatalf("seeding user: %v", err)
	}

	tokens := []APITokenExport{
		{Name: "Bad Token", TokenHash: "", Scopes: "read,write", Username: "alice", Status: "active"},
	}
	result := &ImportResult{}
	if err := svc.importAPITokens(ctx, db, tokens, result, ImportOptions{}); err != nil {
		t.Fatalf("importAPITokens: %v", err)
	}
	if result.APITokensSkipped != 1 {
		t.Errorf("APITokensSkipped: got %d, want 1", result.APITokensSkipped)
	}
}

// TestImportAPITokens_AdminFallbackAssignsToken verifies that when admin-
// fallback is enabled and the owner is absent, the token is assigned to the
// importing admin and OwnershipReassigned is incremented.
func TestImportAPITokens_AdminFallbackAssignsToken(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)
	svc := NewService(db, provSettings, connSvc, platSvc, whSvc)

	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := db.ExecContext(ctx,
		`INSERT INTO users (id, username, role, auth_provider, is_active, created_at, updated_at) VALUES ('u-admin', 'admin', 'administrator', 'local', 1, ?, ?)`,
		now, now); err != nil {
		t.Fatalf("seeding admin: %v", err)
	}

	// "ghost" does not exist; admin-fallback should assign the token to "admin".
	tokens := []APITokenExport{
		{Name: "Ghost Token", TokenHash: "ghost-hash", Scopes: "read,write", Username: "ghost", Status: "active", CreatedAt: now},
	}
	result := &ImportResult{}
	opts := ImportOptions{AdminFallbackTokens: true, ImportingAdminUserID: "u-admin"}
	if err := svc.importAPITokens(ctx, db, tokens, result, opts); err != nil {
		t.Fatalf("importAPITokens: %v", err)
	}
	if result.APITokens != 1 || result.APITokensSkipped != 0 {
		t.Errorf("tokens: imported=%d skipped=%d, want 1/0", result.APITokens, result.APITokensSkipped)
	}
	if result.OwnershipReassigned != 1 {
		t.Errorf("OwnershipReassigned: got %d, want 1", result.OwnershipReassigned)
	}

	var userID string
	if err := db.QueryRowContext(ctx, `SELECT user_id FROM api_tokens WHERE token_hash = 'ghost-hash'`).Scan(&userID); err != nil {
		t.Fatalf("reading token user_id: %v", err)
	}
	if userID != "u-admin" {
		t.Errorf("user_id: got %q, want u-admin", userID)
	}
}

// TestImportAPITokens_ConflictResolution_UpdatesExistingRow verifies that
// re-importing a token whose hash already exists in the DB updates the
// metadata on the existing row rather than inserting a duplicate.
func TestImportAPITokens_ConflictResolution_UpdatesExistingRow(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)
	svc := NewService(db, provSettings, connSvc, platSvc, whSvc)

	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := db.ExecContext(ctx,
		`INSERT INTO users (id, username, role, auth_provider, is_active, created_at, updated_at) VALUES ('u-owner', 'owner', 'operator', 'local', 1, ?, ?)`,
		now, now); err != nil {
		t.Fatalf("seeding user: %v", err)
	}
	// Pre-seed the token row.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO api_tokens (id, name, token_hash, scopes, user_id, created_at, status)
		VALUES ('tok-pre', 'Old Name', 'stable-hash', 'read', 'u-owner', ?, 'active')
	`, now); err != nil {
		t.Fatalf("seeding token: %v", err)
	}

	tokens := []APITokenExport{
		{Name: "New Name", TokenHash: "stable-hash", Scopes: "read,write", Username: "owner", Status: "active", CreatedAt: now},
	}
	result := &ImportResult{}
	if err := svc.importAPITokens(ctx, db, tokens, result, ImportOptions{}); err != nil {
		t.Fatalf("importAPITokens: %v", err)
	}
	if result.APITokens != 1 {
		t.Errorf("APITokens: got %d, want 1", result.APITokens)
	}

	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM api_tokens WHERE token_hash = 'stable-hash'`).Scan(&count); err != nil {
		t.Fatalf("counting rows: %v", err)
	}
	if count != 1 {
		t.Errorf("row count after upsert: got %d, want 1 (no duplicate)", count)
	}

	var name string
	if err := db.QueryRowContext(ctx, `SELECT name FROM api_tokens WHERE token_hash = 'stable-hash'`).Scan(&name); err != nil {
		t.Fatalf("reading name: %v", err)
	}
	if name != "New Name" {
		t.Errorf("name after upsert: got %q, want New Name", name)
	}
}
