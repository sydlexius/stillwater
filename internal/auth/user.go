package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

// ErrLastAdmin is returned when an operation would remove or downgrade
// the last active administrator account.
var ErrLastAdmin = errors.New("cannot remove or downgrade the last administrator")

// ErrProtectedUser is returned when an operation attempts to deactivate or
// change the role of the bootstrap administrator account, which is permanently
// protected from modification.
var ErrProtectedUser = errors.New("cannot modify or deactivate the protected bootstrap administrator")

// ErrSelfDelete is returned when an administrator attempts to permanently
// delete their own user account from the admin Users panel; that surface is
// for cleaning up OTHER accounts. Self-account changes route through the
// Account Settings flow instead.
var ErrSelfDelete = errors.New("cannot delete your own account from the admin users panel")

// User represents a Stillwater user account.
type User struct {
	ID           string  `json:"id"`
	Username     string  `json:"username"`
	DisplayName  string  `json:"display_name"`
	Role         string  `json:"role"`
	AuthProvider string  `json:"auth_provider"`
	ProviderID   string  `json:"provider_id,omitempty"`
	IsActive     bool    `json:"is_active"`
	IsProtected  bool    `json:"is_protected"`
	InvitedBy    *string `json:"invited_by,omitempty"`
	// LastLogin is the RFC3339 timestamp of the most recent successful
	// session creation; nil when the user has never logged in (the inactive
	// admin filter treats nil as "never logged in").
	LastLogin *string `json:"last_login,omitempty"`
	CreatedAt string  `json:"created_at"`
	UpdatedAt string  `json:"updated_at"`
}

// GetUserByID returns a user by their ID. Returns an error wrapping
// sql.ErrNoRows if the user does not exist.
func (s *Service) GetUserByID(ctx context.Context, id string) (*User, error) {
	var u User
	var invitedBy sql.NullString
	var providerID sql.NullString
	var lastLogin sql.NullString

	err := s.db.QueryRowContext(ctx, `
		SELECT id, username, display_name, role, auth_provider, provider_id,
		       is_active, is_protected, invited_by, last_login, created_at, updated_at
		FROM users WHERE id = ?
	`, id).Scan(
		&u.ID, &u.Username, &u.DisplayName, &u.Role, &u.AuthProvider,
		&providerID, &u.IsActive, &u.IsProtected, &invitedBy, &lastLogin, &u.CreatedAt, &u.UpdatedAt,
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
	if lastLogin.Valid {
		u.LastLogin = &lastLogin.String
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
		       is_active, is_protected, invited_by, last_login, created_at, updated_at
		FROM users ORDER BY created_at ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("listing users: %w", err)
	}
	defer rows.Close() //nolint:errcheck // Close error not actionable on cleanup

	return scanUsers(rows)
}

// ListInactiveUsers returns users that have never logged in or whose last
// login is older than thresholdDays. thresholdDays <= 0 disables the
// stale-login criterion and returns only never-logged-in accounts, which is
// the safest default for a delete-flow filter.
func (s *Service) ListInactiveUsers(ctx context.Context, thresholdDays int) ([]User, error) {
	const baseSelect = `
		SELECT id, username, display_name, role, auth_provider, provider_id,
		       is_active, is_protected, invited_by, last_login, created_at, updated_at
		FROM users
	`
	var (
		rows *sql.Rows
		err  error
	)
	if thresholdDays > 0 {
		cutoff := time.Now().UTC().AddDate(0, 0, -thresholdDays).Format(time.RFC3339)
		rows, err = s.db.QueryContext(ctx, baseSelect+`
			WHERE last_login IS NULL OR last_login < ?
			ORDER BY created_at ASC
		`, cutoff)
	} else {
		rows, err = s.db.QueryContext(ctx, baseSelect+`
			WHERE last_login IS NULL
			ORDER BY created_at ASC
		`)
	}
	if err != nil {
		return nil, fmt.Errorf("listing inactive users: %w", err)
	}
	defer rows.Close() //nolint:errcheck // Close error not actionable on cleanup

	return scanUsers(rows)
}

// scanUsers drains a SELECT-shaped rows cursor that matches the canonical
// users-table column list (see ListUsers / ListInactiveUsers).
func scanUsers(rows *sql.Rows) ([]User, error) {
	users := []User{}
	for rows.Next() {
		var u User
		var invitedBy sql.NullString
		var providerID sql.NullString
		var lastLogin sql.NullString

		if err := rows.Scan(
			&u.ID, &u.Username, &u.DisplayName, &u.Role, &u.AuthProvider,
			&providerID, &u.IsActive, &u.IsProtected, &invitedBy, &lastLogin, &u.CreatedAt, &u.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scanning user: %w", err)
		}

		if providerID.Valid {
			u.ProviderID = providerID.String
		}
		if invitedBy.Valid {
			u.InvitedBy = &invitedBy.String
		}
		if lastLogin.Valid {
			u.LastLogin = &lastLogin.String
		}

		users = append(users, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scanning users: %w", err)
	}
	return users, nil
}

// UpdateUserRole changes a user's role. Valid roles are "administrator" and
// "operator". Returns ErrProtectedUser if downgrading the protected bootstrap
// administrator, and ErrLastAdmin if downgrading the last active administrator.
// The checks and role update run inside a BEGIN IMMEDIATE transaction
// to prevent concurrent downgrades from racing past the safeguards.
//
//nolint:gocognit // Txn-bound precondition chain: role enum gate, current-role query, no-op short-circuit, protected-user guard on downgrade, last-admin guard, then a constrained UPDATE that also recognizes the SQL trigger's "cannot change role of a protected user" error so the API returns ErrProtectedUser instead of a generic error. The guards are the security invariant; splitting them into helpers would only obscure ordering.
func (s *Service) UpdateUserRole(ctx context.Context, userID, newRole string) error {
	if newRole != "administrator" && newRole != "operator" {
		return fmt.Errorf("invalid role %q: must be administrator or operator", newRole)
	}

	return s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		var currentRole string
		var isProtected bool
		err := conn.QueryRowContext(ctx, `
			SELECT role, is_protected FROM users WHERE id = ? AND is_active = 1
		`, userID).Scan(&currentRole, &isProtected)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("user not found: %w", sql.ErrNoRows)
		}
		if err != nil {
			return fmt.Errorf("checking current role: %w", err)
		}

		if newRole == currentRole {
			return nil
		}

		if newRole == "operator" {
			if isProtected {
				return ErrProtectedUser
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
			UPDATE users SET role = ?, updated_at = ? WHERE id = ? AND is_active = 1
		`, newRole, now, userID)
		if err != nil {
			if strings.Contains(err.Error(), "cannot change role of a protected user") {
				return ErrProtectedUser
			}
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
// sessions. Returns ErrProtectedUser if the user is the bootstrap administrator,
// and ErrLastAdmin if deactivating the last active administrator.
// All checks and the deactivation run inside a BEGIN IMMEDIATE transaction
// to prevent concurrent deactivations from racing past the safeguards.
func (s *Service) DeactivateUser(ctx context.Context, userID string) error {
	return s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		var role string
		var isProtected bool
		err := conn.QueryRowContext(ctx, `
			SELECT role, is_protected FROM users WHERE id = ? AND is_active = 1
		`, userID).Scan(&role, &isProtected)
		if errors.Is(err, sql.ErrNoRows) {
			// Already inactive or does not exist -- nothing to do.
			return nil
		}
		if err != nil {
			return fmt.Errorf("checking user for deactivation: %w", err)
		}

		if isProtected {
			return ErrProtectedUser
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
			if strings.Contains(err.Error(), "cannot deactivate a protected user") {
				return ErrProtectedUser
			}
			return fmt.Errorf("deactivating user: %w", err)
		}

		_, err = conn.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID)
		if err != nil {
			return fmt.Errorf("deleting sessions for deactivated user: %w", err)
		}

		return nil
	})
}

// DeleteUser permanently removes a user account and records the deletion in
// the audit log. Refuses to delete the actor themselves (ErrSelfDelete), the
// bootstrap protected admin (ErrProtectedUser), or the last active
// administrator (ErrLastAdmin).
//
// The audit row is INSERTTed inside the same BEGIN IMMEDIATE transaction that
// removes the target, so a failed delete cannot leave a half-finished audit
// trail. The new actor_user_id / target_user_id columns (migration 012) carry
// the relationship across the target's FK cascade; the legacy
// audit_log.user_id stays CASCADE because SQLite cannot alter FK semantics in
// place, so the actor is recorded into both for forward compatibility.
//
// reason is optional free text shown in the confirm dialog; trimmed and
// stored verbatim in audit_log.detail.
//
//nolint:gocognit // Single transactional precondition chain: self-delete gate, target lookup, protected guard, last-admin guard (on active admin removal), audit insert, then DELETE. Splitting these into helpers would only obscure the ordering that the security invariant depends on.
func (s *Service) DeleteUser(ctx context.Context, actorID, targetID, reason string) error {
	if actorID == "" {
		return fmt.Errorf("actor id required")
	}
	if targetID == "" {
		return fmt.Errorf("target id required")
	}
	if actorID == targetID {
		return ErrSelfDelete
	}

	reason = strings.TrimSpace(reason)

	return s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		var (
			role        string
			isActive    bool
			isProtected bool
			username    string
		)
		err := conn.QueryRowContext(ctx, `
			SELECT role, is_active, is_protected, username
			FROM users WHERE id = ?
		`, targetID).Scan(&role, &isActive, &isProtected, &username)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("user not found: %w", sql.ErrNoRows)
		}
		if err != nil {
			return fmt.Errorf("checking target user: %w", err)
		}

		if isProtected {
			return ErrProtectedUser
		}

		// Only block on the last-admin guard if the target is an active
		// administrator; deleting an inactive (deactivated) admin cannot
		// drop the active-admin count, so we don't bother counting.
		if isActive && role == "administrator" {
			var count int
			if err := conn.QueryRowContext(ctx, `
				SELECT COUNT(*) FROM users WHERE role = 'administrator' AND is_active = 1
			`).Scan(&count); err != nil {
				return fmt.Errorf("counting active admins: %w", err)
			}
			if count <= 1 {
				return ErrLastAdmin
			}
		}

		// Write the audit entry BEFORE the DELETE so the actor reference is
		// preserved in the legacy user_id column (ON DELETE CASCADE on user_id
		// would otherwise nuke this row if the actor were ever deleted later;
		// the new SET NULL columns hold the relationship long-term).
		now := time.Now().UTC().Format(time.RFC3339)
		detail := reason
		if detail == "" {
			detail = "permanently deleted: " + username
		} else {
			detail = "permanently deleted " + username + ": " + reason
		}
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO audit_log (id, action, token_id, token_name, user_id, detail,
			                       actor_user_id, target_user_id, created_at)
			VALUES (?, 'user.delete', NULL, '', ?, ?, ?, ?, ?)
		`, uuid.New().String(), actorID, detail, actorID, targetID, now); err != nil {
			return fmt.Errorf("writing user delete audit entry: %w", err)
		}

		result, err := conn.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, targetID)
		if err != nil {
			// The prevent_delete_protected_user trigger is the floor-level
			// guard against deleting the bootstrap admin. The in-tx check
			// above should always catch this first, but treat the trigger
			// firing as ErrProtectedUser too so a future code change that
			// drops the in-tx check still surfaces a sensible 409.
			if strings.Contains(err.Error(), "cannot delete a protected user") {
				return ErrProtectedUser
			}
			return fmt.Errorf("deleting user: %w", err)
		}
		n, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("checking delete rows affected: %w", err)
		}
		if n == 0 {
			return fmt.Errorf("user not found: %w", sql.ErrNoRows)
		}

		// sessions.user_id is FK CASCADE in the schema, but FK enforcement
		// is per-connection (PRAGMA foreign_keys) and tests routinely run
		// without it on. Issue an explicit DELETE so the target's live
		// sessions are invalidated regardless of PRAGMA state -- mirrors
		// DeactivateUser's session sweep at the equivalent point.
		if _, err := conn.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, targetID); err != nil {
			return fmt.Errorf("deleting sessions for deleted user: %w", err)
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
		errLower := strings.ToLower(err.Error())
		if strings.Contains(errLower, "unique constraint") && strings.Contains(errLower, "username") {
			return nil, ErrUsernameConflict
		}
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

// SyncDisplayName updates the username and display_name fields for a user to
// match a new display name from an external provider. It is a best-effort
// operation called during federated login to keep the stored name current.
func (s *Service) SyncDisplayName(ctx context.Context, userID, displayName string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
		UPDATE users SET username = ?, display_name = ?, updated_at = ? WHERE id = ?
	`, displayName, displayName, now, userID)
	if err != nil {
		return fmt.Errorf("syncing display name: %w", err)
	}
	return nil
}

// GetUserByProvider returns the user whose auth_provider and provider_id match
// the given values. Returns an error wrapping sql.ErrNoRows if not found.
func (s *Service) GetUserByProvider(ctx context.Context, authProvider, providerID string) (*User, error) {
	var u User
	var invitedBy sql.NullString
	var pID sql.NullString
	var lastLogin sql.NullString

	err := s.db.QueryRowContext(ctx, `
		SELECT id, username, display_name, role, auth_provider, provider_id,
		       is_active, is_protected, invited_by, last_login, created_at, updated_at
		FROM users WHERE auth_provider = ? AND provider_id = ?
	`, authProvider, providerID).Scan(
		&u.ID, &u.Username, &u.DisplayName, &u.Role, &u.AuthProvider,
		&pID, &u.IsActive, &u.IsProtected, &invitedBy, &lastLogin, &u.CreatedAt, &u.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("user not found: %w", sql.ErrNoRows)
	}
	if err != nil {
		return nil, fmt.Errorf("getting user by provider: %w", err)
	}

	if pID.Valid {
		u.ProviderID = pID.String
	}
	if invitedBy.Valid {
		u.InvitedBy = &invitedBy.String
	}
	if lastLogin.Valid {
		u.LastLogin = &lastLogin.String
	}

	return &u, nil
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
	defer conn.Close() //nolint:errcheck // Close error not actionable on cleanup

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("beginning immediate transaction: %w", err)
	}

	// Use a non-cancellable context for COMMIT/ROLLBACK so transaction cleanup
	// completes even if the caller's context is canceled (gosec G118).
	cleanupCtx := context.WithoutCancel(ctx)

	if err := fn(conn); err != nil {
		conn.ExecContext(cleanupCtx, "ROLLBACK") //nolint:errcheck // Best-effort rollback in cleanup context; primary error is the actionable signal
		return err
	}

	if _, err := conn.ExecContext(cleanupCtx, "COMMIT"); err != nil {
		conn.ExecContext(cleanupCtx, "ROLLBACK") //nolint:errcheck // Best-effort rollback in cleanup context; primary error is the actionable signal
		return fmt.Errorf("committing transaction: %w", err)
	}

	return nil
}
