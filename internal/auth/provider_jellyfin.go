package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// JellyfinProvider authenticates users against a Jellyfin server.
type JellyfinProvider struct {
	serverURL     string
	autoProvision bool
	guardRail     string
	defaultRole   string
	client        *http.Client
}

// NewJellyfinProvider creates a Jellyfin authenticator.
func NewJellyfinProvider(serverURL string, autoProvision bool, guardRail, defaultRole string) *JellyfinProvider {
	return &JellyfinProvider{
		serverURL:     strings.TrimRight(serverURL, "/"),
		autoProvision: autoProvision,
		guardRail:     guardRail,
		defaultRole:   defaultRole,
		client:        &http.Client{Timeout: 10 * time.Second},
	}
}

// Type returns "jellyfin".
func (p *JellyfinProvider) Type() string { return "jellyfin" }

// Authenticate validates credentials against the Jellyfin server.
func (p *JellyfinProvider) Authenticate(ctx context.Context, creds Credentials) (*Identity, error) {
	bodyBytes, err := json.Marshal(map[string]string{
		"Username": creds.Username,
		"Pw":       creds.Password,
	})
	if err != nil {
		return nil, fmt.Errorf("encoding jellyfin auth request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.serverURL+"/Users/AuthenticateByName",
		bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("creating jellyfin auth request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization",
		`MediaBrowser Client="Stillwater", Device="Server", DeviceId="stillwater", Version="1.0.0"`)

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling jellyfin auth: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		if resp.StatusCode == http.StatusUnauthorized {
			return nil, fmt.Errorf("invalid jellyfin credentials")
		}
		return nil, fmt.Errorf("jellyfin returned status %d", resp.StatusCode)
	}

	var result struct {
		AccessToken string `json:"AccessToken"`
		User        struct {
			ID     string `json:"Id"`
			Name   string `json:"Name"`
			Policy struct {
				IsAdministrator bool `json:"IsAdministrator"`
			} `json:"Policy"`
		} `json:"User"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding jellyfin response: %w", err)
	}

	if result.User.ID == "" || result.AccessToken == "" {
		return nil, fmt.Errorf("incomplete jellyfin auth response")
	}

	return &Identity{
		ProviderID:   result.User.ID,
		DisplayName:  result.User.Name,
		ProviderType: "jellyfin",
		IsAdmin:      result.User.Policy.IsAdministrator,
		RawToken:     result.AccessToken,
	}, nil
}

// CanAutoProvision checks if the identity meets the configured guard rail.
// Defaults to restrictive (false) on unknown guard rail values.
func (p *JellyfinProvider) CanAutoProvision(identity *Identity) bool {
	if !p.autoProvision {
		return false
	}
	switch p.guardRail {
	case "admin":
		return identity.IsAdmin
	case "any_user":
		return true
	default:
		return false
	}
}

// MapRole maps Jellyfin admin status to a Stillwater role.
func (p *JellyfinProvider) MapRole(identity *Identity) string {
	if identity.IsAdmin {
		return "administrator"
	}
	return p.defaultRole
}
