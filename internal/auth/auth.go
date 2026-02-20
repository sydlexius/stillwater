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
