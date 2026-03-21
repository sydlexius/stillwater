package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/auth"
	"github.com/sydlexius/stillwater/internal/database"
	"github.com/sydlexius/stillwater/internal/nfo"
	"github.com/sydlexius/stillwater/internal/rule"
)

// testRouterWithAuth creates a Router backed by a file-based SQLite DB with a
// seeded admin user. Returns the router, auth service, and the user ID.
func testRouterWithAuth(t *testing.T) (*Router, *auth.Service, string) {
	t.Helper()

	dbDir := t.TempDir()
	dbPath := filepath.Join(dbDir, "test.db")

	db, err := database.Open(dbPath)
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	if err := database.Migrate(db); err != nil {
		t.Fatalf("running migrations: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	authSvc := auth.NewService(db)
	created, err := authSvc.Setup(context.Background(), "admin", "password")
	if err != nil {
		t.Fatalf("setting up admin: %v", err)
	}
	if !created {
		t.Fatal("expected admin user to be created")
	}

	// Get the user ID by logging in.
	_, err = authSvc.Login(context.Background(), "admin", "password")
	if err != nil {
		t.Fatalf("logging in: %v", err)
	}

	// Look up user ID directly.
	var userID string
	if err := db.QueryRow("SELECT id FROM users WHERE username = 'admin'").Scan(&userID); err != nil {
		t.Fatalf("looking up user id: %v", err)
	}

	ruleSvc := rule.NewService(db)
	if err := ruleSvc.SeedDefaults(context.Background()); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}
	nfoSnapSvc := nfo.NewSnapshotService(db)

	r := NewRouter(RouterDeps{
		AuthService:        authSvc,
		RuleService:        ruleSvc,
		NFOSnapshotService: nfoSnapSvc,
		DB:                 db,
		Logger:             logger,
		StaticDir:          "../../web/static",
	})

	return r, authSvc, userID
}

// withUserCtx adds the given user ID to the request context, simulating
// authenticated middleware.
func withUserCtx(req *http.Request, userID string) *http.Request {
	ctx := middleware.WithTestUserID(req.Context(), userID)
	return req.WithContext(ctx)
}

func TestTokenLifecycle_ActiveToRevokedToArchivedToDeleted(t *testing.T) {
	r, authSvc, userID := testRouterWithAuth(t)

	// Step 1: Create a token.
	_, tokenID, err := authSvc.CreateAPIToken(context.Background(), userID, "test-token", "read,write")
	if err != nil {
		t.Fatalf("creating token: %v", err)
	}

	// Verify token is active.
	tok, err := authSvc.GetAPIToken(context.Background(), tokenID, userID)
	if err != nil {
		t.Fatalf("getting token: %v", err)
	}
	if tok.Status != auth.TokenStatusActive {
		t.Fatalf("expected status active, got %s", tok.Status)
	}

	// Step 2: Revoke the token via handler.
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/auth/tokens/"+tokenID, nil)
	req.SetPathValue("id", tokenID)
	req = withUserCtx(req, userID)
	w := httptest.NewRecorder()
	r.handleRevokeAPIToken(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("revoke: status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	tok, err = authSvc.GetAPIToken(context.Background(), tokenID, userID)
	if err != nil {
		t.Fatalf("getting token after revoke: %v", err)
	}
	if tok.Status != auth.TokenStatusRevoked {
		t.Fatalf("expected status revoked, got %s", tok.Status)
	}

	// Step 3: Archive the revoked token.
	req = httptest.NewRequest(http.MethodPatch, "/api/v1/auth/tokens/"+tokenID+"/archive", nil)
	req.SetPathValue("id", tokenID)
	req = withUserCtx(req, userID)
	w = httptest.NewRecorder()
	r.handleArchiveAPIToken(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("archive: status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	tok, err = authSvc.GetAPIToken(context.Background(), tokenID, userID)
	if err != nil {
		t.Fatalf("getting token after archive: %v", err)
	}
	if tok.Status != auth.TokenStatusArchived {
		t.Fatalf("expected status archived, got %s", tok.Status)
	}

	// Verify archived token is excluded from default list.
	tokens, err := authSvc.ListAPITokens(context.Background(), userID)
	if err != nil {
		t.Fatalf("listing tokens: %v", err)
	}
	for _, listed := range tokens {
		if listed.ID == tokenID {
			t.Error("archived token should not appear in default list")
		}
	}

	// Verify archived token IS included in the "all" list.
	allTokens, err := authSvc.ListAPITokensAll(context.Background(), userID)
	if err != nil {
		t.Fatalf("listing all tokens: %v", err)
	}
	found := false
	for _, listed := range allTokens {
		if listed.ID == tokenID {
			found = true
			break
		}
	}
	if !found {
		t.Error("archived token should appear in all-inclusive list")
	}

	// Step 4: Permanently delete the archived token.
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/auth/tokens/"+tokenID+"/permanent", nil)
	req.SetPathValue("id", tokenID)
	req = withUserCtx(req, userID)
	w = httptest.NewRecorder()
	r.handleDeleteAPIToken(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("delete: status = %d, want %d; body: %s", w.Code, http.StatusNoContent, w.Body.String())
	}

	// Verify token is gone.
	_, err = authSvc.GetAPIToken(context.Background(), tokenID, userID)
	if err == nil {
		t.Error("expected error getting deleted token, got nil")
	}
}

func TestArchiveActiveToken_Returns409(t *testing.T) {
	r, authSvc, userID := testRouterWithAuth(t)

	// Create an active token.
	_, tokenID, err := authSvc.CreateAPIToken(context.Background(), userID, "active-token", "read")
	if err != nil {
		t.Fatalf("creating token: %v", err)
	}

	// Try to archive it without revoking first.
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/auth/tokens/"+tokenID+"/archive", nil)
	req.SetPathValue("id", tokenID)
	req = withUserCtx(req, userID)
	w := httptest.NewRecorder()
	r.handleArchiveAPIToken(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("archive active: status = %d, want %d; body: %s", w.Code, http.StatusConflict, w.Body.String())
	}
}

func TestDeleteActiveToken_Returns409(t *testing.T) {
	r, authSvc, userID := testRouterWithAuth(t)

	// Create an active token.
	_, tokenID, err := authSvc.CreateAPIToken(context.Background(), userID, "active-token", "read")
	if err != nil {
		t.Fatalf("creating token: %v", err)
	}

	// Try to delete without revoking first.
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/auth/tokens/"+tokenID+"/permanent", nil)
	req.SetPathValue("id", tokenID)
	req = withUserCtx(req, userID)
	w := httptest.NewRecorder()
	r.handleDeleteAPIToken(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("delete active: status = %d, want %d; body: %s", w.Code, http.StatusConflict, w.Body.String())
	}
}

func TestUnarchive_RestoresToRevoked(t *testing.T) {
	r, authSvc, userID := testRouterWithAuth(t)

	// Create, revoke, and archive a token.
	_, tokenID, err := authSvc.CreateAPIToken(context.Background(), userID, "unarchive-test", "read")
	if err != nil {
		t.Fatalf("creating token: %v", err)
	}
	if err := authSvc.RevokeAPIToken(context.Background(), tokenID, userID); err != nil {
		t.Fatalf("revoking token: %v", err)
	}
	if err := authSvc.ArchiveAPIToken(context.Background(), tokenID, userID); err != nil {
		t.Fatalf("archiving token: %v", err)
	}

	// Unarchive via handler.
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/auth/tokens/"+tokenID+"/unarchive", nil)
	req.SetPathValue("id", tokenID)
	req = withUserCtx(req, userID)
	w := httptest.NewRecorder()
	r.handleUnarchiveAPIToken(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unarchive: status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	tok, err := authSvc.GetAPIToken(context.Background(), tokenID, userID)
	if err != nil {
		t.Fatalf("getting token after unarchive: %v", err)
	}
	if tok.Status != auth.TokenStatusRevoked {
		t.Fatalf("expected status revoked after unarchive, got %s", tok.Status)
	}
}

func TestUnarchiveNonArchivedToken_Returns409(t *testing.T) {
	r, authSvc, userID := testRouterWithAuth(t)

	// Create and revoke a token (but do not archive).
	_, tokenID, err := authSvc.CreateAPIToken(context.Background(), userID, "unarchive-error-test", "read")
	if err != nil {
		t.Fatalf("creating token: %v", err)
	}
	if err := authSvc.RevokeAPIToken(context.Background(), tokenID, userID); err != nil {
		t.Fatalf("revoking token: %v", err)
	}

	// Try to unarchive a revoked (non-archived) token.
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/auth/tokens/"+tokenID+"/unarchive", nil)
	req.SetPathValue("id", tokenID)
	req = withUserCtx(req, userID)
	w := httptest.NewRecorder()
	r.handleUnarchiveAPIToken(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("unarchive non-archived: status = %d, want %d; body: %s", w.Code, http.StatusConflict, w.Body.String())
	}
}

func TestDeleteRevokedToken_AuditAnonymization(t *testing.T) {
	r, authSvc, userID := testRouterWithAuth(t)

	// Create and revoke a token.
	_, tokenID, err := authSvc.CreateAPIToken(context.Background(), userID, "audit-test", "read")
	if err != nil {
		t.Fatalf("creating token: %v", err)
	}

	// Write an audit log entry referencing this token.
	if err := authSvc.WriteAuditLog(context.Background(), "token_created", tokenID, "audit-test", userID, "created"); err != nil {
		t.Fatalf("writing audit log: %v", err)
	}

	// Revoke it.
	if err := authSvc.RevokeAPIToken(context.Background(), tokenID, userID); err != nil {
		t.Fatalf("revoking token: %v", err)
	}

	// Delete it.
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/auth/tokens/"+tokenID+"/permanent", nil)
	req.SetPathValue("id", tokenID)
	req = withUserCtx(req, userID)
	w := httptest.NewRecorder()
	r.handleDeleteAPIToken(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("delete: status = %d, want %d; body: %s", w.Code, http.StatusNoContent, w.Body.String())
	}

	// Verify audit log entries are anonymized. Query the DB directly.
	rows, err := r.db.Query("SELECT token_id, token_name, action FROM audit_log WHERE user_id = ? ORDER BY created_at", userID)
	if err != nil {
		t.Fatalf("querying audit log: %v", err)
	}
	defer rows.Close() //nolint:errcheck

	type auditRow struct {
		tokenID   *string
		tokenName string
		action    string
	}
	var entries []auditRow
	for rows.Next() {
		var e auditRow
		if err := rows.Scan(&e.tokenID, &e.tokenName, &e.action); err != nil {
			t.Fatalf("scanning audit row: %v", err)
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating audit rows: %v", err)
	}

	if len(entries) < 2 {
		t.Fatalf("expected at least 2 audit entries, got %d", len(entries))
	}

	// The original entry should be anonymized.
	original := entries[0]
	if original.tokenID != nil {
		t.Errorf("expected anonymized token_id (nil), got %q", *original.tokenID)
	}
	if original.tokenName != "[deleted token]" {
		t.Errorf("expected anonymized token_name '[deleted token]', got %q", original.tokenName)
	}

	// The final "token_deleted" entry should also have no token_id.
	deletion := entries[len(entries)-1]
	if deletion.action != "token_deleted" {
		t.Errorf("expected last audit entry action 'token_deleted', got %q", deletion.action)
	}
	if deletion.tokenID != nil {
		t.Errorf("expected deletion audit entry to have nil token_id, got %q", *deletion.tokenID)
	}
}

func TestListAPITokens_IncludeArchived(t *testing.T) {
	r, authSvc, userID := testRouterWithAuth(t)

	// Create two tokens, revoke and archive one.
	_, _, err := authSvc.CreateAPIToken(context.Background(), userID, "active-token", "read")
	if err != nil {
		t.Fatalf("creating active token: %v", err)
	}

	_, archivedID, err := authSvc.CreateAPIToken(context.Background(), userID, "archived-token", "read")
	if err != nil {
		t.Fatalf("creating archived token: %v", err)
	}
	if err := authSvc.RevokeAPIToken(context.Background(), archivedID, userID); err != nil {
		t.Fatalf("revoking token: %v", err)
	}
	if err := authSvc.ArchiveAPIToken(context.Background(), archivedID, userID); err != nil {
		t.Fatalf("archiving token: %v", err)
	}

	// Default list (no query param) should exclude archived.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/tokens", nil)
	req = withUserCtx(req, userID)
	w := httptest.NewRecorder()
	r.handleListAPITokens(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("list: status = %d, want %d", w.Code, http.StatusOK)
	}

	var tokens []auth.APIToken
	if err := json.NewDecoder(w.Body).Decode(&tokens); err != nil {
		t.Fatalf("decoding tokens: %v", err)
	}
	if len(tokens) != 1 {
		t.Fatalf("expected 1 token in default list, got %d", len(tokens))
	}

	// With include_archived=true, should see both.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/auth/tokens?include_archived=true", nil)
	req = withUserCtx(req, userID)
	w = httptest.NewRecorder()
	r.handleListAPITokens(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("list all: status = %d, want %d", w.Code, http.StatusOK)
	}

	if err := json.NewDecoder(w.Body).Decode(&tokens); err != nil {
		t.Fatalf("decoding all tokens: %v", err)
	}
	if len(tokens) != 2 {
		t.Fatalf("expected 2 tokens in all-inclusive list, got %d", len(tokens))
	}
}

func TestRevokedToken_CannotAuthenticate(t *testing.T) {
	_, authSvc, userID := testRouterWithAuth(t)

	plaintext, tokenID, err := authSvc.CreateAPIToken(context.Background(), userID, "revoke-auth-test", "read")
	if err != nil {
		t.Fatalf("creating token: %v", err)
	}

	// Validate before revoking -- should succeed.
	validUserID, _, err := authSvc.ValidateAPIToken(context.Background(), plaintext)
	if err != nil {
		t.Fatalf("validate active token: %v", err)
	}
	if validUserID != userID {
		t.Fatalf("expected user %s, got %s", userID, validUserID)
	}

	// Revoke and try again.
	if err := authSvc.RevokeAPIToken(context.Background(), tokenID, userID); err != nil {
		t.Fatalf("revoking token: %v", err)
	}

	_, _, err = authSvc.ValidateAPIToken(context.Background(), plaintext)
	if err == nil {
		t.Fatal("expected error validating revoked token, got nil")
	}
	if !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("expected 'revoked' in error, got: %s", err.Error())
	}
}

func TestCreateAPIToken_ReturnsStatusField(t *testing.T) {
	_, authSvc, userID := testRouterWithAuth(t)

	_, tokenID, err := authSvc.CreateAPIToken(context.Background(), userID, "status-test", "read")
	if err != nil {
		t.Fatalf("creating token: %v", err)
	}

	tok, err := authSvc.GetAPIToken(context.Background(), tokenID, userID)
	if err != nil {
		t.Fatalf("getting token: %v", err)
	}
	if tok.Status != auth.TokenStatusActive {
		t.Errorf("expected status 'active', got %q", tok.Status)
	}
}

// TestArchivedToken_CannotAuthenticate verifies that archived tokens are
// rejected by ValidateAPIToken, same as revoked tokens.
func TestArchivedToken_CannotAuthenticate(t *testing.T) {
	_, authSvc, userID := testRouterWithAuth(t)

	plaintext, tokenID, err := authSvc.CreateAPIToken(context.Background(), userID, "archived-auth-test", "read")
	if err != nil {
		t.Fatalf("creating token: %v", err)
	}
	if err := authSvc.RevokeAPIToken(context.Background(), tokenID, userID); err != nil {
		t.Fatalf("revoking token: %v", err)
	}
	if err := authSvc.ArchiveAPIToken(context.Background(), tokenID, userID); err != nil {
		t.Fatalf("archiving token: %v", err)
	}

	_, _, err = authSvc.ValidateAPIToken(context.Background(), plaintext)
	if err == nil {
		t.Error("expected archived token to be rejected, but ValidateAPIToken returned nil error")
	}
}

// TestDeleteRevokedToken_DirectlyWithoutArchiving verifies that a revoked
// (non-archived) token can be deleted directly without archiving first.
func TestDeleteRevokedToken_DirectlyWithoutArchiving(t *testing.T) {
	router, authSvc, userID := testRouterWithAuth(t)

	_, tokenID, err := authSvc.CreateAPIToken(context.Background(), userID, "direct-delete-test", "read")
	if err != nil {
		t.Fatalf("creating token: %v", err)
	}
	if err := authSvc.RevokeAPIToken(context.Background(), tokenID, userID); err != nil {
		t.Fatalf("revoking token: %v", err)
	}

	// Delete without archiving first.
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/auth/tokens/"+tokenID+"/permanent", nil)
	req.SetPathValue("id", tokenID)
	req = withUserCtx(req, userID)
	w := httptest.NewRecorder()
	router.handleDeleteAPIToken(w, req)

	if w.Code != http.StatusNoContent {
		t.Errorf("expected 204, got %d: %s", w.Code, w.Body.String())
	}

	// Verify the token is gone.
	_, err = authSvc.GetAPIToken(context.Background(), tokenID, userID)
	if err == nil {
		t.Error("expected token to be deleted, but GetAPIToken returned nil error")
	}
}
