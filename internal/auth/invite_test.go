package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// setupInviteTest creates a service with an admin user and returns both
// the service and the admin's user ID.
func setupInviteTest(t *testing.T) (*Service, string) {
	t.Helper()

	svc := setupTestService(t)
	ctx := context.Background()

	_, err := svc.Setup(ctx, "admin", "password")
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}

	// Retrieve the admin ID via Login + ValidateSession.
	token, err := svc.Login(ctx, "admin", "password")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	userID, err := svc.ValidateSession(ctx, token)
	if err != nil {
		t.Fatalf("ValidateSession: %v", err)
	}

	return svc, userID
}

func TestCreateInvite(t *testing.T) {
	svc, adminID := setupInviteTest(t)
	ctx := context.Background()

	inv, err := svc.CreateInvite(ctx, "operator", adminID, 24*time.Hour)
	if err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}

	if inv.ID == "" {
		t.Error("expected non-empty ID")
	}
	if !strings.HasPrefix(inv.Code, "sw_inv_") {
		t.Errorf("Code = %q, expected prefix %q", inv.Code, "sw_inv_")
	}
	// Code should be "sw_inv_" (7 chars) + 8 hex chars = 15 chars total.
	if len(inv.Code) != 15 {
		t.Errorf("Code length = %d, want 15", len(inv.Code))
	}
	if inv.Role != "operator" {
		t.Errorf("Role = %q, want %q", inv.Role, "operator")
	}
	if inv.CreatedBy != adminID {
		t.Errorf("CreatedBy = %q, want %q", inv.CreatedBy, adminID)
	}
	if inv.RedeemedBy != nil {
		t.Error("expected RedeemedBy to be nil")
	}
}

func TestGetInviteByCode(t *testing.T) {
	svc, adminID := setupInviteTest(t)
	ctx := context.Background()

	inv, err := svc.CreateInvite(ctx, "operator", adminID, 24*time.Hour)
	if err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}

	got, err := svc.GetInviteByCode(ctx, inv.Code)
	if err != nil {
		t.Fatalf("GetInviteByCode: %v", err)
	}

	if got.ID != inv.ID {
		t.Errorf("ID = %q, want %q", got.ID, inv.ID)
	}
	if got.Code != inv.Code {
		t.Errorf("Code = %q, want %q", got.Code, inv.Code)
	}
}

func TestGetInviteByCode_NotFound(t *testing.T) {
	svc := setupTestService(t)
	ctx := context.Background()

	_, err := svc.GetInviteByCode(ctx, "sw_inv_notexist")
	if !errors.Is(err, ErrInviteNotFound) {
		t.Errorf("GetInviteByCode nonexistent = %v, want ErrInviteNotFound", err)
	}
}

func TestGetInviteByCode_Expired(t *testing.T) {
	svc, adminID := setupInviteTest(t)
	ctx := context.Background()

	// Create an invite that expired in the past.
	inv, err := svc.CreateInvite(ctx, "operator", adminID, -1*time.Hour)
	if err != nil {
		t.Fatalf("CreateInvite (expired): %v", err)
	}

	_, err = svc.GetInviteByCode(ctx, inv.Code)
	if !errors.Is(err, ErrInviteExpired) {
		t.Errorf("GetInviteByCode expired = %v, want ErrInviteExpired", err)
	}
}

func TestGetInviteByCode_Redeemed(t *testing.T) {
	svc, adminID := setupInviteTest(t)
	ctx := context.Background()

	inv, err := svc.CreateInvite(ctx, "operator", adminID, 24*time.Hour)
	if err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}

	// Create a user to be the redeemer.
	redeemer, err := svc.CreateLocalUser(ctx, "redeemer", "pass", "Redeemer", "operator", "")
	if err != nil {
		t.Fatalf("CreateLocalUser: %v", err)
	}

	_, err = svc.RedeemInvite(ctx, inv.Code, redeemer.ID)
	if err != nil {
		t.Fatalf("RedeemInvite: %v", err)
	}

	_, err = svc.GetInviteByCode(ctx, inv.Code)
	if !errors.Is(err, ErrInviteRedeemed) {
		t.Errorf("GetInviteByCode redeemed = %v, want ErrInviteRedeemed", err)
	}
}

func TestListPendingInvites(t *testing.T) {
	svc, adminID := setupInviteTest(t)
	ctx := context.Background()

	// Create two valid invites and one expired invite.
	inv1, err := svc.CreateInvite(ctx, "operator", adminID, 24*time.Hour)
	if err != nil {
		t.Fatalf("CreateInvite 1: %v", err)
	}
	inv2, err := svc.CreateInvite(ctx, "administrator", adminID, 48*time.Hour)
	if err != nil {
		t.Fatalf("CreateInvite 2: %v", err)
	}
	// Expired invite -- should not appear in listing.
	_, err = svc.CreateInvite(ctx, "operator", adminID, -1*time.Hour)
	if err != nil {
		t.Fatalf("CreateInvite expired: %v", err)
	}

	// Redeem inv1 -- should not appear in listing.
	redeemer, err := svc.CreateLocalUser(ctx, "redeemer", "pass", "Redeemer", "operator", "")
	if err != nil {
		t.Fatalf("CreateLocalUser: %v", err)
	}
	_, err = svc.RedeemInvite(ctx, inv1.Code, redeemer.ID)
	if err != nil {
		t.Fatalf("RedeemInvite: %v", err)
	}

	pending, err := svc.ListPendingInvites(ctx)
	if err != nil {
		t.Fatalf("ListPendingInvites: %v", err)
	}

	if len(pending) != 1 {
		t.Fatalf("ListPendingInvites len = %d, want 1", len(pending))
	}
	if pending[0].ID != inv2.ID {
		t.Errorf("pending[0].ID = %q, want %q", pending[0].ID, inv2.ID)
	}
}

func TestRedeemInvite(t *testing.T) {
	svc, adminID := setupInviteTest(t)
	ctx := context.Background()

	inv, err := svc.CreateInvite(ctx, "operator", adminID, 24*time.Hour)
	if err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}

	redeemer, err := svc.CreateLocalUser(ctx, "redeemer", "pass", "Redeemer", "operator", "")
	if err != nil {
		t.Fatalf("CreateLocalUser: %v", err)
	}

	redeemed, err := svc.RedeemInvite(ctx, inv.Code, redeemer.ID)
	if err != nil {
		t.Fatalf("RedeemInvite: %v", err)
	}

	if redeemed.RedeemedBy == nil || *redeemed.RedeemedBy != redeemer.ID {
		t.Errorf("RedeemedBy = %v, want %q", redeemed.RedeemedBy, redeemer.ID)
	}
	if redeemed.RedeemedAt == nil || *redeemed.RedeemedAt == "" {
		t.Error("expected RedeemedAt to be set")
	}
}

func TestRedeemInvite_AlreadyRedeemed(t *testing.T) {
	svc, adminID := setupInviteTest(t)
	ctx := context.Background()

	inv, err := svc.CreateInvite(ctx, "operator", adminID, 24*time.Hour)
	if err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}

	redeemer, err := svc.CreateLocalUser(ctx, "redeemer", "pass", "Redeemer", "operator", "")
	if err != nil {
		t.Fatalf("CreateLocalUser: %v", err)
	}

	_, err = svc.RedeemInvite(ctx, inv.Code, redeemer.ID)
	if err != nil {
		t.Fatalf("RedeemInvite (first): %v", err)
	}

	_, err = svc.RedeemInvite(ctx, inv.Code, redeemer.ID)
	if !errors.Is(err, ErrInviteRedeemed) {
		t.Errorf("RedeemInvite second attempt = %v, want ErrInviteRedeemed", err)
	}
}

func TestRevokeInvite(t *testing.T) {
	svc, adminID := setupInviteTest(t)
	ctx := context.Background()

	inv, err := svc.CreateInvite(ctx, "operator", adminID, 24*time.Hour)
	if err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}

	if err := svc.RevokeInvite(ctx, inv.ID); err != nil {
		t.Fatalf("RevokeInvite: %v", err)
	}

	// Invite should no longer be found.
	_, err = svc.GetInviteByCode(ctx, inv.Code)
	if !errors.Is(err, ErrInviteNotFound) {
		t.Errorf("GetInviteByCode after revoke = %v, want ErrInviteNotFound", err)
	}
}

func TestRevokeInvite_NotFound(t *testing.T) {
	svc := setupTestService(t)
	ctx := context.Background()

	err := svc.RevokeInvite(ctx, "nonexistent-id")
	if !errors.Is(err, ErrInviteNotFound) {
		t.Errorf("RevokeInvite nonexistent = %v, want ErrInviteNotFound", err)
	}
}
