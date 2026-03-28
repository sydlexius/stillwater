package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

// Sentinel errors for invite operations.
var (
	// ErrInviteExpired is returned when an invite code has passed its expiry time.
	ErrInviteExpired = errors.New("invite has expired")

	// ErrInviteRedeemed is returned when an invite code has already been used.
	ErrInviteRedeemed = errors.New("invite has already been redeemed")

	// ErrInviteNotFound is returned when the invite code does not exist.
	ErrInviteNotFound = errors.New("invite not found")

	// ErrUsernameConflict is returned when a registration username already exists.
	ErrUsernameConflict = errors.New("username already exists")
)

// Invite represents a single-use invitation link.
type Invite struct {
	ID         string  `json:"id"`
	Code       string  `json:"code"`
	Role       string  `json:"role"`
	CreatedBy  string  `json:"created_by"`
	ExpiresAt  string  `json:"expires_at"`
	RedeemedBy *string `json:"redeemed_by,omitempty"`
	RedeemedAt *string `json:"redeemed_at,omitempty"`
	CreatedAt  string  `json:"created_at"`
}

// CreateInvite generates a new invitation with the given role and expiry duration.
// The invite code has the format "sw_inv_" followed by 32 hex characters.
func (s *Service) CreateInvite(ctx context.Context, role, createdBy string, expiresIn time.Duration) (*Invite, error) {
	if role != "administrator" && role != "operator" {
		return nil, fmt.Errorf("invalid role %q: must be administrator or operator", role)
	}

	code, err := generateInviteCode()
	if err != nil {
		return nil, fmt.Errorf("generating invite code: %w", err)
	}

	id := uuid.New().String()
	now := time.Now().UTC()
	expiresAt := now.Add(expiresIn).Format(time.RFC3339)
	createdAt := now.Format(time.RFC3339)

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO invites (id, code, role, created_by, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, id, code, role, createdBy, expiresAt, createdAt)
	if err != nil {
		return nil, fmt.Errorf("creating invite: %w", err)
	}

	return &Invite{
		ID:        id,
		Code:      code,
		Role:      role,
		CreatedBy: createdBy,
		ExpiresAt: expiresAt,
		CreatedAt: createdAt,
	}, nil
}

// GetInviteByCode looks up an invite by its code and validates it.
// Returns ErrInviteNotFound, ErrInviteRedeemed, or ErrInviteExpired as appropriate.
func (s *Service) GetInviteByCode(ctx context.Context, code string) (*Invite, error) {
	var inv Invite
	var redeemedBy sql.NullString
	var redeemedAt sql.NullString

	err := s.db.QueryRowContext(ctx, `
		SELECT id, code, role, created_by, expires_at, redeemed_by, redeemed_at, created_at
		FROM invites WHERE code = ?
	`, code).Scan(
		&inv.ID, &inv.Code, &inv.Role, &inv.CreatedBy, &inv.ExpiresAt,
		&redeemedBy, &redeemedAt, &inv.CreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrInviteNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("getting invite by code: %w", err)
	}

	if redeemedBy.Valid {
		inv.RedeemedBy = &redeemedBy.String
	}
	if redeemedAt.Valid {
		inv.RedeemedAt = &redeemedAt.String
	}

	if inv.RedeemedAt != nil {
		return nil, ErrInviteRedeemed
	}

	expires, err := time.Parse(time.RFC3339, inv.ExpiresAt)
	if err != nil {
		return nil, fmt.Errorf("parsing invite expiry: %w", err)
	}
	// Use !Before for >= comparison, consistent with the SQL "expires_at > ?"
	// in ListPendingInvites (both treat expires_at == now as expired).
	if !time.Now().UTC().Before(expires) {
		return nil, ErrInviteExpired
	}

	return &inv, nil
}

// ListPendingInvites returns all unredeemed, non-expired invites ordered by
// created_at descending.
func (s *Service) ListPendingInvites(ctx context.Context) ([]Invite, error) {
	now := time.Now().UTC().Format(time.RFC3339)

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, code, role, created_by, expires_at, redeemed_by, redeemed_at, created_at
		FROM invites
		WHERE redeemed_at IS NULL AND expires_at > ?
		ORDER BY created_at DESC
	`, now)
	if err != nil {
		return nil, fmt.Errorf("listing pending invites: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var invites []Invite
	for rows.Next() {
		var inv Invite
		var redeemedBy sql.NullString
		var redeemedAt sql.NullString

		if err := rows.Scan(
			&inv.ID, &inv.Code, &inv.Role, &inv.CreatedBy, &inv.ExpiresAt,
			&redeemedBy, &redeemedAt, &inv.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scanning invite: %w", err)
		}

		if redeemedBy.Valid {
			inv.RedeemedBy = &redeemedBy.String
		}
		if redeemedAt.Valid {
			inv.RedeemedAt = &redeemedAt.String
		}

		invites = append(invites, inv)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing pending invites: %w", err)
	}
	return invites, nil
}

// RedeemInvite validates an invite code and marks it as redeemed by the given user.
// Returns ErrInviteNotFound, ErrInviteRedeemed, or ErrInviteExpired if the invite
// cannot be redeemed.
func (s *Service) RedeemInvite(ctx context.Context, code, userID string) (*Invite, error) {
	inv, err := s.GetInviteByCode(ctx, code)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC().Format(time.RFC3339)
	result, err := s.db.ExecContext(ctx, `
		UPDATE invites SET redeemed_by = ?, redeemed_at = ?
		WHERE id = ? AND redeemed_at IS NULL AND expires_at > ?
	`, userID, now, inv.ID, now)
	if err != nil {
		return nil, fmt.Errorf("redeeming invite: %w", err)
	}

	n, err := result.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("checking redeem rows affected: %w", err)
	}
	if n == 0 {
		// The invite was valid at GetInviteByCode but the UPDATE missed.
		// Re-check to return the correct sentinel error.
		var redeemedAt sql.NullString
		var expiresAt string
		rerr := s.db.QueryRowContext(ctx, `
			SELECT expires_at, redeemed_at FROM invites WHERE id = ?
		`, inv.ID).Scan(&expiresAt, &redeemedAt)
		if errors.Is(rerr, sql.ErrNoRows) {
			return nil, ErrInviteNotFound
		}
		if rerr != nil {
			return nil, fmt.Errorf("re-checking invite state: %w", rerr)
		}
		if redeemedAt.Valid {
			return nil, ErrInviteRedeemed
		}
		return nil, ErrInviteExpired
	}

	inv.RedeemedBy = &userID
	inv.RedeemedAt = &now

	return inv, nil
}

// RevokeInvite deletes an unredeemed invite. Returns ErrInviteNotFound if the
// invite does not exist or has already been redeemed.
func (s *Service) RevokeInvite(ctx context.Context, inviteID string) error {
	result, err := s.db.ExecContext(ctx, `
		DELETE FROM invites WHERE id = ? AND redeemed_at IS NULL
	`, inviteID)
	if err != nil {
		return fmt.Errorf("revoking invite: %w", err)
	}

	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking rows affected: %w", err)
	}
	if n == 0 {
		return ErrInviteNotFound
	}

	return nil
}

// ClaimInviteAndRegister atomically validates an invite, creates a local user,
// and redeems the invite within a single database transaction. This eliminates
// the TOCTOU race where two concurrent requests could both validate the same
// invite code before either redeems it.
func (s *Service) ClaimInviteAndRegister(ctx context.Context, code, username, password, displayName string) (*User, error) {
	// Pre-compute the bcrypt hash outside the transaction to avoid holding the
	// write lock during the expensive key derivation.
	hash, err := bcrypt.GenerateFromPassword(PrehashPassword(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("hashing password: %w", err)
	}

	var userID string

	err = s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		// 1. Look up and validate the invite inside the transaction.
		var inv Invite
		var redeemedAt sql.NullString

		err := conn.QueryRowContext(ctx, `
			SELECT id, code, role, created_by, expires_at, redeemed_at, created_at
			FROM invites WHERE code = ?
		`, code).Scan(
			&inv.ID, &inv.Code, &inv.Role, &inv.CreatedBy, &inv.ExpiresAt,
			&redeemedAt, &inv.CreatedAt,
		)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrInviteNotFound
		}
		if err != nil {
			return fmt.Errorf("looking up invite: %w", err)
		}

		if redeemedAt.Valid {
			return ErrInviteRedeemed
		}

		expires, err := time.Parse(time.RFC3339, inv.ExpiresAt)
		if err != nil {
			return fmt.Errorf("parsing invite expiry: %w", err)
		}
		// Use !Before for >= comparison, consistent with GetInviteByCode.
		if !time.Now().UTC().Before(expires) {
			return ErrInviteExpired
		}

		// 2. Create the local user inside the transaction.
		userID = uuid.New().String()
		now := time.Now().UTC().Format(time.RFC3339)

		var invitedByVal sql.NullString
		if inv.CreatedBy != "" {
			invitedByVal = sql.NullString{String: inv.CreatedBy, Valid: true}
		}

		_, err = conn.ExecContext(ctx, `
			INSERT INTO users (id, username, display_name, password_hash, role, auth_provider, provider_id,
			                   is_active, invited_by, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, 'local', '', 1, ?, ?, ?)
		`, userID, username, displayName, string(hash), inv.Role, invitedByVal, now, now)
		if err != nil {
			errLower := strings.ToLower(err.Error())
			if strings.Contains(errLower, "unique constraint") && strings.Contains(errLower, "username") {
				return ErrUsernameConflict
			}
			return fmt.Errorf("creating user: %w", err)
		}

		// 3. Redeem the invite inside the transaction.
		result, err := conn.ExecContext(ctx, `
			UPDATE invites SET redeemed_by = ?, redeemed_at = ?
			WHERE id = ? AND redeemed_at IS NULL AND expires_at > ?
		`, userID, now, inv.ID, now)
		if err != nil {
			return fmt.Errorf("redeeming invite: %w", err)
		}

		n, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("checking redeem rows affected: %w", err)
		}
		if n == 0 {
			// Within the BEGIN IMMEDIATE tx, we already validated the invite
			// was unredeemed and not expired. The most likely cause is a
			// near-boundary expiry between the check and the UPDATE.
			return ErrInviteExpired
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	// Read the user back from the committed data.
	return s.GetUserByID(ctx, userID)
}

// generateInviteCode produces a unique invite code in the format
// "sw_inv_XXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX" where X is a hex digit
// derived from 16 random bytes (32 hex chars).
func generateInviteCode() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "sw_inv_" + hex.EncodeToString(b), nil
}
