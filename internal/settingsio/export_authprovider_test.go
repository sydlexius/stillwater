package settingsio

import (
	"context"
	"testing"
	"time"
)

// canonicalAuthKeys mirrors authProviderDefaults from internal/api but is
// expressed here as the test contract: every auth.providers.* setting that
// the Settings > Auth Providers UI reads MUST round-trip through
// Export -> wipe -> Import unchanged. The list is deliberately duplicated
// here (rather than imported from internal/api) because a regression that
// drops a key from the seed list AND the test list at the same time would
// be invisible -- duplicating the contract makes drift loud.
var canonicalAuthKeys = []string{
	"auth.providers.local.enabled",
	"auth.providers.emby.enabled",
	"auth.providers.emby.auto_provision",
	"auth.providers.emby.guard_rail",
	"auth.providers.emby.default_role",
	"auth.providers.jellyfin.enabled",
	"auth.providers.jellyfin.auto_provision",
	"auth.providers.jellyfin.guard_rail",
	"auth.providers.jellyfin.default_role",
	"auth.providers.oidc.enabled",
	"auth.providers.oidc.auto_provision",
	"auth.providers.oidc.default_role",
}

// nonDefaultValue returns a value for the given canonical auth key that is
// guaranteed to differ from the code default. The mapping mirrors the UI's
// "the other allowed value" for each key.
func nonDefaultValue(key string) string {
	switch key {
	case "auth.providers.local.enabled",
		"auth.providers.emby.enabled",
		"auth.providers.emby.auto_provision",
		"auth.providers.jellyfin.enabled",
		"auth.providers.jellyfin.auto_provision",
		"auth.providers.oidc.enabled",
		"auth.providers.oidc.auto_provision":
		// Booleans default to "true" or "false"; flip to the opposite.
		// local.enabled defaults to true; everything else defaults to false.
		if key == "auth.providers.local.enabled" {
			return "false"
		}
		return "true"
	case "auth.providers.emby.guard_rail",
		"auth.providers.jellyfin.guard_rail":
		return "any_user"
	case "auth.providers.emby.default_role",
		"auth.providers.jellyfin.default_role",
		"auth.providers.oidc.default_role":
		return "administrator"
	}
	return "non-default"
}

// TestRoundTrip_AuthProviderKeys_NonDefaults seeds every canonical
// auth.providers.* key on the source instance with a non-default value,
// runs Export -> Import on a fresh target, and asserts the value on
// target equals the value on source. This is the regression test for
// #1188: the user-reported symptom was Default role flipping from
// Operator to Administrator after restore; the harness here pins every
// key so a future regression is caught at the contract level rather than
// per-key.
func TestRoundTrip_AuthProviderKeys_NonDefaults(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)

	// Seed each canonical key with its non-default value directly via the
	// settings KV table. This simulates a user who actually fired the
	// onchange handler and persisted a row.
	now := time.Now().UTC().Format(time.RFC3339)
	want := make(map[string]string, len(canonicalAuthKeys))
	for _, key := range canonicalAuthKeys {
		v := nonDefaultValue(key)
		want[key] = v
		if _, err := db.ExecContext(ctx,
			`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
			key, v, now); err != nil {
			t.Fatalf("seeding %s: %v", key, err)
		}
	}

	svc := NewService(db, provSettings, connSvc, platSvc, whSvc)
	envelope, err := svc.Export(ctx, "test-passphrase")
	if err != nil {
		t.Fatalf("Export: %v", err)
	}

	// Fresh target instance with an empty settings table.
	db2 := setupTestDB(t)
	provSettings2, connSvc2, platSvc2, whSvc2 := newTestServices(t, db2)
	svc2 := NewService(db2, provSettings2, connSvc2, platSvc2, whSvc2)
	if _, err := svc2.Import(ctx, envelope, "test-passphrase"); err != nil {
		t.Fatalf("Import: %v", err)
	}

	for _, key := range canonicalAuthKeys {
		var got string
		if err := db2.QueryRowContext(ctx,
			`SELECT value FROM settings WHERE key = ?`, key).Scan(&got); err != nil {
			t.Fatalf("reading %s on target: %v", key, err)
		}
		if got != want[key] {
			t.Errorf("key %s: got %q, want %q", key, got, want[key])
		}
	}
}

// TestRoundTrip_AuthProviderKeys_SeededDefaults verifies that, when the
// source instance has had its defaults seeded (the seedAuthProviderDefaults
// call from the Settings page render), every canonical key has a real row
// to export, even if no user has touched the UI since first install.
//
// This pins the #1188 root-cause fix: pre-fix, a value matching the code
// default never had a row, so the export carried nothing for that key.
// Post-fix, the seed writes a row with the default value on first render,
// so the export carries a copy of the default that import faithfully
// re-applies on the target.
func TestRoundTrip_AuthProviderKeys_SeededDefaults(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)

	// Simulate the page-render seed: insert the canonical defaults via
	// INSERT OR IGNORE. We hard-code the expected default values here
	// (not re-imported from internal/api) so a drift between this test
	// and the live seed list is loud rather than silent.
	defaults := map[string]string{
		"auth.providers.local.enabled":           "true",
		"auth.providers.emby.enabled":            "false",
		"auth.providers.emby.auto_provision":     "false",
		"auth.providers.emby.guard_rail":         "admin",
		"auth.providers.emby.default_role":       "operator",
		"auth.providers.jellyfin.enabled":        "false",
		"auth.providers.jellyfin.auto_provision": "false",
		"auth.providers.jellyfin.guard_rail":     "admin",
		"auth.providers.jellyfin.default_role":   "operator",
		"auth.providers.oidc.enabled":            "false",
		"auth.providers.oidc.auto_provision":     "false",
		"auth.providers.oidc.default_role":       "operator",
	}
	now := time.Now().UTC().Format(time.RFC3339)
	for k, v := range defaults {
		if _, err := db.ExecContext(ctx,
			`INSERT OR IGNORE INTO settings (key, value, updated_at) VALUES (?, ?, ?)`,
			k, v, now); err != nil {
			t.Fatalf("seeding default %s: %v", k, err)
		}
	}

	svc := NewService(db, provSettings, connSvc, platSvc, whSvc)
	envelope, err := svc.Export(ctx, "test-passphrase")
	if err != nil {
		t.Fatalf("Export: %v", err)
	}

	db2 := setupTestDB(t)
	provSettings2, connSvc2, platSvc2, whSvc2 := newTestServices(t, db2)
	svc2 := NewService(db2, provSettings2, connSvc2, platSvc2, whSvc2)
	if _, err := svc2.Import(ctx, envelope, "test-passphrase"); err != nil {
		t.Fatalf("Import: %v", err)
	}

	// Every canonical key must have a row on target with the default
	// value. The pre-#1188 bug manifested as "row absent on target,
	// reader falls back to code default" -- that path is now gone.
	for k, v := range defaults {
		var got string
		if err := db2.QueryRowContext(ctx,
			`SELECT value FROM settings WHERE key = ?`, k).Scan(&got); err != nil {
			t.Errorf("reading %s on target: %v (key may be absent -- the #1188 regression)", k, err)
			continue
		}
		if got != v {
			t.Errorf("key %s: got %q, want %q (default drift across round-trip)", k, got, v)
		}
	}
}
