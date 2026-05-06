package settingsio

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// reencodePassphrase is the fixed passphrase used by every test in this
// file. Threading it through mustReencodeWithoutUsers as a parameter
// would be more flexible but unparam flags the duplication; a package-
// level constant keeps the call sites short and the linter happy.
const reencodePassphrase = "pp"

// mustReencodeWithoutUsers strips the Users block from an envelope and
// re-encrypts the payload so the modified envelope still decrypts. This
// simulates either a legacy v1.2 envelope or an export from a binary that
// has no Users awareness.
func mustReencodeWithoutUsers(t *testing.T, env *Envelope) *Envelope {
	t.Helper()
	plaintext, err := decryptWithPassphrase(env.Data, env.Salt, reencodePassphrase)
	if err != nil {
		t.Fatalf("mustReencodeWithoutUsers: decrypt: %v", err)
	}
	var p Payload
	if err := json.Unmarshal(plaintext, &p); err != nil {
		t.Fatalf("mustReencodeWithoutUsers: unmarshal: %v", err)
	}
	p.Users = nil
	out, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("mustReencodeWithoutUsers: marshal: %v", err)
	}
	data, salt, err := encryptWithPassphrase(out, reencodePassphrase)
	if err != nil {
		t.Fatalf("mustReencodeWithoutUsers: encrypt: %v", err)
	}
	env.Data = data
	env.Salt = salt
	return env
}

// seedTokenSource builds a "source" instance that has user "alice" with an
// API token whose hash is `hash`. The function returns the source service
// + the underlying DB so callers can extend the seed.
func seedTokenSource(t *testing.T, hash string) (*Service, context.Context) {
	t.Helper()
	db := setupTestDB(t)
	ctx := context.Background()
	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)

	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO users (id, username, password_hash, role, auth_provider, is_active, created_at, updated_at)
		VALUES ('u-alice', 'alice', 'bcrypt-hash-stub', 'administrator', 'local', 1, ?, ?)
	`, now, now); err != nil {
		t.Fatalf("seeding source user: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO api_tokens (id, name, token_hash, scopes, user_id, created_at, status)
		VALUES ('t-alice-1', 'Alice Token', ?, 'read,write', 'u-alice', ?, 'active')
	`, hash, now); err != nil {
		t.Fatalf("seeding source token: %v", err)
	}

	return NewService(db, provSettings, connSvc, platSvc, whSvc), ctx
}

// TestImport_CrossInstance_RecreatesAbsentUser_ForToken pins the #1283
// happy path: instance B had never heard of "alice", but the envelope
// from instance A carries her user record; on import, B recreates alice
// and her token round-trips with the user_id remap into the new local id.
func TestImport_CrossInstance_RecreatesAbsentUser_ForToken(t *testing.T) {
	hash := "bcrypt-token-hash-cross-instance"
	srcSvc, ctx := seedTokenSource(t, hash)

	envelope, err := srcSvc.Export(ctx, "pp")
	if err != nil {
		t.Fatalf("Export: %v", err)
	}

	// Target B: NO alice user exists locally pre-import.
	db2 := setupTestDB(t)
	provSettings2, connSvc2, platSvc2, whSvc2 := newTestServices(t, db2)
	svc2 := NewService(db2, provSettings2, connSvc2, platSvc2, whSvc2)

	res, err := svc2.Import(ctx, envelope, "pp")
	if err != nil {
		t.Fatalf("Import: %v", err)
	}

	// The token MUST have been imported (not skipped). Pre-#1283 this
	// would have been APITokensSkipped=1, APITokens=0.
	if res.APITokens != 1 || res.APITokensSkipped != 0 {
		t.Errorf("tokens: got imported=%d skipped=%d, want 1/0", res.APITokens, res.APITokensSkipped)
	}
	// Alice must have been recreated on target.
	if res.UsersImported < 1 {
		t.Errorf("users imported: got %d, want >=1", res.UsersImported)
	}
	if res.OwnershipReassigned != 0 {
		t.Errorf("ownership reassigned: got %d, want 0 (envelope carried user, no fallback needed)", res.OwnershipReassigned)
	}

	// The token must be attributed to the freshly-recreated alice on target.
	var aliceID, tokenUserID string
	if err := db2.QueryRowContext(ctx,
		`SELECT id FROM users WHERE username = 'alice'`).Scan(&aliceID); err != nil {
		t.Fatalf("looking up alice on target: %v", err)
	}
	if err := db2.QueryRowContext(ctx,
		`SELECT user_id FROM api_tokens WHERE token_hash = ?`, hash).Scan(&tokenUserID); err != nil {
		t.Fatalf("looking up token user_id: %v", err)
	}
	if tokenUserID != aliceID {
		t.Errorf("token user_id: got %q, want %q (alice's new id)", tokenUserID, aliceID)
	}
}

// TestImport_CrossInstance_AdminFallback_NoUsersInEnvelope pins the
// admin-fallback path: the envelope is synthesized to look like a v1.2
// payload (no Users block), and the target opts into AdminFallbackTokens.
// Alice's token reassigns to the importing admin and OwnershipReassigned
// is incremented exactly once.
func TestImport_CrossInstance_AdminFallback_NoUsersInEnvelope(t *testing.T) {
	hash := "bcrypt-token-hash-admin-fallback"
	srcSvc, ctx := seedTokenSource(t, hash)

	envelope, err := srcSvc.Export(ctx, "pp")
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	// Strip the envelope's Users block to simulate either a legacy v1.2
	// envelope or an operator who deliberately wants admin-fallback. We
	// re-encrypt the trimmed payload with the same passphrase so the
	// envelope still decrypts on import.
	envelope = mustReencodeWithoutUsers(t, envelope)

	// Target: admin user exists locally; alice does NOT.
	db2 := setupTestDB(t)
	provSettings2, connSvc2, platSvc2, whSvc2 := newTestServices(t, db2)
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := db2.ExecContext(ctx, `
		INSERT INTO users (id, username, password_hash, role, auth_provider, is_active, created_at, updated_at)
		VALUES ('u-target-admin', 'admin-b', 'bcrypt-hash-stub', 'administrator', 'local', 1, ?, ?)
	`, now, now); err != nil {
		t.Fatalf("seeding target admin: %v", err)
	}
	svc2 := NewService(db2, provSettings2, connSvc2, platSvc2, whSvc2)

	res, err := svc2.ImportWithOptions(ctx, envelope, "pp", ImportOptions{
		AdminFallbackTokens:  true,
		ImportingAdminUserID: "u-target-admin",
	})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}

	if res.APITokens != 1 || res.APITokensSkipped != 0 {
		t.Errorf("tokens: got imported=%d skipped=%d, want 1/0", res.APITokens, res.APITokensSkipped)
	}
	if res.OwnershipReassigned != 1 {
		t.Errorf("ownership reassigned: got %d, want 1", res.OwnershipReassigned)
	}

	// The token's user_id must be the importing admin, not alice's old id.
	var tokenUserID string
	if err := db2.QueryRowContext(ctx,
		`SELECT user_id FROM api_tokens WHERE token_hash = ?`, hash).Scan(&tokenUserID); err != nil {
		t.Fatalf("looking up token user_id: %v", err)
	}
	if tokenUserID != "u-target-admin" {
		t.Errorf("reassigned user_id: got %q, want u-target-admin", tokenUserID)
	}
}

// TestImport_CrossInstance_FallbackOff_NoUsers preserves the historical
// skip semantics: with admin-fallback off and no Users block, alice's
// token is silently skipped (APITokensSkipped=1) just as it was pre-#1283.
// This guards against an accidental default-on rollout.
func TestImport_CrossInstance_FallbackOff_NoUsers(t *testing.T) {
	hash := "bcrypt-token-hash-fallback-off"
	srcSvc, ctx := seedTokenSource(t, hash)

	envelope, err := srcSvc.Export(ctx, "pp")
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	envelope = mustReencodeWithoutUsers(t, envelope)

	db2 := setupTestDB(t)
	provSettings2, connSvc2, platSvc2, whSvc2 := newTestServices(t, db2)
	svc2 := NewService(db2, provSettings2, connSvc2, platSvc2, whSvc2)

	// Default Import (no opts) must skip the token.
	res, err := svc2.Import(ctx, envelope, "pp")
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.APITokens != 0 || res.APITokensSkipped != 1 {
		t.Errorf("tokens: got imported=%d skipped=%d, want 0/1", res.APITokens, res.APITokensSkipped)
	}
	if res.OwnershipReassigned != 0 {
		t.Errorf("ownership reassigned: got %d, want 0", res.OwnershipReassigned)
	}
}

// TestImport_AdminFallback_RejectsBogusAdminID guards the FK integrity:
// even when admin-fallback is requested, an ImportingAdminUserID that
// does not exist on the target must NOT result in a token row with a
// dangling user_id. The token is skipped instead.
func TestImport_AdminFallback_RejectsBogusAdminID(t *testing.T) {
	hash := "bcrypt-token-hash-bogus-admin"
	srcSvc, ctx := seedTokenSource(t, hash)
	envelope, err := srcSvc.Export(ctx, "pp")
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	envelope = mustReencodeWithoutUsers(t, envelope)

	db2 := setupTestDB(t)
	provSettings2, connSvc2, platSvc2, whSvc2 := newTestServices(t, db2)
	svc2 := NewService(db2, provSettings2, connSvc2, platSvc2, whSvc2)

	res, err := svc2.ImportWithOptions(ctx, envelope, "pp", ImportOptions{
		AdminFallbackTokens:  true,
		ImportingAdminUserID: "does-not-exist",
	})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.APITokens != 0 || res.APITokensSkipped != 1 {
		t.Errorf("tokens: got imported=%d skipped=%d, want 0/1 (bogus admin id must skip)", res.APITokens, res.APITokensSkipped)
	}
	if res.OwnershipReassigned != 0 {
		t.Errorf("ownership reassigned: got %d, want 0", res.OwnershipReassigned)
	}
}

// TestImport_LegacyEnvelopeWithoutUsers verifies a synthesized v1.2
// envelope (lacking Users) still imports under default options: tokens
// for absent owners skip, like before.
func TestImport_LegacyEnvelopeWithoutUsers(t *testing.T) {
	hash := "legacy-envelope-hash"
	srcSvc, ctx := seedTokenSource(t, hash)
	envelope, err := srcSvc.Export(ctx, "pp")
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	envelope = mustReencodeWithoutUsers(t, envelope)
	envelope.Version = "1.2"

	db2 := setupTestDB(t)
	provSettings2, connSvc2, platSvc2, whSvc2 := newTestServices(t, db2)
	svc2 := NewService(db2, provSettings2, connSvc2, platSvc2, whSvc2)

	res, err := svc2.Import(ctx, envelope, "pp")
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.APITokensSkipped != 1 {
		t.Errorf("legacy envelope: got skipped=%d, want 1", res.APITokensSkipped)
	}
	if res.UsersImported != 0 {
		t.Errorf("legacy envelope: got users_imported=%d, want 0", res.UsersImported)
	}
}
