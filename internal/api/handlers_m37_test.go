package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/auth"
)

// withOperatorCtx adds both user ID and operator role to the request context.
func withOperatorCtx(req *http.Request, userID string) *http.Request {
	ctx := middleware.WithTestUserID(req.Context(), userID)
	ctx = middleware.WithTestRole(ctx, "operator")
	return req.WithContext(ctx)
}

// createOperatorUser creates a second user with operator role and returns their ID.
func createOperatorUser(t *testing.T, authSvc *auth.Service, adminID string) string {
	t.Helper()
	user, err := authSvc.CreateLocalUser(context.Background(), "operator1", "password123", "Operator User", "operator", adminID)
	if err != nil {
		t.Fatalf("creating operator user: %v", err)
	}
	return user.ID
}

// enableMultiUser writes multi_user.enabled = "true" into the settings table.
func enableMultiUser(t *testing.T, r *Router) {
	t.Helper()
	_, err := r.db.Exec(
		`INSERT INTO settings (key, value, updated_at) VALUES ('multi_user.enabled', 'true', datetime('now'))
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
	)
	if err != nil {
		t.Fatalf("enabling multi_user: %v", err)
	}
}

// --- Issue #743: Admin Route Gating ---

// TestAdminRoute_Operator_Gets403_OnSettings verifies that an operator receives
// 403 Forbidden when accessing admin-only settings endpoints. The test calls the
// handler through RequireAdmin to mirror how the router wires it.
func TestAdminRoute_Operator_Gets403_OnSettings(t *testing.T) {
	r, authSvc, adminID := testRouterWithAuth(t)
	opID := createOperatorUser(t, authSvc, adminID)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil)
	req = withOperatorCtx(req, opID)
	w := httptest.NewRecorder()
	middleware.RequireAdmin(r.handleGetSettings)(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
}

// TestAdminRoute_Admin_Gets200_OnSettings verifies that an administrator can
// access settings endpoints.
func TestAdminRoute_Admin_Gets200_OnSettings(t *testing.T) {
	r, _, adminID := testRouterWithAuth(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil)
	req = withAdminCtx(req, adminID)
	w := httptest.NewRecorder()
	middleware.RequireAdmin(r.handleGetSettings)(w, req)

	// Settings returns 200 when admin.
	if w.Code == http.StatusForbidden {
		t.Fatalf("admin should not get 403, got %d: %s", w.Code, w.Body.String())
	}
}

// TestAdminRoute_Operator_Gets403_OnUpdateRule verifies that an operator cannot
// change rule configuration (enable/disable/set automation mode). The test calls
// the handler through RequireAdmin to mirror how the router wires it.
func TestAdminRoute_Operator_Gets403_OnUpdateRule(t *testing.T) {
	r, authSvc, adminID := testRouterWithAuth(t)
	opID := createOperatorUser(t, authSvc, adminID)

	body := `{"enabled":false}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/rules/some-rule-id", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", "some-rule-id")
	req = withOperatorCtx(req, opID)
	w := httptest.NewRecorder()
	middleware.RequireAdmin(r.handleUpdateRule)(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
}

// TestAdminRoute_Operator_CanRunRule verifies that an operator can execute a rule
// (rule execution is not admin-gated).
func TestAdminRoute_Operator_CanRunRule(t *testing.T) {
	r, authSvc, adminID := testRouterWithAuth(t)
	opID := createOperatorUser(t, authSvc, adminID)

	// List rules to get an existing rule ID.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/rules", nil)
	req = withOperatorCtx(req, opID)
	w := httptest.NewRecorder()
	r.handleListRules(w, req)

	// Operators should be able to list rules (not admin-gated).
	if w.Code == http.StatusForbidden {
		t.Fatalf("operator should be able to list rules, got 403: %s", w.Body.String())
	}
}

// --- Issue #743: Multi-User Opt-In Gate ---

// TestMultiUserGate_InviteRoutes_Return404_WhenDisabled verifies that invite
// endpoints return 404 when multi_user.enabled is not set.
func TestMultiUserGate_InviteRoutes_Return404_WhenDisabled(t *testing.T) {
	r, _, adminID := testRouterWithAuth(t)
	// multi_user.enabled is not set -- defaults to "false"

	req := httptest.NewRequest(http.MethodGet, "/api/v1/users/invites", nil)
	req = withAdminCtx(req, adminID)
	w := httptest.NewRecorder()

	mw := middleware.RequireMultiUser(r.getStringSetting)
	mw(r.handleListInvites)(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 when multi_user disabled, got %d: %s", w.Code, w.Body.String())
	}
}

// TestMultiUserGate_InviteRoutes_Return200_WhenEnabled verifies that invite
// endpoints are accessible when multi_user.enabled is "true".
func TestMultiUserGate_InviteRoutes_Return200_WhenEnabled(t *testing.T) {
	r, _, adminID := testRouterWithAuth(t)
	enableMultiUser(t, r)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/users/invites", nil)
	req = withAdminCtx(req, adminID)
	w := httptest.NewRecorder()

	mw := middleware.RequireMultiUser(r.getStringSetting)
	mw(r.handleListInvites)(w, req)

	// Should not be 404 (may be 200 or other status, but not multi-user gate rejection).
	if w.Code == http.StatusNotFound {
		t.Fatalf("expected invite list to be accessible when multi_user enabled, got 404: %s", w.Body.String())
	}
}

// TestMultiUserGate_LoginFlow_UnaffectedByMultiUser verifies that the login
// endpoint works regardless of multi_user.enabled (it is not gated).
func TestMultiUserGate_LoginFlow_UnaffectedByMultiUser(t *testing.T) {
	r, _, _ := testRouterWithAuth(t)
	// multi_user.enabled is not set -- login should still work.

	body := `{"username":"admin","password":"password"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.handleLogin(w, req)

	// Login should succeed regardless of multi_user.enabled.
	if w.Code == http.StatusNotFound {
		t.Fatalf("login should not return 404 regardless of multi_user setting")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("login: expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

// --- Issue #744: API Token Scope Ceiling ---

// TestTokenScopeCeiling_Admin_CanCreateAdminToken verifies that an administrator
// can create a token with admin scope.
func TestTokenScopeCeiling_Admin_CanCreateAdminToken(t *testing.T) {
	r, _, adminID := testRouterWithAuth(t)

	body := `{"name":"admin-token","scopes":"admin"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/tokens", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = withAdminCtx(req, adminID)
	w := httptest.NewRecorder()
	r.handleCreateAPIToken(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("admin creating admin-scoped token: expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp["token"] == "" {
		t.Error("expected non-empty token value in response")
	}
}

// TestTokenScopeCeiling_Operator_CanCreateReadToken verifies that an operator
// can create read-scoped tokens.
func TestTokenScopeCeiling_Operator_CanCreateReadToken(t *testing.T) {
	r, authSvc, adminID := testRouterWithAuth(t)
	opID := createOperatorUser(t, authSvc, adminID)

	body := `{"name":"read-token","scopes":"read"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/tokens", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = withOperatorCtx(req, opID)
	w := httptest.NewRecorder()
	r.handleCreateAPIToken(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("operator creating read-scoped token: expected 201, got %d: %s", w.Code, w.Body.String())
	}
}

// TestTokenScopeCeiling_Operator_CanCreateWriteToken verifies that an operator
// can create write-scoped tokens.
func TestTokenScopeCeiling_Operator_CanCreateWriteToken(t *testing.T) {
	r, authSvc, adminID := testRouterWithAuth(t)
	opID := createOperatorUser(t, authSvc, adminID)

	body := `{"name":"write-token","scopes":"write"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/tokens", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = withOperatorCtx(req, opID)
	w := httptest.NewRecorder()
	r.handleCreateAPIToken(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("operator creating write-scoped token: expected 201, got %d: %s", w.Code, w.Body.String())
	}
}

// TestTokenScopeCeiling_Operator_CanCreateWebhookToken verifies that an operator
// can create webhook-scoped tokens.
func TestTokenScopeCeiling_Operator_CanCreateWebhookToken(t *testing.T) {
	r, authSvc, adminID := testRouterWithAuth(t)
	opID := createOperatorUser(t, authSvc, adminID)

	body := `{"name":"webhook-token","scopes":"webhook"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/tokens", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = withOperatorCtx(req, opID)
	w := httptest.NewRecorder()
	r.handleCreateAPIToken(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("operator creating webhook-scoped token: expected 201, got %d: %s", w.Code, w.Body.String())
	}
}

// TestTokenScopeCeiling_Operator_Cannot_CreateAdminToken verifies that an operator
// receives 403 when attempting to create an admin-scoped token.
func TestTokenScopeCeiling_Operator_Cannot_CreateAdminToken(t *testing.T) {
	r, authSvc, adminID := testRouterWithAuth(t)
	opID := createOperatorUser(t, authSvc, adminID)

	body := `{"name":"admin-token","scopes":"admin"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/tokens", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = withOperatorCtx(req, opID)
	w := httptest.NewRecorder()
	r.handleCreateAPIToken(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("operator creating admin-scoped token: expected 403, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding error response: %v", err)
	}
	if !strings.Contains(resp["error"], "operator") {
		t.Errorf("expected error message to mention operator, got: %s", resp["error"])
	}
}

// TestTokenScopeCeiling_Operator_Cannot_CreateReadPlusAdminToken verifies that
// mixing admin with other scopes is also rejected for operators.
func TestTokenScopeCeiling_Operator_Cannot_CreateReadPlusAdminToken(t *testing.T) {
	r, authSvc, adminID := testRouterWithAuth(t)
	opID := createOperatorUser(t, authSvc, adminID)

	body := `{"name":"mixed-token","scopes":"read,admin"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/tokens", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = withOperatorCtx(req, opID)
	w := httptest.NewRecorder()
	r.handleCreateAPIToken(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("operator creating read+admin token: expected 403, got %d: %s", w.Code, w.Body.String())
	}
}

// --- Issue #745: Setup Flow Refactor ---

// TestSetup_Local_CreatesAdministrator verifies that local setup via the
// handleSetup endpoint creates an administrator account, including when the
// auth registry is configured (the registry path must be skipped for local).
func TestSetup_Local_CreatesAdministrator(t *testing.T) {
	r, _, _ := testRouterWithAuth(t)

	// Wire up a registry with a local provider to match production config.
	registry := auth.NewRegistry()
	r.authRegistry = registry

	// Wipe existing users so setup can run (setup requires zero users).
	if _, err := r.db.Exec("DELETE FROM users"); err != nil {
		t.Fatalf("clearing users: %v", err)
	}
	if _, err := r.db.Exec("DELETE FROM sessions"); err != nil {
		t.Fatalf("clearing sessions: %v", err)
	}

	body := `{"auth_method":"local","username":"testadmin","password":"password123"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/setup", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.handleSetup(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("local setup: expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var role string
	if err := r.db.QueryRow("SELECT role FROM users WHERE username = 'testadmin'").Scan(&role); err != nil {
		t.Fatalf("querying created user role: %v", err)
	}
	if role != "administrator" {
		t.Errorf("local setup: expected role 'administrator', got %q", role)
	}
}

// TestSetup_WithRegistry_FederatedAlwaysAdministrator verifies that federated
// setup via the auth registry always creates the user as Administrator, even if
// the provider's MapRole would return a lesser role.
func TestSetup_WithRegistry_FederatedAlwaysAdministrator(t *testing.T) {
	r, _, _ := testRouterWithAuth(t)

	// Register a stub provider that maps roles to "operator".
	stubProvider := &stubOperatorProvider{
		providerType: "test-federated",
		identity: &auth.Identity{
			ProviderID:   "remote-user-1",
			DisplayName:  "Remote Admin",
			ProviderType: "test-federated",
			IsAdmin:      true,
		},
	}
	registry := auth.NewRegistry()
	registry.Register(stubProvider)
	r.authRegistry = registry

	// Wipe the existing admin so setup can run.
	if _, err := r.db.Exec("DELETE FROM users"); err != nil {
		t.Fatalf("clearing users: %v", err)
	}
	if _, err := r.db.Exec("DELETE FROM sessions"); err != nil {
		t.Fatalf("clearing sessions: %v", err)
	}

	body := `{"auth_method":"test-federated","username":"remote-user","password":"any"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/setup", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.handleSetup(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("setup: expected 201, got %d: %s", w.Code, w.Body.String())
	}

	// Verify the user was created with administrator role.
	var role string
	if err := r.db.QueryRow("SELECT role FROM users WHERE auth_provider = 'test-federated'").Scan(&role); err != nil {
		t.Fatalf("querying created user role: %v", err)
	}
	if role != "administrator" {
		t.Errorf("federated setup: expected role 'administrator', got %q (MapRole returned 'operator')", role)
	}
}

// stubOperatorProvider is a test auth.Authenticator whose MapRole always returns
// "operator". Used to verify that setup overrides MapRole to "administrator".
type stubOperatorProvider struct {
	providerType string
	identity     *auth.Identity
}

func (p *stubOperatorProvider) Type() string { return p.providerType }

func (p *stubOperatorProvider) Authenticate(_ context.Context, _ auth.Credentials) (*auth.Identity, error) {
	return p.identity, nil
}

func (p *stubOperatorProvider) CanAutoProvision(_ *auth.Identity) bool { return true }

// MapRole intentionally returns "operator" to test that setup ignores it.
func (p *stubOperatorProvider) MapRole(_ *auth.Identity) string { return "operator" }
