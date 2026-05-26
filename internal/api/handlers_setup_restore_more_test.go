package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/auth"
	"github.com/sydlexius/stillwater/internal/settingsio"
)

// TestRestoreOOBE_NilService pins the early ServiceUnavailable path when
// the router was constructed without a settingsIOService (e.g. a partial
// build that omitted the import wiring). The handler must return 503
// before touching the DB or parsing the multipart body.
func TestRestoreOOBE_NilService(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	db := newTestDB(t)
	r := NewRouter(RouterDeps{
		AuthService: auth.NewService(db),
		DB:          db,
		Logger:      logger,
		StaticFS:    os.DirFS("../../web/static"),
	})

	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	if err := mw.Close(); err != nil {
		t.Fatalf("closing multipart writer: %v", err)
	}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/setup/restore", body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()

	r.handleSetupRestore(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503; body: %s", w.Code, w.Body.String())
	}
}

// TestRestoreOOBE_MalformedMultipart pins the 400 returned when the
// request body is not valid multipart data. ParseMultipartForm fails
// before the handler reaches the file field.
func TestRestoreOOBE_MalformedMultipart(t *testing.T) {
	t.Parallel()
	router, _, _ := settingsIOTestDeps(t)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
		"/api/v1/setup/restore", strings.NewReader("this is not multipart"))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=nope")
	w := httptest.NewRecorder()

	router.handleSetupRestore(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
}

// TestRestoreOOBE_MissingFileField pins the 400 returned when the
// passphrase is supplied but the file field is absent. This guards
// against a UI bug that submits only the passphrase.
func TestRestoreOOBE_MissingFileField(t *testing.T) {
	t.Parallel()
	router, _, _ := settingsIOTestDeps(t)

	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	if err := mw.WriteField("passphrase", "secret"); err != nil {
		t.Fatalf("writing passphrase: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("closing multipart writer: %v", err)
	}

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
		"/api/v1/setup/restore", body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()

	router.handleSetupRestore(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
}

// TestRestoreOOBE_InvalidJSONEnvelope pins the 400 returned when the
// uploaded file is not a valid Stillwater envelope JSON. The handler
// must surface a user-friendly hint rather than the raw unmarshal error.
func TestRestoreOOBE_InvalidJSONEnvelope(t *testing.T) {
	t.Parallel()
	router, _, _ := settingsIOTestDeps(t)

	body, contentType := restoreMultipart(t, []byte("not json at all {{{"), "secret")
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
		"/api/v1/setup/restore", body)
	req.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()

	router.handleSetupRestore(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "valid Stillwater export") {
		t.Errorf("body lacks user-friendly hint: %s", w.Body.String())
	}
}

// TestRestoreOOBE_UnsupportedVersion pins the 400 mapping for
// settingsio.ErrUnsupportedVersion. A future-version envelope must not
// surface as a 500.
func TestRestoreOOBE_UnsupportedVersion(t *testing.T) {
	t.Parallel()
	router, _, _ := settingsIOTestDeps(t)

	// A well-formed envelope with an unsupported version field. Import
	// rejects the version after the empty-data guard, so include a stub
	// Data value; the version check fires before decryption.
	envelope := settingsio.Envelope{Version: "99.9", Data: "stub", Salt: "stub"}
	envBytes, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshaling envelope: %v", err)
	}

	body, contentType := restoreMultipart(t, envBytes, "any-pass")
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
		"/api/v1/setup/restore", body)
	req.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()

	router.handleSetupRestore(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "unsupported format version") {
		t.Errorf("body lacks unsupported-version hint: %s", w.Body.String())
	}
}

// TestRestoreOOBE_HTMXResponseShape pins the HX-Request branch of the
// handler: on success the writer must emit an HX-Redirect header and an
// HTML body fragment, not a JSON payload. Without HX-Request the same
// success path returns JSON (covered by TestRestoreOOBE_BypassesAdminWizard).
func TestRestoreOOBE_HTMXResponseShape(t *testing.T) {
	t.Parallel()
	_, svc, db := settingsIOTestDeps(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO users (id, username, password_hash, role,
		                   auth_provider, is_active, is_protected, created_at)
		VALUES ('u-htmx', 'htmxer', 'bcrypt$hash', 'administrator',
		        'local', 1, 0, '2026-01-01T00:00:00Z')
	`); err != nil {
		t.Fatalf("seeding source admin: %v", err)
	}
	const passphrase = "htmx-pass"
	envBytes := buildExportedEnvelope(t, svc, passphrase)

	target, _, _ := settingsIOTestDeps(t)
	body, contentType := restoreMultipart(t, envBytes, passphrase)
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/v1/setup/restore", body)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()

	target.handleSetupRestore(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if w.Header().Get("HX-Redirect") == "" {
		t.Error("expected non-empty HX-Redirect header")
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/html" {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	if !strings.Contains(w.Body.String(), "Restore complete") {
		t.Errorf("body lacks success copy: %s", w.Body.String())
	}
}

// TestRestoreOOBE_HTMXErrorShape pins that an error reply on an
// HX-Request returns an HTML error fragment (not JSON). The setup.templ
// UI swaps that fragment into the page; if it received JSON the user
// would see a raw object.
func TestRestoreOOBE_HTMXErrorShape(t *testing.T) {
	t.Parallel()
	router, _, _ := settingsIOTestDeps(t)

	body, contentType := restoreMultipart(t, []byte("not json"), "secret")
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
		"/api/v1/setup/restore", body)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()

	router.handleSetupRestore(w, req)

	// HX-Request error responses must carry the intended error status on
	// the wire so HTMX's swap-on-error path activates. A malformed
	// envelope body (not-JSON) routes through writeRestoreErr with status
	// 400; the prior implementation left WriteHeader unset and HTMX saw
	// an implicit 200, silently treating the error fragment as success.
	if w.Code != http.StatusBadRequest {
		t.Errorf("HX error status = %d, want %d (writeRestoreErr must set the wire status)",
			w.Code, http.StatusBadRequest)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/html" {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	if !strings.Contains(w.Body.String(), "text-red-600") {
		t.Errorf("error fragment missing red error class: %s", w.Body.String())
	}
}

// TestRestoreOOBE_GenericInternalError pins the default-branch mapping
// in the Import error switch: anything that is not ErrWrongPassphrase /
// ErrUnsupportedVersion / ErrUserIDCollision must surface as a 500 with
// the generic "see server logs" hint -- never the raw internal error
// (which could leak passphrase fragments via wrap chains).
func TestRestoreOOBE_GenericInternalError(t *testing.T) {
	t.Parallel()
	router, _, _ := settingsIOTestDeps(t)

	// An envelope with version 1.4 (accepted), but empty Data triggers
	// the "empty export data" error inside ImportWithOptions; that error
	// is not one of the sentinel errors, so the handler routes to the
	// default 500 branch.
	envelope := settingsio.Envelope{Version: "1.4", Data: "", Salt: ""}
	envBytes, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshaling envelope: %v", err)
	}

	body, contentType := restoreMultipart(t, envBytes, "any-pass")
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
		"/api/v1/setup/restore", body)
	req.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()

	router.handleSetupRestore(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500; body: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "see server logs") {
		t.Errorf("body lacks generic hint: %s", w.Body.String())
	}
}

// TestRestoreOOBE_OversizedFile pins the 413 response when the uploaded
// file exceeds maxImportSize. The multipart reader honors the limit so
// we exercise the inner LimitReader path by attaching a body larger than
// maxImportSize.
func TestRestoreOOBE_OversizedFile(t *testing.T) {
	t.Parallel()
	router, _, _ := settingsIOTestDeps(t)

	// Build a payload that exceeds maxImportSize after multipart framing.
	// 11 MB of 'a' bytes is comfortably over the 10 MB cap.
	huge := bytes.Repeat([]byte("a"), 11<<20)

	body, contentType := restoreMultipart(t, huge, "secret")
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
		"/api/v1/setup/restore", body)
	req.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()

	router.handleSetupRestore(w, req)

	// The documented oversized-upload contract is 413. The handler must
	// reject the payload via the LimitReader path before it reaches the
	// JSON unmarshal step.
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413; body: %s", w.Code, w.Body.String())
	}
}
