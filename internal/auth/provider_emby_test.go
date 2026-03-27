package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEmbyProviderAuthenticate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/Users/AuthenticateByName" {
			http.NotFound(w, r)
			return
		}
		resp := map[string]interface{}{
			"AccessToken": "emby-token-123",
			"User": map[string]interface{}{
				"Id":   "emby-user-42",
				"Name": "EmbyAdmin",
				"Policy": map[string]interface{}{
					"IsAdministrator": true,
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	provider := NewEmbyProvider(server.URL, false, "admin", "operator")

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
			provider := NewEmbyProvider("http://unused", true, tt.guardRail, "operator")
			identity := &Identity{IsAdmin: tt.isAdmin}
			if got := provider.CanAutoProvision(identity); got != tt.want {
				t.Errorf("CanAutoProvision() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEmbyProviderAutoProvisionDisabled(t *testing.T) {
	provider := NewEmbyProvider("http://unused", false, "admin", "operator")
	identity := &Identity{IsAdmin: true}
	if provider.CanAutoProvision(identity) {
		t.Error("expected false when auto-provision is disabled")
	}
}

func TestEmbyProviderMapRole(t *testing.T) {
	provider := NewEmbyProvider("http://unused", true, "admin", "operator")

	if got := provider.MapRole(&Identity{IsAdmin: true}); got != "administrator" {
		t.Errorf("MapRole(admin) = %q, want %q", got, "administrator")
	}
	if got := provider.MapRole(&Identity{IsAdmin: false}); got != "operator" {
		t.Errorf("MapRole(non-admin) = %q, want %q", got, "operator")
	}
}
