package settingsio

import (
	"context"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/settingsvalidate"
)

// TestImport_RejectsInvalidSettingValue is the #3008 regression test.
//
// Import was the second write path into the settings table and the unguarded
// one: importSettings upserted every key raw, so a hand-edited envelope walked
// straight past the validators that PUT /api/v1/settings applies. The value
// that motivated it is "0.5" for a 0-100 threshold: the boot reader parses with
// fmt.Sscanf("%d"), which stops at the first non-digit and REPORTS SUCCESS, so
// the stored garbage came back as a real, in-range 0 -- and a name-similarity
// threshold of 0 matches every name, making the check pass everything.
//
// The bad row must not land, the rest of the restore must still succeed, and
// the rejection must be reported rather than silent.
func TestImport_RejectsInvalidSettingValue(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)

	// A valid neighbor and an invalid value, in the same envelope. The
	// neighbor is what proves a rejection SKIPS rather than aborts.
	now := time.Now().UTC().Format(time.RFC3339)
	for k, v := range map[string]string{
		"mbid_revalidate.interval_hours": "6",
		// Seeded DIRECTLY into the settings table, bypassing every validator.
		// That is the real path: a row reaches this table from an older
		// version, a script, or a hand-edit, never having passed through PUT.
		"mbid_revalidate.name_similarity_threshold": "0.5",
	} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
			k, v, now); err != nil {
			t.Fatalf("seeding %s: %v", k, err)
		}
	}

	svc := NewService(db, provSettings, connSvc, platSvc, whSvc)
	envelope, err := svc.Export(ctx, "test-passphrase")
	if err != nil {
		t.Fatalf("Export: %v", err)
	}

	// PRECONDITION: the bad value must really be on the source, or the whole
	// test degenerates into "importing a clean envelope works".
	var seeded string
	if err := db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`,
		"mbid_revalidate.name_similarity_threshold").Scan(&seeded); err != nil {
		t.Fatalf("precondition: reading the seeded bad value: %v", err)
	}
	if seeded != "0.5" {
		t.Fatalf("precondition: source holds %q, want the invalid %q", seeded, "0.5")
	}

	db2 := setupTestDB(t)
	provSettings2, connSvc2, platSvc2, whSvc2 := newTestServices(t, db2)
	svc2 := NewService(db2, provSettings2, connSvc2, platSvc2, whSvc2)
	res, err := svc2.Import(ctx, envelope, "test-passphrase")
	if err != nil {
		t.Fatalf("Import must SUCCEED with one bad row (a backup has to stay restorable): %v", err)
	}

	// The bad row must not exist on the target at all. Asserting absence
	// rather than "not 0.5": storing it under any coercion is the defect.
	var rows int
	if err := db2.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM settings WHERE key = ?`,
		"mbid_revalidate.name_similarity_threshold").Scan(&rows); err != nil {
		t.Fatalf("counting the rejected key: %v", err)
	}
	if rows != 0 {
		t.Errorf("the invalid value was persisted (%d row(s)); import must reject it "+
			"the same way PUT /api/v1/settings does (#3008)", rows)
	}

	// The valid neighbor must still have landed -- skip, not abort.
	var got string
	if err := db2.QueryRowContext(ctx,
		`SELECT value FROM settings WHERE key = ?`,
		"mbid_revalidate.interval_hours").Scan(&got); err != nil {
		t.Fatalf("the valid neighbor did not survive the import: %v", err)
	}
	if got != "6" {
		t.Errorf("valid neighbor = %q, want %q", got, "6")
	}

	// The rejection must be REPORTED. A restore that silently drops settings
	// looks exactly like a successful restore.
	if res.SettingsRejected != 1 {
		t.Errorf("SettingsRejected = %d, want 1", res.SettingsRejected)
	}
	if len(res.SettingsRejectedKeys) != 1 ||
		res.SettingsRejectedKeys[0] != "mbid_revalidate.name_similarity_threshold" {
		t.Errorf("SettingsRejectedKeys = %v, want exactly the threshold key",
			res.SettingsRejectedKeys)
	}
}

// TestImport_CanonicalisesValidSettingValue pins the half of the fix that is
// easy to miss: import must store the validator's CANONICAL form, not the raw
// envelope value.
//
// validateBool rewrites "TRUE" to "true", and the boot reader tests
// v == "true" || v == "1". An un-canonicalised "TRUE" therefore reads back as
// DISABLED -- an operator restores a backup and silently loses a setting they
// had switched on, with no error anywhere.
func TestImport_CanonicalisesValidSettingValue(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)

	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := db.ExecContext(ctx,
		`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)`,
		// A legal but NON-CANONICAL spelling, as an older version or a
		// hand-edit could leave it. Seeded directly, bypassing validateBool.
		"mbid_revalidate.enabled", "TRUE", now); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	svc := NewService(db, provSettings, connSvc, platSvc, whSvc)
	envelope, err := svc.Export(ctx, "test-passphrase")
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	db2 := setupTestDB(t)
	provSettings2, connSvc2, platSvc2, whSvc2 := newTestServices(t, db2)
	svc2 := NewService(db2, provSettings2, connSvc2, platSvc2, whSvc2)
	res, err := svc2.Import(ctx, envelope, "test-passphrase")
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.SettingsRejected != 0 {
		t.Errorf("SettingsRejected = %d, want 0 -- \"TRUE\" is VALID, merely non-canonical",
			res.SettingsRejected)
	}

	var got string
	if err := db2.QueryRowContext(ctx,
		`SELECT value FROM settings WHERE key = ?`, "mbid_revalidate.enabled").Scan(&got); err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if got != "true" {
		t.Errorf("persisted %q, want the canonical %q -- getDBBoolSetting tests "+
			"v == \"true\" || v == \"1\", so %q reads back as DISABLED", got, "true", got)
	}
}

// TestImport_MigratesRenamedSettingKey covers the second half of #3008.
//
// #3004 renamed mbid_revalidate.name_similarity to ...name_similarity_threshold
// and shipped no migration, correctly: the old key is in no release tag, so no
// upgrade path can hold that row. Import breaks that reasoning, because an
// envelope is not bound to a release -- a pre-rename dev or nightly instance
// can hand a current build the old key at any time, and a boot-time migration
// cannot help because import runs long after boot.
func TestImport_MigratesRenamedSettingKey(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)

	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := db.ExecContext(ctx,
		`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)`,
		"mbid_revalidate.name_similarity", "42", now); err != nil {
		t.Fatalf("seeding the pre-rename key: %v", err)
	}

	svc := NewService(db, provSettings, connSvc, platSvc, whSvc)
	envelope, err := svc.Export(ctx, "test-passphrase")
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	var seeded string
	if err := db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`,
		"mbid_revalidate.name_similarity").Scan(&seeded); err != nil {
		t.Fatalf("precondition: the pre-rename key is not on the source: %v", err)
	}
	if seeded != "42" {
		t.Fatalf("precondition: source holds %q, want %q", seeded, "42")
	}

	db2 := setupTestDB(t)
	provSettings2, connSvc2, platSvc2, whSvc2 := newTestServices(t, db2)
	svc2 := NewService(db2, provSettings2, connSvc2, platSvc2, whSvc2)
	if _, err := svc2.Import(ctx, envelope, "test-passphrase"); err != nil {
		t.Fatalf("Import: %v", err)
	}

	// The operator's configured value must survive under the CURRENT name --
	// the one cmd/stillwater actually reads.
	var got string
	if err := db2.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`,
		"mbid_revalidate.name_similarity_threshold").Scan(&got); err != nil {
		t.Fatalf("the renamed key did not land under its current name: %v", err)
	}
	if got != "42" {
		t.Errorf("migrated value = %q, want %q", got, "42")
	}

	// And the dead name must NOT persist, or a later export carries it forward.
	var oldRows int
	if err := db2.QueryRowContext(ctx, `SELECT COUNT(*) FROM settings WHERE key = ?`,
		"mbid_revalidate.name_similarity").Scan(&oldRows); err != nil {
		t.Fatalf("counting the old key: %v", err)
	}
	if oldRows != 0 {
		t.Errorf("the pre-rename key persisted (%d row(s)); it would ride into the next export", oldRows)
	}
}

// TestImport_RenamedKeyDefersToCurrentName covers the collision an envelope can
// legitimately contain: an operator set the value, upgraded, then set it again,
// so BOTH names are present. The current name must win -- it is the one the
// running build reads and the one a later export carries. Preferring the old
// name would silently restore the STALER value.
func TestImport_RenamedKeyDefersToCurrentName(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)

	now := time.Now().UTC().Format(time.RFC3339)
	for k, v := range map[string]string{
		"mbid_revalidate.name_similarity":           "42", // stale, pre-rename
		"mbid_revalidate.name_similarity_threshold": "77", // what the operator set after upgrading
	} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)`, k, v, now); err != nil {
			t.Fatalf("seeding %s: %v", k, err)
		}
	}

	svc := NewService(db, provSettings, connSvc, platSvc, whSvc)
	envelope, err := svc.Export(ctx, "test-passphrase")
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	// PRECONDITION: both names must really be on the source with DISTINCT
	// values, or this test degenerates into the single-key case above.
	var oldV, newV string
	if err := db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`,
		"mbid_revalidate.name_similarity").Scan(&oldV); err != nil {
		t.Fatalf("precondition: old key absent on source: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`,
		"mbid_revalidate.name_similarity_threshold").Scan(&newV); err != nil {
		t.Fatalf("precondition: current key absent on source: %v", err)
	}
	if oldV != "42" || newV != "77" {
		t.Fatalf("precondition: source holds old=%q new=%q, want 42/77 (they must differ)", oldV, newV)
	}

	db2 := setupTestDB(t)
	provSettings2, connSvc2, platSvc2, whSvc2 := newTestServices(t, db2)
	svc2 := NewService(db2, provSettings2, connSvc2, platSvc2, whSvc2)
	res, err := svc2.Import(ctx, envelope, "test-passphrase")
	if err != nil {
		t.Fatalf("Import: %v", err)
	}

	var got string
	if err := db2.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`,
		"mbid_revalidate.name_similarity_threshold").Scan(&got); err != nil {
		t.Fatalf("reading the current key: %v", err)
	}
	if got != "77" {
		t.Errorf("current key = %q, want %q -- the alias overwrote a newer value with a staler one", got, "77")
	}

	// The value assertion above is NECESSARY BUT NOT SUFFICIENT, and the gap is
	// not theoretical: without the collision guard BOTH names upsert the SAME
	// row (the alias rewrites the key before the write), so Go's randomized map
	// iteration decides which value survives. Measured on this exact test,
	// deleting the guard failed only 3 runs in 6 -- a coin-flip test certifies
	// a broken guard half the times it runs, and in CI the flake gets blamed on
	// infrastructure.
	//
	// So assert the ORDER-INDEPENDENT fact instead: the old key was DROPPED,
	// not migrated. That counter moves on exactly one branch and is unaffected
	// by which key the loop reaches first.
	if res.SettingsRenamedDropped != 1 {
		t.Errorf("SettingsRenamedDropped = %d, want 1 -- with both names present the "+
			"pre-rename key must be dropped, never applied over the current one",
			res.SettingsRenamedDropped)
	}
	if res.SettingsRenamed != 0 {
		t.Errorf("SettingsRenamed = %d, want 0 -- nothing should have been migrated "+
			"when the current name is already present", res.SettingsRenamed)
	}
}

// TestImport_AppliesPolicyRefusedSetting pins the exemption for WRITE-TIME
// POLICY validators, which #3008 surfaced rather than created.
//
// validateLocalAuthEnabled refuses every falsy value so local auth cannot be
// switched off through PUT /api/v1/settings -- it is the break-glass path when
// every other provider is misconfigured. That is a rule about WHO IS WRITING,
// not about whether the value is usable, and a restore is not an operator
// disabling anything: it re-establishes a state the operator already had and
// chose to back up.
//
// Without the exemption, validating on import silently dropped this row, which
// either changes a security posture during a restore or makes a legitimate
// backup unrestorable (#2534). A pre-existing round-trip test caught it; this
// test states the property directly so it is not covered only by accident.
func TestImport_AppliesPolicyRefusedSetting(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)

	// PRECONDITION: the value must be one the validator actually refuses, or
	// this test proves nothing about the exemption.
	if _, _, err := settingsvalidate.Validate("auth.providers.local.enabled", "false"); err == nil {
		t.Fatal("precondition: validateLocalAuthEnabled no longer refuses \"false\"; " +
			"if the policy changed, this test's premise is gone")
	}
	if _, isPolicy := settingsvalidate.IsPolicy("auth.providers.local.enabled"); !isPolicy {
		t.Fatal("precondition: auth.providers.local.enabled is not marked as a policy key")
	}

	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := db.ExecContext(ctx,
		`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		"auth.providers.local.enabled", "false", now); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	svc := NewService(db, provSettings, connSvc, platSvc, whSvc)
	envelope, err := svc.Export(ctx, "test-passphrase")
	if err != nil {
		t.Fatalf("Export: %v", err)
	}

	db2 := setupTestDB(t)
	provSettings2, connSvc2, platSvc2, whSvc2 := newTestServices(t, db2)
	svc2 := NewService(db2, provSettings2, connSvc2, platSvc2, whSvc2)
	res, err := svc2.Import(ctx, envelope, "test-passphrase")
	if err != nil {
		t.Fatalf("Import: %v", err)
	}

	var got string
	if err := db2.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`,
		"auth.providers.local.enabled").Scan(&got); err != nil {
		t.Fatalf("the policy-refused setting did not survive the restore: %v", err)
	}
	if got != "false" {
		t.Errorf("restored value = %q, want %q -- a restore re-establishes stored state "+
			"and must not silently change it", got, "false")
	}
	// It must not be counted as a rejection either: nothing was dropped.
	if res.SettingsRejected != 0 {
		t.Errorf("SettingsRejected = %d, want 0 -- the row was applied, not rejected",
			res.SettingsRejected)
	}
}
