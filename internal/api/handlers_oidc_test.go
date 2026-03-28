package api

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-jose/go-jose/v4"
	josejwt "github.com/go-jose/go-jose/v4/jwt"

	"github.com/sydlexius/stillwater/internal/auth"
)

// mockOIDCServer creates a minimal OIDC provider (discovery + JWKS + token endpoint)
// backed by an httptest server. The returned server must be closed by the caller.
func mockOIDCServer(t *testing.T) (*httptest.Server, *rsa.PrivateKey) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating RSA key: %v", err)
	}

	mux := http.NewServeMux()

	// Discovery endpoint.
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		// We need the server URL but cannot reference the variable before it exists.
		// The caller must replace {BASE} in the discovery response. Instead, we read
		// the Host header and reconstruct the base URL.
		scheme := "http"
		base := scheme + "://" + r.Host

		disc := map[string]interface{}{
			"issuer":                                base,
			"authorization_endpoint":                base + "/authorize",
			"token_endpoint":                        base + "/token",
			"jwks_uri":                              base + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(disc); err != nil {
			t.Errorf("encoding discovery: %v", err)
		}
	})

	// JWKS endpoint.
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, r *http.Request) {
		jwk := jose.JSONWebKey{
			Key:       &key.PublicKey,
			KeyID:     "test-key",
			Algorithm: "RS256",
			Use:       "sig",
		}
		jwks := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{jwk}}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(jwks); err != nil {
			t.Errorf("encoding JWKS: %v", err)
		}
	})

	srv := httptest.NewServer(mux)
	return srv, key
}

// signIDToken creates a signed JWT ID token using the test RSA key.
func signIDToken(t *testing.T, key *rsa.PrivateKey, issuer, clientID, sub, nonce string, groups []string) string {
	t.Helper()

	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: key},
		(&jose.SignerOptions{}).WithHeader("kid", "test-key").WithType("JWT"),
	)
	if err != nil {
		t.Fatalf("creating signer: %v", err)
	}

	now := time.Now()
	claims := josejwt.Claims{
		Issuer:    issuer,
		Subject:   sub,
		Audience:  josejwt.Audience{clientID},
		IssuedAt:  josejwt.NewNumericDate(now),
		Expiry:    josejwt.NewNumericDate(now.Add(time.Hour)),
		NotBefore: josejwt.NewNumericDate(now.Add(-time.Minute)),
	}

	extra := map[string]interface{}{
		"preferred_username": "testuser",
		"name":               "Test User",
		"nonce":              nonce,
	}
	if groups != nil {
		extra["groups"] = groups
	}

	raw, err := josejwt.Signed(signer).Claims(claims).Claims(extra).Serialize()
	if err != nil {
		t.Fatalf("signing token: %v", err)
	}
	return raw
}

func TestOIDCProviderInitAndAuthenticate(t *testing.T) {
	srv, key := mockOIDCServer(t)
	defer srv.Close()

	clientID := "test-client"
	nonce := "test-nonce-123"

	// Add token endpoint to the mock server.
	idToken := signIDToken(t, key, srv.URL, clientID, "user-sub-123", nonce, []string{"admins", "users"})

	// Create a new server that wraps the discovery/JWKS server and adds a token endpoint.
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" && r.Method == http.MethodPost {
			resp := map[string]interface{}{
				"access_token": "mock-access-token",
				"token_type":   "Bearer",
				"id_token":     idToken,
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(resp); err != nil {
				t.Errorf("encoding token response: %v", err)
			}
			return
		}
		srv.Config.Handler.ServeHTTP(w, r)
	}))
	defer tokenSrv.Close()
	// Re-sign the ID token with the token server's URL as issuer.
	idToken = signIDToken(t, key, tokenSrv.URL, clientID, "user-sub-123", nonce, []string{"admins", "users"})

	// Update token endpoint handler to use the re-signed token.
	tokenSrv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" && r.Method == http.MethodPost {
			resp := map[string]interface{}{
				"access_token": "mock-access-token",
				"token_type":   "Bearer",
				"id_token":     idToken,
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(resp); err != nil {
				t.Errorf("encoding token response: %v", err)
			}
			return
		}
		// Forward to discovery and JWKS handlers, but replace the issuer URL.
		if r.URL.Path == "/.well-known/openid-configuration" {
			disc := map[string]interface{}{
				"issuer":                                tokenSrv.URL,
				"authorization_endpoint":                tokenSrv.URL + "/authorize",
				"token_endpoint":                        tokenSrv.URL + "/token",
				"jwks_uri":                              tokenSrv.URL + "/jwks",
				"id_token_signing_alg_values_supported": []string{"RS256"},
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(disc); err != nil {
				t.Errorf("encoding discovery: %v", err)
			}
			return
		}
		srv.Config.Handler.ServeHTTP(w, r)
	})

	p := auth.NewOIDCProvider(auth.OIDCConfig{
		IssuerURL:     tokenSrv.URL,
		ClientID:      clientID,
		ClientSecret:  "test-secret",
		RedirectURL:   "http://localhost/callback",
		AdminGroups:   []string{"admins"},
		AutoProvision: true,
	})

	if err := p.Init(t.Context()); err != nil {
		t.Fatalf("Init() error: %v", err)
	}

	// Test Authenticate with a valid code.
	identity, err := p.Authenticate(t.Context(), auth.Credentials{
		Code: "test-auth-code",
		Extra: map[string]string{
			"code_verifier": "test-verifier",
		},
	})
	if err != nil {
		t.Fatalf("Authenticate() error: %v", err)
	}

	if identity.ProviderID != "user-sub-123" {
		t.Errorf("ProviderID = %q, want %q", identity.ProviderID, "user-sub-123")
	}
	if identity.DisplayName != "testuser" {
		t.Errorf("DisplayName = %q, want %q", identity.DisplayName, "testuser")
	}
	if identity.ProviderType != "oidc" {
		t.Errorf("ProviderType = %q, want %q", identity.ProviderType, "oidc")
	}
	if !identity.IsAdmin {
		t.Error("expected IsAdmin to be true for user in admins group")
	}
	if len(identity.Groups) != 2 {
		t.Errorf("Groups len = %d, want 2", len(identity.Groups))
	}
	if identity.Extra["nonce"] != nonce {
		t.Errorf("nonce = %q, want %q", identity.Extra["nonce"], nonce)
	}

	// Test role mapping.
	if role := p.MapRole(identity); role != "administrator" {
		t.Errorf("MapRole() = %q, want %q", role, "administrator")
	}

	// Test auto-provision.
	if !p.CanAutoProvision(identity) {
		t.Error("expected CanAutoProvision to return true")
	}
}

func TestOIDCAuthenticateEmptyCode(t *testing.T) {
	p := auth.NewOIDCProvider(auth.OIDCConfig{
		IssuerURL: "https://example.com",
		ClientID:  "test",
	})

	_, err := p.Authenticate(t.Context(), auth.Credentials{})
	if err == nil {
		t.Error("expected error for empty code")
	}
}

func TestOIDCInitBadIssuer(t *testing.T) {
	p := auth.NewOIDCProvider(auth.OIDCConfig{
		IssuerURL: "https://invalid.example.test.invalid",
		ClientID:  "test",
	})

	err := p.Init(t.Context())
	if err == nil {
		t.Error("expected error for invalid issuer")
	}
}

// TestMockOIDCServerDiscovery verifies the mock OIDC server returns valid discovery.
func TestMockOIDCServerDiscovery(t *testing.T) {
	srv, _ := mockOIDCServer(t)
	defer srv.Close()

	// Verify discovery returns valid JSON with expected fields.
	resp, err := http.Get(srv.URL + "/.well-known/openid-configuration") //nolint:gosec // G107: test URL
	if err != nil {
		t.Fatalf("discovery request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var disc map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&disc); err != nil {
		t.Fatalf("decoding discovery: %v", err)
	}

	if disc["issuer"] != srv.URL {
		t.Errorf("issuer = %q, want %q", disc["issuer"], srv.URL)
	}

	// Verify OIDC library can discover the provider.
	_, err = oidc.NewProvider(t.Context(), srv.URL)
	if err != nil {
		t.Fatalf("oidc.NewProvider failed: %v", err)
	}
}

// Ensure the RSA key and token signing produce valid outputs.
func TestSignIDToken(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}

	token := signIDToken(t, key, "https://issuer.example.com", "client-id", "sub-1", "nonce-1", []string{"group-a"})
	if token == "" {
		t.Error("signed token should not be empty")
	}

	// Token should have 3 parts (header.payload.signature).
	parts := 0
	for i := range token {
		if token[i] == '.' {
			parts++
		}
	}
	if parts != 2 {
		t.Errorf("expected 2 dots in JWT, got %d", parts)
	}
}
