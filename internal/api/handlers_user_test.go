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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

func TestHandleDeleteUser_HappyPath(t *testing.T) {
	t.Parallel()
	r, authSvc, adminID := testRouterWithAuth(t)

	target, err := authSvc.CreateLocalUser(context.Background(), "op1", "password123", "Op One", "operator", adminID)
	if err != nil {
		t.Fatalf("creating target: %v", err)
	}

	body := `{"reason":"left team"}`
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/users/"+target.ID+"/account/permanent", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", target.ID)
	req = withAdminCtx(req, adminID)
	w := httptest.NewRecorder()
	r.handleDeleteUser(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("delete user: status = %d, want %d; body: %s", w.Code, http.StatusNoContent, w.Body.String())
	}
}

func TestHandleDeleteUser_PreventsSelfDelete(t *testing.T) {
	t.Parallel()
	r, _, adminID := testRouterWithAuth(t)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/users/"+adminID+"/account/permanent", nil)
	req.SetPathValue("id", adminID)
	req = withAdminCtx(req, adminID)
	w := httptest.NewRecorder()
	r.handleDeleteUser(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("self-delete: status = %d, want %d; body: %s", w.Code, http.StatusForbidden, w.Body.String())
	}
	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if !strings.Contains(strings.ToLower(resp["error"]), "account settings") {
		t.Errorf("error = %q, want message routing to Account Settings", resp["error"])
	}
}

func TestHandleDeleteUser_PreventsProtected(t *testing.T) {
	t.Parallel()
	r, authSvc, bootstrapID := testRouterWithAuth(t)

	// Second admin so the last-admin guard isn't what blocks the delete;
	// the protected-user guard must fire first.
	other, err := authSvc.CreateLocalUser(context.Background(), "admin2", "password123", "Admin 2", "administrator", bootstrapID)
	if err != nil {
		t.Fatalf("creating second admin: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/users/"+bootstrapID+"/account/permanent", nil)
	req.SetPathValue("id", bootstrapID)
	req = withAdminCtx(req, other.ID)
	w := httptest.NewRecorder()
	r.handleDeleteUser(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("delete protected: status = %d, want %d; body: %s", w.Code, http.StatusConflict, w.Body.String())
	}
}

func TestHandleListUsers_InactiveFilter(t *testing.T) {
	t.Parallel()
	r, authSvc, adminID := testRouterWithAuth(t)

	if _, err := authSvc.CreateLocalUser(context.Background(), "neverin", "password123", "Never", "operator", adminID); err != nil {
		t.Fatalf("creating neverin: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/users?inactive_only=true", nil)
	req = withAdminCtx(req, adminID)
	w := httptest.NewRecorder()
	r.handleListUsers(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("list inactive: status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var users []auth.User
	if err := json.NewDecoder(w.Body).Decode(&users); err != nil {
		t.Fatalf("decoding users: %v", err)
	}
	if len(users) < 1 {
		t.Errorf("expected at least one never-logged-in user, got %d", len(users))
	}
	for _, u := range users {
		if u.LastLogin != nil && *u.LastLogin != "" {
			t.Errorf("inactive_only returned user with LastLogin = %v", *u.LastLogin)
		}
	}
}

func TestHandleDeleteUser_NotFound(t *testing.T) {
	t.Parallel()
	r, _, adminID := testRouterWithAuth(t)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/users/no-such-id/account/permanent", nil)
	req.SetPathValue("id", "no-such-id")
	req = withAdminCtx(req, adminID)
	w := httptest.NewRecorder()
	r.handleDeleteUser(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("not-found: status = %d, want %d; body: %s", w.Code, http.StatusNotFound, w.Body.String())
	}
}

func TestHandleDeleteUser_MissingID(t *testing.T) {
	t.Parallel()
	r, _, adminID := testRouterWithAuth(t)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/users//account/permanent", nil)
	// Intentionally no SetPathValue("id", ...); handler must reject blank ID.
	req = withAdminCtx(req, adminID)
	w := httptest.NewRecorder()
	r.handleDeleteUser(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing id: status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

func TestHandleListUsers_DefaultNoFilter(t *testing.T) {
	t.Parallel()
	r, _, adminID := testRouterWithAuth(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/users", nil)
	req = withAdminCtx(req, adminID)
	w := httptest.NewRecorder()
	r.handleListUsers(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("list default: status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestHandleListUsers_HTMXFragment(t *testing.T) {
	t.Parallel()
	r, _, adminID := testRouterWithAuth(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/users", nil)
	req.Header.Set("HX-Request", "true")
	req = withAdminCtx(req, adminID)
	w := httptest.NewRecorder()
	r.handleListUsers(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("list htmx: status = %d, want %d", w.Code, http.StatusOK)
	}
	if !strings.Contains(w.Body.String(), "user-row-") {
		t.Errorf("HTMX fragment missing user-row-* markup; body: %s", w.Body.String())
	}
}

func TestRollbackFederatedSetupUser_WipesProtected(t *testing.T) {
	t.Parallel()
	r, authSvc, _ := testRouterWithAuth(t)

	// testRouterWithAuth seeds a protected admin via Setup. The rollback
	// helper must clear is_protected and DELETE that row, the same flow
	// that fires when a federated Setup attempt fails its settings write.
	r.rollbackFederatedSetupUser(context.Background(), "local", "")

	// admin had auth_provider='local' provider_id=''; check it's gone.
	users, err := authSvc.ListUsers(context.Background())
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	for _, u := range users {
		if u.AuthProvider == "local" && u.ProviderID == "" {
			t.Errorf("rollback left protected admin in place: %+v", u)
		}
	}
}

func TestRollbackFederatedSetupUser_LogsErrorOnClosedDB(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithAuth(t)
	if err := r.db.Close(); err != nil {
		t.Fatalf("closing db: %v", err)
	}
	// Helper logs and returns; the test passes if it doesn't panic and
	// exercises the error-log branches for coverage.
	r.rollbackFederatedSetupUser(context.Background(), "local", "")
}

func TestRollbackLocalSetupUserByID_LogsErrorOnClosedDB(t *testing.T) {
	t.Parallel()
	r, _, adminID := testRouterWithAuth(t)
	if err := r.db.Close(); err != nil {
		t.Fatalf("closing db: %v", err)
	}
	r.rollbackLocalSetupUserByID(context.Background(), adminID)
}

func TestRollbackLocalSetupUserByID_WipesProtected(t *testing.T) {
	t.Parallel()
	r, _, adminID := testRouterWithAuth(t)

	r.rollbackLocalSetupUserByID(context.Background(), adminID)

	var count int
	if err := r.db.QueryRow("SELECT COUNT(*) FROM users WHERE id = ?", adminID).Scan(&count); err != nil {
		t.Fatalf("counting users: %v", err)
	}
	if count != 0 {
		t.Errorf("rollback failed to delete user %s (still %d row(s))", adminID, count)
	}
}

func TestDeleteUser_ConcurrentAdminDemotion(t *testing.T) {
	t.Parallel()
	// Two non-bootstrap admins delete each other simultaneously; the
	// withImmediateTx last-admin guard must let at most one win so we
	// never end up with zero active admins. Race detector covers the
	// shared connection pool.
	r, authSvc, _ := testRouterWithAuth(t)

	// testRouterWithAuth runs Setup which seeds a protected bootstrap
	// admin. Clear it before the goroutines race so the active-admin
	// count is exactly 2 (a1 + a2); otherwise the bootstrap shields the
	// pair from the last-admin guard and both deletes succeed.
	if _, err := r.db.ExecContext(context.Background(), `UPDATE users SET is_protected = 0`); err != nil {
		t.Fatalf("clearing is_protected: %v", err)
	}
	if _, err := r.db.ExecContext(context.Background(), `DELETE FROM users`); err != nil {
		t.Fatalf("wiping bootstrap admin: %v", err)
	}

	a1, err := authSvc.CreateLocalUser(context.Background(), "ax1", "password123", "A1", "administrator", "")
	if err != nil {
		t.Fatalf("creating a1: %v", err)
	}
	a2, err := authSvc.CreateLocalUser(context.Background(), "ax2", "password123", "A2", "administrator", "")
	if err != nil {
		t.Fatalf("creating a2: %v", err)
	}

	errCh := make(chan error, 2)
	go func() { errCh <- authSvc.DeleteUser(context.Background(), a1.ID, a2.ID, "") }()
	go func() { errCh <- authSvc.DeleteUser(context.Background(), a2.ID, a1.ID, "") }()
	err1 := <-errCh
	err2 := <-errCh
	// At least one delete must fail; if both succeed the last-admin
	// guard didn't fire. The bootstrap admin alone could keep the
	// global count above zero, so we also need a pair-scoped count to
	// catch the case where both ax1 and ax2 were wiped.
	if err1 == nil && err2 == nil {
		t.Fatalf("expected at least one concurrent delete to be rejected; got err1=%v err2=%v", err1, err2)
	}

	users, err := authSvc.ListUsers(context.Background())
	if err != nil {
		t.Fatalf("ListUsers after concurrent delete: %v", err)
	}
	var activePairAdmins int
	for _, u := range users {
		if (u.ID == a1.ID || u.ID == a2.ID) && u.IsActive && u.Role == "administrator" {
			activePairAdmins++
		}
	}
	if activePairAdmins == 0 {
		t.Fatalf("concurrent deletes removed both target admins; errs %v / %v", err1, err2)
	}
}
