package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/auth"
)

// withAdminCtx adds both user ID and administrator role to the request context.
func withAdminCtx(req *http.Request, userID string) *http.Request {
	ctx := middleware.WithTestUserID(req.Context(), userID)
	ctx = middleware.WithTestRole(ctx, "administrator")
	return req.WithContext(ctx)
}

func TestHandleRegister_Success(t *testing.T) {
	r, authSvc, adminID := testRouterWithAuth(t)

	// Create an invite as the admin.
	invite, err := authSvc.CreateInvite(context.Background(), "operator", adminID, 24*time.Hour)
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
		t.Errorf("Username = %q, want %q", user.Username, "newuser")
	}
	if user.DisplayName != "New User" {
		t.Errorf("DisplayName = %q, want %q", user.DisplayName, "New User")
	}
	if user.Role != "operator" {
		t.Errorf("Role = %q, want %q", user.Role, "operator")
	}

	// Verify the session cookie was set (auto-login).
	cookies := w.Result().Cookies()
	found := false
	for _, c := range cookies {
		if c.Name == "session" && c.Value != "" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected session cookie to be set after registration")
	}
}

func TestHandleRegister_RedeemedInvite(t *testing.T) {
	r, authSvc, adminID := testRouterWithAuth(t)

	// Create and use an invite.
	invite, err := authSvc.CreateInvite(context.Background(), "operator", adminID, 24*time.Hour)
	if err != nil {
		t.Fatalf("creating invite: %v", err)
	}

	// Register first user with this invite.
	body := `{"code":"` + invite.Code + `","username":"firstuser","password":"securepassword123"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/users/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.handleRegister(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("first register: status = %d, want %d; body: %s", w.Code, http.StatusCreated, w.Body.String())
	}

	// Attempt to use the same invite again.
	body = `{"code":"` + invite.Code + `","username":"seconduser","password":"securepassword123"}`
	req = httptest.NewRequest(http.MethodPost, "/api/v1/users/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	r.handleRegister(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("second register: status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding error response: %v", err)
	}
	if !strings.Contains(resp["error"], "already been used") {
		t.Errorf("error = %q, want message about invite already used", resp["error"])
	}
}

func TestHandleDeactivateUser_BootstrapAdmin(t *testing.T) {
	r, authSvc, adminID := testRouterWithAuth(t)

	// Create a second admin so the last-admin guard is not the reason for refusal.
	_, err := authSvc.CreateLocalUser(context.Background(), "admin2", "password123", "Admin 2", "administrator", adminID)
	if err != nil {
		t.Fatalf("creating second admin: %v", err)
	}

	// Attempt to deactivate the bootstrap admin (adminID from Setup).
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/users/"+adminID, nil)
	req.SetPathValue("id", adminID)
	req = withAdminCtx(req, adminID)
	w := httptest.NewRecorder()
	r.handleDeactivateUser(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("deactivate bootstrap admin: status = %d, want %d; body: %s", w.Code, http.StatusConflict, w.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding error response: %v", err)
	}
	if !strings.Contains(resp["error"], "bootstrap administrator") {
		t.Errorf("error = %q, want message about bootstrap administrator", resp["error"])
	}
}

func TestHandleUpdateUser_InvalidRole(t *testing.T) {
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
		t.Fatalf("decoding error response: %v", err)
	}
	if !strings.Contains(resp["error"], "administrator or operator") {
		t.Errorf("error = %q, want message about valid roles", resp["error"])
	}
}

func TestHandleUpdateUser_ValidRole(t *testing.T) {
	r, authSvc, adminID := testRouterWithAuth(t)

	// Create a second admin to downgrade (the first is protected).
	second, err := authSvc.CreateLocalUser(context.Background(), "admin2", "password123", "Admin 2", "administrator", adminID)
	if err != nil {
		t.Fatalf("creating second admin: %v", err)
	}

	// Downgrade the second (non-protected) admin to operator.
	body := `{"role":"operator"}`
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/users/"+second.ID, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", second.ID)
	req = withAdminCtx(req, adminID)
	w := httptest.NewRecorder()
	r.handleUpdateUser(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("update user: status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var user auth.User
	if err := json.NewDecoder(w.Body).Decode(&user); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if user.Role != "operator" {
		t.Errorf("Role = %q, want %q", user.Role, "operator")
	}
}

func TestHandleUpdateUser_BootstrapAdmin(t *testing.T) {
	r, authSvc, adminID := testRouterWithAuth(t)

	// Create a second admin so the last-admin guard is not the reason for refusal.
	_, err := authSvc.CreateLocalUser(context.Background(), "admin2", "password123", "Admin 2", "administrator", adminID)
	if err != nil {
		t.Fatalf("creating second admin: %v", err)
	}

	// Attempt to downgrade the protected bootstrap admin.
	body := `{"role":"operator"}`
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/users/"+adminID, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", adminID)
	req = withAdminCtx(req, adminID)
	w := httptest.NewRecorder()
	r.handleUpdateUser(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("update bootstrap admin role: status = %d, want %d; body: %s", w.Code, http.StatusConflict, w.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding error response: %v", err)
	}
	if !strings.Contains(resp["error"], "bootstrap administrator") {
		t.Errorf("error = %q, want message about bootstrap administrator", resp["error"])
	}
}
