package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"golang.org/x/crypto/bcrypt"
)

// LocalProvider authenticates users with local username/password.
type LocalProvider struct {
	db *sql.DB
}

// NewLocalProvider creates a local password authenticator.
// Panics if db is nil (startup-time misconfiguration).
func NewLocalProvider(db *sql.DB) *LocalProvider {
	if db == nil {
		panic("auth: NewLocalProvider called with nil db")
	}
	return &LocalProvider{db: db}
}

// Type returns "local".
func (p *LocalProvider) Type() string { return "local" }

// Authenticate validates a username/password against the local users table.
// Only matches users with auth_provider = 'local' and is_active = 1.
func (p *LocalProvider) Authenticate(ctx context.Context, creds Credentials) (*Identity, error) {
	var id, hash, displayName string
	err := p.db.QueryRowContext(ctx, `
		SELECT id, password_hash, display_name FROM users
		WHERE username = ? AND auth_provider = 'local' AND is_active = 1
	`, creds.Username).Scan(&id, &hash, &displayName)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errors.New("invalid credentials")
	}
	if err != nil {
		return nil, fmt.Errorf("querying user: %w", err)
	}

	if err := bcrypt.CompareHashAndPassword([]byte(hash), PrehashPassword(creds.Password)); err != nil {
		return nil, errors.New("invalid credentials")
	}

	name := displayName
	if name == "" {
		name = creds.Username
	}

	return &Identity{
		ProviderID:   id,
		DisplayName:  name,
		ProviderType: "local",
	}, nil
}

// CanAutoProvision always returns false for local auth.
func (p *LocalProvider) CanAutoProvision(_ *Identity) bool { return false }

// MapRole returns empty string. Local users get their role from invites or admin assignment.
func (p *LocalProvider) MapRole(_ *Identity) string { return "" }
