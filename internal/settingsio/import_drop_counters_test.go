package settingsio

// Drives the real importer through a sealed envelope to prove every row the
// importer skips is counted (#3012), rather than calling the skip helpers.

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/sydlexius/stillwater/internal/rule"
	"github.com/sydlexius/stillwater/internal/scraper"
)

const dropTestPassphrase = "drop-counter-pass"

// importSealed seals p into a real envelope and runs the full Import on a
// service wired with seeded rule and scraper services.
func importSealed(t *testing.T, p Payload) (*ImportResult, *rule.Service) {
	t.Helper()
	db := setupTestDB(t)
	ctx := context.Background()
	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)
	ruleSvc := rule.NewService(db)
	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}
	scraperSvc := scraper.NewService(db, slog.Default())
	if err := scraperSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding scraper: %v", err)
	}
	svc := NewService(db, provSettings, connSvc, platSvc, whSvc).
		WithRuleService(ruleSvc).WithScraperService(scraperSvc)

	plain, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshaling payload: %v", err)
	}
	data, salt, err := encryptWithPassphrase(plain, dropTestPassphrase)
	if err != nil {
		t.Fatalf("sealing payload: %v", err)
	}
	res, err := svc.Import(ctx, &Envelope{Version: CurrentEnvelopeVersion, Data: data, Salt: salt}, dropTestPassphrase)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	return res, ruleSvc
}

func TestImportCountsSkippedRules(t *testing.T) {
	// 1 empty id + 2 unknown ids + 3 invalid modes against a real seeded id:
	// distinct multiplicities so each branch is independently detectable.
	p := Payload{Rules: []RuleExport{
		{ID: "", AutomationMode: "auto"},
		{ID: "future_unknown_rule", AutomationMode: "auto"},
		{ID: "logo_trimmable", AutomationMode: "auto"},
		{ID: rule.RuleThumbExists, AutomationMode: "bogus"},
		{ID: rule.RuleThumbExists, AutomationMode: "bogus"},
		{ID: rule.RuleThumbExists, AutomationMode: "disabled"},
	}}
	res, ruleSvc := importSealed(t, p)
	// Precondition: the id used for the invalid-mode rows really exists.
	if _, err := ruleSvc.GetByID(context.Background(), rule.RuleThumbExists); err != nil {
		t.Fatalf("precondition: seeded rule missing: %v", err)
	}
	if res.RulesSkipped != 6 {
		t.Errorf("RulesSkipped = %d, want 6", res.RulesSkipped)
	}
	if res.Rules != 0 {
		t.Errorf("Rules = %d, want 0", res.Rules)
	}
}

func TestImportCountsSkippedScraperConfigs(t *testing.T) {
	res, _ := importSealed(t, Payload{ScraperConfigs: []ScraperConfigExport{
		{Scope: ""}, {Scope: ""}, {Scope: "global"},
	}})
	if res.ScraperConfigs != 1 {
		t.Fatalf("precondition: ScraperConfigs = %d, want 1", res.ScraperConfigs)
	}
	if res.ScraperConfigsSkipped != 2 {
		t.Errorf("ScraperConfigsSkipped = %d, want 2", res.ScraperConfigsSkipped)
	}
}

func TestImportCountsSkippedUserPreferenceRows(t *testing.T) {
	res, _ := importSealed(t, Payload{UserPreferences: []UserPrefsExport{
		{Username: "ghost", Preferences: map[string]string{"a": "1", "b": "2", "c": "3"}},
		{Username: "ghost2", Preferences: map[string]string{}},
	}})
	if res.UserPreferences != 0 {
		t.Fatalf("precondition: UserPreferences = %d, want 0", res.UserPreferences)
	}
	if res.UserPreferencesSkipped != 3 {
		t.Errorf("UserPreferencesSkipped = %d, want 3 (rows, not entries)", res.UserPreferencesSkipped)
	}
}

func TestImportCountsSkippedUsers(t *testing.T) {
	res, _ := importSealed(t, Payload{Users: []UserExport{
		{Username: "", Role: "operator", IsActive: true, CreatedAt: "2026-01-01T00:00:00Z"},
		{Username: "bob", Role: "operator", IsActive: true, CreatedAt: "2026-01-01T00:00:00Z"},
	}})
	if res.UsersImported != 1 {
		t.Fatalf("precondition: UsersImported = %d, want 1", res.UsersImported)
	}
	if res.UsersSkipped != 1 {
		t.Errorf("UsersSkipped = %d, want 1", res.UsersSkipped)
	}
}

// Legitimate no-ops must stay uncounted: a valid rule, a provider-owned
// settings key, and a pre-1.4 username collision (existing target user wins).
func TestImportLegitimateNoOpsStayUncounted(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO users (id, username, role, display_name) VALUES ('u-target', 'alice', 'operator', 'Target Alice')`); err != nil {
		t.Fatalf("seeding target alice: %v", err)
	}
	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)
	ruleSvc := rule.NewService(db)
	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}
	svc := NewService(db, provSettings, connSvc, platSvc, whSvc).WithRuleService(ruleSvc)
	plain, err := json.Marshal(Payload{
		Rules:    []RuleExport{{ID: rule.RuleThumbExists, Enabled: true, AutomationMode: "auto"}},
		Settings: map[string]string{"provider.fanart.api_key": "x"},
		Users:    []UserExport{{ID: "", Username: "alice", Role: "operator", IsActive: true, CreatedAt: "2026-01-01T00:00:00Z"}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	data, salt, err := encryptWithPassphrase(plain, dropTestPassphrase)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	res, err := svc.Import(ctx, &Envelope{Version: CurrentEnvelopeVersion, Data: data, Salt: salt}, dropTestPassphrase)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.Rules != 1 || res.UsersImported != 0 {
		t.Fatalf("precondition: Rules=%d UsersImported=%d, want 1 and 0", res.Rules, res.UsersImported)
	}
	if res.RulesSkipped != 0 || res.ScraperConfigsSkipped != 0 || res.UserPreferencesSkipped != 0 || res.UsersSkipped != 0 {
		t.Errorf("legitimate no-ops were counted: %+v", res)
	}
}
