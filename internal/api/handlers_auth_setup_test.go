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

	"github.com/sydlexius/stillwater/internal/auth"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/database"
	"github.com/sydlexius/stillwater/internal/encryption"
	"github.com/sydlexius/stillwater/internal/nfo"
	"github.com/sydlexius/stillwater/internal/rule"
)

// authSetupTestRouter wires a fully-dependent Router specifically for the
// handleSetup{Local,Federated,WithIdentity} + handleLoginFederated tests.
// Unlike testRouterWithAuth (which seeds an admin) this fixture leaves the
// users table empty so handleSetup can run, and it wires a real
// connection.Service backed by an in-memory Encryptor so we can assert that
// API tokens are persisted as ciphertext.
//
// Helper names are prefixed with "authSetup" to avoid colliding with the
// other M49 W5 agents adding helpers to internal/api/ in parallel worktrees.
func authSetupTestRouter(t *testing.T) (*Router, *encryption.Encryptor) {
	t.Helper()

	dbDir := t.TempDir()
	dbPath := filepath.Join(dbDir, "test.db")
	db, err := database.Open(dbPath)
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	if err := database.Migrate(db); err != nil {
		t.Fatalf("migrating test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	enc, _, err := encryption.NewEncryptor("")
	if err != nil {
		t.Fatalf("creating encryptor: %v", err)
	}

	authSvc := auth.NewService(db)
	connSvc := connection.NewService(db, enc)
	ruleSvc := rule.NewService(db)
	if err := ruleSvc.SeedDefaults(context.Background()); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}
	nfoSnapSvc := nfo.NewSnapshotService(db)

	r := NewRouter(RouterDeps{
		AuthService:        authSvc,
		ConnectionService:  connSvc,
		RuleService:        ruleSvc,
		NFOSnapshotService: nfoSnapSvc,
		DB:                 db,
		Logger:             logger,
		StaticFS:           os.DirFS("../../web/static"),
	})

	return r, enc
}

// authSetupFakeMediaServer returns an httptest.Server that emulates the
// Emby / Jellyfin /Users/AuthenticateByName endpoint. The behavior is
// parameterised so individual tests can drive auth failure, server errors,
// and non-admin responses without copy-pasting handler code.
type authSetupFakeServerOpts struct {
	// statusCode overrides the response status. Defaults to 200.
	statusCode int
	// accessToken is the token returned to the caller. Defaults to "ft-1".
	accessToken string
	// userID is the media-server user ID. Defaults to "remote-uid-1".
	userID string
	// userName is the display name. Defaults to "remoteadmin".
	userName string
	// isAdmin sets Policy.IsAdministrator. Defaults to true (admin) so that
	// happy-path tests work; opt out for the "non-admin" assertion.
	isAdmin *bool
}

func authSetupFakeMediaServer(t *testing.T, opts authSetupFakeServerOpts) *httptest.Server {
	t.Helper()
	if opts.statusCode == 0 {
		opts.statusCode = http.StatusOK
	}
	if opts.accessToken == "" {
		opts.accessToken = "ft-1"
	}
	if opts.userID == "" {
		opts.userID = "remote-uid-1"
	}
	if opts.userName == "" {
		opts.userName = "remoteadmin"
	}
	isAdmin := true
	if opts.isAdmin != nil {
		isAdmin = *opts.isAdmin
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/Users/AuthenticateByName" {
			http.NotFound(w, req)
			return
		}
		// Guard the inbound request shape so a regression in handleSetup
		// (wrong verb, missing Content-Type, malformed body) surfaces here
		// instead of silently passing through. Mirrors the
		// "Mock servers/handlers check request bodies and headers" guideline.
		if req.Method != http.MethodPost {
			http.Error(w, "expected POST", http.StatusMethodNotAllowed)
			return
		}
		if ct := req.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
			// Body kept generic; the failing-test signal is the 400 status,
			// not the response text. Matches the gate's "no raw error in
			// client-visible message" rule.
			http.Error(w, "unexpected Content-Type", http.StatusBadRequest)
			return
		}
		var inbound map[string]any
		if err := json.NewDecoder(req.Body).Decode(&inbound); err != nil {
			http.Error(w, "malformed request body", http.StatusBadRequest)
			return
		}
		if opts.statusCode != http.StatusOK {
			w.WriteHeader(opts.statusCode)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Both Emby and Jellyfin share the same response shape for this endpoint,
		// so a single JSON literal works for either provider type under test.
		body := map[string]any{
			"AccessToken": opts.accessToken,
			"User": map[string]any{
				"Id":   opts.userID,
				"Name": opts.userName,
				"Policy": map[string]any{
					"IsAdministrator": isAdmin,
				},
			},
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// boolPtr is a tiny helper for the optional isAdmin override above.
func boolPtr(b bool) *bool { return &b }

// --- handleSetupLocal ---

func TestHandleSetupLocal_HappyPath(t *testing.T) {
	t.Parallel()
	r, _ := authSetupTestRouter(t)

	body := `{"auth_method":"local","username":"admin","password":"correcthorse"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/setup", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleSetup(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusCreated, w.Body.String())
	}
	if got := w.Header().Get("HX-Redirect"); got == "" {
		t.Error("expected HX-Redirect header to be set")
	}

	// Verify the user row exists and is an admin.
	var role string
	if err := r.db.QueryRow("SELECT role FROM users WHERE username = 'admin'").Scan(&role); err != nil {
		t.Fatalf("admin row not found: %v", err)
	}
	if role != "administrator" {
		t.Errorf("role = %q, want administrator", role)
	}
}

func TestHandleSetupLocal_MissingCredentials(t *testing.T) {
	t.Parallel()
	r, _ := authSetupTestRouter(t)

	cases := []struct {
		name string
		body string
	}{
		{"no username", `{"auth_method":"local","username":"","password":"correcthorse"}`},
		{"no password", `{"auth_method":"local","username":"admin","password":""}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/setup", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			r.handleSetup(w, req)
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400; body: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestHandleSetupLocal_ShortPassword(t *testing.T) {
	t.Parallel()
	r, _ := authSetupTestRouter(t)

	body := `{"auth_method":"local","username":"admin","password":"short"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/setup", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleSetup(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
}

func TestHandleSetupLocal_AlreadyExists(t *testing.T) {
	t.Parallel()
	r, _ := authSetupTestRouter(t)

	// First setup succeeds.
	body := `{"auth_method":"local","username":"admin","password":"correcthorse"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/setup", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.handleSetup(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("first setup: status = %d, want 201", w.Code)
	}

	// Second setup is blocked by handleSetup's HasUsers gate (returns 409).
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/auth/setup", strings.NewReader(body))
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	r.handleSetup(w2, req2)
	if w2.Code != http.StatusConflict {
		t.Errorf("second setup: status = %d, want 409", w2.Code)
	}
}

// TestHandleSetupLocal_DirectInvocation_ConflictPath calls handleSetupLocal
// directly with a pre-seeded admin row so the handler itself returns 409
// (without going through handleSetup's outer HasUsers gate). This is the only
// way to reach handleSetupLocal's "!created" branch.
func TestHandleSetupLocal_DirectInvocation_ConflictPath(t *testing.T) {
	t.Parallel()
	r, _ := authSetupTestRouter(t)

	// Seed an existing admin via the auth service.
	if _, err := r.authService.Setup(context.Background(), "preexisting", "correcthorse"); err != nil {
		t.Fatalf("seeding admin: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/setup", nil)
	w := httptest.NewRecorder()

	r.handleSetupLocal(w, req, "second", "correcthorse")

	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", w.Code)
	}
}

// --- handleSetupFederated ---

func TestHandleSetupFederated_HappyPath_Emby(t *testing.T) {
	t.Parallel()
	r, enc := authSetupTestRouter(t)

	srv := authSetupFakeMediaServer(t, authSetupFakeServerOpts{
		accessToken: "emby-token-supersecret-9999",
		userID:      "emby-uid-1",
		userName:    "embyadmin",
	})

	body := `{"auth_method":"emby","username":"embyadmin","password":"correcthorse","server_url":"` + srv.URL + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/setup", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleSetup(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", w.Code, w.Body.String())
	}

	// Session cookie should be set on auto-login.
	var sessionCookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == "session" {
			sessionCookie = c
		}
	}
	if sessionCookie == nil || sessionCookie.Value == "" {
		t.Error("expected session cookie to be set after federated setup")
	}

	// auth.method and auth.server_url settings must be persisted so the login
	// page picks the right provider on next visit.
	var authMethod string
	if err := r.db.QueryRow("SELECT value FROM settings WHERE key = 'auth.method'").Scan(&authMethod); err != nil {
		t.Fatalf("auth.method setting not stored: %v", err)
	}
	if authMethod != "emby" {
		t.Errorf("auth.method = %q, want emby", authMethod)
	}

	// The auto-created Emby connection must carry the access token encrypted
	// at rest. Read the raw ciphertext column and verify it is NOT plaintext.
	var encryptedKey string
	if err := r.db.QueryRow("SELECT encrypted_api_key FROM connections WHERE type = 'emby'").Scan(&encryptedKey); err != nil {
		t.Fatalf("connection row not created: %v", err)
	}
	if encryptedKey == "" {
		t.Fatal("encrypted_api_key column is empty -- token not persisted")
	}
	if encryptedKey == "emby-token-supersecret-9999" {
		t.Error("encrypted_api_key matches plaintext token -- encryption-at-rest broken")
	}
	if strings.Contains(encryptedKey, "supersecret") {
		t.Errorf("encrypted column leaks plaintext substring: %q", encryptedKey)
	}
	// Sanity-check: feeding the column through the same encryptor should
	// recover the original plaintext, proving it is real ciphertext rather
	// than a different encoding of the secret.
	got, err := enc.Decrypt(encryptedKey)
	if err != nil {
		t.Fatalf("decrypting persisted token: %v", err)
	}
	if got != "emby-token-supersecret-9999" {
		t.Errorf("decrypted token = %q, want emby-token-supersecret-9999", got)
	}
}

func TestHandleSetupFederated_HappyPath_Jellyfin(t *testing.T) {
	t.Parallel()
	r, _ := authSetupTestRouter(t)

	srv := authSetupFakeMediaServer(t, authSetupFakeServerOpts{
		accessToken: "jf-token-xyz",
		userID:      "jf-uid-1",
		userName:    "jfadmin",
	})

	body := `{"auth_method":"jellyfin","username":"jfadmin","password":"correcthorse","server_url":"` + srv.URL + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/setup", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleSetup(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", w.Code, w.Body.String())
	}

	// auth_provider column on the new user row should be "jellyfin".
	var prov string
	if err := r.db.QueryRow("SELECT auth_provider FROM users WHERE provider_id = 'jf-uid-1'").Scan(&prov); err != nil {
		t.Fatalf("jellyfin user row not created: %v", err)
	}
	if prov != "jellyfin" {
		t.Errorf("auth_provider = %q, want jellyfin", prov)
	}
}

func TestHandleSetupFederated_MissingCredentials(t *testing.T) {
	t.Parallel()
	r, _ := authSetupTestRouter(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/setup", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	r.handleSetupFederated(w, req, "emby", "", "", "http://emby.local")
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestHandleSetupFederated_MissingServerURL(t *testing.T) {
	t.Parallel()
	r, _ := authSetupTestRouter(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/setup", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	r.handleSetupFederated(w, req, "emby", "admin", "correcthorse", "")
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestHandleSetupFederated_InvalidServerURL(t *testing.T) {
	t.Parallel()
	r, _ := authSetupTestRouter(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/setup", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	// Scheme "ftp://" is rejected by connection.ValidateBaseURL.
	r.handleSetupFederated(w, req, "emby", "admin", "correcthorse", "ftp://nope")
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestHandleSetupFederated_InvalidCredentials(t *testing.T) {
	t.Parallel()
	r, _ := authSetupTestRouter(t)

	srv := authSetupFakeMediaServer(t, authSetupFakeServerOpts{
		statusCode: http.StatusUnauthorized,
	})

	body := `{"auth_method":"emby","username":"embyadmin","password":"wrong","server_url":"` + srv.URL + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/setup", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleSetup(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401; body: %s", w.Code, w.Body.String())
	}
}

func TestHandleSetupFederated_ServerUnreachable(t *testing.T) {
	t.Parallel()
	r, _ := authSetupTestRouter(t)

	// 127.0.0.1:1 is a reserved port; the connection refusal exercises the
	// 502 BadGateway branch.
	body := `{"auth_method":"emby","username":"embyadmin","password":"correcthorse","server_url":"http://127.0.0.1:1"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/setup", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleSetup(w, req)
	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502; body: %s", w.Code, w.Body.String())
	}
}

// TestHandleSetupFederated_DirectInvocation_Conflict exercises the
// "!created" branch in handleSetupFederated. handleSetup's outer HasUsers gate
// returns 409 before reaching the federated handler, so we must call it
// directly with a pre-seeded users table to hit this branch.
func TestHandleSetupFederated_DirectInvocation_Conflict(t *testing.T) {
	t.Parallel()
	r, _ := authSetupTestRouter(t)

	// Seed an existing federated admin via the auth service so SetupFederated
	// returns (created=false, nil err) on the second call.
	if _, err := r.authService.SetupFederated(context.Background(), auth.FederatedAuthResult{
		AccessToken: "preexisting",
		UserID:      "preexisting-uid",
		UserName:    "preexisting",
		IsAdmin:     true,
	}, "emby"); err != nil {
		t.Fatalf("seeding federated admin: %v", err)
	}

	srv := authSetupFakeMediaServer(t, authSetupFakeServerOpts{})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/setup", strings.NewReader("{}"))
	w := httptest.NewRecorder()
	r.handleSetupFederated(w, req, "emby", "embyadmin", "correcthorse", srv.URL)

	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", w.Code)
	}
}

// TestHandleSetupFederated_AuthSetupReturnsErr drives the inner
// authService.SetupFederated error branch by having the fake media server
// return an empty user Name. The Emby client accepts empty Name (only Id is
// required), but auth.Service.SetupFederated rejects "incomplete federated
// auth result: missing user ID or name", which forces handleSetupFederated
// down the 500 path at line 932.
func TestHandleSetupFederated_AuthSetupReturnsErr(t *testing.T) {
	t.Parallel()
	r, _ := authSetupTestRouter(t)

	// Empty Name passes the Emby client's incomplete-response guard (which
	// only checks ID + AccessToken) but trips auth.Service.SetupFederated's
	// "missing user ID or name" check inside the handler.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/Users/AuthenticateByName" {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"AccessToken":"tok","User":{"Id":"uid-empty-name","Name":"","Policy":{"IsAdministrator":true}}}`))
	}))
	t.Cleanup(srv.Close)

	body := `{"auth_method":"emby","username":"admin","password":"correcthorse","server_url":"` + srv.URL + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/setup", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleSetup(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 (SetupFederated rejects empty Name); body: %s", w.Code, w.Body.String())
	}
}

func TestHandleSetupFederated_NonAdminRejected(t *testing.T) {
	t.Parallel()
	r, _ := authSetupTestRouter(t)

	srv := authSetupFakeMediaServer(t, authSetupFakeServerOpts{
		isAdmin: boolPtr(false),
	})

	body := `{"auth_method":"emby","username":"normaluser","password":"correcthorse","server_url":"` + srv.URL + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/setup", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleSetup(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403; body: %s", w.Code, w.Body.String())
	}
}

// --- handleSetupWithIdentity (registry path) ---

// authSetupStubProvider satisfies auth.Authenticator and returns a fixed
// identity on every call. Mirrors stubFederatedProvider in handlers_login_test.go
// but is duplicated here under a unique name so each W5 test file is
// self-contained and won't fight over symbol names during parallel rebases.
type authSetupStubProvider struct {
	providerType  string
	identity      *auth.Identity
	authErr       error
	autoProvision bool
}

func (p *authSetupStubProvider) Type() string { return p.providerType }

func (p *authSetupStubProvider) Authenticate(_ context.Context, _ auth.Credentials) (*auth.Identity, error) {
	if p.authErr != nil {
		return nil, p.authErr
	}
	return p.identity, nil
}

func (p *authSetupStubProvider) CanAutoProvision(_ *auth.Identity) bool { return p.autoProvision }

func (p *authSetupStubProvider) MapRole(identity *auth.Identity) string {
	if identity.IsAdmin {
		return "administrator"
	}
	return "operator"
}

func TestHandleSetupWithIdentity_HappyPath_Emby(t *testing.T) {
	t.Parallel()
	r, enc := authSetupTestRouter(t)

	provider := &authSetupStubProvider{
		providerType: "emby",
		identity: &auth.Identity{
			ProviderID:   "emby-uid-99",
			DisplayName:  "Emby Boss",
			ProviderType: "emby",
			IsAdmin:      true,
			RawToken:     "registry-emby-token-supersecret-7777",
		},
		autoProvision: true,
	}
	registry := auth.NewRegistry()
	registry.Register(provider)
	r.authRegistry = registry

	// Even though the registry path skips authenticateByName, the connection
	// auto-create still requires a valid server URL (used as connections.url).
	body := `{"auth_method":"emby","username":"any","password":"any","server_url":"http://emby.local:8096"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/setup", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleSetup(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", w.Code, w.Body.String())
	}

	// Session cookie should be set (auto-login).
	var sessionCookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == "session" {
			sessionCookie = c
		}
	}
	if sessionCookie == nil || sessionCookie.Value == "" {
		t.Error("expected session cookie after registry setup")
	}

	// Role is forced to administrator regardless of provider.MapRole.
	var role string
	if err := r.db.QueryRow("SELECT role FROM users WHERE provider_id = 'emby-uid-99'").Scan(&role); err != nil {
		t.Fatalf("user row missing: %v", err)
	}
	if role != "administrator" {
		t.Errorf("role = %q, want administrator (first-user rule)", role)
	}

	// Connection row exists with encrypted RawToken.
	var encryptedKey string
	if err := r.db.QueryRow("SELECT encrypted_api_key FROM connections WHERE type = 'emby'").Scan(&encryptedKey); err != nil {
		t.Fatalf("connection row missing: %v", err)
	}
	if strings.Contains(encryptedKey, "supersecret") {
		t.Error("encrypted_api_key column contains plaintext substring")
	}
	got, err := enc.Decrypt(encryptedKey)
	if err != nil {
		t.Fatalf("decrypt persisted token: %v", err)
	}
	if got != "registry-emby-token-supersecret-7777" {
		t.Errorf("decrypted token = %q, want registry-emby-token-supersecret-7777", got)
	}
}

func TestHandleSetupWithIdentity_NonAdmin(t *testing.T) {
	t.Parallel()
	r, _ := authSetupTestRouter(t)

	provider := &authSetupStubProvider{
		providerType: "emby",
		identity: &auth.Identity{
			ProviderID:   "emby-uid-100",
			DisplayName:  "Regular User",
			ProviderType: "emby",
			IsAdmin:      false, // <-- not admin on the media server
		},
		autoProvision: true,
	}
	registry := auth.NewRegistry()
	registry.Register(provider)
	r.authRegistry = registry

	body := `{"auth_method":"emby","username":"reg","password":"any","server_url":"http://emby.local:8096"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/setup", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleSetup(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403; body: %s", w.Code, w.Body.String())
	}
}

func TestHandleSetupWithIdentity_MissingServerURL(t *testing.T) {
	t.Parallel()
	r, _ := authSetupTestRouter(t)

	provider := &authSetupStubProvider{
		providerType: "emby",
		identity: &auth.Identity{
			ProviderID:   "emby-uid-101",
			ProviderType: "emby",
			IsAdmin:      true,
		},
		autoProvision: true,
	}
	registry := auth.NewRegistry()
	registry.Register(provider)
	r.authRegistry = registry

	body := `{"auth_method":"emby","username":"any","password":"any","server_url":""}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/setup", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleSetup(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
}

func TestHandleSetupWithIdentity_InvalidServerURL(t *testing.T) {
	t.Parallel()
	r, _ := authSetupTestRouter(t)

	provider := &authSetupStubProvider{
		providerType: "emby",
		identity: &auth.Identity{
			ProviderID:   "emby-uid-102",
			ProviderType: "emby",
			IsAdmin:      true,
		},
		autoProvision: true,
	}
	registry := auth.NewRegistry()
	registry.Register(provider)
	r.authRegistry = registry

	body := `{"auth_method":"emby","username":"any","password":"any","server_url":"ftp://nope"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/setup", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleSetup(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
}

// TestHandleSetupWithIdentity_ConnectionCreateFails exercises the
// non-fatal connection.Create error branch. We feed an identity with an empty
// RawToken so Connection.Validate fails (api_key required) inside the service.
// Setup must still complete successfully (201) because the connection auto-
// create is best-effort.
func TestHandleSetupWithIdentity_ConnectionCreateFails(t *testing.T) {
	t.Parallel()
	r, _ := authSetupTestRouter(t)

	identity := &auth.Identity{
		ProviderID:   "emby-uid-no-token",
		DisplayName:  "No Token User",
		ProviderType: "emby",
		IsAdmin:      true,
		RawToken:     "", // <-- triggers connection.Validate "api_key is required"
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/setup", strings.NewReader("{}"))
	w := httptest.NewRecorder()
	r.handleSetupWithIdentity(w, req, identity, "emby", "http://emby.local:8096")

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (connection-create failure must be non-fatal); body: %s", w.Code, w.Body.String())
	}

	// User row was created.
	var role string
	if err := r.db.QueryRow("SELECT role FROM users WHERE provider_id = 'emby-uid-no-token'").Scan(&role); err != nil {
		t.Fatalf("user row missing: %v", err)
	}
	if role != "administrator" {
		t.Errorf("role = %q, want administrator", role)
	}
	// No connection row was created.
	var n int
	if err := r.db.QueryRow("SELECT COUNT(*) FROM connections").Scan(&n); err != nil {
		t.Fatalf("counting connections: %v", err)
	}
	if n != 0 {
		t.Errorf("connections count = %d, want 0 (validation rejected the create)", n)
	}
}

// TestHandleSetupWithIdentity_CreateFederatedUserFails exercises the
// createErr branch in handleSetupWithIdentity. CreateFederatedUser rejects an
// identity with an empty DisplayName, so we feed exactly that to drive the
// failure path. Calling handleSetupWithIdentity directly bypasses handleSetup
// and the registry resolution so we land in the target branch deterministically.
func TestHandleSetupWithIdentity_CreateFederatedUserFails(t *testing.T) {
	t.Parallel()
	r, _ := authSetupTestRouter(t)

	identity := &auth.Identity{
		ProviderID:   "emby-uid-broken",
		DisplayName:  "", // <-- triggers CreateFederatedUser validation error
		ProviderType: "emby",
		IsAdmin:      true,
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/setup", strings.NewReader("{}"))
	w := httptest.NewRecorder()
	r.handleSetupWithIdentity(w, req, identity, "emby", "http://emby.local:8096")

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500; body: %s", w.Code, w.Body.String())
	}
}

// TestHandleSetupWithIdentity_NonMediaProvider exercises the path where the
// requiresServerURL guard is bypassed (e.g. OIDC). No server URL needed, no
// connection row is created, but the user is still seeded as administrator.
func TestHandleSetupWithIdentity_NonMediaProvider(t *testing.T) {
	t.Parallel()
	r, _ := authSetupTestRouter(t)

	provider := &authSetupStubProvider{
		providerType: "oidc",
		identity: &auth.Identity{
			ProviderID:   "oidc-sub-1",
			DisplayName:  "OIDC User",
			ProviderType: "oidc",
			IsAdmin:      false, // OIDC identity does not need IsAdmin to pass setup
		},
		autoProvision: true,
	}
	registry := auth.NewRegistry()
	registry.Register(provider)
	r.authRegistry = registry

	body := `{"auth_method":"oidc","username":"any","password":"any"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/setup", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleSetup(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", w.Code, w.Body.String())
	}

	// First user must still be administrator regardless of IsAdmin.
	var role string
	if err := r.db.QueryRow("SELECT role FROM users WHERE provider_id = 'oidc-sub-1'").Scan(&role); err != nil {
		t.Fatalf("user row missing: %v", err)
	}
	if role != "administrator" {
		t.Errorf("role = %q, want administrator", role)
	}

	// No connection row should be auto-created for non-media providers.
	var n int
	if err := r.db.QueryRow("SELECT COUNT(*) FROM connections").Scan(&n); err != nil {
		t.Fatalf("counting connections: %v", err)
	}
	if n != 0 {
		t.Errorf("connections count = %d, want 0 for non-media provider", n)
	}
}

// --- handleLoginFederated (legacy path) ---

func TestHandleLoginFederated_HappyPath(t *testing.T) {
	t.Parallel()
	r, _ := authSetupTestRouter(t)

	srv := authSetupFakeMediaServer(t, authSetupFakeServerOpts{
		accessToken: "fed-login-token-rotated",
		userID:      "remote-uid-1",
		userName:    "embyadmin",
	})

	// First: complete federated setup so a user exists and auth.server_url is
	// stored. handleLoginFederated reads auth.server_url from settings on every
	// call, so we must persist it before the login attempt.
	setupBody := `{"auth_method":"emby","username":"embyadmin","password":"correcthorse","server_url":"` + srv.URL + `"}`
	setupReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/setup", strings.NewReader(setupBody))
	setupReq.Header.Set("Content-Type", "application/json")
	setupW := httptest.NewRecorder()
	r.handleSetup(setupW, setupReq)
	if setupW.Code != http.StatusCreated {
		t.Fatalf("setup prereq: status = %d, want 201; body: %s", setupW.Code, setupW.Body.String())
	}

	// Now the actual login through the legacy federated path.
	loginReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader("{}"))
	loginW := httptest.NewRecorder()
	r.handleLoginFederated(loginW, loginReq, "embyadmin", "correcthorse", "emby")

	if loginW.Code != http.StatusOK {
		t.Fatalf("login: status = %d, want 200; body: %s", loginW.Code, loginW.Body.String())
	}

	// Session cookie must be set.
	var found bool
	for _, c := range loginW.Result().Cookies() {
		if c.Name == "session" && c.Value != "" {
			found = true
		}
	}
	if !found {
		t.Error("expected session cookie after federated login")
	}

	// Token rotation: the auto-created connection's stored APIKey must have
	// been refreshed to the new access token AND remain encrypted at rest.
	var encryptedKey string
	if err := r.db.QueryRow("SELECT encrypted_api_key FROM connections WHERE type = 'emby'").Scan(&encryptedKey); err != nil {
		t.Fatalf("connection row missing: %v", err)
	}
	if encryptedKey == "fed-login-token-rotated" {
		t.Error("encrypted_api_key column matches plaintext after login -- encryption-at-rest broken")
	}
}

func TestHandleLoginFederated_MissingServerURL(t *testing.T) {
	t.Parallel()
	r, _ := authSetupTestRouter(t)

	// No auth.server_url stored, so the handler must reject the call.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil)
	w := httptest.NewRecorder()
	r.handleLoginFederated(w, req, "embyadmin", "correcthorse", "emby")
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

func TestHandleLoginFederated_InvalidCredentials(t *testing.T) {
	t.Parallel()
	r, _ := authSetupTestRouter(t)

	srv := authSetupFakeMediaServer(t, authSetupFakeServerOpts{
		statusCode: http.StatusUnauthorized,
	})
	// Seed auth.server_url so the handler proceeds to the auth step.
	if _, err := r.db.Exec(`INSERT INTO settings (key, value, updated_at) VALUES ('auth.server_url', ?, datetime('now'))`, srv.URL); err != nil {
		t.Fatalf("seeding auth.server_url: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil)
	w := httptest.NewRecorder()
	r.handleLoginFederated(w, req, "embyadmin", "wrong", "emby")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestHandleLoginFederated_ServerUnreachable(t *testing.T) {
	t.Parallel()
	r, _ := authSetupTestRouter(t)

	// Point at a port nothing is listening on, but seed auth.server_url so
	// the early "URL not configured" guard does not preempt.
	if _, err := r.db.Exec(`INSERT INTO settings (key, value, updated_at) VALUES ('auth.server_url', 'http://127.0.0.1:1', datetime('now'))`); err != nil {
		t.Fatalf("seeding auth.server_url: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil)
	w := httptest.NewRecorder()
	r.handleLoginFederated(w, req, "embyadmin", "correcthorse", "emby")
	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502; body: %s", w.Code, w.Body.String())
	}
}

func TestHandleLoginFederated_UserNotConfigured(t *testing.T) {
	t.Parallel()
	r, _ := authSetupTestRouter(t)

	// Media server authenticates but no local user row exists for that
	// provider_id. handleLoginFederated must respond with 401 + the
	// "not authorized for this Stillwater instance" message.
	srv := authSetupFakeMediaServer(t, authSetupFakeServerOpts{
		userID:   "unknown-remote-uid",
		userName: "ghost",
	})
	if _, err := r.db.Exec(`INSERT INTO settings (key, value, updated_at) VALUES ('auth.server_url', ?, datetime('now'))`, srv.URL); err != nil {
		t.Fatalf("seeding auth.server_url: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil)
	w := httptest.NewRecorder()
	r.handleLoginFederated(w, req, "ghost", "correcthorse", "emby")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401; body: %s", w.Code, w.Body.String())
	}
}
