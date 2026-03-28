package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/auth"
)

// --- Flow 1: Local Login ---

// TestLocalLoginFlow verifies the end-to-end local authentication path:
// setup creates admin, login returns a session cookie, and the cookie grants
// access to authenticated endpoints with the correct role.
func TestLocalLoginFlow(t *testing.T) {
	r, _, _ := testRouterWithAuth(t)

	// Wipe all users so handleSetup can run (requires zero existing users).
	if _, err := r.db.Exec("DELETE FROM sessions"); err != nil {
		t.Fatalf("clearing sessions: %v", err)
	}
	if _, err := r.db.Exec("DELETE FROM users"); err != nil {
		t.Fatalf("clearing users: %v", err)
	}

	// Step 1: Call handleSetup to create the admin account.
	setupBody := `{"auth_method":"local","username":"localadmin","password":"password123"}`
	setupReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/setup", strings.NewReader(setupBody))
	setupReq.Header.Set("Content-Type", "application/json")
	setupW := httptest.NewRecorder()
	r.handleSetup(setupW, setupReq)

	if setupW.Code != http.StatusCreated {
		t.Fatalf("setup: expected 201, got %d: %s", setupW.Code, setupW.Body.String())
	}

	// Step 2: Call handleLogin with correct credentials.
	loginBody := `{"username":"localadmin","password":"password123"}`
	loginReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(loginBody))
	loginReq.Header.Set("Content-Type", "application/json")
	loginW := httptest.NewRecorder()
	r.handleLogin(loginW, loginReq)

	if loginW.Code != http.StatusOK {
		t.Fatalf("login: expected 200, got %d: %s", loginW.Code, loginW.Body.String())
	}

	// Step 3: Verify session cookie is set in the login response.
	var sessionCookie *http.Cookie
	for _, c := range loginW.Result().Cookies() {
		if c.Name == "session" {
			sessionCookie = c
			break
		}
	}
	if sessionCookie == nil {
		t.Fatal("login: expected session cookie in response, got none")
	}
	if sessionCookie.Value == "" {
		t.Fatal("login: session cookie has empty value")
	}

	// Step 4: Use the session cookie to call an authenticated endpoint (handleMe).
	meReq := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	meReq.AddCookie(sessionCookie)
	meW := httptest.NewRecorder()
	// Run through the Auth middleware so the session is validated.
	authMw := middleware.Auth(r.authService)
	authMw(http.HandlerFunc(r.handleMe)).ServeHTTP(meW, meReq)

	if meW.Code != http.StatusOK {
		t.Fatalf("auth/me: expected 200, got %d: %s", meW.Code, meW.Body.String())
	}

	var meResp map[string]string
	if err := json.NewDecoder(meW.Body).Decode(&meResp); err != nil {
		t.Fatalf("decoding auth/me response: %v", err)
	}
	if meResp["user_id"] == "" {
		t.Error("auth/me: expected non-empty user_id in response")
	}

	// Step 5: Verify the admin role is stored correctly in the DB.
	var role string
	if err := r.db.QueryRow("SELECT role FROM users WHERE username = 'localadmin'").Scan(&role); err != nil {
		t.Fatalf("querying role: %v", err)
	}
	if role != "administrator" {
		t.Errorf("expected role 'administrator', got %q", role)
	}
}

// TestLocalLoginFlow_WrongPassword verifies that handleLogin rejects invalid
// credentials with 401.
func TestLocalLoginFlow_WrongPassword(t *testing.T) {
	r, _, _ := testRouterWithAuth(t)

	loginBody := `{"username":"admin","password":"wrongpassword"}`
	loginReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(loginBody))
	loginReq.Header.Set("Content-Type", "application/json")
	loginW := httptest.NewRecorder()
	r.handleLogin(loginW, loginReq)

	if loginW.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for wrong password, got %d: %s", loginW.Code, loginW.Body.String())
	}
}

// --- Flow 2: Invite + Register ---

// TestInviteRegisterFlow verifies the multi-user invite + registration path:
// admin creates invite, new user redeems it, new user logs in and has the
// correct operator role, and role-based access is enforced correctly.
func TestInviteRegisterFlow(t *testing.T) {
	r, authSvc, adminID := testRouterWithAuth(t)
	enableMultiUser(t, r)

	// Step 1: Admin creates an invite for an operator via the auth service.
	invite, err := authSvc.CreateInvite(context.Background(), "operator", adminID, 24*time.Hour)
	if err != nil {
		t.Fatalf("creating invite: %v", err)
	}

	// Step 2: New user redeems the invite via handleRegister.
	registerBody := `{"code":"` + invite.Code + `","username":"opuser","password":"securepass99","display_name":"Op User"}`
	registerReq := httptest.NewRequest(http.MethodPost, "/api/v1/users/register", strings.NewReader(registerBody))
	registerReq.Header.Set("Content-Type", "application/json")
	registerW := httptest.NewRecorder()
	r.handleRegister(registerW, registerReq)

	if registerW.Code != http.StatusCreated {
		t.Fatalf("register: expected 201, got %d: %s", registerW.Code, registerW.Body.String())
	}

	// Step 3: Verify the new user was created with operator role.
	var newRole string
	if err := r.db.QueryRow("SELECT role FROM users WHERE username = 'opuser'").Scan(&newRole); err != nil {
		t.Fatalf("querying new user role: %v", err)
	}
	if newRole != "operator" {
		t.Errorf("expected role 'operator', got %q", newRole)
	}

	// Step 4: New user logs in via handleLogin.
	loginBody := `{"username":"opuser","password":"securepass99"}`
	loginReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(loginBody))
	loginReq.Header.Set("Content-Type", "application/json")
	loginW := httptest.NewRecorder()
	r.handleLogin(loginW, loginReq)

	if loginW.Code != http.StatusOK {
		t.Fatalf("operator login: expected 200, got %d: %s", loginW.Code, loginW.Body.String())
	}

	// Capture the session cookie from the login response.
	var opSessionCookie *http.Cookie
	for _, c := range loginW.Result().Cookies() {
		if c.Name == "session" {
			opSessionCookie = c
			break
		}
	}
	if opSessionCookie == nil {
		t.Fatal("operator login: expected session cookie, got none")
	}

	// Step 5: Look up the new user's ID.
	var opUserID string
	if err := r.db.QueryRow("SELECT id FROM users WHERE username = 'opuser'").Scan(&opUserID); err != nil {
		t.Fatalf("looking up operator user id: %v", err)
	}

	// Step 6: Verify operator gets 403 on admin-only endpoint (GET /api/v1/settings).
	settingsReq := httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil)
	settingsReq = withOperatorCtx(settingsReq, opUserID)
	settingsW := httptest.NewRecorder()
	middleware.RequireAdmin(r.handleGetSettings)(settingsW, settingsReq)

	if settingsW.Code != http.StatusForbidden {
		t.Fatalf("operator on settings: expected 403, got %d: %s", settingsW.Code, settingsW.Body.String())
	}

	// Step 7: Verify operator can access operator-allowed endpoint (GET /api/v1/rules).
	rulesReq := httptest.NewRequest(http.MethodGet, "/api/v1/rules", nil)
	rulesReq = withOperatorCtx(rulesReq, opUserID)
	rulesW := httptest.NewRecorder()
	r.handleListRules(rulesW, rulesReq)

	if rulesW.Code == http.StatusForbidden {
		t.Fatalf("operator on rules: should not get 403, got %d: %s", rulesW.Code, rulesW.Body.String())
	}
}

// TestInviteRegisterFlow_InvalidCode verifies that handleRegister rejects an
// invalid invite code with 400.
func TestInviteRegisterFlow_InvalidCode(t *testing.T) {
	r, _, _ := testRouterWithAuth(t)
	enableMultiUser(t, r)

	registerBody := `{"code":"bad-code","username":"nobody","password":"securepass99"}`
	registerReq := httptest.NewRequest(http.MethodPost, "/api/v1/users/register", strings.NewReader(registerBody))
	registerReq.Header.Set("Content-Type", "application/json")
	registerW := httptest.NewRecorder()
	r.handleRegister(registerW, registerReq)

	if registerW.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid code, got %d: %s", registerW.Code, registerW.Body.String())
	}
}

// --- Flow 3: Federated Login ---

// stubFederatedProvider is a test auth.Authenticator that always returns a
// successful authentication with a fixed identity. Used for federated login
// tests without a real Emby/Jellyfin server.
type stubFederatedProvider struct {
	providerType string
	identity     *auth.Identity
}

func (p *stubFederatedProvider) Type() string { return p.providerType }

func (p *stubFederatedProvider) Authenticate(_ context.Context, _ auth.Credentials) (*auth.Identity, error) {
	return p.identity, nil
}

func (p *stubFederatedProvider) CanAutoProvision(_ *auth.Identity) bool { return true }

func (p *stubFederatedProvider) MapRole(identity *auth.Identity) string {
	if identity.IsAdmin {
		return "administrator"
	}
	return "operator"
}

// TestFederatedLoginFlow_AutoProvision verifies that a federated provider login
// auto-provisions the user on first login and creates a valid session.
func TestFederatedLoginFlow_AutoProvision(t *testing.T) {
	r, _, _ := testRouterWithAuth(t)

	stubProvider := &stubFederatedProvider{
		providerType: "test-stub",
		identity: &auth.Identity{
			ProviderID:   "remote-user-42",
			DisplayName:  "Stub User",
			ProviderType: "test-stub",
			IsAdmin:      false,
		},
	}
	registry := auth.NewRegistry()
	registry.Register(stubProvider)
	r.authRegistry = registry

	// Login via the stub federated provider.
	loginBody := `{"username":"remoteuser","password":"any","provider":"test-stub"}`
	loginReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(loginBody))
	loginReq.Header.Set("Content-Type", "application/json")
	loginW := httptest.NewRecorder()
	r.handleLogin(loginW, loginReq)

	if loginW.Code != http.StatusOK {
		t.Fatalf("federated login: expected 200, got %d: %s", loginW.Code, loginW.Body.String())
	}

	// Verify session cookie is set.
	var sessionCookie *http.Cookie
	for _, c := range loginW.Result().Cookies() {
		if c.Name == "session" {
			sessionCookie = c
			break
		}
	}
	if sessionCookie == nil {
		t.Fatal("federated login: expected session cookie, got none")
	}
	if sessionCookie.Value == "" {
		t.Fatal("federated login: session cookie has empty value")
	}

	// Verify the user was auto-provisioned with operator role.
	var role string
	if err := r.db.QueryRow("SELECT role FROM users WHERE auth_provider = 'test-stub'").Scan(&role); err != nil {
		t.Fatalf("querying auto-provisioned user: %v", err)
	}
	if role != "operator" {
		t.Errorf("federated auto-provision: expected role 'operator', got %q", role)
	}
}

// TestFederatedLoginFlow_AdminAutoProvision verifies that a federated provider
// auto-provisions the user as administrator when IsAdmin is true.
func TestFederatedLoginFlow_AdminAutoProvision(t *testing.T) {
	r, _, _ := testRouterWithAuth(t)

	stubProvider := &stubFederatedProvider{
		providerType: "test-stub-admin",
		identity: &auth.Identity{
			ProviderID:   "remote-admin-99",
			DisplayName:  "Stub Admin",
			ProviderType: "test-stub-admin",
			IsAdmin:      true,
		},
	}
	registry := auth.NewRegistry()
	registry.Register(stubProvider)
	r.authRegistry = registry

	loginBody := `{"username":"remoteadmin","password":"any","provider":"test-stub-admin"}`
	loginReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(loginBody))
	loginReq.Header.Set("Content-Type", "application/json")
	loginW := httptest.NewRecorder()
	r.handleLogin(loginW, loginReq)

	if loginW.Code != http.StatusOK {
		t.Fatalf("federated admin login: expected 200, got %d: %s", loginW.Code, loginW.Body.String())
	}

	// Verify the user was auto-provisioned with administrator role.
	var role string
	if err := r.db.QueryRow("SELECT role FROM users WHERE auth_provider = 'test-stub-admin'").Scan(&role); err != nil {
		t.Fatalf("querying auto-provisioned admin user: %v", err)
	}
	if role != "administrator" {
		t.Errorf("federated admin auto-provision: expected role 'administrator', got %q", role)
	}
}

// TestFederatedLoginFlow_SessionValid verifies that a session cookie returned
// by federated login is accepted by the Auth middleware.
func TestFederatedLoginFlow_SessionValid(t *testing.T) {
	r, _, _ := testRouterWithAuth(t)

	stubProvider := &stubFederatedProvider{
		providerType: "test-stub-session",
		identity: &auth.Identity{
			ProviderID:   "remote-user-session",
			DisplayName:  "Session User",
			ProviderType: "test-stub-session",
			IsAdmin:      false,
		},
	}
	registry := auth.NewRegistry()
	registry.Register(stubProvider)
	r.authRegistry = registry

	// Login to get a session cookie.
	loginBody := `{"username":"sessionuser","password":"any","provider":"test-stub-session"}`
	loginReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(loginBody))
	loginReq.Header.Set("Content-Type", "application/json")
	loginW := httptest.NewRecorder()
	r.handleLogin(loginW, loginReq)

	if loginW.Code != http.StatusOK {
		t.Fatalf("federated login: expected 200, got %d: %s", loginW.Code, loginW.Body.String())
	}

	var sessionCookie *http.Cookie
	for _, c := range loginW.Result().Cookies() {
		if c.Name == "session" {
			sessionCookie = c
			break
		}
	}
	if sessionCookie == nil {
		t.Fatal("federated login: no session cookie in response")
	}

	// Use the session cookie to access a protected endpoint via Auth middleware.
	meReq := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	meReq.AddCookie(sessionCookie)
	meW := httptest.NewRecorder()
	authMw := middleware.Auth(r.authService)
	authMw(http.HandlerFunc(r.handleMe)).ServeHTTP(meW, meReq)

	if meW.Code != http.StatusOK {
		t.Fatalf("auth/me with federated session: expected 200, got %d: %s", meW.Code, meW.Body.String())
	}
}

// --- Flow 4: Role Enforcement ---

// TestRoleEnforcementFlow_OperatorForbiddenOnAdminRoutes verifies that an
// operator is denied access to all admin-only endpoints.
func TestRoleEnforcementFlow_OperatorForbiddenOnAdminRoutes(t *testing.T) {
	r, authSvc, adminID := testRouterWithAuth(t)
	opID := createOperatorUser(t, authSvc, adminID)

	cases := []struct {
		name    string
		method  string
		path    string
		handler http.HandlerFunc
	}{
		{
			name:    "GET /api/v1/settings",
			method:  http.MethodGet,
			path:    "/api/v1/settings",
			handler: r.handleGetSettings,
		},
		{
			name:   "POST /api/v1/libraries",
			method: http.MethodPost,
			path:   "/api/v1/libraries",
			handler: func(w http.ResponseWriter, req *http.Request) {
				middleware.RequireAdmin(r.handleCreateLibrary)(w, req)
			},
		},
		{
			name:   "PUT /api/v1/rules/{id}",
			method: http.MethodPut,
			path:   "/api/v1/rules/some-rule-id",
			handler: func(w http.ResponseWriter, req *http.Request) {
				middleware.RequireAdmin(r.handleUpdateRule)(w, req)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader("{}"))
			req.Header.Set("Content-Type", "application/json")
			req.SetPathValue("id", "some-rule-id")
			req = withOperatorCtx(req, opID)
			w := httptest.NewRecorder()
			middleware.RequireAdmin(tc.handler)(w, req)

			if w.Code != http.StatusForbidden {
				t.Errorf("operator on %s: expected 403, got %d: %s", tc.name, w.Code, w.Body.String())
			}
		})
	}
}

// TestRoleEnforcementFlow_OperatorAllowedOnOperatorRoutes verifies that an
// operator can access endpoints that do not require admin.
func TestRoleEnforcementFlow_OperatorAllowedOnOperatorRoutes(t *testing.T) {
	r, authSvc, adminID := testRouterWithAuth(t)
	opID := createOperatorUser(t, authSvc, adminID)

	cases := []struct {
		name    string
		method  string
		path    string
		handler http.HandlerFunc
	}{
		{
			name:    "GET /api/v1/rules",
			method:  http.MethodGet,
			path:    "/api/v1/rules",
			handler: r.handleListRules,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req = withOperatorCtx(req, opID)
			w := httptest.NewRecorder()
			tc.handler(w, req)

			if w.Code == http.StatusForbidden {
				t.Errorf("operator on %s: should not get 403, got %d: %s", tc.name, w.Code, w.Body.String())
			}
		})
	}
}

// TestRoleEnforcementFlow_AdminCanAccessAll verifies that an administrator
// can access both admin-only and operator-allowed endpoints.
func TestRoleEnforcementFlow_AdminCanAccessAll(t *testing.T) {
	r, _, adminID := testRouterWithAuth(t)

	cases := []struct {
		name    string
		method  string
		path    string
		handler http.HandlerFunc
	}{
		{
			name:    "GET /api/v1/settings (admin only)",
			method:  http.MethodGet,
			path:    "/api/v1/settings",
			handler: r.handleGetSettings,
		},
		{
			name:    "GET /api/v1/rules (operator allowed)",
			method:  http.MethodGet,
			path:    "/api/v1/rules",
			handler: r.handleListRules,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req = withAdminCtx(req, adminID)
			w := httptest.NewRecorder()
			tc.handler(w, req)

			if w.Code == http.StatusForbidden {
				t.Errorf("admin on %s: should not get 403, got %d: %s", tc.name, w.Code, w.Body.String())
			}
		})
	}
}

// TestRoleEnforcementFlow_AdminCanUpdateRule verifies that an administrator
// can modify rule configuration (enable/disable/automation mode).
func TestRoleEnforcementFlow_AdminCanUpdateRule(t *testing.T) {
	r, _, adminID := testRouterWithAuth(t)

	// Get an existing rule ID from the seeded defaults.
	var ruleID string
	if err := r.db.QueryRow("SELECT id FROM rules LIMIT 1").Scan(&ruleID); err != nil {
		t.Fatalf("no rules found (seed defaults must run): %v", err)
	}

	body := `{"enabled":false}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/rules/"+ruleID, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", ruleID)
	req = withAdminCtx(req, adminID)
	w := httptest.NewRecorder()
	middleware.RequireAdmin(r.handleUpdateRule)(w, req)

	if w.Code == http.StatusForbidden {
		t.Fatalf("admin on PUT /api/v1/rules/{id}: should not get 403, got %d: %s", w.Code, w.Body.String())
	}
}

// TestRoleEnforcementFlow_OperatorCanRunScanner verifies that an operator can
// trigger a scanner run (not admin-gated).
func TestRoleEnforcementFlow_OperatorCanRunScanner(t *testing.T) {
	r, authSvc, adminID := testRouterWithAuth(t)
	opID := createOperatorUser(t, authSvc, adminID)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/scanner/run", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req = withOperatorCtx(req, opID)
	w := httptest.NewRecorder()
	r.handleScannerRun(w, req)

	if w.Code == http.StatusForbidden {
		t.Fatalf("operator on POST /api/v1/scanner/run: should not get 403, got %d: %s", w.Code, w.Body.String())
	}
}
