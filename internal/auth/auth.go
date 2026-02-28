package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

const sessionDuration = 24 * time.Hour

// APITokenPrefix identifies Stillwater API tokens.
const APITokenPrefix = "sw_"

// TokenScope defines a permission scope for API tokens.
type TokenScope string

// Known token scopes.
const (
	ScopeRead    TokenScope = "read"
	ScopeWrite   TokenScope = "write"
	ScopeWebhook TokenScope = "webhook"
	ScopeAdmin   TokenScope = "admin"
)

// ValidScopes contains all valid token scope values.
var ValidScopes = map[TokenScope]bool{
	ScopeRead:    true,
	ScopeWrite:   true,
	ScopeWebhook: true,
	ScopeAdmin:   true,
}

// APIToken represents an API token (without the secret hash).
type APIToken struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	Scopes     string  `json:"scopes"`
	UserID     string  `json:"user_id"`
	CreatedAt  string  `json:"created_at"`
	LastUsedAt *string `json:"last_used_at,omitempty"`
	RevokedAt  *string `json:"revoked_at,omitempty"`
}

// Service provides authentication operations.
type Service struct {
	db *sql.DB
}

// NewService creates an auth service.
func NewService(db *sql.DB) *Service {
	return &Service{db: db}
}

// Setup creates the initial admin account if no users exist.
// Returns true if a new account was created.
func (s *Service) Setup(ctx context.Context, username, password string) (bool, error) {
	var count int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM users").Scan(&count); err != nil {
		return false, fmt.Errorf("counting users: %w", err)
	}

	if count > 0 {
		return false, nil
	}

	hash, err := bcrypt.GenerateFromPassword(prehashPassword(password), bcrypt.DefaultCost)
	if err != nil {
		return false, fmt.Errorf("hashing password: %w", err)
	}

	id := uuid.New().String()
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO users (id, username, password_hash, role)
		VALUES (?, ?, ?, 'admin')
	`, id, username, string(hash))
	if err != nil {
		return false, fmt.Errorf("creating admin user: %w", err)
	}

	return true, nil
}

// Login authenticates a user and returns a session token.
func (s *Service) Login(ctx context.Context, username, password string) (string, error) {
	var id, hash string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, password_hash FROM users WHERE username = ?
	`, username).Scan(&id, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errors.New("invalid credentials")
	}
	if err != nil {
		return "", fmt.Errorf("querying user: %w", err)
	}

	if err := bcrypt.CompareHashAndPassword([]byte(hash), prehashPassword(password)); err != nil {
		return "", errors.New("invalid credentials")
	}

	token, err := generateToken()
	if err != nil {
		return "", fmt.Errorf("generating session token: %w", err)
	}

	expiresAt := time.Now().Add(sessionDuration).UTC().Format(time.RFC3339)
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO sessions (id, user_id, expires_at)
		VALUES (?, ?, ?)
	`, token, id, expiresAt)
	if err != nil {
		return "", fmt.Errorf("creating session: %w", err)
	}

	return token, nil
}

// ValidateSession checks if a session token is valid and returns the user ID.
func (s *Service) ValidateSession(ctx context.Context, token string) (string, error) {
	var userID, expiresAt string
	err := s.db.QueryRowContext(ctx, `
		SELECT user_id, expires_at FROM sessions WHERE id = ?
	`, token).Scan(&userID, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errors.New("invalid session")
	}
	if err != nil {
		return "", fmt.Errorf("querying session: %w", err)
	}

	expires, err := time.Parse(time.RFC3339, expiresAt)
	if err != nil {
		return "", fmt.Errorf("parsing expiry: %w", err)
	}

	if time.Now().UTC().After(expires) {
		_ = s.Logout(ctx, token)
		return "", errors.New("session expired")
	}

	return userID, nil
}

// Logout deletes a session.
func (s *Service) Logout(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM sessions WHERE id = ?", token)
	return err
}

// CleanExpiredSessions removes all expired sessions.
func (s *Service) CleanExpiredSessions(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM sessions WHERE expires_at < ?
	`, time.Now().UTC().Format(time.RFC3339))
	return err
}

// HasUsers returns true if at least one user account exists.
func (s *Service) HasUsers(ctx context.Context) (bool, error) {
	var count int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM users").Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

// CreateAPIToken generates a new API token with the given scopes.
// Returns the plaintext token (shown once) and the token ID.
func (s *Service) CreateAPIToken(ctx context.Context, userID, name string, scopes string) (plaintext, id string, err error) {
	raw, err := generateToken()
	if err != nil {
		return "", "", fmt.Errorf("generating api token: %w", err)
	}
	plaintext = APITokenPrefix + raw

	hash := sha256.Sum256([]byte(plaintext))
	tokenHash := hex.EncodeToString(hash[:])

	id = uuid.New().String()
	now := time.Now().UTC().Format(time.RFC3339)

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO api_tokens (id, name, token_hash, scopes, user_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, id, name, tokenHash, scopes, userID, now)
	if err != nil {
		return "", "", fmt.Errorf("inserting api token: %w", err)
	}

	return plaintext, id, nil
}

// ValidateAPIToken checks if an API token is valid and returns the user ID and scopes.
// Updates last_used_at asynchronously.
func (s *Service) ValidateAPIToken(ctx context.Context, token string) (userID string, scopes string, err error) {
	hash := sha256.Sum256([]byte(token))
	tokenHash := hex.EncodeToString(hash[:])

	var revokedAt sql.NullString
	err = s.db.QueryRowContext(ctx, `
		SELECT user_id, scopes, revoked_at FROM api_tokens WHERE token_hash = ?
	`, tokenHash).Scan(&userID, &scopes, &revokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", errors.New("invalid api token")
	}
	if err != nil {
		return "", "", fmt.Errorf("querying api token: %w", err)
	}

	if revokedAt.Valid {
		return "", "", errors.New("api token revoked")
	}

	// Best-effort update of last_used_at using the caller's context.
	now := time.Now().UTC().Format(time.RFC3339)
	_, _ = s.db.ExecContext(ctx, //nolint:gosec // G701: static query
		`UPDATE api_tokens SET last_used_at = ? WHERE token_hash = ?`, now, tokenHash)

	return userID, scopes, nil
}

// ListAPITokens returns all tokens for a user (never exposes the hash).
func (s *Service) ListAPITokens(ctx context.Context, userID string) ([]APIToken, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, scopes, user_id, created_at, last_used_at, revoked_at
		FROM api_tokens WHERE user_id = ? ORDER BY created_at DESC
	`, userID)
	if err != nil {
		return nil, fmt.Errorf("listing api tokens: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var tokens []APIToken
	for rows.Next() {
		var t APIToken
		if err := rows.Scan(&t.ID, &t.Name, &t.Scopes, &t.UserID, &t.CreatedAt, &t.LastUsedAt, &t.RevokedAt); err != nil {
			return nil, fmt.Errorf("scanning api token: %w", err)
		}
		tokens = append(tokens, t)
	}
	return tokens, rows.Err()
}

// RevokeAPIToken marks a token as revoked.
func (s *Service) RevokeAPIToken(ctx context.Context, id, userID string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	result, err := s.db.ExecContext(ctx, `
		UPDATE api_tokens SET revoked_at = ? WHERE id = ? AND user_id = ? AND revoked_at IS NULL
	`, now, id, userID)
	if err != nil {
		return fmt.Errorf("revoking api token: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return errors.New("token not found or already revoked")
	}
	return nil
}

// prehashPassword hashes the password with SHA-256 before bcrypt to support
// passwords longer than bcrypt's 72-byte limit. The hex-encoded SHA-256
// digest is 64 bytes, safely within the limit.
func prehashPassword(password string) []byte {
	h := sha256.Sum256([]byte(password))
	return []byte(hex.EncodeToString(h[:]))
}

func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
