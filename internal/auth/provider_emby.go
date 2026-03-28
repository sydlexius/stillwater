package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/sydlexius/stillwater/internal/connection"
)

// EmbyProvider authenticates users against an Emby server.
type EmbyProvider struct {
	serverURL     string
	autoProvision bool
	guardRail     string // "admin" or "any_user"
	defaultRole   string // "operator" or "administrator"
	client        *http.Client
}

// NewEmbyProvider creates an Emby authenticator.
// The serverURL is validated using connection.ValidateBaseURL.
func NewEmbyProvider(serverURL string, autoProvision bool, guardRail, defaultRole string) (*EmbyProvider, error) {
	cleaned, err := connection.ValidateBaseURL(serverURL)
	if err != nil {
		return nil, fmt.Errorf("invalid emby server URL: %w", err)
	}
	return &EmbyProvider{
		serverURL:     cleaned,
		autoProvision: autoProvision,
		guardRail:     guardRail,
		defaultRole:   defaultRole,
		client:        &http.Client{Timeout: 10 * time.Second},
	}, nil
}

// Type returns "emby".
func (p *EmbyProvider) Type() string { return "emby" }

// Authenticate validates credentials against the Emby server's AuthenticateByName API.
func (p *EmbyProvider) Authenticate(ctx context.Context, creds Credentials) (*Identity, error) {
	bodyBytes, err := json.Marshal(map[string]string{
		"Username": creds.Username,
		"Pw":       creds.Password,
	})
	if err != nil {
		return nil, fmt.Errorf("encoding emby auth request: %w", err)
	}

	reqURL := connection.BuildRequestURL(p.serverURL, "/Users/AuthenticateByName")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(bodyBytes)) //nolint:gosec // G107: URL validated by connection.ValidateBaseURL in constructor
	if err != nil {
		return nil, fmt.Errorf("creating emby auth request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Emby-Authorization",
		`MediaBrowser Client="Stillwater", Device="Server", DeviceId="stillwater", Version="1.0.0"`)

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling emby auth: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		if resp.StatusCode == http.StatusUnauthorized {
			return nil, fmt.Errorf("emby: %w", ErrInvalidCredentials)
		}
		return nil, fmt.Errorf("emby returned status %d", resp.StatusCode)
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
		return nil, fmt.Errorf("decoding emby response: %w", err)
	}

	if result.User.ID == "" || result.AccessToken == "" {
		return nil, fmt.Errorf("incomplete emby auth response")
	}

	return &Identity{
		ProviderID:   result.User.ID,
		DisplayName:  result.User.Name,
		ProviderType: "emby",
		IsAdmin:      result.User.Policy.IsAdministrator,
		RawToken:     result.AccessToken,
	}, nil
}

// CanAutoProvision checks if the identity meets the configured guard rail.
// Defaults to restrictive (false) on unknown guard rail values.
func (p *EmbyProvider) CanAutoProvision(identity *Identity) bool {
	if identity == nil || !p.autoProvision {
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

// MapRole maps Emby admin status to a Stillwater role.
func (p *EmbyProvider) MapRole(identity *Identity) string {
	if identity == nil {
		return p.defaultRole
	}
	if identity.IsAdmin {
		return "administrator"
	}
	return p.defaultRole
}
