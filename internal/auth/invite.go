package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Sentinel errors for invite operations.
var (
	// ErrInviteExpired is returned when an invite code has passed its expiry time.
	ErrInviteExpired = errors.New("invite has expired")

	// ErrInviteRedeemed is returned when an invite code has already been used.
	ErrInviteRedeemed = errors.New("invite has already been redeemed")

	// ErrInviteNotFound is returned when the invite code does not exist.
	ErrInviteNotFound = errors.New("invite not found")
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
// The invite code has the format "sw_inv_" followed by 8 hex characters.
func (s *Service) CreateInvite(ctx context.Context, role, createdBy string, expiresIn time.Duration) (*Invite, error) {
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

	if inv.RedeemedBy != nil {
		return nil, ErrInviteRedeemed
	}

	expires, err := time.Parse(time.RFC3339, inv.ExpiresAt)
	if err != nil {
		return nil, fmt.Errorf("parsing invite expiry: %w", err)
	}
	if time.Now().UTC().After(expires) {
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
		WHERE redeemed_by IS NULL AND expires_at > ?
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
	return invites, rows.Err()
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
	_, err = s.db.ExecContext(ctx, `
		UPDATE invites SET redeemed_by = ?, redeemed_at = ? WHERE id = ?
	`, userID, now, inv.ID)
	if err != nil {
		return nil, fmt.Errorf("redeeming invite: %w", err)
	}

	inv.RedeemedBy = &userID
	inv.RedeemedAt = &now

	return inv, nil
}

// RevokeInvite deletes an unredeemed invite. Returns ErrInviteNotFound if the
// invite does not exist or has already been redeemed.
func (s *Service) RevokeInvite(ctx context.Context, inviteID string) error {
	result, err := s.db.ExecContext(ctx, `
		DELETE FROM invites WHERE id = ? AND redeemed_by IS NULL
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

// generateInviteCode produces a unique invite code in the format "sw_inv_XXXXXXXX"
// where X is a hex digit derived from 6 random bytes (8 hex chars used).
func generateInviteCode() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "sw_inv_" + hex.EncodeToString(b)[:8], nil
}
