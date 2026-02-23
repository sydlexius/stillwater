package provider

import (
	"context"
	"testing"
)

// mockWebImageProvider is a minimal WebImageProvider for testing.
type mockWebImageProvider struct {
	name ProviderName
}

func (m *mockWebImageProvider) Name() ProviderName { return m.name }
func (m *mockWebImageProvider) RequiresAuth() bool { return false }
func (m *mockWebImageProvider) SearchImages(_ context.Context, _ string, _ ImageType) ([]ImageResult, error) {
	return nil, nil
}

func TestWebSearchRegistryRegisterAndGet(t *testing.T) {
	reg := NewWebSearchRegistry()

	ddg := &mockWebImageProvider{name: NameDuckDuckGo}
	reg.Register(ddg)

	got := reg.Get(NameDuckDuckGo)
	if got == nil {
		t.Fatal("expected to get duckduckgo provider")
	}
	if got.Name() != NameDuckDuckGo {
		t.Errorf("expected name duckduckgo, got %s", got.Name())
	}
}

func TestWebSearchRegistryGetUnknown(t *testing.T) {
	reg := NewWebSearchRegistry()

	got := reg.Get(ProviderName("nonexistent"))
	if got != nil {
		t.Errorf("expected nil for unregistered provider, got %v", got)
	}
}

func TestWebSearchRegistryAll(t *testing.T) {
	reg := NewWebSearchRegistry()

	ddg := &mockWebImageProvider{name: NameDuckDuckGo}
	reg.Register(ddg)

	all := reg.All()
	if len(all) != 1 {
		t.Fatalf("expected 1 provider, got %d", len(all))
	}
	if all[0].Name() != NameDuckDuckGo {
		t.Errorf("expected duckduckgo, got %s", all[0].Name())
	}
}

func TestWebSearchRegistryAllEmpty(t *testing.T) {
	reg := NewWebSearchRegistry()

	all := reg.All()
	if len(all) != 0 {
		t.Errorf("expected 0 providers, got %d", len(all))
	}
}
