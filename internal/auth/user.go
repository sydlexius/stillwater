package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

// ErrLastAdmin is returned when an operation would remove or downgrade
// the last active administrator account.
var ErrLastAdmin = errors.New("cannot remove or downgrade the last administrator")

// User represents a Stillwater user account.
type User struct {
	ID           string  `json:"id"`
	Username     string  `json:"username"`
	DisplayName  string  `json:"display_name"`
	Role         string  `json:"role"`
	AuthProvider string  `json:"auth_provider"`
	ProviderID   string  `json:"provider_id,omitempty"`
	IsActive     bool    `json:"is_active"`
	InvitedBy    *string `json:"invited_by,omitempty"`
	CreatedAt    string  `json:"created_at"`
	UpdatedAt    string  `json:"updated_at"`
}

// GetUserByID returns a user by their ID. Returns an error wrapping
// sql.ErrNoRows if the user does not exist.
func (s *Service) GetUserByID(ctx context.Context, id string) (*User, error) {
	var u User
	var invitedBy sql.NullString
	var providerID sql.NullString

	err := s.db.QueryRowContext(ctx, `
		SELECT id, username, display_name, role, auth_provider, provider_id,
		       is_active, invited_by, created_at, updated_at
		FROM users WHERE id = ?
	`, id).Scan(
		&u.ID, &u.Username, &u.DisplayName, &u.Role, &u.AuthProvider,
		&providerID, &u.IsActive, &invitedBy, &u.CreatedAt, &u.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("user not found: %w", sql.ErrNoRows)
	}
	if err != nil {
		return nil, fmt.Errorf("getting user by id: %w", err)
	}

	if providerID.Valid {
		u.ProviderID = providerID.String
	}
	if invitedBy.Valid {
		u.InvitedBy = &invitedBy.String
	}

	return &u, nil
}

// GetUserRole returns the role for an active user. Returns an empty string
// if the user is not found or is inactive.
func (s *Service) GetUserRole(ctx context.Context, userID string) (string, error) {
	var role string
	err := s.db.QueryRowContext(ctx, `
		SELECT role FROM users WHERE id = ? AND is_active = 1
	`, userID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("getting user role: %w", err)
	}
	return role, nil
}

// ListUsers returns all users ordered by created_at ascending.
func (s *Service) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, username, display_name, role, auth_provider, provider_id,
		       is_active, invited_by, created_at, updated_at
		FROM users ORDER BY created_at ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("listing users: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var users []User
	for rows.Next() {
		var u User
		var invitedBy sql.NullString
		var providerID sql.NullString

		if err := rows.Scan(
			&u.ID, &u.Username, &u.DisplayName, &u.Role, &u.AuthProvider,
			&providerID, &u.IsActive, &invitedBy, &u.CreatedAt, &u.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scanning user: %w", err)
		}

		if providerID.Valid {
			u.ProviderID = providerID.String
		}
		if invitedBy.Valid {
			u.InvitedBy = &invitedBy.String
		}

		users = append(users, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing users: %w", err)
	}
	return users, nil
}

// UpdateUserRole changes a user's role. Valid roles are "administrator" and
// "operator". Returns ErrLastAdmin if downgrading the last active administrator.
// The admin-count check and role update run inside a BEGIN IMMEDIATE transaction
// to prevent concurrent downgrades from racing past the last-admin safeguard.
func (s *Service) UpdateUserRole(ctx context.Context, userID, newRole string) error {
	if newRole != "administrator" && newRole != "operator" {
		return fmt.Errorf("invalid role %q: must be administrator or operator", newRole)
	}

	return s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		if newRole == "operator" {
			var currentRole string
			err := conn.QueryRowContext(ctx, `
				SELECT role FROM users WHERE id = ? AND is_active = 1
			`, userID).Scan(&currentRole)
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("user not found: %w", sql.ErrNoRows)
			}
			if err != nil {
				return fmt.Errorf("checking current role: %w", err)
			}

			if currentRole == "administrator" {
				var count int
				err := conn.QueryRowContext(ctx, `
					SELECT COUNT(*) FROM users WHERE role = 'administrator' AND is_active = 1
				`).Scan(&count)
				if err != nil {
					return fmt.Errorf("counting active admins: %w", err)
				}
				if count <= 1 {
					return ErrLastAdmin
				}
			}
		}

		now := time.Now().UTC().Format(time.RFC3339)
		result, err := conn.ExecContext(ctx, `
			UPDATE users SET role = ?, updated_at = ? WHERE id = ?
		`, newRole, now, userID)
		if err != nil {
			return fmt.Errorf("updating user role: %w", err)
		}

		n, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("checking rows affected: %w", err)
		}
		if n == 0 {
			return fmt.Errorf("user not found: %w", sql.ErrNoRows)
		}

		return nil
	})
}

// DeactivateUser sets a user's is_active flag to 0 and deletes all their
// sessions. Returns ErrLastAdmin if deactivating the last active administrator.
// The admin-count check and deactivation run inside a BEGIN IMMEDIATE transaction
// to prevent concurrent deactivations from racing past the last-admin safeguard.
func (s *Service) DeactivateUser(ctx context.Context, userID string) error {
	return s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		var role string
		err := conn.QueryRowContext(ctx, `
			SELECT role FROM users WHERE id = ? AND is_active = 1
		`, userID).Scan(&role)
		if errors.Is(err, sql.ErrNoRows) {
			// Already inactive or does not exist -- nothing to do.
			return nil
		}
		if err != nil {
			return fmt.Errorf("checking user for deactivation: %w", err)
		}

		if role == "administrator" {
			var count int
			err := conn.QueryRowContext(ctx, `
				SELECT COUNT(*) FROM users WHERE role = 'administrator' AND is_active = 1
			`).Scan(&count)
			if err != nil {
				return fmt.Errorf("counting active admins: %w", err)
			}
			if count <= 1 {
				return ErrLastAdmin
			}
		}

		now := time.Now().UTC().Format(time.RFC3339)
		_, err = conn.ExecContext(ctx, `
			UPDATE users SET is_active = 0, updated_at = ? WHERE id = ?
		`, now, userID)
		if err != nil {
			return fmt.Errorf("deactivating user: %w", err)
		}

		_, err = conn.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID)
		if err != nil {
			return fmt.Errorf("deleting sessions for deactivated user: %w", err)
		}

		return nil
	})
}

// CreateLocalUser creates a new user with local password authentication.
// The password is bcrypt-hashed via PrehashPassword before storage.
func (s *Service) CreateLocalUser(ctx context.Context, username, password, displayName, role, invitedBy string) (*User, error) {
	if role != "administrator" && role != "operator" {
		return nil, fmt.Errorf("invalid role %q: must be administrator or operator", role)
	}

	hash, err := bcrypt.GenerateFromPassword(PrehashPassword(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("hashing password: %w", err)
	}

	id := uuid.New().String()
	now := time.Now().UTC().Format(time.RFC3339)

	var invitedByVal sql.NullString
	if invitedBy != "" {
		invitedByVal = sql.NullString{String: invitedBy, Valid: true}
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO users (id, username, display_name, password_hash, role, auth_provider, provider_id,
		                   is_active, invited_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, 'local', '', 1, ?, ?, ?)
	`, id, username, displayName, string(hash), role, invitedByVal, now, now)
	if err != nil {
		return nil, fmt.Errorf("creating local user: %w", err)
	}

	return s.GetUserByID(ctx, id)
}

// CreateFederatedUser creates a new user from a federated authentication identity.
func (s *Service) CreateFederatedUser(ctx context.Context, identity *Identity, role, invitedBy string) (*User, error) {
	if identity == nil {
		return nil, fmt.Errorf("identity must not be nil")
	}
	if identity.ProviderType == "" {
		return nil, fmt.Errorf("identity provider type must not be empty")
	}
	if identity.ProviderID == "" {
		return nil, fmt.Errorf("identity provider ID must not be empty")
	}
	if identity.DisplayName == "" {
		return nil, fmt.Errorf("identity display name must not be empty")
	}
	if role != "administrator" && role != "operator" {
		return nil, fmt.Errorf("invalid role %q: must be administrator or operator", role)
	}

	id := uuid.New().String()
	now := time.Now().UTC().Format(time.RFC3339)

	var invitedByVal sql.NullString
	if invitedBy != "" {
		invitedByVal = sql.NullString{String: invitedBy, Valid: true}
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO users (id, username, display_name, password_hash, role, auth_provider, provider_id,
		                   is_active, invited_by, created_at, updated_at)
		VALUES (?, ?, ?, '', ?, ?, ?, 1, ?, ?, ?)
	`, id, identity.DisplayName, identity.DisplayName, role, identity.ProviderType, identity.ProviderID,
		invitedByVal, now, now)
	if err != nil {
		return nil, fmt.Errorf("creating federated user: %w", err)
	}

	return s.GetUserByID(ctx, id)
}

// withImmediateTx executes fn within a BEGIN IMMEDIATE transaction, which
// acquires the SQLite write lock at the start of the transaction rather than
// deferring it to the first write. This prevents check-then-act races where
// concurrent transactions read stale data before their respective writes.
func (s *Service) withImmediateTx(ctx context.Context, fn func(conn *sql.Conn) error) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquiring connection: %w", err)
	}
	defer conn.Close() //nolint:errcheck

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("beginning immediate transaction: %w", err)
	}

	if err := fn(conn); err != nil {
		conn.ExecContext(ctx, "ROLLBACK") //nolint:errcheck
		return err
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		conn.ExecContext(ctx, "ROLLBACK") //nolint:errcheck
		return fmt.Errorf("committing transaction: %w", err)
	}

	return nil
}
