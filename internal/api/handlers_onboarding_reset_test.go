package api

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/auth"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/encryption"
	"github.com/sydlexius/stillwater/internal/i18n"
	"github.com/sydlexius/stillwater/internal/library"
	"github.com/sydlexius/stillwater/internal/platform"
	"github.com/sydlexius/stillwater/internal/provider"
)

// newTestRouterForReset creates a minimal Router with a fully-migrated DB
// for testing handlePostOnboardingReset.
func newTestRouterForReset(t *testing.T) *Router {
	t.Helper()

	db := newTestDB(t)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	enc, _, err := encryption.NewEncryptor("")
	if err != nil {
		t.Fatalf("creating encryptor: %v", err)
	}

	i18nBundle, err := i18n.LoadEmbedded()
	if err != nil {
		t.Fatalf("loading i18n bundle: %v", err)
	}

	r := NewRouter(RouterDeps{
		AuthService:       auth.NewService(db),
		PlatformService:   platform.NewService(db),
		ProviderSettings:  provider.NewSettingsService(db, enc),
		ConnectionService: connection.NewService(db, enc),
		LibraryService:    library.NewService(db),
		I18nBundle:        i18nBundle,
		DB:                db,
		Logger:            logger,
		StaticFS:          os.DirFS("../../web/static"),
	})

	return r
}

func TestHandlePostOnboardingReset_AdminClearsFlags(t *testing.T) {
	t.Parallel()
	r := newTestRouterForReset(t)

	// Seed onboarding flags to simulate a completed wizard.
	mustExec(t, r.db,
		`INSERT OR REPLACE INTO settings (key, value) VALUES ('onboarding.completed', 'true')`)
	mustExec(t, r.db,
		`INSERT OR REPLACE INTO settings (key, value) VALUES ('onboarding.step', '7')`)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/onboarding/reset", nil)
	ctx := middleware.WithTestUserID(req.Context(), "admin-user")
	ctx = middleware.WithTestRole(ctx, "administrator")
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	middleware.RequireAdmin(r.handlePostOnboardingReset)(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d body=%s", w.Code, w.Body.String())
	}

	var completed string
	if err := r.db.QueryRowContext(context.Background(),
		`SELECT value FROM settings WHERE key='onboarding.completed'`).Scan(&completed); err != nil {
		t.Fatalf("scanning onboarding.completed: %v", err)
	}
	if completed != "" {
		t.Errorf("expected onboarding.completed cleared, got %q", completed)
	}

	var step string
	if err := r.db.QueryRowContext(context.Background(),
		`SELECT value FROM settings WHERE key='onboarding.step'`).Scan(&step); err != nil {
		t.Fatalf("scanning onboarding.step: %v", err)
	}
	if step != "0" {
		t.Errorf("expected onboarding.step reset to 0, got %q", step)
	}
}

func TestHandlePostOnboardingReset_NonAdminForbidden(t *testing.T) {
	t.Parallel()
	r := newTestRouterForReset(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/onboarding/reset", nil)
	ctx := middleware.WithTestUserID(req.Context(), "regular-user")
	ctx = middleware.WithTestRole(ctx, "operator")
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	middleware.RequireAdmin(r.handlePostOnboardingReset)(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestHandlePostOnboardingReset_UnauthReturnsUnauthorized(t *testing.T) {
	t.Parallel()
	r := newTestRouterForReset(t)

	// No user ID injected: middleware.UserIDFromContext returns "".
	req := httptest.NewRequest(http.MethodPost, "/api/v1/onboarding/reset", nil)
	w := httptest.NewRecorder()
	r.handlePostOnboardingReset(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthenticated request, got %d", w.Code)
	}
}

// TestHandlePostOnboardingReset_CancelledContextReturns500 verifies that the
// handler returns 500 when the request context is canceled (simulating a DB
// error without requiring a mock database or connection failure). A canceled
// context fails at BeginTx, covering the transaction-start error path.
func TestHandlePostOnboardingReset_CancelledContextReturns500(t *testing.T) {
	t.Parallel()
	r := newTestRouterForReset(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/onboarding/reset", nil)
	ctx := middleware.WithTestUserID(req.Context(), "admin-user")
	ctx = middleware.WithTestRole(ctx, "administrator")

	// Cancel the context before passing it to the handler.  A canceled context
	// causes r.db.BeginTx to return an error, which exercises the 500 path.
	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()
	req = req.WithContext(cancelCtx)

	w := httptest.NewRecorder()
	middleware.RequireAdmin(r.handlePostOnboardingReset)(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 with canceled context, got %d body=%s", w.Code, w.Body.String())
	}
}

// TestHandlePostOnboardingReset_FirstUpsertFailsReturns500 forces the first
// in-transaction UPSERT (clearing onboarding.completed) to fail by installing
// a BEFORE INSERT trigger that raises on that key. This exercises the
// transactional handler's first-UPSERT-error branch -- without it, the only
// covered error path is BeginTx itself.
func TestHandlePostOnboardingReset_FirstUpsertFailsReturns500(t *testing.T) {
	t.Parallel()
	r := newTestRouterForReset(t)

	// Install a trigger that fails any INSERT setting key = onboarding.completed.
	// The UPSERT in the handler is `INSERT ... ON CONFLICT(key) DO UPDATE`, so
	// the BEFORE INSERT trigger fires before the conflict resolution and the
	// statement aborts with RAISE(FAIL, ...).
	mustExec(t, r.db, `
		CREATE TRIGGER fail_onboarding_completed_insert
		BEFORE INSERT ON settings
		WHEN NEW.key = 'onboarding.completed'
		BEGIN
			SELECT RAISE(FAIL, 'forced failure for test');
		END`)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/onboarding/reset", nil)
	ctx := middleware.WithTestUserID(req.Context(), "admin-user")
	ctx = middleware.WithTestRole(ctx, "administrator")
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	middleware.RequireAdmin(r.handlePostOnboardingReset)(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 when first UPSERT fails, got %d body=%s", w.Code, w.Body.String())
	}
}

// TestHandlePostOnboardingReset_SecondUpsertFailsReturns500 forces the second
// in-transaction UPSERT (resetting onboarding.step to 0) to fail while letting
// the first UPSERT succeed. This exercises the second-UPSERT-error branch and
// implicitly verifies that the deferred Rollback releases the transaction
// after a mid-transaction failure (no leaked locks would let the test DB
// continue to accept writes).
func TestHandlePostOnboardingReset_SecondUpsertFailsReturns500(t *testing.T) {
	t.Parallel()
	r := newTestRouterForReset(t)

	mustExec(t, r.db, `
		CREATE TRIGGER fail_onboarding_step_insert
		BEFORE INSERT ON settings
		WHEN NEW.key = 'onboarding.step'
		BEGIN
			SELECT RAISE(FAIL, 'forced failure for test');
		END`)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/onboarding/reset", nil)
	ctx := middleware.WithTestUserID(req.Context(), "admin-user")
	ctx = middleware.WithTestRole(ctx, "administrator")
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	middleware.RequireAdmin(r.handlePostOnboardingReset)(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 when second UPSERT fails, got %d body=%s", w.Code, w.Body.String())
	}

	// The deferred Rollback should have released the tx; confirm a fresh
	// write to a different key still succeeds (would block if the tx leaked).
	mustExec(t, r.db,
		`INSERT INTO settings (key, value) VALUES ('post_rollback_canary', 'ok')`)
}
