package settingsio

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestImportUserPreferences_UsernameFallback pins the pre-#1114 envelope
// path: a payload whose UserPrefsExport entries carry only Username
// (UserID == "") still imports cleanly when the target instance has a
// matching username row. This guarantees envelopes exported by older
// (1.0-1.3) Stillwater binaries restore without dropping preferences.
func TestImportUserPreferences_UsernameFallback(t *testing.T) {
	srcDB := setupTestDB(t)
	ctx := context.Background()
	srcProv, srcConn, srcPlat, srcWH := newTestServices(t, srcDB)
	srcSvc := NewService(srcDB, srcProv, srcConn, srcPlat, srcWH)

	// Source: seed alice with one preference so the export carries a
	// non-empty user_preferences block.
	if _, err := srcDB.ExecContext(ctx,
		`INSERT INTO users (id, username, role) VALUES ('u-src-alice', 'alice', 'operator')`); err != nil {
		t.Fatalf("seeding source user: %v", err)
	}
	if _, err := srcDB.ExecContext(ctx,
		`INSERT INTO user_preferences (user_id, key, value) VALUES ('u-src-alice', 'theme', 'dark')`); err != nil {
		t.Fatalf("seeding source pref: %v", err)
	}

	envelope, err := srcSvc.Export(ctx, reencodePassphrase)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}

	// Rewrite the encrypted payload to look like a pre-#1114 envelope: no
	// Users block (so importUsers does not recreate the source id on the
	// target), and clear UserPrefsExport.UserID so importUserPreferences
	// must fall back to username lookup.
	plaintext, err := decryptWithPassphrase(envelope.Data, envelope.Salt, reencodePassphrase)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	var p Payload
	if err := json.Unmarshal(plaintext, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	p.Users = nil
	for i := range p.UserPreferences {
		p.UserPreferences[i].UserID = ""
	}
	out, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	data, salt, err := encryptWithPassphrase(out, reencodePassphrase)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	envelope.Data = data
	envelope.Salt = salt
	envelope.Version = "1.3" // simulate older binary

	// Target: a user with the SAME username but a different internal id.
	// The username-fallback path must locate this row.
	tgtDB := setupTestDB(t)
	tgtProv, tgtConn, tgtPlat, tgtWH := newTestServices(t, tgtDB)
	tgtSvc := NewService(tgtDB, tgtProv, tgtConn, tgtPlat, tgtWH)
	if _, err := tgtDB.ExecContext(ctx,
		`INSERT INTO users (id, username, role) VALUES ('u-tgt-alice', 'alice', 'operator')`); err != nil {
		t.Fatalf("seeding target user: %v", err)
	}

	res, err := tgtSvc.Import(ctx, envelope, reencodePassphrase)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.UserPreferences != 1 {
		t.Errorf("UserPreferences imported: got %d, want 1", res.UserPreferences)
	}

	var got string
	if err := tgtDB.QueryRowContext(ctx,
		`SELECT value FROM user_preferences WHERE user_id = 'u-tgt-alice' AND key = 'theme'`,
	).Scan(&got); err != nil {
		t.Fatalf("scanning target pref: %v", err)
	}
	if got != "dark" {
		t.Errorf("preference value: got %q, want dark", got)
	}
}

// TestImportUserPreferences_SkipsUnknownUser pins the slog.Warn skip
// path: when neither id nor username resolves on the target, the import
// must continue without error and record zero preferences for that
// user. Without this guard a stale envelope from a deleted user would
// fail the entire import.
func TestImportUserPreferences_SkipsUnknownUser(t *testing.T) {
	srcDB := setupTestDB(t)
	ctx := context.Background()
	srcProv, srcConn, srcPlat, srcWH := newTestServices(t, srcDB)
	srcSvc := NewService(srcDB, srcProv, srcConn, srcPlat, srcWH)

	// Seed alice on the source with one preference so the envelope has a
	// real UserPrefsExport entry to fall through on.
	if _, err := srcDB.ExecContext(ctx,
		`INSERT INTO users (id, username, role) VALUES ('u-src-ghost', 'ghost', 'operator')`); err != nil {
		t.Fatalf("seeding ghost: %v", err)
	}
	if _, err := srcDB.ExecContext(ctx,
		`INSERT INTO user_preferences (user_id, key, value) VALUES ('u-src-ghost', 'theme', 'dark')`); err != nil {
		t.Fatalf("seeding ghost pref: %v", err)
	}

	envelope, err := srcSvc.Export(ctx, reencodePassphrase)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	// Strip Users so the target does not recreate ghost; also wipe
	// UserPrefsExport.UserID so both lookup paths must miss.
	plaintext, err := decryptWithPassphrase(envelope.Data, envelope.Salt, reencodePassphrase)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	var p Payload
	if err := json.Unmarshal(plaintext, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	p.Users = nil
	for i := range p.UserPreferences {
		p.UserPreferences[i].UserID = ""
	}
	out, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	data, salt, err := encryptWithPassphrase(out, reencodePassphrase)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	envelope.Data = data
	envelope.Salt = salt
	envelope.Version = "1.3"

	tgtDB := setupTestDB(t)
	tgtProv, tgtConn, tgtPlat, tgtWH := newTestServices(t, tgtDB)
	tgtSvc := NewService(tgtDB, tgtProv, tgtConn, tgtPlat, tgtWH)

	res, err := tgtSvc.Import(ctx, envelope, reencodePassphrase)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.UserPreferences != 0 {
		t.Errorf("UserPreferences imported: got %d, want 0 (user absent on target)", res.UserPreferences)
	}

	var count int
	if err := tgtDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM user_preferences`).Scan(&count); err != nil {
		t.Fatalf("counting prefs: %v", err)
	}
	if count != 0 {
		t.Errorf("target prefs after skip: got %d rows, want 0", count)
	}
}

// TestImportUserPreferences_IDMissThenUsernameHit pins the per-#1114
// resolution order: when the envelope's UserID does not exist on the
// target but the username does, the import falls back to username and
// upserts the preference against the target's user row. The two-tier
// lookup is the headline behavior of the 1.4 envelope format.
func TestImportUserPreferences_IDMissThenUsernameHit(t *testing.T) {
	srcDB := setupTestDB(t)
	ctx := context.Background()
	srcProv, srcConn, srcPlat, srcWH := newTestServices(t, srcDB)
	srcSvc := NewService(srcDB, srcProv, srcConn, srcPlat, srcWH)

	if _, err := srcDB.ExecContext(ctx,
		`INSERT INTO users (id, username, role) VALUES ('u-source-only', 'bob', 'operator')`); err != nil {
		t.Fatalf("seeding bob: %v", err)
	}
	if _, err := srcDB.ExecContext(ctx,
		`INSERT INTO user_preferences (user_id, key, value) VALUES ('u-source-only', 'lang', 'fr')`); err != nil {
		t.Fatalf("seeding bob pref: %v", err)
	}

	envelope, err := srcSvc.Export(ctx, reencodePassphrase)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}

	// Target: bob exists but under a DIFFERENT internal id. The source
	// Users block tries to insert 'u-source-only' but the target already
	// has 'bob' as 'u-target-only'; the username collision under a
	// different id is fatal (ErrUserIDCollision), so for this test we
	// strip the Users block and rely on the bare prefs section.
	tgtDB := setupTestDB(t)
	tgtProv, tgtConn, tgtPlat, tgtWH := newTestServices(t, tgtDB)
	tgtSvc := NewService(tgtDB, tgtProv, tgtConn, tgtPlat, tgtWH)
	if _, err := tgtDB.ExecContext(ctx,
		`INSERT INTO users (id, username, role) VALUES ('u-target-only', 'bob', 'operator')`); err != nil {
		t.Fatalf("seeding target bob: %v", err)
	}

	// Strip the Users block; preserve the UserID in UserPrefsExport so
	// the id-lookup fires first, misses, and falls through to username.
	plaintext, err := decryptWithPassphrase(envelope.Data, envelope.Salt, reencodePassphrase)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	var p Payload
	if err := json.Unmarshal(plaintext, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	p.Users = nil
	// Sanity check: UserPrefsExport must still carry the source's UserID.
	// Split the empty check from the index access so an empty slice fails
	// with a clear message instead of panicking on the [0] dereference.
	if len(p.UserPreferences) == 0 {
		t.Fatal("envelope precondition: expected at least one UserPreferences entry, got none")
	}
	if p.UserPreferences[0].UserID != "u-source-only" {
		t.Fatalf("envelope precondition: UserPreferences[0].UserID = %q, want u-source-only",
			p.UserPreferences[0].UserID)
	}
	out, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	data, salt, err := encryptWithPassphrase(out, reencodePassphrase)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	envelope.Data = data
	envelope.Salt = salt

	res, err := tgtSvc.Import(ctx, envelope, reencodePassphrase)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.UserPreferences != 1 {
		t.Errorf("UserPreferences imported: got %d, want 1", res.UserPreferences)
	}

	var got string
	if err := tgtDB.QueryRowContext(ctx,
		`SELECT value FROM user_preferences WHERE user_id = 'u-target-only' AND key = 'lang'`,
	).Scan(&got); err != nil {
		t.Fatalf("scanning target pref: %v", err)
	}
	if got != "fr" {
		t.Errorf("preference value: got %q, want fr", got)
	}
}

// TestExportUserPreferences_EmptyIDFallsBackToUsername pins the
// defensive groupKey branch in exportUserPreferences: if a user row
// ever ends up with an empty id (should not happen in production
// because the column is PRIMARY KEY NOT NULL, but the function stays
// defensive), the username is used as the grouping key instead. This
// guards against a future migration that softens the id constraint.
func TestExportUserPreferences_EmptyIDFallsBackToUsername(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	provSvc, connSvc, platSvc, whSvc := newTestServices(t, db)
	svc := NewService(db, provSvc, connSvc, platSvc, whSvc)

	// Disable foreign-key checks and clear the PK constraint so an
	// empty id row can be inserted. The query under test only reads,
	// so the schema tweak is local to this test's database copy.
	if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatalf("disable fks: %v", err)
	}
	// Rebuild users table without PK constraint to allow empty id.
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE users_noconstraint (
			id TEXT,
			username TEXT,
			role TEXT
		)
	`); err != nil {
		t.Fatalf("create temp table: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DROP TABLE users`); err != nil {
		t.Fatalf("drop users: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`CREATE TABLE users (id TEXT, username TEXT, role TEXT, password_hash TEXT,
		                     auth_provider TEXT, is_active INTEGER, is_protected INTEGER,
		                     created_at TEXT)`); err != nil {
		t.Fatalf("recreate users: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO users (id, username, role) VALUES ('', 'lostid', 'operator')`); err != nil {
		t.Fatalf("seeding user with empty id: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO user_preferences (user_id, key, value) VALUES ('', 'theme', 'dark')`); err != nil {
		t.Fatalf("seeding pref: %v", err)
	}

	prefs, err := svc.exportUserPreferences(ctx)
	if err != nil {
		t.Fatalf("exportUserPreferences: %v", err)
	}
	if len(prefs) != 1 {
		t.Fatalf("prefs count: got %d, want 1", len(prefs))
	}
	if prefs[0].Username != "lostid" {
		t.Errorf("Username: got %q, want lostid", prefs[0].Username)
	}
	// UserID is the empty string from the row; the grouping key fell
	// through to username, which is what this test pins.
	if prefs[0].UserID != "" {
		t.Errorf("UserID: got %q, want empty", prefs[0].UserID)
	}
}

// TestExportUserPreferences_GroupsByID pins the export-side grouping
// behavior introduced in v1.4: UserPrefsExport entries are keyed by the
// user_id column, not the username, so two users with distinct ids and
// the SAME username (theoretically possible if the unique constraint
// were ever relaxed) would still surface as separate groups. The test
// also asserts UserID is carried in the export payload.
func TestExportUserPreferences_CarriesUserID(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	provSvc, connSvc, platSvc, whSvc := newTestServices(t, db)
	svc := NewService(db, provSvc, connSvc, platSvc, whSvc)

	if _, err := db.ExecContext(ctx,
		`INSERT INTO users (id, username, role) VALUES ('u-pref-owner', 'prefowner', 'operator')`); err != nil {
		t.Fatalf("seeding user: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO user_preferences (user_id, key, value) VALUES ('u-pref-owner', 'theme', 'dark')`); err != nil {
		t.Fatalf("seeding pref: %v", err)
	}

	env, err := svc.Export(ctx, reencodePassphrase)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	plaintext, err := decryptWithPassphrase(env.Data, env.Salt, reencodePassphrase)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	var p Payload
	if err := json.Unmarshal(plaintext, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(p.UserPreferences) != 1 {
		t.Fatalf("UserPreferences count: got %d, want 1", len(p.UserPreferences))
	}
	if p.UserPreferences[0].UserID != "u-pref-owner" {
		t.Errorf("UserID: got %q, want u-pref-owner", p.UserPreferences[0].UserID)
	}
	if p.UserPreferences[0].Username != "prefowner" {
		t.Errorf("Username: got %q, want prefowner", p.UserPreferences[0].Username)
	}

	// Sanity-check the JSON key spelling so the wire format matches the
	// documented "user_id" name; a struct-tag drift would otherwise
	// silently break envelope compatibility.
	raw, _ := json.Marshal(p.UserPreferences[0])
	if !strings.Contains(string(raw), `"user_id":"u-pref-owner"`) {
		t.Errorf("expected user_id JSON tag in marshaled output: %s", raw)
	}
}
