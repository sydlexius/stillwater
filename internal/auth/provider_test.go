package auth

import (
	"context"
	"testing"
)

func TestRegistryRegisterAndGet(t *testing.T) {
	reg := NewRegistry()
	provider := &stubProvider{providerType: "test"}
	reg.Register(provider)

	got, ok := reg.Get("test")
	if !ok {
		t.Fatal("expected provider to be registered")
	}
	if got.Type() != "test" {
		t.Errorf("Type() = %q, want %q", got.Type(), "test")
	}
}

func TestRegistryGetMissing(t *testing.T) {
	reg := NewRegistry()
	_, ok := reg.Get("nonexistent")
	if ok {
		t.Fatal("expected provider not found")
	}
}

func TestRegistryEnabled(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&stubProvider{providerType: "a"})
	reg.Register(&stubProvider{providerType: "b"})

	enabled := reg.Enabled()
	if len(enabled) != 2 {
		t.Fatalf("Enabled() returned %d providers, want 2", len(enabled))
	}
}

type stubProvider struct {
	providerType string
}

func (s *stubProvider) Type() string { return s.providerType }
func (s *stubProvider) Authenticate(_ context.Context, _ Credentials) (*Identity, error) {
	return &Identity{ProviderID: "stub-id", DisplayName: "Stub User", ProviderType: s.providerType}, nil
}
func (s *stubProvider) CanAutoProvision(_ *Identity) bool { return false }
func (s *stubProvider) MapRole(_ *Identity) string        { return "" }
