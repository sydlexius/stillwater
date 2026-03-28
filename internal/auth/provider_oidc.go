package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// OIDCProvider authenticates users via OpenID Connect (Authorization Code + PKCE).
// It validates ID tokens from the identity provider and maps group claims to
// Stillwater roles. No ongoing IdP connection is maintained after login; a local
// 24h session is created instead.
type OIDCProvider struct {
	issuerURL    string
	clientID     string
	clientSecret string
	redirectURL  string
	adminGroups  []string // Groups that map to "administrator" role
	userGroups   []string // Groups allowed to log in (empty = any authenticated user)
	defaultRole  string   // Fallback role when no admin group matches
	autoProv     bool     // Whether to auto-provision unknown users

	initialized bool // Set by Init; guards AuthURL and Authenticate
	provider    *oidc.Provider
	oauth2Cfg   oauth2.Config
	verifier    *oidc.IDTokenVerifier
}

// OIDCConfig holds the configuration for creating an OIDC provider.
type OIDCConfig struct {
	IssuerURL     string
	ClientID      string
	ClientSecret  string
	RedirectURL   string
	AdminGroups   []string
	UserGroups    []string
	DefaultRole   string
	AutoProvision bool
}

// NewOIDCProvider creates an OIDC authenticator. Call Init() before use to
// discover the IdP endpoints via .well-known/openid-configuration.
func NewOIDCProvider(cfg OIDCConfig) *OIDCProvider {
	role := cfg.DefaultRole
	if role == "" {
		role = "operator"
	}
	return &OIDCProvider{
		issuerURL:    cfg.IssuerURL,
		clientID:     cfg.ClientID,
		clientSecret: cfg.ClientSecret,
		redirectURL:  cfg.RedirectURL,
		adminGroups:  cfg.AdminGroups,
		userGroups:   cfg.UserGroups,
		defaultRole:  role,
		autoProv:     cfg.AutoProvision,
	}
}

// Type returns "oidc".
func (p *OIDCProvider) Type() string { return "oidc" }

// Init discovers the IdP endpoints and configures the OAuth2 client.
// Must be called once at startup before any authentication attempts.
func (p *OIDCProvider) Init(ctx context.Context) error {
	provider, err := oidc.NewProvider(ctx, p.issuerURL)
	if err != nil {
		return fmt.Errorf("oidc discovery failed for %s: %w", p.issuerURL, err)
	}
	p.provider = provider
	p.verifier = provider.Verifier(&oidc.Config{ClientID: p.clientID})
	p.initialized = true
	// The "groups" scope is not a standard OIDC scope. Many IdPs (Authentik,
	// Keycloak) support it but some (Okta, Azure AD) may ignore it silently,
	// causing admin_groups / user_groups mapping to have no effect. If group
	// mapping is not working, check that the IdP includes the "groups" claim
	// in ID tokens.
	p.oauth2Cfg = oauth2.Config{
		ClientID:     p.clientID,
		ClientSecret: p.clientSecret,
		Endpoint:     provider.Endpoint(),
		RedirectURL:  p.redirectURL,
		Scopes:       []string{oidc.ScopeOpenID, "profile", "groups"},
	}
	return nil
}

// AuthURL generates an authorization URL for the OIDC login flow.
// The state parameter protects against CSRF and the nonce binds the ID token
// to this specific authentication request. PKCE (S256) is used for additional
// security. Returns the authorization URL and the PKCE code verifier that must
// be stored for the callback.
func (p *OIDCProvider) AuthURL(state, nonce string) (authURL, codeVerifier string) {
	if !p.initialized {
		return "", ""
	}
	verifier := generateCodeVerifier()
	challenge := computeS256Challenge(verifier)

	url := p.oauth2Cfg.AuthCodeURL(
		state,
		oauth2.SetAuthURLParam("nonce", nonce),
		oauth2.SetAuthURLParam("code_challenge", challenge),
		oauth2.SetAuthURLParam("code_challenge_method", "S256"),
	)
	return url, verifier
}

// oidcClaims holds the standard claims extracted from an OIDC ID token.
type oidcClaims struct {
	Sub               string   `json:"sub"`
	PreferredUsername string   `json:"preferred_username"`
	Name              string   `json:"name"`
	Groups            []string `json:"groups"`
}

// Authenticate exchanges an authorization code for tokens, verifies the ID token,
// and extracts user identity claims. The Credentials.Code field must contain the
// authorization code and Credentials.State is used for logging only (state
// validation should happen in the HTTP handler before calling this method).
//
// The Extra map on the returned Identity contains:
//   - "nonce": the nonce from the ID token (handler should validate this)
//   - "code_verifier": must be passed via Credentials.Extra["code_verifier"]
func (p *OIDCProvider) Authenticate(ctx context.Context, creds Credentials) (*Identity, error) {
	if !p.initialized {
		return nil, fmt.Errorf("oidc: provider not initialized, call Init() first")
	}
	if creds.Code == "" {
		return nil, fmt.Errorf("oidc: %w: authorization code is required", ErrInvalidCredentials)
	}

	// Build token exchange options. Include PKCE verifier if provided.
	var opts []oauth2.AuthCodeOption
	if v, ok := creds.Extra["code_verifier"]; ok && v != "" {
		opts = append(opts, oauth2.SetAuthURLParam("code_verifier", v))
	}

	oauth2Token, err := p.oauth2Cfg.Exchange(ctx, creds.Code, opts...)
	if err != nil {
		return nil, fmt.Errorf("oidc token exchange: %w", err)
	}

	rawIDToken, ok := oauth2Token.Extra("id_token").(string)
	if !ok {
		return nil, fmt.Errorf("oidc: no id_token in token response")
	}

	idToken, err := p.verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return nil, fmt.Errorf("oidc id_token verification: %w", err)
	}

	var claims oidcClaims
	if err := idToken.Claims(&claims); err != nil {
		return nil, fmt.Errorf("oidc claims extraction: %w", err)
	}

	displayName := claims.PreferredUsername
	if displayName == "" {
		displayName = claims.Name
	}
	if displayName == "" {
		displayName = claims.Sub
	}

	identity := &Identity{
		ProviderID:   claims.Sub,
		DisplayName:  displayName,
		ProviderType: "oidc",
		Groups:       claims.Groups,
		Extra: map[string]string{
			"nonce": idToken.Nonce,
		},
	}

	// Check admin status based on group membership.
	identity.IsAdmin = p.isInAdminGroup(claims.Groups)

	return identity, nil
}

// CanAutoProvision checks whether the identity meets the configured guard rails
// for automatic user creation. If userGroups is empty, any authenticated user is
// allowed. If userGroups is set, the user must be a member of at least one.
func (p *OIDCProvider) CanAutoProvision(identity *Identity) bool {
	if identity == nil || !p.autoProv {
		return false
	}

	// If no user groups are configured, any authenticated user qualifies.
	if len(p.userGroups) == 0 {
		return true
	}

	// Check if the user belongs to at least one allowed group.
	for _, allowed := range p.userGroups {
		for _, g := range identity.Groups {
			if strings.EqualFold(g, allowed) {
				return true
			}
		}
	}

	return false
}

// MapRole determines the Stillwater role based on OIDC group claims.
// If the user is a member of any configured admin group, they get "administrator".
// Otherwise, the default role is returned.
func (p *OIDCProvider) MapRole(identity *Identity) string {
	if identity == nil {
		return p.defaultRole
	}
	if p.isInAdminGroup(identity.Groups) {
		return "administrator"
	}
	return p.defaultRole
}

// isInAdminGroup checks if any of the user's groups match the configured admin groups.
func (p *OIDCProvider) isInAdminGroup(groups []string) bool {
	for _, admin := range p.adminGroups {
		for _, g := range groups {
			if strings.EqualFold(g, admin) {
				return true
			}
		}
	}
	return false
}

// generateCodeVerifier creates a cryptographically random PKCE code verifier
// (43-128 characters, using unreserved URL-safe characters per RFC 7636).
func generateCodeVerifier() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand should never fail on supported platforms; panic is
		// preferable to returning a predictable verifier.
		panic(fmt.Sprintf("crypto/rand.Read failed: %v", err))
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

// computeS256Challenge derives the S256 code challenge from a code verifier
// per RFC 7636: BASE64URL(SHA256(code_verifier)).
func computeS256Challenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

// GenerateNonce creates a cryptographically random nonce for OIDC requests.
func GenerateNonce() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating nonce: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// GenerateState creates a cryptographically random state parameter for OAuth2 CSRF protection.
func GenerateState() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating state: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
