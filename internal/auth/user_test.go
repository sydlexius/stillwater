package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestGetUserByID(t *testing.T) {
	svc := setupTestService(t)
	ctx := context.Background()

	_, err := svc.Setup(ctx, "admin", "password")
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}

	users, err := svc.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) == 0 {
		t.Fatal("expected at least one user")
	}

	got, err := svc.GetUserByID(ctx, users[0].ID)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}

	if got.Username != "admin" {
		t.Errorf("Username = %q, want %q", got.Username, "admin")
	}
	if got.Role != "administrator" {
		t.Errorf("Role = %q, want %q", got.Role, "administrator")
	}
	if !got.IsActive {
		t.Error("expected IsActive = true")
	}
}

func TestGetUserByID_NotFound(t *testing.T) {
	svc := setupTestService(t)
	ctx := context.Background()

	_, err := svc.GetUserByID(ctx, "nonexistent-id")
	if err == nil {
		t.Fatal("expected error for nonexistent user")
	}
	if !strings.Contains(err.Error(), "user not found") {
		t.Errorf("expected 'user not found' error, got: %v", err)
	}
}

func TestGetUserRole(t *testing.T) {
	svc := setupTestService(t)
	ctx := context.Background()

	_, err := svc.Setup(ctx, "admin", "password")
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}

	users, err := svc.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}

	role, err := svc.GetUserRole(ctx, users[0].ID)
	if err != nil {
		t.Fatalf("GetUserRole: %v", err)
	}
	if role != "administrator" {
		t.Errorf("role = %q, want %q", role, "administrator")
	}
}

func TestGetUserRole_NotFound(t *testing.T) {
	svc := setupTestService(t)
	ctx := context.Background()

	role, err := svc.GetUserRole(ctx, "nonexistent-id")
	if err != nil {
		t.Fatalf("GetUserRole: %v", err)
	}
	if role != "" {
		t.Errorf("role = %q, want empty string", role)
	}
}

func TestListUsers(t *testing.T) {
	svc := setupTestService(t)
	ctx := context.Background()

	// No users initially.
	users, err := svc.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers (empty): %v", err)
	}
	if len(users) != 0 {
		t.Errorf("expected 0 users, got %d", len(users))
	}

	_, err = svc.Setup(ctx, "admin", "password")
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}

	_, err = svc.CreateLocalUser(ctx, "operator1", "pass1", "Op One", "operator", "")
	if err != nil {
		t.Fatalf("CreateLocalUser: %v", err)
	}

	users, err = svc.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) != 2 {
		t.Errorf("expected 2 users, got %d", len(users))
	}
}

func TestUpdateUserRole(t *testing.T) {
	svc := setupTestService(t)
	ctx := context.Background()

	_, err := svc.Setup(ctx, "admin", "password")
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}

	// Create a second admin so we can downgrade the first.
	second, err := svc.CreateLocalUser(ctx, "admin2", "pass2", "Admin Two", "administrator", "")
	if err != nil {
		t.Fatalf("CreateLocalUser: %v", err)
	}

	// Downgrade second admin to operator.
	if err := svc.UpdateUserRole(ctx, second.ID, "operator"); err != nil {
		t.Fatalf("UpdateUserRole to operator: %v", err)
	}

	role, err := svc.GetUserRole(ctx, second.ID)
	if err != nil {
		t.Fatalf("GetUserRole: %v", err)
	}
	if role != "operator" {
		t.Errorf("role = %q, want %q", role, "operator")
	}

	// Promote back to administrator.
	if err := svc.UpdateUserRole(ctx, second.ID, "administrator"); err != nil {
		t.Fatalf("UpdateUserRole to administrator: %v", err)
	}

	role, err = svc.GetUserRole(ctx, second.ID)
	if err != nil {
		t.Fatalf("GetUserRole: %v", err)
	}
	if role != "administrator" {
		t.Errorf("role = %q, want %q", role, "administrator")
	}
}

func TestUpdateUserRole_InvalidRole(t *testing.T) {
	svc := setupTestService(t)
	ctx := context.Background()

	_, err := svc.Setup(ctx, "admin", "password")
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}

	users, err := svc.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}

	err = svc.UpdateUserRole(ctx, users[0].ID, "superuser")
	if err == nil {
		t.Fatal("expected error for invalid role")
	}
}

func TestUpdateUserRole_LastAdmin(t *testing.T) {
	svc := setupTestService(t)
	ctx := context.Background()

	_, err := svc.Setup(ctx, "admin", "password")
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}

	users, err := svc.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}

	err = svc.UpdateUserRole(ctx, users[0].ID, "operator")
	if !errors.Is(err, ErrLastAdmin) {
		t.Errorf("UpdateUserRole last admin = %v, want ErrLastAdmin", err)
	}
}

func TestDeactivateUser(t *testing.T) {
	svc := setupTestService(t)
	ctx := context.Background()

	_, err := svc.Setup(ctx, "admin", "password")
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}

	op, err := svc.CreateLocalUser(ctx, "op1", "pass1", "Op One", "operator", "")
	if err != nil {
		t.Fatalf("CreateLocalUser: %v", err)
	}

	// Create a session for the operator.
	token, err := svc.Login(ctx, "op1", "pass1")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	if err := svc.DeactivateUser(ctx, op.ID); err != nil {
		t.Fatalf("DeactivateUser: %v", err)
	}

	// Session should no longer be valid.
	_, err = svc.ValidateSession(ctx, token)
	if err == nil {
		t.Error("expected session to be invalidated after deactivation")
	}

	// Role should return empty (inactive user).
	role, err := svc.GetUserRole(ctx, op.ID)
	if err != nil {
		t.Fatalf("GetUserRole: %v", err)
	}
	if role != "" {
		t.Errorf("expected empty role for inactive user, got %q", role)
	}
}

func TestDeactivateUser_LastAdmin(t *testing.T) {
	svc := setupTestService(t)
	ctx := context.Background()

	_, err := svc.Setup(ctx, "admin", "password")
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}

	users, err := svc.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}

	err = svc.DeactivateUser(ctx, users[0].ID)
	if !errors.Is(err, ErrLastAdmin) {
		t.Errorf("DeactivateUser last admin = %v, want ErrLastAdmin", err)
	}
}

func TestCreateLocalUser(t *testing.T) {
	svc := setupTestService(t)
	ctx := context.Background()

	user, err := svc.CreateLocalUser(ctx, "newuser", "hunter2", "New User", "operator", "")
	if err != nil {
		t.Fatalf("CreateLocalUser: %v", err)
	}

	if user.ID == "" {
		t.Error("expected non-empty ID")
	}
	if user.Username != "newuser" {
		t.Errorf("Username = %q, want %q", user.Username, "newuser")
	}
	if user.DisplayName != "New User" {
		t.Errorf("DisplayName = %q, want %q", user.DisplayName, "New User")
	}
	if user.Role != "operator" {
		t.Errorf("Role = %q, want %q", user.Role, "operator")
	}
	if user.AuthProvider != "local" {
		t.Errorf("AuthProvider = %q, want %q", user.AuthProvider, "local")
	}
	if !user.IsActive {
		t.Error("expected IsActive = true")
	}

	// Verify the user can authenticate.
	token, err := svc.Login(ctx, "newuser", "hunter2")
	if err != nil {
		t.Fatalf("Login after CreateLocalUser: %v", err)
	}
	if token == "" {
		t.Error("expected non-empty session token")
	}
}

func TestCreateLocalUser_WithInvitedBy(t *testing.T) {
	svc := setupTestService(t)
	ctx := context.Background()

	_, err := svc.Setup(ctx, "admin", "password")
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}

	admins, err := svc.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	adminID := admins[0].ID

	user, err := svc.CreateLocalUser(ctx, "invited", "pass", "Invited User", "operator", adminID)
	if err != nil {
		t.Fatalf("CreateLocalUser with invitedBy: %v", err)
	}

	if user.InvitedBy == nil || *user.InvitedBy != adminID {
		t.Errorf("InvitedBy = %v, want %q", user.InvitedBy, adminID)
	}
}

func TestCreateFederatedUser(t *testing.T) {
	svc := setupTestService(t)
	ctx := context.Background()

	identity := &Identity{
		ProviderID:   "emby-user-123",
		DisplayName:  "Emby User",
		ProviderType: "emby",
		IsAdmin:      false,
	}

	user, err := svc.CreateFederatedUser(ctx, identity, "operator", "")
	if err != nil {
		t.Fatalf("CreateFederatedUser: %v", err)
	}

	if user.ID == "" {
		t.Error("expected non-empty ID")
	}
	if user.Username != "Emby User" {
		t.Errorf("Username = %q, want %q", user.Username, "Emby User")
	}
	if user.Role != "operator" {
		t.Errorf("Role = %q, want %q", user.Role, "operator")
	}
	if user.AuthProvider != "emby" {
		t.Errorf("AuthProvider = %q, want %q", user.AuthProvider, "emby")
	}
	if user.ProviderID != "emby-user-123" {
		t.Errorf("ProviderID = %q, want %q", user.ProviderID, "emby-user-123")
	}
	if !user.IsActive {
		t.Error("expected IsActive = true")
	}
}
