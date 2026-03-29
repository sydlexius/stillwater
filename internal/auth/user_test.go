package auth

import (
	"context"
	"database/sql"
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
	if !got.IsProtected {
		t.Error("expected IsProtected = true for bootstrap admin")
	}
}

func TestGetUserByID_NotFound(t *testing.T) {
	svc := setupTestService(t)
	ctx := context.Background()

	_, err := svc.GetUserByID(ctx, "nonexistent-id")
	if err == nil {
		t.Fatal("expected error for nonexistent user")
	}
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("expected sql.ErrNoRows, got: %v", err)
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

	// Create a non-bootstrap admin directly (no Setup call) so it is not protected.
	admin, err := svc.CreateLocalUser(ctx, "admin", "password", "Admin", "administrator", "")
	if err != nil {
		t.Fatalf("CreateLocalUser: %v", err)
	}

	err = svc.UpdateUserRole(ctx, admin.ID, "operator")
	if !errors.Is(err, ErrLastAdmin) {
		t.Errorf("UpdateUserRole last admin = %v, want ErrLastAdmin", err)
	}

	// Same-role no-op on the sole admin must return nil, not ErrLastAdmin.
	if err = svc.UpdateUserRole(ctx, admin.ID, "administrator"); err != nil {
		t.Errorf("UpdateUserRole same-role sole admin = %v, want nil", err)
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

func TestDeactivateUser_BootstrapAdmin(t *testing.T) {
	svc := setupTestService(t)
	ctx := context.Background()

	_, err := svc.Setup(ctx, "admin", "password")
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}

	// Create a second admin so the last-admin guard is not the reason for refusal.
	secondAdmin, err := svc.CreateLocalUser(ctx, "admin2", "pass2", "Admin Two", "administrator", "")
	if err != nil {
		t.Fatalf("CreateLocalUser: %v", err)
	}

	users, err := svc.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}

	// Find the bootstrap admin by username and assert admin2 is not protected.
	var bootstrapID string
	for _, u := range users {
		switch u.Username {
		case "admin":
			if !u.IsProtected {
				t.Fatal("expected bootstrap admin to be protected")
			}
			bootstrapID = u.ID
		case "admin2":
			if u.ID == secondAdmin.ID && u.IsProtected {
				t.Fatal("expected second admin to be unprotected")
			}
		}
	}
	if bootstrapID == "" {
		t.Fatal("no bootstrap user found after Setup")
	}

	err = svc.DeactivateUser(ctx, bootstrapID)
	if !errors.Is(err, ErrProtectedUser) {
		t.Errorf("DeactivateUser bootstrap admin = %v, want ErrProtectedUser", err)
	}
}

func TestDeactivateUser_LastAdmin(t *testing.T) {
	svc := setupTestService(t)
	ctx := context.Background()

	// Create a non-bootstrap admin directly (no Setup call) so it is not protected.
	admin, err := svc.CreateLocalUser(ctx, "admin", "password", "Admin", "administrator", "")
	if err != nil {
		t.Fatalf("CreateLocalUser: %v", err)
	}

	err = svc.DeactivateUser(ctx, admin.ID)
	if !errors.Is(err, ErrLastAdmin) {
		t.Errorf("DeactivateUser last admin = %v, want ErrLastAdmin", err)
	}
}

func TestDeactivateUser_NonBootstrapAdmin(t *testing.T) {
	svc := setupTestService(t)
	ctx := context.Background()

	// Create two non-bootstrap admins (no Setup call) so neither is protected.
	admin1, err := svc.CreateLocalUser(ctx, "admin1", "pass1", "Admin One", "administrator", "")
	if err != nil {
		t.Fatalf("CreateLocalUser admin1: %v", err)
	}
	_, err = svc.CreateLocalUser(ctx, "admin2", "pass2", "Admin Two", "administrator", "")
	if err != nil {
		t.Fatalf("CreateLocalUser admin2: %v", err)
	}

	// Deactivating one of multiple non-bootstrap admins should succeed.
	if err := svc.DeactivateUser(ctx, admin1.ID); err != nil {
		t.Fatalf("DeactivateUser non-bootstrap admin: %v", err)
	}
	got, err := svc.GetUserByID(ctx, admin1.ID)
	if err != nil {
		t.Fatalf("GetUserByID admin1: %v", err)
	}
	if got.IsActive {
		t.Error("expected deactivated non-bootstrap admin to be inactive")
	}
}

func TestListUsers_BootstrapAdminIsProtected(t *testing.T) {
	svc := setupTestService(t)
	ctx := context.Background()

	_, err := svc.Setup(ctx, "admin", "password")
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}

	_, err = svc.CreateLocalUser(ctx, "op1", "pass1", "Op One", "operator", "")
	if err != nil {
		t.Fatalf("CreateLocalUser: %v", err)
	}

	users, err := svc.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}

	if len(users) != 2 {
		t.Fatalf("expected 2 users, got %d", len(users))
	}

	// Find each user by username to avoid relying on slice ordering.
	var (
		foundAdmin     bool
		foundOp1       bool
		protectedCount int
	)
	for _, u := range users {
		if u.IsProtected {
			protectedCount++
		}
		switch u.Username {
		case "admin":
			foundAdmin = true
			if !u.IsProtected {
				t.Errorf("bootstrap admin %q: expected IsProtected = true", u.Username)
			}
		case "op1":
			foundOp1 = true
			if u.IsProtected {
				t.Errorf("non-bootstrap user %q: expected IsProtected = false", u.Username)
			}
		}
	}
	if !foundAdmin || !foundOp1 {
		t.Fatalf("expected users %q and %q to be present", "admin", "op1")
	}
	if protectedCount != 1 {
		t.Errorf("expected exactly 1 protected user, got %d", protectedCount)
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

func TestUpdateUserRole_BootstrapAdmin(t *testing.T) {
	svc := setupTestService(t)
	ctx := context.Background()

	_, err := svc.Setup(ctx, "admin", "password")
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}

	// Create a second admin so the last-admin guard is not the reason for refusal.
	_, err = svc.CreateLocalUser(ctx, "admin2", "pass2", "Admin Two", "administrator", "")
	if err != nil {
		t.Fatalf("CreateLocalUser: %v", err)
	}

	users, err := svc.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}

	var bootstrapID string
	for _, u := range users {
		if u.IsProtected {
			bootstrapID = u.ID
			break
		}
	}
	if bootstrapID == "" {
		t.Fatal("no protected user found after Setup")
	}

	err = svc.UpdateUserRole(ctx, bootstrapID, "operator")
	if !errors.Is(err, ErrProtectedUser) {
		t.Errorf("UpdateUserRole bootstrap admin = %v, want ErrProtectedUser", err)
	}

	// Same-role no-op on the protected admin must return nil, not ErrProtectedUser.
	if err = svc.UpdateUserRole(ctx, bootstrapID, "administrator"); err != nil {
		t.Errorf("UpdateUserRole same-role protected admin = %v, want nil", err)
	}
}

func TestGetUserByProvider_IsProtected(t *testing.T) {
	svc := setupTestService(t)
	ctx := context.Background()

	// SetupFederated creates the bootstrap admin via the federated path.
	result := FederatedAuthResult{
		UserID:   "emby-user-1",
		UserName: "Alice",
		IsAdmin:  true,
	}
	if _, err := svc.SetupFederated(ctx, result, "emby"); err != nil {
		t.Fatalf("SetupFederated: %v", err)
	}

	user, err := svc.GetUserByProvider(ctx, "emby", "emby-user-1")
	if err != nil {
		t.Fatalf("GetUserByProvider: %v", err)
	}
	if !user.IsProtected {
		t.Error("expected federated bootstrap admin to have IsProtected = true")
	}
}

func TestDBTrigger_PreventDeactivateProtectedUser(t *testing.T) {
	svc := setupTestService(t)
	ctx := context.Background()

	_, err := svc.Setup(ctx, "admin", "password")
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}

	// Execute the UPDATE directly, bypassing the service layer.
	_, err = svc.db.ExecContext(ctx, `UPDATE users SET is_active = 0 WHERE is_protected = 1`)
	if err == nil {
		t.Fatal("expected DB trigger to reject UPDATE is_active=0 on protected user, got nil error")
	}
	if !strings.Contains(err.Error(), "cannot deactivate a protected user") {
		t.Errorf("unexpected trigger error: %v", err)
	}
}

func TestDBTrigger_PreventRoleChangeProtectedUser(t *testing.T) {
	svc := setupTestService(t)
	ctx := context.Background()

	_, err := svc.Setup(ctx, "admin", "password")
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}

	// Execute the UPDATE directly, bypassing the service layer.
	_, err = svc.db.ExecContext(ctx, `UPDATE users SET role = 'operator' WHERE is_protected = 1`)
	if err == nil {
		t.Fatal("expected DB trigger to reject role UPDATE on protected user, got nil error")
	}
	if !strings.Contains(err.Error(), "cannot change role of a protected user") {
		t.Errorf("unexpected trigger error: %v", err)
	}
}
