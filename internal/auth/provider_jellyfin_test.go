package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestJellyfinProvider(t *testing.T, serverURL string, autoProvision bool, guardRail string) *JellyfinProvider {
	t.Helper()
	p, err := NewJellyfinProvider(serverURL, autoProvision, guardRail, "operator")
	if err != nil {
		t.Fatalf("NewJellyfinProvider: %v", err)
	}
	return p
}

func TestJellyfinProviderAuthenticate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/Users/AuthenticateByName" {
			http.NotFound(w, r)
			return
		}
		resp := map[string]any{
			"AccessToken": "jf-token-456",
			"User": map[string]any{
				"Id":   "jf-user-99",
				"Name": "JellyfinUser",
				"Policy": map[string]any{
					"IsAdministrator": false,
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Errorf("encoding test response: %v", err)
		}
	}))
	defer server.Close()

	provider := newTestJellyfinProvider(t, server.URL, false, "admin")

	identity, err := provider.Authenticate(context.Background(), Credentials{
		Username: "user",
		Password: "password",
	})
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if identity.ProviderID != "jf-user-99" {
		t.Errorf("ProviderID = %q, want %q", identity.ProviderID, "jf-user-99")
	}
	if identity.DisplayName != "JellyfinUser" {
		t.Errorf("DisplayName = %q, want %q", identity.DisplayName, "JellyfinUser")
	}
	if identity.IsAdmin {
		t.Error("expected IsAdmin = false")
	}
	if identity.RawToken != "jf-token-456" {
		t.Errorf("RawToken = %q, want %q", identity.RawToken, "jf-token-456")
	}
	if identity.ProviderType != "jellyfin" {
		t.Errorf("ProviderType = %q, want %q", identity.ProviderType, "jellyfin")
	}
}

func TestJellyfinProviderAuthenticate_Unauthorized(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	provider := newTestJellyfinProvider(t, server.URL, false, "admin")
	_, err := provider.Authenticate(context.Background(), Credentials{Username: "bad", Password: "creds"})
	if err == nil {
		t.Fatal("expected error for 401 response")
	}
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("expected ErrInvalidCredentials, got: %v", err)
	}
}

func TestJellyfinProviderAuthenticate_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	provider := newTestJellyfinProvider(t, server.URL, false, "admin")
	_, err := provider.Authenticate(context.Background(), Credentials{Username: "u", Password: "p"})
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}

func TestJellyfinProviderCanAutoProvision(t *testing.T) {
	tests := []struct {
		name      string
		guardRail string
		isAdmin   bool
		want      bool
	}{
		{"admin guard, is admin", "admin", true, true},
		{"admin guard, not admin", "admin", false, false},
		{"any_user guard, is admin", "any_user", true, true},
		{"any_user guard, not admin", "any_user", false, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := newTestJellyfinProvider(t, "http://localhost", true, tt.guardRail)
			identity := &Identity{IsAdmin: tt.isAdmin}
			if got := provider.CanAutoProvision(identity); got != tt.want {
				t.Errorf("CanAutoProvision() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestJellyfinProviderCanAutoProvision_NilIdentity(t *testing.T) {
	provider := newTestJellyfinProvider(t, "http://localhost", true, "any_user")
	if provider.CanAutoProvision(nil) {
		t.Error("expected false for nil identity")
	}
}

func TestJellyfinProviderAutoProvisionDisabled(t *testing.T) {
	provider := newTestJellyfinProvider(t, "http://localhost", false, "admin")
	identity := &Identity{IsAdmin: true}
	if provider.CanAutoProvision(identity) {
		t.Error("expected false when auto-provision is disabled")
	}
}

func TestJellyfinProviderMapRole(t *testing.T) {
	provider := newTestJellyfinProvider(t, "http://localhost", true, "admin")

	if got := provider.MapRole(&Identity{IsAdmin: true}); got != "administrator" {
		t.Errorf("MapRole(admin) = %q, want %q", got, "administrator")
	}
	if got := provider.MapRole(&Identity{IsAdmin: false}); got != "operator" {
		t.Errorf("MapRole(non-admin) = %q, want %q", got, "operator")
	}
}

func TestJellyfinProviderMapRole_NilIdentity(t *testing.T) {
	provider := newTestJellyfinProvider(t, "http://localhost", true, "admin")
	if got := provider.MapRole(nil); got != "operator" {
		t.Errorf("MapRole(nil) = %q, want %q", got, "operator")
	}
}

func TestJellyfinProviderType(t *testing.T) {
	provider := newTestJellyfinProvider(t, "http://localhost", false, "admin")
	if provider.Type() != "jellyfin" {
		t.Errorf("Type() = %q, want %q", provider.Type(), "jellyfin")
	}
}

func TestNewJellyfinProvider_InvalidURL(t *testing.T) {
	_, err := NewJellyfinProvider("ftp://bad", false, "admin", "operator")
	if err == nil {
		t.Fatal("expected error for invalid URL scheme")
	}
}
