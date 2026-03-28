package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestEmbyProvider(t *testing.T, serverURL string, autoProvision bool, guardRail string) *EmbyProvider {
	t.Helper()
	p, err := NewEmbyProvider(serverURL, autoProvision, guardRail, "operator")
	if err != nil {
		t.Fatalf("NewEmbyProvider: %v", err)
	}
	return p
}

func TestEmbyProviderAuthenticate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/Users/AuthenticateByName" {
			http.NotFound(w, r)
			return
		}
		resp := map[string]any{
			"AccessToken": "emby-token-123",
			"User": map[string]any{
				"Id":   "emby-user-42",
				"Name": "EmbyAdmin",
				"Policy": map[string]any{
					"IsAdministrator": true,
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Errorf("encoding test response: %v", err)
		}
	}))
	defer server.Close()

	provider := newTestEmbyProvider(t, server.URL, false, "admin")

	identity, err := provider.Authenticate(context.Background(), Credentials{
		Username: "admin",
		Password: "password",
	})
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if identity.ProviderID != "emby-user-42" {
		t.Errorf("ProviderID = %q, want %q", identity.ProviderID, "emby-user-42")
	}
	if identity.DisplayName != "EmbyAdmin" {
		t.Errorf("DisplayName = %q, want %q", identity.DisplayName, "EmbyAdmin")
	}
	if !identity.IsAdmin {
		t.Error("expected IsAdmin = true")
	}
	if identity.RawToken != "emby-token-123" {
		t.Errorf("RawToken = %q, want %q", identity.RawToken, "emby-token-123")
	}
}

func TestEmbyProviderAuthenticate_Unauthorized(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	provider := newTestEmbyProvider(t, server.URL, false, "admin")
	_, err := provider.Authenticate(context.Background(), Credentials{Username: "bad", Password: "creds"})
	if err == nil {
		t.Fatal("expected error for 401 response")
	}
}

func TestEmbyProviderAuthenticate_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	provider := newTestEmbyProvider(t, server.URL, false, "admin")
	_, err := provider.Authenticate(context.Background(), Credentials{Username: "u", Password: "p"})
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}

func TestEmbyProviderType(t *testing.T) {
	provider := newTestEmbyProvider(t, "http://localhost", false, "admin")
	if provider.Type() != "emby" {
		t.Errorf("Type() = %q, want %q", provider.Type(), "emby")
	}
}

func TestEmbyProviderCanAutoProvision(t *testing.T) {
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
			provider := newTestEmbyProvider(t, "http://localhost", true, tt.guardRail)
			identity := &Identity{IsAdmin: tt.isAdmin}
			if got := provider.CanAutoProvision(identity); got != tt.want {
				t.Errorf("CanAutoProvision() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEmbyProviderCanAutoProvision_NilIdentity(t *testing.T) {
	provider := newTestEmbyProvider(t, "http://localhost", true, "any_user")
	if provider.CanAutoProvision(nil) {
		t.Error("expected false for nil identity")
	}
}

func TestEmbyProviderAutoProvisionDisabled(t *testing.T) {
	provider := newTestEmbyProvider(t, "http://localhost", false, "admin")
	identity := &Identity{IsAdmin: true}
	if provider.CanAutoProvision(identity) {
		t.Error("expected false when auto-provision is disabled")
	}
}

func TestEmbyProviderMapRole(t *testing.T) {
	provider := newTestEmbyProvider(t, "http://localhost", true, "admin")

	if got := provider.MapRole(&Identity{IsAdmin: true}); got != "administrator" {
		t.Errorf("MapRole(admin) = %q, want %q", got, "administrator")
	}
	if got := provider.MapRole(&Identity{IsAdmin: false}); got != "operator" {
		t.Errorf("MapRole(non-admin) = %q, want %q", got, "operator")
	}
}

func TestEmbyProviderMapRole_NilIdentity(t *testing.T) {
	provider := newTestEmbyProvider(t, "http://localhost", true, "admin")
	if got := provider.MapRole(nil); got != "operator" {
		t.Errorf("MapRole(nil) = %q, want %q", got, "operator")
	}
}

func TestNewEmbyProvider_InvalidURL(t *testing.T) {
	_, err := NewEmbyProvider("ftp://bad", false, "admin", "operator")
	if err == nil {
		t.Fatal("expected error for invalid URL scheme")
	}
}
