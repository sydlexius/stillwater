package provider

import "sync"

// Registry holds all registered provider adapters keyed by name.
type Registry struct {
	mu        sync.RWMutex
	providers map[ProviderName]Provider
}

// NewRegistry creates an empty provider registry.
func NewRegistry() *Registry {
	return &Registry{
		providers: make(map[ProviderName]Provider),
	}
}

// Register adds a provider to the registry.
func (r *Registry) Register(p Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers[p.Name()] = p
}

// Get returns a provider by name, or nil if not registered.
func (r *Registry) Get(name ProviderName) Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.providers[name]
}

// All returns all registered providers in a stable order.
func (r *Registry) All() []Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var result []Provider
	for _, name := range AllProviderNames() {
		if p, ok := r.providers[name]; ok {
			result = append(result, p)
		}
	}
	return result
}

// WebSearchRegistry holds registered web image search adapters.
type WebSearchRegistry struct {
	mu        sync.RWMutex
	providers map[ProviderName]WebImageProvider
}

// NewWebSearchRegistry creates an empty web search provider registry.
func NewWebSearchRegistry() *WebSearchRegistry {
	return &WebSearchRegistry{
		providers: make(map[ProviderName]WebImageProvider),
	}
}

// Register adds a web search provider to the registry.
func (r *WebSearchRegistry) Register(p WebImageProvider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers[p.Name()] = p
}

// Get returns a web search provider by name, or nil if not registered.
func (r *WebSearchRegistry) Get(name ProviderName) WebImageProvider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.providers[name]
}

// All returns all registered web search providers in a stable order.
func (r *WebSearchRegistry) All() []WebImageProvider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var result []WebImageProvider
	for _, name := range AllWebSearchProviderNames() {
		if p, ok := r.providers[name]; ok {
			result = append(result, p)
		}
	}
	return result
}
