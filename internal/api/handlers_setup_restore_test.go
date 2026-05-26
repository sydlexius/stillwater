package api

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
)

// restoreMultipart builds a multipart/form-data body containing an envelope
// file plus a passphrase, matching what handleSetupRestore expects.
func restoreMultipart(t *testing.T, envelopeJSON []byte, passphrase string) (body *bytes.Buffer, contentType string) {
	t.Helper()
	body = &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	if err := mw.WriteField("passphrase", passphrase); err != nil {
		t.Fatalf("writing passphrase field: %v", err)
	}
	fw, err := mw.CreateFormFile("file", "backup.json")
	if err != nil {
		t.Fatalf("creating form file: %v", err)
	}
	if _, err := fw.Write(envelopeJSON); err != nil {
		t.Fatalf("writing form file: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("closing multipart writer: %v", err)
	}
	return body, mw.FormDataContentType()
}

// TestRestoreOOBE_BypassesAdminWizard pins #1114: a passphrase + backup
// uploaded to /api/v1/setup/restore on a fresh install applies the
// envelope and marks onboarding.completed = "true" so the wizard step is
// genuinely bypassed. The handler must redirect the client back to the
// root (login) and accept exactly one round-trip without requiring the
// admin-creation step to have run first.
func TestRestoreOOBE_BypassesAdminWizard(t *testing.T) {
	t.Parallel()
	_, svc, db := settingsIOTestDeps(t)

	ctx := context.Background()
	// Source instance: seed a real admin user so the envelope carries a
	// users block worth restoring.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO users (id, username, display_name, password_hash, role,
		                   auth_provider, is_active, is_protected, created_at)
		VALUES (?, 'alice', 'Alice', 'bcrypt$alice-hash', 'administrator',
		        'local', 1, 0, '2026-01-01T00:00:00Z')
	`, "u-alice"); err != nil {
		t.Fatalf("seeding source admin: %v", err)
	}
	const passphrase = "restore-pass-1"
	envBytes := buildExportedEnvelope(t, svc, passphrase)

	// Target instance: brand new DB, onboarding.completed is unset.
	target, _, targetDB := settingsIOTestDeps(t)

	body, contentType := restoreMultipart(t, envBytes, passphrase)
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/v1/setup/restore", body)
	req.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()

	target.handleSetupRestore(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("restore status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	// Every documented contract key must be present so a future regression
	// that drops one is caught here rather than silently shrinking the
	// payload to whatever subset a client happens to read.
	for _, k := range []string{"status", "redirect", "users_imported", "connections", "libraries", "api_tokens"} {
		if _, ok := raw[k]; !ok {
			t.Fatalf("missing response key %q in restore success payload; body: %s", k, w.Body.String())
		}
	}
	var resp struct {
		Status        string `json:"status"`
		Redirect      string `json:"redirect"`
		UsersImported int    `json:"users_imported"`
		Connections   int    `json:"connections"`
		Libraries     int    `json:"libraries"`
		APITokens     int    `json:"api_tokens"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal typed response: %v", err)
	}
	if resp.Status != "restored" {
		t.Errorf("status: got %q, want restored", resp.Status)
	}
	if resp.Redirect == "" {
		t.Errorf("expected non-empty redirect")
	}
	if resp.UsersImported < 1 {
		t.Errorf("users_imported: got %d, want >=1", resp.UsersImported)
	}

	// Target's user row must carry the source's id.
	var id string
	if err := targetDB.QueryRowContext(ctx,
		`SELECT id FROM users WHERE username = 'alice'`).Scan(&id); err != nil {
		t.Fatalf("scanning restored alice: %v", err)
	}
	if id != "u-alice" {
		t.Errorf("restored user id: got %q, want u-alice", id)
	}

	// onboarding.completed flipped.
	var completed string
	if err := targetDB.QueryRowContext(ctx,
		`SELECT value FROM settings WHERE key = 'onboarding.completed'`).Scan(&completed); err != nil {
		t.Fatalf("scanning onboarding.completed: %v", err)
	}
	if completed != "true" {
		t.Errorf("onboarding.completed: got %q, want true", completed)
	}
}

// TestRestoreOOBE_RejectsWhenAdminAlreadyExists pins the pre-admin gate:
// if any user row exists on the target -- even if onboarding.completed is
// still unset -- the restore endpoint returns 403. This guards against the
// "throwaway admin" foot-gun where a user creates the admin first, then
// tries to restore, and ends up with two admin rows under different UUIDs.
func TestRestoreOOBE_RejectsWhenAdminAlreadyExists(t *testing.T) {
	t.Parallel()
	_, svc, _ := settingsIOTestDeps(t)
	const passphrase = "restore-pass-3"
	envBytes := buildExportedEnvelope(t, svc, passphrase)

	// Target: seed an admin so HasUsers returns true. Onboarding flag
	// deliberately left unset so the only thing blocking the call is the
	// new HasUsers gate.
	target, _, targetDB := settingsIOTestDeps(t)
	ctx := context.Background()
	if _, err := targetDB.ExecContext(ctx, `
		INSERT INTO users (id, username, password_hash, role,
		                   auth_provider, is_active, is_protected, created_at)
		VALUES ('u-throwaway', 'throwaway', 'bcrypt$hash', 'administrator',
		        'local', 1, 0, '2026-01-01T00:00:00Z')
	`); err != nil {
		t.Fatalf("seeding throwaway admin on target: %v", err)
	}

	body, contentType := restoreMultipart(t, envBytes, passphrase)
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/v1/setup/restore", body)
	req.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()

	target.handleSetupRestore(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("restore status = %d, want 403; body: %s", w.Code, w.Body.String())
	}
}

// TestRestoreOOBE_CSRFExempt pins that POST /api/v1/setup/restore is
// reachable via the full router middleware chain without any CSRF token --
// the route is in the csrfExempt list because the setup.templ page that
// hosts the UI is rendered before any session exists. Without this
// exemption a real-browser submission from the OOBE page would 403 even
// though the rate-limited handler is intentionally pre-auth.
func TestRestoreOOBE_CSRFExempt(t *testing.T) {
	t.Parallel()
	router, svc, _ := settingsIOTestDeps(t)
	const passphrase = "restore-pass-csrf"
	envBytes := buildExportedEnvelope(t, svc, passphrase)

	body, contentType := restoreMultipart(t, envBytes, passphrase)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
		"/api/v1/setup/restore", body)
	req.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()

	// Go through the full Handler chain (which includes CSRF middleware)
	// rather than calling the bare handler. If the route were not in the
	// csrfExempt list, CSRF would 403 here before the handler ever ran.
	router.Handler(context.Background()).ServeHTTP(w, req)

	if w.Code == http.StatusForbidden {
		t.Fatalf("CSRF middleware blocked /setup/restore (status 403); body: %s", w.Body.String())
	}
	// 200 is the happy path on a fresh DB; the wider point is "not a CSRF
	// 403". Other non-403 codes would indicate a separate handler bug, so
	// assert specifically for 200 to keep the contract tight.
	if w.Code != http.StatusOK {
		t.Fatalf("restore through full chain status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
}

// TestRestoreOOBE_GatedAfterOnboarding pins the post-install lockout: once
// onboarding.completed == "true", the restore endpoint returns 403 even
// for a valid envelope + passphrase. Without this gate the route would
// become a post-install privilege-escalation vector (any logged-in user
// could replace every account on the instance).
func TestRestoreOOBE_GatedAfterOnboarding(t *testing.T) {
	t.Parallel()
	router, svc, db := settingsIOTestDeps(t)
	ctx := context.Background()
	const passphrase = "restore-pass-2"
	envBytes := buildExportedEnvelope(t, svc, passphrase)

	// Flip onboarding.completed = true on the target.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO settings (key, value, updated_at)
		VALUES ('onboarding.completed', 'true', '2026-01-01T00:00:00Z')
	`); err != nil {
		t.Fatalf("seeding onboarding.completed: %v", err)
	}

	body, contentType := restoreMultipart(t, envBytes, passphrase)
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/v1/setup/restore", body)
	req.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()

	router.handleSetupRestore(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("restore status = %d, want 403; body: %s", w.Code, w.Body.String())
	}
}

// TestRestoreOOBE_WrongPassphrase pins the user-input error path: a
// wrong passphrase returns 400, not 500, and the response includes a
// human-friendly hint instead of the raw GCM error.
func TestRestoreOOBE_WrongPassphrase(t *testing.T) {
	t.Parallel()
	_, svc, _ := settingsIOTestDeps(t)
	envBytes := buildExportedEnvelope(t, svc, "right-pass")

	target, _, _ := settingsIOTestDeps(t)
	body, contentType := restoreMultipart(t, envBytes, "wrong-pass")
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/setup/restore", body)
	req.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()

	target.handleSetupRestore(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("wrong-passphrase status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
}

// TestRestoreOOBE_MissingPassphrase pins the form-validation path: an
// upload without a passphrase returns 400 with a clear message.
func TestRestoreOOBE_MissingPassphrase(t *testing.T) {
	t.Parallel()
	router, _, _ := settingsIOTestDeps(t)

	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	fw, err := mw.CreateFormFile("file", "backup.json")
	if err != nil {
		t.Fatalf("creating form file: %v", err)
	}
	if _, err = fw.Write([]byte(`{"version":"1.4"}`)); err != nil {
		t.Fatalf("writing form file: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("closing multipart writer: %v", err)
	}

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/setup/restore", body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()

	router.handleSetupRestore(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing-passphrase status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
}

// TestRestoreOOBE_RoundTripSignIn is the full round-trip integration: an
// admin on instance A exports a backup; instance B starts fresh, restores
// the backup via OOBE, and then a login with A's original credentials
// resolves against the restored user row. This validates the password
// hash survived the envelope encrypt/decrypt cycle byte-for-byte.
func TestRestoreOOBE_RoundTripSignIn(t *testing.T) {
	t.Parallel()
	// Instance A: real auth service so we can produce a real bcrypt hash
	// for the seeded admin. The Setup helper writes hash + admin row in
	// one shot so we exercise the same code path a live install would.
	routerA, svcA, dbA := settingsIOTestDeps(t)
	const username = "carol"
	// Test-only password; the entropy is high enough to satisfy the
	// auth.Setup minimum length but the string itself is published in this
	// test file so gitleaks would otherwise flag it. The "test-fixture-"
	// prefix makes intent clear and lets gitleaks's stopwords skip it.
	const password = "test-fixture-pw-do-not-use-in-prod"
	ctx := context.Background()
	if _, err := routerA.authService.Setup(ctx, username, password); err != nil {
		t.Fatalf("seeding admin on instance A: %v", err)
	}

	const passphrase = "round-trip-pass"
	envBytes := buildExportedEnvelope(t, svcA, passphrase)

	// Instance B: fresh DB, no users.
	routerB, _, dbB := settingsIOTestDeps(t)
	body, contentType := restoreMultipart(t, envBytes, passphrase)
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/v1/setup/restore", body)
	req.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()
	routerB.handleSetupRestore(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("restore status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	// Verify the restored row matches A's hash byte-for-byte.
	var hashA, hashB string
	if err := dbA.QueryRowContext(ctx,
		`SELECT password_hash FROM users WHERE username = ?`, username).Scan(&hashA); err != nil {
		t.Fatalf("scanning A hash: %v", err)
	}
	if err := dbB.QueryRowContext(ctx,
		`SELECT password_hash FROM users WHERE username = ?`, username).Scan(&hashB); err != nil {
		t.Fatalf("scanning B hash: %v", err)
	}
	if hashA == "" {
		t.Fatal("instance A's seeded admin has no bcrypt hash")
	}
	if hashA != hashB {
		t.Errorf("password_hash drift across restore: A=%q B=%q", hashA, hashB)
	}

	// Sign in on B with A's plaintext credentials. Login returns a session
	// id on success; the test passes if the bcrypt hash verification did
	// not reject the supplied password.
	if _, err := routerB.authService.Login(ctx, username, password); err != nil {
		t.Errorf("Login on B failed: %v", err)
	}
}
