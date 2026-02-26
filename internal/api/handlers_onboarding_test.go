package api

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/auth"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/database"
	"github.com/sydlexius/stillwater/internal/encryption"
	"github.com/sydlexius/stillwater/internal/library"
	"github.com/sydlexius/stillwater/internal/platform"
	"github.com/sydlexius/stillwater/internal/provider"
)

func testRouterForOnboarding(t *testing.T) *Router {
	t.Helper()

	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	if err := database.Migrate(db); err != nil {
		t.Fatalf("running migrations: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	enc, _, err := encryption.NewEncryptor("")
	if err != nil {
		t.Fatalf("creating encryptor: %v", err)
	}

	r := NewRouter(RouterDeps{
		AuthService:       auth.NewService(db),
		PlatformService:   platform.NewService(db),
		ProviderSettings:  provider.NewSettingsService(db, enc),
		ConnectionService: connection.NewService(db, enc),
		LibraryService:    library.NewService(db),
		DB:                db,
		Logger:            logger,
		StaticDir:         "../../web/static",
	})

	return r
}

// onboardingRequest creates a GET /setup/wizard request with a test user ID in the context.
func onboardingRequest() *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/setup/wizard", nil)
	ctx := middleware.WithTestUserID(req.Context(), "test-user-id")
	return req.WithContext(ctx)
}

func TestHandleOnboardingPage_DefaultStep(t *testing.T) {
	r := testRouterForOnboarding(t)

	req := onboardingRequest()
	w := httptest.NewRecorder()

	r.handleOnboardingPage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	body := w.Body.String()
	// Step 0 should be the default; body should contain data-current-step="0"
	if !strings.Contains(body, `data-current-step="0"`) {
		t.Error("expected data-current-step=\"0\" in response body")
	}
}

func TestHandleOnboardingPage_StoredSteps(t *testing.T) {
	r := testRouterForOnboarding(t)

	tests := []struct {
		name     string
		stored   string
		wantStep string
	}{
		{"step 1", "1", "1"},
		{"step 2", "2", "2"},
		{"step 3", "3", "3"},
		{"step 4", "4", "4"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set the onboarding step in settings.
			_, err := r.db.ExecContext(context.Background(),
				`INSERT OR REPLACE INTO settings (key, value) VALUES ('onboarding.step', ?)`, tt.stored)
			if err != nil {
				t.Fatalf("inserting setting: %v", err)
			}

			req := onboardingRequest()
			w := httptest.NewRecorder()

			r.handleOnboardingPage(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
			}

			body := w.Body.String()
			want := `data-current-step="` + tt.wantStep + `"`
			if !strings.Contains(body, want) {
				t.Errorf("expected %s in response body", want)
			}
		})
	}
}

func TestHandleOnboardingPage_InvalidStep(t *testing.T) {
	r := testRouterForOnboarding(t)

	// Store an invalid value.
	_, err := r.db.ExecContext(context.Background(),
		`INSERT OR REPLACE INTO settings (key, value) VALUES ('onboarding.step', 'garbage')`)
	if err != nil {
		t.Fatalf("inserting setting: %v", err)
	}

	req := onboardingRequest()
	w := httptest.NewRecorder()

	r.handleOnboardingPage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	body := w.Body.String()
	// Invalid value should clamp to 0.
	if !strings.Contains(body, `data-current-step="0"`) {
		t.Error("expected data-current-step=\"0\" for invalid stored value")
	}
}

func TestHandleOnboardingPage_CompletedRedirects(t *testing.T) {
	r := testRouterForOnboarding(t)

	// Mark onboarding as completed.
	_, err := r.db.ExecContext(context.Background(),
		`INSERT OR REPLACE INTO settings (key, value) VALUES ('onboarding.completed', 'true')`)
	if err != nil {
		t.Fatalf("inserting setting: %v", err)
	}

	req := onboardingRequest()
	w := httptest.NewRecorder()

	r.handleOnboardingPage(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d (redirect); body: %s", w.Code, http.StatusSeeOther, w.Body.String())
	}

	loc := w.Header().Get("Location")
	if loc != "/" {
		t.Errorf("redirect location = %q, want %q", loc, "/")
	}
}

func TestHandleOnboardingPage_NoAuth(t *testing.T) {
	r := testRouterForOnboarding(t)

	// Request without user ID in context.
	req := httptest.NewRequest(http.MethodGet, "/setup/wizard", nil)
	w := httptest.NewRecorder()

	r.handleOnboardingPage(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d (redirect); body: %s", w.Code, http.StatusSeeOther, w.Body.String())
	}
}
