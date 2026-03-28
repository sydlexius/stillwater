package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/auth"
)

// withAdminCtx adds the given user ID and administrator role to the request
// context, simulating authenticated admin middleware.
func withAdminCtx(req *http.Request, userID string) *http.Request {
	ctx := middleware.WithTestUserID(req.Context(), userID)
	ctx = middleware.WithTestRole(ctx, "administrator")
	return req.WithContext(ctx)
}

func TestHandleRegister_ValidInvite(t *testing.T) {
	r, authSvc, userID := testRouterWithAuth(t)

	// Create an invite as the admin.
	invite, err := authSvc.CreateInvite(context.Background(), "operator", userID, 24*60*60*1e9) // 24h
	if err != nil {
		t.Fatalf("creating invite: %v", err)
	}

	body := `{"code":"` + invite.Code + `","username":"newuser","password":"securepassword123","display_name":"New User"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/users/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.handleRegister(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("register: status = %d, want %d; body: %s", w.Code, http.StatusCreated, w.Body.String())
	}

	var user auth.User
	if err := json.NewDecoder(w.Body).Decode(&user); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if user.Username != "newuser" {
		t.Errorf("expected username 'newuser', got %q", user.Username)
	}
	if user.DisplayName != "New User" {
		t.Errorf("expected display_name 'New User', got %q", user.DisplayName)
	}
	if user.Role != "operator" {
		t.Errorf("expected role 'operator', got %q", user.Role)
	}
}

func TestHandleUpdateUser_InvalidRole_Returns400(t *testing.T) {
	r, _, userID := testRouterWithAuth(t)

	body := `{"role":"superadmin"}`
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/users/"+userID, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", userID)
	req = withAdminCtx(req, userID)
	w := httptest.NewRecorder()
	r.handleUpdateUser(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("update user: status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if !strings.Contains(resp["error"], "Role must be") {
		t.Errorf("expected role validation error, got %q", resp["error"])
	}
}

func TestHandleUpdateUser_ValidRole(t *testing.T) {
	r, authSvc, adminID := testRouterWithAuth(t)

	// Create a second user to update (cannot downgrade the only admin).
	user, err := authSvc.CreateLocalUser(context.Background(), "operator1", "password1234", "Operator One", "operator", adminID)
	if err != nil {
		t.Fatalf("creating operator user: %v", err)
	}

	body := `{"role":"administrator"}`
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/users/"+user.ID, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", user.ID)
	req = withAdminCtx(req, adminID)
	w := httptest.NewRecorder()
	r.handleUpdateUser(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("update user: status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var updated auth.User
	if err := json.NewDecoder(w.Body).Decode(&updated); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if updated.Role != "administrator" {
		t.Errorf("expected role 'administrator', got %q", updated.Role)
	}
}

func TestHandleDeactivateUser_Success(t *testing.T) {
	r, authSvc, adminID := testRouterWithAuth(t)

	// Create a user to deactivate.
	user, err := authSvc.CreateLocalUser(context.Background(), "toDeactivate", "password1234", "Deactivate Me", "operator", adminID)
	if err != nil {
		t.Fatalf("creating user: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/users/"+user.ID, nil)
	req.SetPathValue("id", user.ID)
	req = withAdminCtx(req, adminID)
	w := httptest.NewRecorder()
	r.handleDeactivateUser(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("deactivate: status = %d, want %d; body: %s", w.Code, http.StatusNoContent, w.Body.String())
	}

	// Verify user is deactivated.
	deactivated, err := authSvc.GetUserByID(context.Background(), user.ID)
	if err != nil {
		t.Fatalf("getting deactivated user: %v", err)
	}
	if deactivated.IsActive {
		t.Error("expected user to be inactive after deactivation")
	}
}
