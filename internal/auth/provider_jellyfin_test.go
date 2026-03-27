package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestJellyfinProviderAuthenticate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/Users/AuthenticateByName" {
			http.NotFound(w, r)
			return
		}
		resp := map[string]interface{}{
			"AccessToken": "jf-token-456",
			"User": map[string]interface{}{
				"Id":   "jf-user-99",
				"Name": "JellyfinUser",
				"Policy": map[string]interface{}{
					"IsAdministrator": false,
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	provider := NewJellyfinProvider(server.URL, false, "admin", "operator")

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
			provider := NewJellyfinProvider("http://unused", true, tt.guardRail, "operator")
			identity := &Identity{IsAdmin: tt.isAdmin}
			if got := provider.CanAutoProvision(identity); got != tt.want {
				t.Errorf("CanAutoProvision() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestJellyfinProviderAutoProvisionDisabled(t *testing.T) {
	provider := NewJellyfinProvider("http://unused", false, "admin", "operator")
	identity := &Identity{IsAdmin: true}
	if provider.CanAutoProvision(identity) {
		t.Error("expected false when auto-provision is disabled")
	}
}

func TestJellyfinProviderMapRole(t *testing.T) {
	provider := NewJellyfinProvider("http://unused", true, "admin", "operator")

	if got := provider.MapRole(&Identity{IsAdmin: true}); got != "administrator" {
		t.Errorf("MapRole(admin) = %q, want %q", got, "administrator")
	}
	if got := provider.MapRole(&Identity{IsAdmin: false}); got != "operator" {
		t.Errorf("MapRole(non-admin) = %q, want %q", got, "operator")
	}
}

func TestJellyfinProviderType(t *testing.T) {
	provider := NewJellyfinProvider("http://unused", false, "admin", "operator")
	if provider.Type() != "jellyfin" {
		t.Errorf("Type() = %q, want %q", provider.Type(), "jellyfin")
	}
}
