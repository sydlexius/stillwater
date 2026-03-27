package auth

import "context"

// Authenticator handles authentication for a specific provider type.
type Authenticator interface {
	// Type returns the provider identifier ("local", "emby", "jellyfin", "oidc").
	Type() string

	// Authenticate validates credentials and returns a provider-specific identity.
	Authenticate(ctx context.Context, creds Credentials) (*Identity, error)

	// CanAutoProvision checks whether a given identity meets the guard rails
	// configured for this provider.
	CanAutoProvision(identity *Identity) bool

	// MapRole determines the role for a user based on provider-specific signals.
	// Returns empty string if no mapping applies (falls back to configured default).
	MapRole(identity *Identity) string
}

// Credentials carries authentication input from the user.
type Credentials struct {
	Username string // For local, Emby, Jellyfin
	Password string // For local, Emby, Jellyfin
	Code     string // OIDC authorization code
	State    string // OIDC state parameter
}

// Identity is the result of a successful authentication.
type Identity struct {
	ProviderID   string            // Stable user ID from the provider
	DisplayName  string            // Human-readable name
	ProviderType string            // "local", "emby", "jellyfin", "oidc"
	IsAdmin      bool              // Whether user is admin on the provider
	Groups       []string          // OIDC group claims
	RawToken     string            // Provider access token (for Emby/Jellyfin connection sync)
	Extra        map[string]string // Provider-specific metadata
}

// Registry holds enabled authentication providers.
type Registry struct {
	providers map[string]Authenticator
}

// NewRegistry creates an empty provider registry.
func NewRegistry() *Registry {
	return &Registry{providers: make(map[string]Authenticator)}
}

// Register adds a provider to the registry, keyed by its Type().
func (r *Registry) Register(p Authenticator) {
	r.providers[p.Type()] = p
}

// Get returns a provider by type. Returns false if not registered.
func (r *Registry) Get(providerType string) (Authenticator, bool) {
	p, ok := r.providers[providerType]
	return p, ok
}

// Enabled returns all registered providers.
func (r *Registry) Enabled() []Authenticator {
	result := make([]Authenticator, 0, len(r.providers))
	for _, p := range r.providers {
		result = append(result, p)
	}
	return result
}
