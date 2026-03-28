package auth

import (
	"testing"
)

func TestOIDCProviderType(t *testing.T) {
	p := NewOIDCProvider(OIDCConfig{})
	if got := p.Type(); got != "oidc" {
		t.Errorf("Type() = %q, want %q", got, "oidc")
	}
}

func TestOIDCProviderDefaultRole(t *testing.T) {
	p := NewOIDCProvider(OIDCConfig{})
	if p.defaultRole != "operator" {
		t.Errorf("default role = %q, want %q", p.defaultRole, "operator")
	}

	p2 := NewOIDCProvider(OIDCConfig{DefaultRole: "administrator"})
	if p2.defaultRole != "administrator" {
		t.Errorf("explicit role = %q, want %q", p2.defaultRole, "administrator")
	}
}

func TestOIDCMapRole(t *testing.T) {
	tests := []struct {
		name        string
		adminGroups []string
		groups      []string
		wantRole    string
	}{
		{
			name:        "admin group match",
			adminGroups: []string{"stillwater-admins"},
			groups:      []string{"users", "stillwater-admins"},
			wantRole:    "administrator",
		},
		{
			name:        "admin group case insensitive",
			adminGroups: []string{"Admins"},
			groups:      []string{"admins"},
			wantRole:    "administrator",
		},
		{
			name:        "no admin group match",
			adminGroups: []string{"stillwater-admins"},
			groups:      []string{"users", "developers"},
			wantRole:    "operator",
		},
		{
			name:        "no admin groups configured",
			adminGroups: nil,
			groups:      []string{"anything"},
			wantRole:    "operator",
		},
		{
			name:        "nil identity returns default",
			adminGroups: []string{"admins"},
			groups:      nil,
			wantRole:    "operator",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewOIDCProvider(OIDCConfig{
				AdminGroups: tt.adminGroups,
				DefaultRole: "operator",
			})

			var identity *Identity
			if tt.name != "nil identity returns default" {
				identity = &Identity{Groups: tt.groups}
			}

			got := p.MapRole(identity)
			if got != tt.wantRole {
				t.Errorf("MapRole() = %q, want %q", got, tt.wantRole)
			}
		})
	}
}

func TestOIDCCanAutoProvision(t *testing.T) {
	tests := []struct {
		name       string
		autoProv   bool
		userGroups []string
		identity   *Identity
		want       bool
	}{
		{
			name:     "nil identity",
			autoProv: true,
			identity: nil,
			want:     false,
		},
		{
			name:     "auto provision disabled",
			autoProv: false,
			identity: &Identity{Groups: []string{"users"}},
			want:     false,
		},
		{
			name:       "no user groups restriction",
			autoProv:   true,
			userGroups: nil,
			identity:   &Identity{Groups: []string{"anything"}},
			want:       true,
		},
		{
			name:       "user in allowed group",
			autoProv:   true,
			userGroups: []string{"stillwater-users"},
			identity:   &Identity{Groups: []string{"dev", "stillwater-users"}},
			want:       true,
		},
		{
			name:       "user in allowed group case insensitive",
			autoProv:   true,
			userGroups: []string{"Users"},
			identity:   &Identity{Groups: []string{"users"}},
			want:       true,
		},
		{
			name:       "user not in allowed group",
			autoProv:   true,
			userGroups: []string{"stillwater-users"},
			identity:   &Identity{Groups: []string{"other-app-users"}},
			want:       false,
		},
		{
			name:       "user with no groups and restriction set",
			autoProv:   true,
			userGroups: []string{"stillwater-users"},
			identity:   &Identity{Groups: nil},
			want:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewOIDCProvider(OIDCConfig{
				AutoProvision: tt.autoProv,
				UserGroups:    tt.userGroups,
			})
			got := p.CanAutoProvision(tt.identity)
			if got != tt.want {
				t.Errorf("CanAutoProvision() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestOIDCIsInAdminGroup(t *testing.T) {
	p := NewOIDCProvider(OIDCConfig{
		AdminGroups: []string{"admins", "super-admins"},
	})

	if !p.isInAdminGroup([]string{"admins"}) {
		t.Error("expected admins to match")
	}
	if !p.isInAdminGroup([]string{"other", "super-admins"}) {
		t.Error("expected super-admins to match")
	}
	if p.isInAdminGroup([]string{"users"}) {
		t.Error("expected users not to match")
	}
	if p.isInAdminGroup(nil) {
		t.Error("expected nil groups not to match")
	}
}

func TestGenerateCodeVerifier(t *testing.T) {
	v1 := generateCodeVerifier()
	v2 := generateCodeVerifier()

	if v1 == "" {
		t.Error("code verifier should not be empty")
	}
	if len(v1) < 43 {
		t.Errorf("code verifier too short: %d chars", len(v1))
	}
	if v1 == v2 {
		t.Error("two sequential code verifiers should differ")
	}
}

func TestComputeS256Challenge(t *testing.T) {
	// The challenge must be deterministic for the same verifier.
	verifier := "test-verifier-12345"
	c1 := computeS256Challenge(verifier)
	c2 := computeS256Challenge(verifier)

	if c1 == "" {
		t.Error("challenge should not be empty")
	}
	if c1 != c2 {
		t.Error("same verifier should produce same challenge")
	}

	// Different verifiers produce different challenges.
	c3 := computeS256Challenge("different-verifier")
	if c1 == c3 {
		t.Error("different verifiers should produce different challenges")
	}
}

func TestGenerateState(t *testing.T) {
	s1, err := GenerateState()
	if err != nil {
		t.Fatalf("GenerateState() error: %v", err)
	}
	s2, err := GenerateState()
	if err != nil {
		t.Fatalf("GenerateState() error: %v", err)
	}

	if s1 == "" {
		t.Error("state should not be empty")
	}
	if s1 == s2 {
		t.Error("two sequential states should differ")
	}
}

func TestGenerateNonce(t *testing.T) {
	n1, err := GenerateNonce()
	if err != nil {
		t.Fatalf("GenerateNonce() error: %v", err)
	}
	n2, err := GenerateNonce()
	if err != nil {
		t.Fatalf("GenerateNonce() error: %v", err)
	}

	if n1 == "" {
		t.Error("nonce should not be empty")
	}
	if n1 == n2 {
		t.Error("two sequential nonces should differ")
	}
}

func TestOIDCAuthURLIncludesPKCE(t *testing.T) {
	p := NewOIDCProvider(OIDCConfig{
		IssuerURL:   "https://idp.example.com",
		ClientID:    "test-client",
		RedirectURL: "http://localhost:1973/api/v1/auth/oidc/callback",
	})

	// Manually set a minimal oauth2 config and mark initialized so AuthURL
	// works without a real IdP discovery call.
	p.oauth2Cfg.ClientID = "test-client"
	p.oauth2Cfg.RedirectURL = "http://localhost:1973/api/v1/auth/oidc/callback"
	p.oauth2Cfg.Endpoint.AuthURL = "https://idp.example.com/authorize"
	p.initialized = true

	url, verifier := p.AuthURL("test-state", "test-nonce")

	if verifier == "" {
		t.Error("code verifier should not be empty")
	}

	// The URL should contain PKCE parameters.
	for _, param := range []string{"code_challenge=", "code_challenge_method=S256", "state=test-state", "nonce=test-nonce"} {
		if !contains(url, param) {
			t.Errorf("AuthURL missing %q in %s", param, url)
		}
	}
}

// contains checks if substr is present in s (simple string contains).
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsImpl(s, substr))
}

func containsImpl(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
