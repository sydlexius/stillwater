package settingsio

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// TestExport_IncludesUsers verifies that local user rows surface in the
// envelope payload with their source UUID id preserved, password hash
// intact, and no session/remember-me material attached (those live in the
// sessions table and are not touched by export).
func TestExport_IncludesUsers(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)

	// Seed two users with distinctive ids and password hashes. The export
	// must surface both rows under the Users block; export ordering is
	// (created_at, username) so the seed order maps directly to the output.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO users (id, username, display_name, password_hash, role,
		                   auth_provider, is_active, is_protected, created_at)
		VALUES (?, ?, ?, ?, ?, ?, 1, 1, '2026-01-01T00:00:00Z')
	`, "u-admin", "admin", "Bootstrap Admin", "bcrypt$admin-hash",
		"administrator", "local"); err != nil {
		t.Fatalf("seeding admin: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO users (id, username, display_name, password_hash, role,
		                   auth_provider, is_active, is_protected, created_at)
		VALUES (?, ?, ?, ?, ?, ?, 1, 0, '2026-02-01T00:00:00Z')
	`, "u-bob", "bob", "Bob", "bcrypt$bob-hash",
		"operator", "local"); err != nil {
		t.Fatalf("seeding bob: %v", err)
	}

	svc := NewService(db, provSettings, connSvc, platSvc, whSvc)
	envelope, err := svc.Export(ctx, "p")
	if err != nil {
		t.Fatalf("Export: %v", err)
	}

	plaintext, err := decryptWithPassphrase(envelope.Data, envelope.Salt, "p")
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	var payload Payload
	if err := json.Unmarshal(plaintext, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(payload.Users) != 2 {
		t.Fatalf("Users count: got %d, want 2", len(payload.Users))
	}
	byID := map[string]UserExport{}
	for _, u := range payload.Users {
		byID[u.ID] = u
	}
	admin, ok := byID["u-admin"]
	if !ok {
		t.Fatalf("admin user not present in export by id")
	}
	if admin.Username != "admin" || admin.Role != "administrator" {
		t.Errorf("admin row: got username=%q role=%q", admin.Username, admin.Role)
	}
	if admin.PasswordHash != "bcrypt$admin-hash" {
		t.Errorf("admin password_hash drift: got %q", admin.PasswordHash)
	}
	if !admin.IsProtected {
		t.Errorf("admin is_protected=true was not preserved in export")
	}
	bob, ok := byID["u-bob"]
	if !ok {
		t.Fatalf("bob user not present in export by id")
	}
	if bob.PasswordHash != "bcrypt$bob-hash" {
		t.Errorf("bob password_hash drift: got %q", bob.PasswordHash)
	}

	// Session and remember-me tokens are NOT exported. The Payload struct
	// has no field for them, so the JSON cannot contain a "sessions" key.
	// Pin that contract structurally so a future field addition has to
	// update this assertion too.
	if got := payload.Users[0]; got.Username == "" {
		t.Errorf("first user has empty username; export shape broken")
	}
	raw := string(plaintext)
	for _, banned := range []string{"sessions", "remember_me", "session_token"} {
		if containsField(raw, banned) {
			t.Errorf("envelope payload must not carry %q-shaped fields", banned)
		}
	}
}

// containsField is a coarse keyword check used by TestExport_IncludesUsers.
// We do not want to JSON-walk the structure here; a substring match of
// quoted-key forms is enough to flag a regression that adds session data
// to the payload.
func containsField(payload, key string) bool {
	probe := `"` + key + `"`
	for i := 0; i+len(probe) <= len(payload); i++ {
		if payload[i:i+len(probe)] == probe {
			return true
		}
	}
	return false
}

// TestImport_UsersIdCollisionFails pins the #1114 hard-fail behavior: if
// the target carries the same username under a DIFFERENT id, the import
// must abort instead of silently remapping that username onto the
// envelope's user.
func TestImport_UsersIdCollisionFails(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)

	if _, err := db.ExecContext(ctx, `
		INSERT INTO users (id, username, role) VALUES (?, ?, ?)
	`, "u-source", "alice", "administrator"); err != nil {
		t.Fatalf("seeding source user: %v", err)
	}

	svc := NewService(db, provSettings, connSvc, platSvc, whSvc)
	envelope, err := svc.Export(ctx, "p")
	if err != nil {
		t.Fatalf("Export: %v", err)
	}

	// Fresh target with alice under a DIFFERENT id.
	db2 := setupTestDB(t)
	provSettings2, connSvc2, platSvc2, whSvc2 := newTestServices(t, db2)
	if _, err := db2.ExecContext(ctx, `
		INSERT INTO users (id, username, role) VALUES (?, ?, ?)
	`, "u-target", "alice", "operator"); err != nil {
		t.Fatalf("seeding target user: %v", err)
	}

	svc2 := NewService(db2, provSettings2, connSvc2, platSvc2, whSvc2)
	_, err = svc2.Import(ctx, envelope, "p")
	if err == nil {
		t.Fatalf("Import: expected ErrUserIDCollision, got nil")
	}
	if !errors.Is(err, ErrUserIDCollision) {
		t.Fatalf("Import: expected ErrUserIDCollision wrapped, got %v", err)
	}

	// Target's pre-existing alice row must be unchanged (role=operator,
	// id=u-target) -- import halted before any user write touched it.
	var role, id string
	if err := db2.QueryRowContext(ctx,
		`SELECT id, role FROM users WHERE username = 'alice'`).Scan(&id, &role); err != nil {
		t.Fatalf("scanning target alice: %v", err)
	}
	if id != "u-target" || role != "operator" {
		t.Errorf("target alice modified despite collision: id=%q role=%q", id, role)
	}
}

// TestImport_UsersIDMatchUpdates verifies the happy path: when the
// envelope's user id is already present on the target, the import updates
// the row in place (display_name, role, password_hash) without changing
// is_protected. This is the cross-instance restore that #1114 enables.
func TestImport_UsersIDMatchUpdates(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)

	if _, err := db.ExecContext(ctx, `
		INSERT INTO users (id, username, display_name, password_hash, role,
		                   auth_provider, is_active, is_protected, created_at)
		VALUES (?, 'admin', 'Renamed Admin', 'new-hash', 'administrator',
		        'local', 1, 1, '2026-01-01T00:00:00Z')
	`, "u-admin"); err != nil {
		t.Fatalf("seeding source admin: %v", err)
	}

	svc := NewService(db, provSettings, connSvc, platSvc, whSvc)
	envelope, err := svc.Export(ctx, "p")
	if err != nil {
		t.Fatalf("Export: %v", err)
	}

	db2 := setupTestDB(t)
	provSettings2, connSvc2, platSvc2, whSvc2 := newTestServices(t, db2)
	// Target has SAME id but stale fields. is_protected stays 1 throughout
	// the test; the schema's protected-role trigger forbids flipping role
	// on a protected row, so the source row also lands as administrator
	// (matching the target's role) and only display_name + password_hash
	// observably change. The point of this test is the id-match update
	// path itself, not the role mutation.
	if _, err := db2.ExecContext(ctx, `
		INSERT INTO users (id, username, display_name, password_hash, role,
		                   auth_provider, is_active, is_protected, created_at)
		VALUES (?, 'admin', 'Old Display', 'stale-hash', 'administrator',
		        'local', 1, 1, '2026-01-01T00:00:00Z')
	`, "u-admin"); err != nil {
		t.Fatalf("seeding target admin: %v", err)
	}

	svc2 := NewService(db2, provSettings2, connSvc2, platSvc2, whSvc2)
	res, err := svc2.Import(ctx, envelope, "p")
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.UsersImported < 1 {
		t.Errorf("UsersImported: got %d, want >=1", res.UsersImported)
	}

	var displayName, passwordHash, role string
	var isProtected int
	if err := db2.QueryRowContext(ctx,
		`SELECT display_name, password_hash, role, is_protected FROM users WHERE id = ?`,
		"u-admin").Scan(&displayName, &passwordHash, &role, &isProtected); err != nil {
		t.Fatalf("scanning target admin: %v", err)
	}
	if displayName != "Renamed Admin" {
		t.Errorf("display_name: got %q, want Renamed Admin", displayName)
	}
	if passwordHash != "new-hash" {
		t.Errorf("password_hash: got %q, want new-hash", passwordHash)
	}
	if role != "administrator" {
		t.Errorf("role: got %q, want administrator", role)
	}
	// is_protected is per-install policy -- the target already had it set
	// and the envelope must not have flipped it off.
	if isProtected != 1 {
		t.Errorf("is_protected: got %d, want 1 (per-install invariant)", isProtected)
	}
}
