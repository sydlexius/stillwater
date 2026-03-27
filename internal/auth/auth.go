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

// Sentinel errors for token operations.
var (
	// ErrTokenNotFound is returned when an API token does not exist or is already revoked.
	ErrTokenNotFound = errors.New("token not found or already revoked")

	// ErrTokenNotRevoked is returned when an operation requires a revoked token
	// but the token is still active.
	ErrTokenNotRevoked = errors.New("token must be revoked before this operation")

	// ErrTokenActive is returned when trying to delete an active token.
	ErrTokenActive = errors.New("token is still active; revoke it first")
)

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

// TokenStatus represents the lifecycle state of an API token.
type TokenStatus string

// Token lifecycle states: Active -> Revoked -> Deleted.
const (
	TokenStatusActive  TokenStatus = "active"
	TokenStatusRevoked TokenStatus = "revoked"
)

// APIToken represents an API token (without the secret hash).
type APIToken struct {
	ID         string      `json:"id"`
	Name       string      `json:"name"`
	Scopes     string      `json:"scopes"`
	UserID     string      `json:"user_id"`
	Status     TokenStatus `json:"status"`
	CreatedAt  string      `json:"created_at"`
	LastUsedAt *string     `json:"last_used_at,omitempty"`
	RevokedAt  *string     `json:"revoked_at,omitempty"`
}

// AuditEntry represents a single audit log entry for token lifecycle events.
type AuditEntry struct {
	ID        string `json:"id"`
	Action    string `json:"action"`
	TokenID   string `json:"token_id,omitempty"`
	TokenName string `json:"token_name"`
	UserID    string `json:"user_id"`
	Detail    string `json:"detail,omitempty"`
	CreatedAt string `json:"created_at"`
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

	hash, err := bcrypt.GenerateFromPassword(PrehashPassword(password), bcrypt.DefaultCost)
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

	if err := bcrypt.CompareHashAndPassword([]byte(hash), PrehashPassword(password)); err != nil {
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
		INSERT INTO api_tokens (id, name, token_hash, scopes, user_id, created_at, status)
		VALUES (?, ?, ?, ?, ?, ?, 'active')
	`, id, name, tokenHash, scopes, userID, now)
	if err != nil {
		return "", "", fmt.Errorf("inserting api token: %w", err)
	}

	return plaintext, id, nil
}

// ValidateAPIToken checks if an API token is valid and returns the user ID and scopes.
// Only tokens with status "active" are considered valid.
// Updates last_used_at on successful validation.
func (s *Service) ValidateAPIToken(ctx context.Context, token string) (userID string, scopes string, err error) {
	hash := sha256.Sum256([]byte(token))
	tokenHash := hex.EncodeToString(hash[:])

	var status string
	err = s.db.QueryRowContext(ctx, `
		SELECT user_id, scopes, status FROM api_tokens WHERE token_hash = ?
	`, tokenHash).Scan(&userID, &scopes, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", errors.New("invalid api token")
	}
	if err != nil {
		return "", "", fmt.Errorf("querying api token: %w", err)
	}

	if status != string(TokenStatusActive) {
		return "", "", fmt.Errorf("api token is %s", status)
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
		SELECT id, name, scopes, user_id,
		       CASE WHEN status = 'archived' THEN 'revoked' ELSE status END AS status,
		       created_at, last_used_at, revoked_at
		FROM api_tokens WHERE user_id = ?
		ORDER BY created_at DESC
	`, userID)
	if err != nil {
		return nil, fmt.Errorf("listing api tokens: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var tokens []APIToken
	for rows.Next() {
		var t APIToken
		if err := rows.Scan(&t.ID, &t.Name, &t.Scopes, &t.UserID, &t.Status, &t.CreatedAt, &t.LastUsedAt, &t.RevokedAt); err != nil {
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
		UPDATE api_tokens SET revoked_at = ?, status = 'revoked'
		WHERE id = ? AND user_id = ? AND status = 'active'
	`, now, id, userID)
	if err != nil {
		return fmt.Errorf("revoking api token: %w", err)
	}
	n, raErr := result.RowsAffected()
	if raErr != nil {
		return fmt.Errorf("checking rows affected: %w", raErr)
	}
	if n == 0 {
		return ErrTokenNotFound
	}
	return nil
}

// DeleteAPIToken permanently removes a token and anonymizes its audit log entries.
// Only revoked tokens can be deleted. Active tokens return ErrTokenActive.
func (s *Service) DeleteAPIToken(ctx context.Context, id, userID string) error {
	// Check current status.
	var status, name string
	err := s.db.QueryRowContext(ctx, `
		SELECT status, name FROM api_tokens WHERE id = ? AND user_id = ?
	`, id, userID).Scan(&status, &name)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrTokenNotFound
	}
	if err != nil {
		return fmt.Errorf("checking token for delete: %w", err)
	}

	if status == string(TokenStatusActive) {
		return ErrTokenActive
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning delete transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// Anonymize audit log entries that reference this token.
	_, err = tx.ExecContext(ctx, `
		UPDATE audit_log SET token_id = NULL, token_name = '[deleted token]'
		WHERE token_id = ?
	`, id)
	if err != nil {
		return fmt.Errorf("anonymizing audit log: %w", err)
	}

	// Delete the token record.
	delResult, err := tx.ExecContext(ctx, `
		DELETE FROM api_tokens WHERE id = ? AND user_id = ?
	`, id, userID)
	if err != nil {
		return fmt.Errorf("deleting api token: %w", err)
	}
	delN, raErr := delResult.RowsAffected()
	if raErr != nil {
		return fmt.Errorf("checking delete rows affected: %w", raErr)
	}
	if delN == 0 {
		return ErrTokenNotFound
	}

	// Write a final audit entry recording the deletion.
	now := time.Now().UTC().Format(time.RFC3339)
	_, err = tx.ExecContext(ctx, `
		INSERT INTO audit_log (id, action, token_id, token_name, user_id, detail, created_at)
		VALUES (?, 'token_deleted', NULL, ?, ?, 'permanently deleted', ?)
	`, uuid.New().String(), name, userID, now)
	if err != nil {
		return fmt.Errorf("writing deletion audit entry: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing token delete: %w", err)
	}
	return nil
}

// WriteAuditLog records a token lifecycle event in the audit log.
func (s *Service) WriteAuditLog(ctx context.Context, action, tokenID, tokenName, userID, detail string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO audit_log (id, action, token_id, token_name, user_id, detail, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, uuid.New().String(), action, tokenID, tokenName, userID, detail, now)
	if err != nil {
		return fmt.Errorf("writing audit log: %w", err)
	}
	return nil
}

// GetAPIToken returns a single token by ID for the given user.
func (s *Service) GetAPIToken(ctx context.Context, id, userID string) (*APIToken, error) {
	var t APIToken
	err := s.db.QueryRowContext(ctx, `
		SELECT id, name, scopes, user_id, status, created_at, last_used_at, revoked_at
		FROM api_tokens WHERE id = ? AND user_id = ?
	`, id, userID).Scan(&t.ID, &t.Name, &t.Scopes, &t.UserID, &t.Status, &t.CreatedAt, &t.LastUsedAt, &t.RevokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrTokenNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("getting api token: %w", err)
	}
	return &t, nil
}

// PrehashPassword hashes the password with SHA-256 before bcrypt to support
// passwords longer than bcrypt's 72-byte limit. The hex-encoded SHA-256
// digest is 64 bytes, safely within the limit.
func PrehashPassword(password string) []byte {
	h := sha256.Sum256([]byte(password))
	return []byte(hex.EncodeToString(h[:]))
}

// FederatedAuthResult holds the response from a media server's AuthenticateByName API.
type FederatedAuthResult struct {
	AccessToken string // Media server access token (becomes the connection API key)
	UserID      string // Media server user ID
	UserName    string // Display name from media server
	IsAdmin     bool   // Whether the user is an administrator on the media server
}

// ErrUserNotConfigured is returned when a federated user is not registered on this instance.
var ErrUserNotConfigured = errors.New("user not configured on this instance")

// SetupFederated creates the initial admin account from a federated auth response.
// The provider must be "emby" or "jellyfin". Returns true if a new account was created.
// Uses INSERT...WHERE NOT EXISTS for race safety under concurrent requests.
func (s *Service) SetupFederated(ctx context.Context, result FederatedAuthResult, provider string) (bool, error) {
	if result.UserID == "" || result.UserName == "" {
		return false, errors.New("incomplete federated auth result: missing user ID or name")
	}
	if !result.IsAdmin {
		return false, errors.New("federated user is not an administrator")
	}
	if provider != "emby" && provider != "jellyfin" {
		return false, fmt.Errorf("unsupported auth provider: %s", provider)
	}

	id := uuid.New().String()
	execResult, err := s.db.ExecContext(ctx, `
		INSERT INTO users (id, username, password_hash, role, auth_provider, server_user_id)
		SELECT ?, ?, '', 'admin', ?, ?
		WHERE NOT EXISTS (SELECT 1 FROM users)
	`, id, result.UserName, provider, result.UserID)
	if err != nil {
		return false, fmt.Errorf("creating federated admin user: %w", err)
	}

	rows, err := execResult.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("checking setup rows affected: %w", err)
	}
	if rows == 0 {
		return false, nil
	}

	return true, nil
}

// LoginFederated authenticates a federated user and returns a Stillwater session token.
// The caller must have already validated credentials with the media server.
func (s *Service) LoginFederated(ctx context.Context, result FederatedAuthResult, provider string) (string, error) {
	if result.UserID == "" {
		return "", errors.New("missing federated user ID")
	}
	var id, username string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, username FROM users WHERE auth_provider = ? AND server_user_id = ?
	`, provider, result.UserID).Scan(&id, &username)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrUserNotConfigured
	}
	if err != nil {
		return "", fmt.Errorf("querying federated user: %w", err)
	}

	// Sync display name if it changed on the media server (skip if empty).
	if result.UserName != "" && result.UserName != username {
		now := time.Now().UTC().Format(time.RFC3339)
		_, err = s.db.ExecContext(ctx, `
			UPDATE users SET username = ?, updated_at = ? WHERE id = ?
		`, result.UserName, now, id)
		if err != nil {
			return "", fmt.Errorf("syncing federated username: %w", err)
		}
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
		return "", fmt.Errorf("creating federated session: %w", err)
	}

	return token, nil
}

func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
