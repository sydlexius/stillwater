package auth

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/database"
)

// setupTestService creates an in-memory database with migrations applied
// and returns a new auth Service. The database is closed automatically
// when the test completes.
func setupTestService(t *testing.T) *Service {
	t.Helper()

	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	if err := database.Migrate(db); err != nil {
		t.Fatalf("running migrations: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	return NewService(db)
}

// createTestUser creates an admin user via Setup and returns the service.
// This is a convenience for tests that need an existing user.
func createTestUser(t *testing.T, password string) *Service {
	t.Helper()

	svc := setupTestService(t)
	ctx := context.Background()

	created, err := svc.Setup(ctx, "admin", password)
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if !created {
		t.Fatal("expected user to be created")
	}

	return svc
}

// --- PrehashPassword tests ---

func TestPrehashPassword(t *testing.T) {
	tests := []struct {
		name     string
		password string
	}{
		{"simple password", "hello"},
		{"empty password", ""},
		{"long password that exceeds bcrypt 72 byte limit", strings.Repeat("a", 200)},
		{"unicode password", "p@ssw0rd-with-unicode"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := PrehashPassword(tt.password)

			// PrehashPassword returns SHA-256 hex digest which is always 64 bytes.
			if len(result) != 64 {
				t.Errorf("PrehashPassword length = %d, want 64", len(result))
			}

			// Verify it matches a direct SHA-256 computation.
			h := sha256.Sum256([]byte(tt.password))
			want := hex.EncodeToString(h[:])
			if string(result) != want {
				t.Errorf("PrehashPassword = %q, want %q", result, want)
			}
		})
	}
}

func TestPrehashPassword_Deterministic(t *testing.T) {
	a := PrehashPassword("test-password")
	b := PrehashPassword("test-password")

	if string(a) != string(b) {
		t.Error("PrehashPassword should be deterministic for the same input")
	}
}

func TestPrehashPassword_DifferentInputsDifferentOutputs(t *testing.T) {
	a := PrehashPassword("password1")
	b := PrehashPassword("password2")

	if string(a) == string(b) {
		t.Error("different passwords should produce different hashes")
	}
}

// --- NewService tests ---

func TestNewService(t *testing.T) {
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	svc := NewService(db)
	if svc == nil {
		t.Fatal("expected non-nil Service")
	}
}

// --- Setup tests ---

func TestSetup_CreatesUser(t *testing.T) {
	svc := setupTestService(t)
	ctx := context.Background()

	created, err := svc.Setup(ctx, "admin", "password123")
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if !created {
		t.Error("expected Setup to return true for first user")
	}

	// Verify the user exists.
	hasUsers, err := svc.HasUsers(ctx)
	if err != nil {
		t.Fatalf("HasUsers: %v", err)
	}
	if !hasUsers {
		t.Error("expected HasUsers to return true after Setup")
	}
}

func TestSetup_NoopWhenUsersExist(t *testing.T) {
	svc := createTestUser(t, "password123")
	ctx := context.Background()

	// Second Setup call should be a no-op.
	created, err := svc.Setup(ctx, "another", "pass456")
	if err != nil {
		t.Fatalf("second Setup: %v", err)
	}
	if created {
		t.Error("expected Setup to return false when users already exist")
	}
}

// --- HasUsers tests ---

func TestHasUsers_EmptyDB(t *testing.T) {
	svc := setupTestService(t)
	ctx := context.Background()

	has, err := svc.HasUsers(ctx)
	if err != nil {
		t.Fatalf("HasUsers: %v", err)
	}
	if has {
		t.Error("expected HasUsers to return false on empty database")
	}
}

func TestHasUsers_WithUser(t *testing.T) {
	svc := createTestUser(t, "password")
	ctx := context.Background()

	has, err := svc.HasUsers(ctx)
	if err != nil {
		t.Fatalf("HasUsers: %v", err)
	}
	if !has {
		t.Error("expected HasUsers to return true after user creation")
	}
}

// --- Login tests ---

func TestLogin_Success(t *testing.T) {
	svc := createTestUser(t, "secret")
	ctx := context.Background()

	token, err := svc.Login(ctx, "admin", "secret")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if token == "" {
		t.Error("expected non-empty session token")
	}
	// Session token should be a hex string (64 chars for 32 bytes).
	if len(token) != 64 {
		t.Errorf("token length = %d, want 64", len(token))
	}
}

func TestLogin_WrongPassword(t *testing.T) {
	svc := createTestUser(t, "secret")
	ctx := context.Background()

	_, err := svc.Login(ctx, "admin", "wrongpassword")
	if err == nil {
		t.Fatal("expected error for wrong password")
	}
	if !strings.Contains(err.Error(), "invalid credentials") {
		t.Errorf("error = %q, want 'invalid credentials'", err.Error())
	}
}

func TestLogin_UnknownUser(t *testing.T) {
	svc := createTestUser(t, "secret")
	ctx := context.Background()

	_, err := svc.Login(ctx, "nonexistent", "secret")
	if err == nil {
		t.Fatal("expected error for unknown user")
	}
	if !strings.Contains(err.Error(), "invalid credentials") {
		t.Errorf("error = %q, want 'invalid credentials'", err.Error())
	}
}

func TestLogin_UniqueTokens(t *testing.T) {
	svc := createTestUser(t, "secret")
	ctx := context.Background()

	token1, err := svc.Login(ctx, "admin", "secret")
	if err != nil {
		t.Fatalf("first Login: %v", err)
	}

	token2, err := svc.Login(ctx, "admin", "secret")
	if err != nil {
		t.Fatalf("second Login: %v", err)
	}

	if token1 == token2 {
		t.Error("expected distinct tokens for separate login calls")
	}
}

// --- ValidateSession tests ---

func TestValidateSession_Valid(t *testing.T) {
	svc := createTestUser(t, "secret")
	ctx := context.Background()

	token, err := svc.Login(ctx, "admin", "secret")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	userID, err := svc.ValidateSession(ctx, token)
	if err != nil {
		t.Fatalf("ValidateSession: %v", err)
	}
	if userID == "" {
		t.Error("expected non-empty user ID")
	}
}

func TestValidateSession_InvalidToken(t *testing.T) {
	svc := setupTestService(t)
	ctx := context.Background()

	_, err := svc.ValidateSession(ctx, "nonexistent-token")
	if err == nil {
		t.Fatal("expected error for invalid session token")
	}
	if !strings.Contains(err.Error(), "invalid session") {
		t.Errorf("error = %q, want 'invalid session'", err.Error())
	}
}

func TestValidateSession_ExpiredSession(t *testing.T) {
	svc := createTestUser(t, "secret")
	ctx := context.Background()

	token, err := svc.Login(ctx, "admin", "secret")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	// Manually set the session expiry to the past to simulate expiration.
	pastTime := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	_, err = svc.db.ExecContext(ctx, "UPDATE sessions SET expires_at = ? WHERE id = ?", pastTime, token)
	if err != nil {
		t.Fatalf("updating session expiry: %v", err)
	}

	_, err = svc.ValidateSession(ctx, token)
	if err == nil {
		t.Fatal("expected error for expired session")
	}
	if !strings.Contains(err.Error(), "session expired") {
		t.Errorf("error = %q, want 'session expired'", err.Error())
	}

	// Expired session should be deleted (Logout is called internally).
	var count int
	if err := svc.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sessions WHERE id = ?", token).Scan(&count); err != nil {
		t.Fatalf("counting sessions: %v", err)
	}
	if count != 0 {
		t.Error("expired session should have been deleted")
	}
}

// --- Logout tests ---

func TestLogout_DeletesSession(t *testing.T) {
	svc := createTestUser(t, "secret")
	ctx := context.Background()

	token, err := svc.Login(ctx, "admin", "secret")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	if err := svc.Logout(ctx, token); err != nil {
		t.Fatalf("Logout: %v", err)
	}

	// Session should no longer validate.
	_, err = svc.ValidateSession(ctx, token)
	if err == nil {
		t.Error("expected error after logout")
	}
}

func TestLogout_NonexistentToken(t *testing.T) {
	svc := setupTestService(t)
	ctx := context.Background()

	// Logout with a nonexistent token should not error (DELETE is idempotent).
	if err := svc.Logout(ctx, "nonexistent-token"); err != nil {
		t.Fatalf("Logout nonexistent: %v", err)
	}
}

// --- CleanExpiredSessions tests ---

func TestCleanExpiredSessions(t *testing.T) {
	svc := createTestUser(t, "secret")
	ctx := context.Background()

	// Create two sessions.
	token1, err := svc.Login(ctx, "admin", "secret")
	if err != nil {
		t.Fatalf("Login 1: %v", err)
	}
	token2, err := svc.Login(ctx, "admin", "secret")
	if err != nil {
		t.Fatalf("Login 2: %v", err)
	}

	// Expire token1 but keep token2 valid.
	pastTime := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	_, err = svc.db.ExecContext(ctx, "UPDATE sessions SET expires_at = ? WHERE id = ?", pastTime, token1)
	if err != nil {
		t.Fatalf("expiring session: %v", err)
	}

	if err := svc.CleanExpiredSessions(ctx); err != nil {
		t.Fatalf("CleanExpiredSessions: %v", err)
	}

	// token1 should be gone.
	_, err = svc.ValidateSession(ctx, token1)
	if err == nil {
		t.Error("expected expired session to be cleaned")
	}

	// token2 should still be valid.
	userID, err := svc.ValidateSession(ctx, token2)
	if err != nil {
		t.Fatalf("ValidateSession for valid token: %v", err)
	}
	if userID == "" {
		t.Error("expected non-empty user ID for valid session")
	}
}

func TestCleanExpiredSessions_NoSessions(t *testing.T) {
	svc := setupTestService(t)
	ctx := context.Background()

	// Should not error on empty database.
	if err := svc.CleanExpiredSessions(ctx); err != nil {
		t.Fatalf("CleanExpiredSessions on empty db: %v", err)
	}
}

// --- CreateAPIToken tests ---

func TestCreateAPIToken(t *testing.T) {
	svc := createTestUser(t, "secret")
	ctx := context.Background()

	// Get the user ID from a login.
	token, err := svc.Login(ctx, "admin", "secret")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	userID, err := svc.ValidateSession(ctx, token)
	if err != nil {
		t.Fatalf("ValidateSession: %v", err)
	}

	plaintext, id, err := svc.CreateAPIToken(ctx, userID, "test-token", "read,write")
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}

	if id == "" {
		t.Error("expected non-empty token ID")
	}
	if plaintext == "" {
		t.Error("expected non-empty plaintext token")
	}
	if !strings.HasPrefix(plaintext, APITokenPrefix) {
		t.Errorf("plaintext should start with %q, got %q", APITokenPrefix, plaintext)
	}
}

func TestCreateAPIToken_UniqueTokens(t *testing.T) {
	svc := createTestUser(t, "secret")
	ctx := context.Background()

	token, err := svc.Login(ctx, "admin", "secret")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	userID, err := svc.ValidateSession(ctx, token)
	if err != nil {
		t.Fatalf("ValidateSession: %v", err)
	}

	pt1, id1, err := svc.CreateAPIToken(ctx, userID, "token-1", "read")
	if err != nil {
		t.Fatalf("CreateAPIToken 1: %v", err)
	}

	pt2, id2, err := svc.CreateAPIToken(ctx, userID, "token-2", "read")
	if err != nil {
		t.Fatalf("CreateAPIToken 2: %v", err)
	}

	if pt1 == pt2 {
		t.Error("expected unique plaintext tokens")
	}
	if id1 == id2 {
		t.Error("expected unique token IDs")
	}
}

// --- ValidateAPIToken tests ---

func TestValidateAPIToken_Valid(t *testing.T) {
	svc := createTestUser(t, "secret")
	ctx := context.Background()

	token, err := svc.Login(ctx, "admin", "secret")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	userID, err := svc.ValidateSession(ctx, token)
	if err != nil {
		t.Fatalf("ValidateSession: %v", err)
	}

	plaintext, _, err := svc.CreateAPIToken(ctx, userID, "test", "read,write")
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}

	gotUserID, gotScopes, err := svc.ValidateAPIToken(ctx, plaintext)
	if err != nil {
		t.Fatalf("ValidateAPIToken: %v", err)
	}
	if gotUserID != userID {
		t.Errorf("user ID = %q, want %q", gotUserID, userID)
	}
	if gotScopes != "read,write" {
		t.Errorf("scopes = %q, want %q", gotScopes, "read,write")
	}
}

func TestValidateAPIToken_InvalidToken(t *testing.T) {
	svc := setupTestService(t)
	ctx := context.Background()

	_, _, err := svc.ValidateAPIToken(ctx, "sw_nonexistent_token")
	if err == nil {
		t.Fatal("expected error for invalid API token")
	}
	if !strings.Contains(err.Error(), "invalid api token") {
		t.Errorf("error = %q, want 'invalid api token'", err.Error())
	}
}

func TestValidateAPIToken_RevokedToken(t *testing.T) {
	svc := createTestUser(t, "secret")
	ctx := context.Background()

	token, err := svc.Login(ctx, "admin", "secret")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	userID, err := svc.ValidateSession(ctx, token)
	if err != nil {
		t.Fatalf("ValidateSession: %v", err)
	}

	plaintext, id, err := svc.CreateAPIToken(ctx, userID, "test", "read")
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}

	if err := svc.RevokeAPIToken(ctx, id, userID); err != nil {
		t.Fatalf("RevokeAPIToken: %v", err)
	}

	_, _, err = svc.ValidateAPIToken(ctx, plaintext)
	if err == nil {
		t.Fatal("expected error for revoked API token")
	}
	if !strings.Contains(err.Error(), "revoked") {
		t.Errorf("error = %q, want to contain 'revoked'", err.Error())
	}
}

func TestValidateAPIToken_UpdatesLastUsedAt(t *testing.T) {
	svc := createTestUser(t, "secret")
	ctx := context.Background()

	token, err := svc.Login(ctx, "admin", "secret")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	userID, err := svc.ValidateSession(ctx, token)
	if err != nil {
		t.Fatalf("ValidateSession: %v", err)
	}

	plaintext, id, err := svc.CreateAPIToken(ctx, userID, "test", "read")
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}

	// Before validation, last_used_at should be NULL.
	apiToken, err := svc.GetAPIToken(ctx, id, userID)
	if err != nil {
		t.Fatalf("GetAPIToken before validation: %v", err)
	}
	if apiToken.LastUsedAt != nil {
		t.Error("expected last_used_at to be nil before first validation")
	}

	// Validate the token; this should update last_used_at.
	if _, _, err := svc.ValidateAPIToken(ctx, plaintext); err != nil {
		t.Fatalf("ValidateAPIToken: %v", err)
	}

	apiToken, err = svc.GetAPIToken(ctx, id, userID)
	if err != nil {
		t.Fatalf("GetAPIToken after validation: %v", err)
	}
	if apiToken.LastUsedAt == nil {
		t.Error("expected last_used_at to be set after validation")
	}
}

// --- ListAPITokens tests ---

func TestListAPITokens(t *testing.T) {
	svc := createTestUser(t, "secret")
	ctx := context.Background()

	token, err := svc.Login(ctx, "admin", "secret")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	userID, err := svc.ValidateSession(ctx, token)
	if err != nil {
		t.Fatalf("ValidateSession: %v", err)
	}

	// Create a couple of tokens.
	_, _, err = svc.CreateAPIToken(ctx, userID, "token-a", "read")
	if err != nil {
		t.Fatalf("CreateAPIToken a: %v", err)
	}
	_, _, err = svc.CreateAPIToken(ctx, userID, "token-b", "read,write")
	if err != nil {
		t.Fatalf("CreateAPIToken b: %v", err)
	}

	tokens, err := svc.ListAPITokens(ctx, userID)
	if err != nil {
		t.Fatalf("ListAPITokens: %v", err)
	}

	if len(tokens) != 2 {
		t.Fatalf("expected 2 tokens, got %d", len(tokens))
	}

	// Verify both tokens are present (order depends on created_at which may
	// be identical when both are created within the same second).
	names := map[string]bool{tokens[0].Name: true, tokens[1].Name: true}
	if !names["token-a"] {
		t.Error("expected token-a to be in the list")
	}
	if !names["token-b"] {
		t.Error("expected token-b to be in the list")
	}
}

func TestListAPITokens_Empty(t *testing.T) {
	svc := createTestUser(t, "secret")
	ctx := context.Background()

	token, err := svc.Login(ctx, "admin", "secret")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	userID, err := svc.ValidateSession(ctx, token)
	if err != nil {
		t.Fatalf("ValidateSession: %v", err)
	}

	tokens, err := svc.ListAPITokens(ctx, userID)
	if err != nil {
		t.Fatalf("ListAPITokens: %v", err)
	}
	if len(tokens) != 0 {
		t.Errorf("expected 0 tokens, got %d", len(tokens))
	}
}

// --- RevokeAPIToken tests ---

func TestRevokeAPIToken(t *testing.T) {
	svc := createTestUser(t, "secret")
	ctx := context.Background()

	token, err := svc.Login(ctx, "admin", "secret")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	userID, err := svc.ValidateSession(ctx, token)
	if err != nil {
		t.Fatalf("ValidateSession: %v", err)
	}

	_, id, err := svc.CreateAPIToken(ctx, userID, "to-revoke", "read")
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}

	if err := svc.RevokeAPIToken(ctx, id, userID); err != nil {
		t.Fatalf("RevokeAPIToken: %v", err)
	}

	// Verify status is revoked.
	apiToken, err := svc.GetAPIToken(ctx, id, userID)
	if err != nil {
		t.Fatalf("GetAPIToken: %v", err)
	}
	if apiToken.Status != TokenStatusRevoked {
		t.Errorf("status = %q, want %q", apiToken.Status, TokenStatusRevoked)
	}
	if apiToken.RevokedAt == nil {
		t.Error("expected revoked_at to be set")
	}
}

func TestRevokeAPIToken_NotFound(t *testing.T) {
	svc := createTestUser(t, "secret")
	ctx := context.Background()

	token, err := svc.Login(ctx, "admin", "secret")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	userID, err := svc.ValidateSession(ctx, token)
	if err != nil {
		t.Fatalf("ValidateSession: %v", err)
	}

	err = svc.RevokeAPIToken(ctx, "nonexistent-id", userID)
	if err != ErrTokenNotFound {
		t.Errorf("error = %v, want ErrTokenNotFound", err)
	}
}

func TestRevokeAPIToken_AlreadyRevoked(t *testing.T) {
	svc := createTestUser(t, "secret")
	ctx := context.Background()

	token, err := svc.Login(ctx, "admin", "secret")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	userID, err := svc.ValidateSession(ctx, token)
	if err != nil {
		t.Fatalf("ValidateSession: %v", err)
	}

	_, id, err := svc.CreateAPIToken(ctx, userID, "double-revoke", "read")
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}

	// First revocation should succeed.
	if err := svc.RevokeAPIToken(ctx, id, userID); err != nil {
		t.Fatalf("first RevokeAPIToken: %v", err)
	}

	// Second revocation should return ErrTokenNotFound because the WHERE
	// clause requires status = 'active'.
	err = svc.RevokeAPIToken(ctx, id, userID)
	if err != ErrTokenNotFound {
		t.Errorf("second revoke error = %v, want ErrTokenNotFound", err)
	}
}

func TestRevokeAPIToken_WrongUser(t *testing.T) {
	svc := createTestUser(t, "secret")
	ctx := context.Background()

	token, err := svc.Login(ctx, "admin", "secret")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	userID, err := svc.ValidateSession(ctx, token)
	if err != nil {
		t.Fatalf("ValidateSession: %v", err)
	}

	_, id, err := svc.CreateAPIToken(ctx, userID, "other-user", "read")
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}

	// Attempt to revoke with a different user ID.
	err = svc.RevokeAPIToken(ctx, id, "wrong-user-id")
	if err != ErrTokenNotFound {
		t.Errorf("error = %v, want ErrTokenNotFound", err)
	}
}

// --- DeleteAPIToken tests ---

func TestDeleteAPIToken_Success(t *testing.T) {
	svc := createTestUser(t, "secret")
	ctx := context.Background()

	token, err := svc.Login(ctx, "admin", "secret")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	userID, err := svc.ValidateSession(ctx, token)
	if err != nil {
		t.Fatalf("ValidateSession: %v", err)
	}

	_, id, err := svc.CreateAPIToken(ctx, userID, "to-delete", "read")
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}

	// Must revoke before deleting.
	if err := svc.RevokeAPIToken(ctx, id, userID); err != nil {
		t.Fatalf("RevokeAPIToken: %v", err)
	}

	if err := svc.DeleteAPIToken(ctx, id, userID); err != nil {
		t.Fatalf("DeleteAPIToken: %v", err)
	}

	// Token should no longer exist.
	_, err = svc.GetAPIToken(ctx, id, userID)
	if err != ErrTokenNotFound {
		t.Errorf("error after delete = %v, want ErrTokenNotFound", err)
	}
}

func TestDeleteAPIToken_ActiveToken(t *testing.T) {
	svc := createTestUser(t, "secret")
	ctx := context.Background()

	token, err := svc.Login(ctx, "admin", "secret")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	userID, err := svc.ValidateSession(ctx, token)
	if err != nil {
		t.Fatalf("ValidateSession: %v", err)
	}

	_, id, err := svc.CreateAPIToken(ctx, userID, "still-active", "read")
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}

	// Attempt to delete without revoking first.
	err = svc.DeleteAPIToken(ctx, id, userID)
	if err != ErrTokenActive {
		t.Errorf("error = %v, want ErrTokenActive", err)
	}
}

func TestDeleteAPIToken_NotFound(t *testing.T) {
	svc := createTestUser(t, "secret")
	ctx := context.Background()

	token, err := svc.Login(ctx, "admin", "secret")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	userID, err := svc.ValidateSession(ctx, token)
	if err != nil {
		t.Fatalf("ValidateSession: %v", err)
	}

	err = svc.DeleteAPIToken(ctx, "nonexistent-id", userID)
	if err != ErrTokenNotFound {
		t.Errorf("error = %v, want ErrTokenNotFound", err)
	}
}

func TestDeleteAPIToken_AnonymizesAuditLog(t *testing.T) {
	svc := createTestUser(t, "secret")
	ctx := context.Background()

	token, err := svc.Login(ctx, "admin", "secret")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	userID, err := svc.ValidateSession(ctx, token)
	if err != nil {
		t.Fatalf("ValidateSession: %v", err)
	}

	_, id, err := svc.CreateAPIToken(ctx, userID, "audit-test-token", "read")
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}

	// Write an audit entry referencing this token.
	if err := svc.WriteAuditLog(ctx, "token_created", id, "audit-test-token", userID, "created for testing"); err != nil {
		t.Fatalf("WriteAuditLog: %v", err)
	}

	// Revoke and delete.
	if err := svc.RevokeAPIToken(ctx, id, userID); err != nil {
		t.Fatalf("RevokeAPIToken: %v", err)
	}
	if err := svc.DeleteAPIToken(ctx, id, userID); err != nil {
		t.Fatalf("DeleteAPIToken: %v", err)
	}

	// Check that audit entries referencing the old token_id have been anonymized.
	var tokenID sql.NullString
	var tokenName string
	err = svc.db.QueryRowContext(ctx, `
		SELECT token_id, token_name FROM audit_log
		WHERE action = 'token_created' AND user_id = ?
	`, userID).Scan(&tokenID, &tokenName)
	if err != nil {
		t.Fatalf("querying audit log: %v", err)
	}
	if tokenID.Valid {
		t.Error("expected token_id to be NULL after delete anonymization")
	}
	if tokenName != "[deleted token]" {
		t.Errorf("token_name = %q, want %q", tokenName, "[deleted token]")
	}

	// Verify deletion audit entry was written.
	var deleteCount int
	err = svc.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM audit_log WHERE action = 'token_deleted' AND user_id = ?
	`, userID).Scan(&deleteCount)
	if err != nil {
		t.Fatalf("counting deletion audit entries: %v", err)
	}
	if deleteCount != 1 {
		t.Errorf("expected 1 deletion audit entry, got %d", deleteCount)
	}
}

// --- GetAPIToken tests ---

func TestGetAPIToken_Success(t *testing.T) {
	svc := createTestUser(t, "secret")
	ctx := context.Background()

	token, err := svc.Login(ctx, "admin", "secret")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	userID, err := svc.ValidateSession(ctx, token)
	if err != nil {
		t.Fatalf("ValidateSession: %v", err)
	}

	_, id, err := svc.CreateAPIToken(ctx, userID, "get-test", "read,write")
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}

	apiToken, err := svc.GetAPIToken(ctx, id, userID)
	if err != nil {
		t.Fatalf("GetAPIToken: %v", err)
	}

	if apiToken.ID != id {
		t.Errorf("ID = %q, want %q", apiToken.ID, id)
	}
	if apiToken.Name != "get-test" {
		t.Errorf("Name = %q, want %q", apiToken.Name, "get-test")
	}
	if apiToken.Scopes != "read,write" {
		t.Errorf("Scopes = %q, want %q", apiToken.Scopes, "read,write")
	}
	if apiToken.Status != TokenStatusActive {
		t.Errorf("Status = %q, want %q", apiToken.Status, TokenStatusActive)
	}
	if apiToken.UserID != userID {
		t.Errorf("UserID = %q, want %q", apiToken.UserID, userID)
	}
}

func TestGetAPIToken_NotFound(t *testing.T) {
	svc := createTestUser(t, "secret")
	ctx := context.Background()

	token, err := svc.Login(ctx, "admin", "secret")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	userID, err := svc.ValidateSession(ctx, token)
	if err != nil {
		t.Fatalf("ValidateSession: %v", err)
	}

	_, err = svc.GetAPIToken(ctx, "nonexistent", userID)
	if err != ErrTokenNotFound {
		t.Errorf("error = %v, want ErrTokenNotFound", err)
	}
}

func TestGetAPIToken_WrongUser(t *testing.T) {
	svc := createTestUser(t, "secret")
	ctx := context.Background()

	token, err := svc.Login(ctx, "admin", "secret")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	userID, err := svc.ValidateSession(ctx, token)
	if err != nil {
		t.Fatalf("ValidateSession: %v", err)
	}

	_, id, err := svc.CreateAPIToken(ctx, userID, "owned-by-admin", "read")
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}

	// Attempting to get the token with a different user ID should fail.
	_, err = svc.GetAPIToken(ctx, id, "different-user-id")
	if err != ErrTokenNotFound {
		t.Errorf("error = %v, want ErrTokenNotFound", err)
	}
}

// --- WriteAuditLog tests ---

func TestWriteAuditLog(t *testing.T) {
	svc := createTestUser(t, "secret")
	ctx := context.Background()

	token, err := svc.Login(ctx, "admin", "secret")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	userID, err := svc.ValidateSession(ctx, token)
	if err != nil {
		t.Fatalf("ValidateSession: %v", err)
	}

	_, id, err := svc.CreateAPIToken(ctx, userID, "audit-token", "read")
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}

	if err := svc.WriteAuditLog(ctx, "test_action", id, "audit-token", userID, "some detail"); err != nil {
		t.Fatalf("WriteAuditLog: %v", err)
	}

	// Verify the entry was written.
	var action, tokenName, detail string
	err = svc.db.QueryRowContext(ctx, `
		SELECT action, token_name, detail FROM audit_log
		WHERE token_id = ? AND user_id = ?
	`, id, userID).Scan(&action, &tokenName, &detail)
	if err != nil {
		t.Fatalf("querying audit log: %v", err)
	}

	if action != "test_action" {
		t.Errorf("action = %q, want %q", action, "test_action")
	}
	if tokenName != "audit-token" {
		t.Errorf("token_name = %q, want %q", tokenName, "audit-token")
	}
	if detail != "some detail" {
		t.Errorf("detail = %q, want %q", detail, "some detail")
	}
}

// --- Full lifecycle test ---

func TestFullTokenLifecycle(t *testing.T) {
	svc := createTestUser(t, "secure-password")
	ctx := context.Background()

	// 1. Login.
	sessionToken, err := svc.Login(ctx, "admin", "secure-password")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	// 2. Validate session.
	userID, err := svc.ValidateSession(ctx, sessionToken)
	if err != nil {
		t.Fatalf("ValidateSession: %v", err)
	}

	// 3. Create API token.
	plaintext, tokenID, err := svc.CreateAPIToken(ctx, userID, "full-lifecycle", "read,write")
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}

	// 4. Validate API token.
	gotUID, gotScopes, err := svc.ValidateAPIToken(ctx, plaintext)
	if err != nil {
		t.Fatalf("ValidateAPIToken: %v", err)
	}
	if gotUID != userID {
		t.Errorf("validated user ID = %q, want %q", gotUID, userID)
	}
	if gotScopes != "read,write" {
		t.Errorf("validated scopes = %q, want %q", gotScopes, "read,write")
	}

	// 5. List tokens.
	tokens, err := svc.ListAPITokens(ctx, userID)
	if err != nil {
		t.Fatalf("ListAPITokens: %v", err)
	}
	if len(tokens) != 1 {
		t.Fatalf("expected 1 token, got %d", len(tokens))
	}

	// 6. Revoke.
	if err := svc.RevokeAPIToken(ctx, tokenID, userID); err != nil {
		t.Fatalf("RevokeAPIToken: %v", err)
	}

	// 7. Validate revoked token should fail.
	_, _, err = svc.ValidateAPIToken(ctx, plaintext)
	if err == nil {
		t.Error("expected error when validating revoked token")
	}

	// 8. Delete.
	if err := svc.DeleteAPIToken(ctx, tokenID, userID); err != nil {
		t.Fatalf("DeleteAPIToken: %v", err)
	}

	// 9. Token should be gone.
	_, err = svc.GetAPIToken(ctx, tokenID, userID)
	if err != ErrTokenNotFound {
		t.Errorf("error after delete = %v, want ErrTokenNotFound", err)
	}

	// 10. Logout.
	if err := svc.Logout(ctx, sessionToken); err != nil {
		t.Fatalf("Logout: %v", err)
	}

	// 11. Session should be invalid.
	_, err = svc.ValidateSession(ctx, sessionToken)
	if err == nil {
		t.Error("expected error after logout")
	}
}

// --- Sentinel error tests ---

func TestSentinelErrors(t *testing.T) {
	// Verify sentinel errors have distinct messages.
	errors := []struct {
		name string
		err  error
	}{
		{"ErrTokenNotFound", ErrTokenNotFound},
		{"ErrTokenNotRevoked", ErrTokenNotRevoked},
		{"ErrTokenActive", ErrTokenActive},
	}

	for _, e := range errors {
		t.Run(e.name, func(t *testing.T) {
			if e.err == nil {
				t.Error("sentinel error should not be nil")
			}
			if e.err.Error() == "" {
				t.Error("sentinel error message should not be empty")
			}
		})
	}
}

// --- ValidScopes tests ---

func TestValidScopes(t *testing.T) {
	expected := []TokenScope{ScopeRead, ScopeWrite, ScopeWebhook, ScopeAdmin}
	for _, scope := range expected {
		if !ValidScopes[scope] {
			t.Errorf("expected %q to be a valid scope", scope)
		}
	}

	// Unknown scope should not be valid.
	if ValidScopes["unknown"] {
		t.Error("expected 'unknown' to be an invalid scope")
	}
}

// --- Constants tests ---

func TestAPITokenPrefix(t *testing.T) {
	if APITokenPrefix != "sw_" {
		t.Errorf("APITokenPrefix = %q, want %q", APITokenPrefix, "sw_")
	}
}

func TestTokenStatusValues(t *testing.T) {
	if TokenStatusActive != "active" {
		t.Errorf("TokenStatusActive = %q, want %q", TokenStatusActive, "active")
	}
	if TokenStatusRevoked != "revoked" {
		t.Errorf("TokenStatusRevoked = %q, want %q", TokenStatusRevoked, "revoked")
	}
}
