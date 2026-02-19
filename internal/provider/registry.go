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
