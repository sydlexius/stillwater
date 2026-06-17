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
	"github.com/sydlexius/stillwater/internal/encryption"
	"github.com/sydlexius/stillwater/internal/i18n"
	"github.com/sydlexius/stillwater/internal/langpref"
	"github.com/sydlexius/stillwater/internal/library"
	"github.com/sydlexius/stillwater/internal/platform"
	"github.com/sydlexius/stillwater/internal/provider"
)

func testRouterForOnboarding(t *testing.T) *Router {
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
		SessionSecret:     testSessionSecret,
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

// onboardingRequest creates a GET /setup/wizard request with a test user ID in the context.
func onboardingRequest() *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/setup/wizard", nil)
	ctx := middleware.WithTestUserID(req.Context(), "test-user-id")
	return req.WithContext(ctx)
}

func TestHandleOnboardingPage_DefaultStep(t *testing.T) {
	t.Parallel()
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
	// Empty database should report 0 unidentified artists.
	if !strings.Contains(body, `data-unidentified-count="0"`) {
		t.Error("expected data-unidentified-count=\"0\" for empty database")
	}
}

func TestHandleOnboardingPage_StoredSteps(t *testing.T) {
	t.Parallel()
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
		{"step 5", "5", "5"},
		{"step 6", "6", "6"},
		{"step 7", "7", "7"},
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	r := testRouterForOnboarding(t)

	// Request without user ID in context.
	req := httptest.NewRequest(http.MethodGet, "/setup/wizard", nil)
	w := httptest.NewRecorder()

	r.handleOnboardingPage(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d (redirect); body: %s", w.Code, http.StatusSeeOther, w.Body.String())
	}
}

func TestHandleOnboardingPage_UnidentifiedCount(t *testing.T) {
	t.Parallel()
	r := testRouterForOnboarding(t)
	ctx := context.Background()

	// Insert artist WITH a MusicBrainz ID (identified).
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO artists (id, name, sort_name, path) VALUES ('a1', 'Pink Floyd', 'Pink Floyd', '/music/Pink Floyd')`)
	if err != nil {
		t.Fatalf("inserting artist a1: %v", err)
	}
	_, err = r.db.ExecContext(ctx,
		`INSERT INTO artist_provider_ids (artist_id, provider, provider_id) VALUES ('a1', 'musicbrainz', 'mb-1234')`)
	if err != nil {
		t.Fatalf("inserting provider ID for a1: %v", err)
	}

	// Insert artist WITHOUT a MusicBrainz ID (unidentified).
	_, err = r.db.ExecContext(ctx,
		`INSERT INTO artists (id, name, sort_name, path) VALUES ('a2', 'Unknown Band', 'Unknown Band', '/music/Unknown Band')`)
	if err != nil {
		t.Fatalf("inserting artist a2: %v", err)
	}

	// Insert excluded artist WITHOUT a MusicBrainz ID (should NOT count).
	_, err = r.db.ExecContext(ctx,
		`INSERT INTO artists (id, name, sort_name, path, is_excluded) VALUES ('a3', 'Various Artists', 'Various Artists', '/music/Various Artists', 1)`)
	if err != nil {
		t.Fatalf("inserting artist a3: %v", err)
	}

	// Insert locked artist WITHOUT a MusicBrainz ID (should NOT count --
	// bulk-identify skips locked artists).
	_, err = r.db.ExecContext(ctx,
		`INSERT INTO artists (id, name, sort_name, path, locked, locked_at) VALUES ('a4', 'Locked Artist', 'Locked Artist', '/music/Locked Artist', 1, '2026-01-01T00:00:00Z')`)
	if err != nil {
		t.Fatalf("inserting artist a4: %v", err)
	}

	req := onboardingRequest()
	w := httptest.NewRecorder()

	r.handleOnboardingPage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	body := w.Body.String()
	// Only the non-excluded artist without MBID should be counted.
	if !strings.Contains(body, `data-unidentified-count="1"`) {
		t.Errorf("expected data-unidentified-count=\"1\" in response body, got body:\n%s", body)
	}
}

func TestHandleOnboardingPage_UserAuthProvider(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		authProvider  string
		insertUser    bool
		wantAttribute string
	}{
		{"local auth", "local", true, `data-user-auth-provider="local"`},
		{"emby federated", "emby", true, `data-user-auth-provider="emby"`},
		{"jellyfin federated", "jellyfin", true, `data-user-auth-provider="jellyfin"`},
		{"oidc federated", "oidc", true, `data-user-auth-provider="oidc"`},
		{"user not found", "", false, `data-user-auth-provider=""`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a fresh router for each test case to avoid username conflicts.
			r := testRouterForOnboarding(t)
			ctx := context.Background()

			if tt.insertUser {
				// Insert a test user with the specified auth provider.
				_, err := r.db.ExecContext(ctx,
					`INSERT INTO users (id, username, display_name, role, auth_provider, is_active, is_protected, created_at, updated_at)
					 VALUES ('test-user-id', 'testuser', 'Test User', 'admin', ?, 1, 0, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
					tt.authProvider)
				if err != nil {
					t.Fatalf("inserting test user: %v", err)
				}
			}

			req := onboardingRequest()
			w := httptest.NewRecorder()

			r.handleOnboardingPage(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
			}

			body := w.Body.String()
			if !strings.Contains(body, tt.wantAttribute) {
				t.Errorf("expected %s in response body", tt.wantAttribute)
			}
		})
	}
}

// TestHandleOnboardingPage_LanguagePreferences verifies that the user's stored
// language preferences are rendered into the language step input field.
func TestHandleOnboardingPage_LanguagePreferences(t *testing.T) {
	t.Parallel()
	r := testRouterForOnboarding(t)
	ctx := context.Background()

	// Write language preferences via the repository, just as the langpref
	// save endpoint would.
	langRepo := langpref.NewRepository(r.db)
	if err := langRepo.Set(ctx, "test-user-id", []string{"fr", "de", "en"}); err != nil {
		t.Fatalf("setting language preferences: %v", err)
	}

	req := onboardingRequest()
	w := httptest.NewRecorder()
	r.handleOnboardingPage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	// The shared LanguagePicker component embeds the tags as a JSON
	// array on data-languages; that's what the script reads on init.
	body := w.Body.String()
	if !strings.Contains(body, `data-languages="[&#34;fr&#34;,&#34;de&#34;,&#34;en&#34;]"`) {
		t.Errorf("expected language preferences serialized as data-languages JSON array in response body")
	}
}

// TestHandleOnboardingPage_DefaultLanguages verifies that when no language
// preferences are stored the page still renders without error and includes
// the default language tag.
func TestHandleOnboardingPage_DefaultLanguages(t *testing.T) {
	t.Parallel()
	r := testRouterForOnboarding(t)

	// No preferences stored -- handler should fall back to langpref.DefaultTags().
	req := onboardingRequest()
	w := httptest.NewRecorder()
	r.handleOnboardingPage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	// DefaultTags returns ["en"]; assert the concrete serialized payload on
	// data-languages rather than a loose "en" substring (which can
	// false-pass on unrelated markup or text content).
	body := w.Body.String()
	if !strings.Contains(body, `data-languages="[&#34;en&#34;]"`) {
		t.Errorf("expected default language payload data-languages=[\"en\"] in response body")
	}
}

// TestHandleOnboardingPage_ForeignFileCountShown verifies that when foreign
// files exist the baseline sub-section is rendered in the Discovery step.
func TestHandleOnboardingPage_ForeignFileCountShown(t *testing.T) {
	t.Parallel()
	r := testRouterForOnboarding(t)
	ctx := context.Background()

	// NewRouter wires foreignRepo automatically when DB is provided.
	if r.foreignRepo == nil {
		t.Fatal("foreignRepo should be wired by NewRouter when DB is provided")
	}

	// Seed a foreign file record so Count() returns > 0.
	// FK enforcement is off in test DBs (EnableForeignKeys is production-only),
	// so we can use an empty artist_id without a parent artists row.
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO foreign_files (id, artist_id, file_path, file_name, size_bytes, detected_at)
		 VALUES ('ff-1', '', '/music/Pink Floyd/cover.jpg', 'cover.jpg', 12345, '2026-01-01T00:00:00Z')`)
	if err != nil {
		t.Fatalf("inserting foreign file: %v", err)
	}

	req := onboardingRequest()
	w := httptest.NewRecorder()
	r.handleOnboardingPage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	// The baseline sub-section should be rendered when count > 0.
	body := w.Body.String()
	if !strings.Contains(body, "baseline-subsection") {
		t.Errorf("expected baseline-subsection in response body when foreign files present")
	}
}

// TestHandleOnboardingPage_LangPrefError verifies that when the language
// preference query fails (simulated by dropping the user_preferences table),
// the handler falls back to default tags and still returns 200.
func TestHandleOnboardingPage_LangPrefError(t *testing.T) {
	// Not parallel: schema mutation must not race with the other test's DB.
	r := testRouterForOnboarding(t)

	// Drop user_preferences so langRepo.Get returns an error. All other
	// queries in handleOnboardingPage (settings, artists, connections, etc.)
	// use separate tables and succeed normally.
	if _, err := r.db.ExecContext(context.Background(),
		`DROP TABLE IF EXISTS user_preferences`); err != nil {
		t.Fatalf("dropping user_preferences: %v", err)
	}

	req := onboardingRequest()
	w := httptest.NewRecorder()
	r.handleOnboardingPage(w, req)

	// The fallback must succeed and render the page (not a 5xx error).
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	// The page must still contain the default serialized language payload
	// from the fallback. Tightened to the concrete data-languages JSON so
	// the test can't false-pass on unrelated "en" occurrences.
	body := w.Body.String()
	if !strings.Contains(body, `data-languages="[&#34;en&#34;]"`) {
		t.Errorf("expected default language payload data-languages=[\"en\"] in fallback response body")
	}
}

// TestHandleOnboardingPage_ForeignCountError verifies that when the foreign
// file count query fails (simulated by dropping the foreign_files table),
// the handler falls back to count=0 and still returns 200 without the
// baseline sub-section.
func TestHandleOnboardingPage_ForeignCountError(t *testing.T) {
	// Not parallel: schema mutation must not race with the other test's DB.
	r := testRouterForOnboarding(t)

	// Drop foreign_files so Count() returns an error. The user_preferences
	// and other tables used earlier in the handler still exist.
	if _, err := r.db.ExecContext(context.Background(),
		`DROP TABLE IF EXISTS foreign_files`); err != nil {
		t.Fatalf("dropping foreign_files: %v", err)
	}

	req := onboardingRequest()
	w := httptest.NewRecorder()
	r.handleOnboardingPage(w, req)

	// The handler must fall back gracefully and render the page.
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	// With count=0 fallback, the baseline sub-section must not appear.
	body := w.Body.String()
	if strings.Contains(body, "baseline-subsection") {
		t.Errorf("unexpected baseline-subsection when foreign_files table unavailable")
	}
}

// TestHandleOnboardingPage_ForeignFileCountZero verifies that when no foreign
// files exist the baseline sub-section is NOT rendered.
func TestHandleOnboardingPage_ForeignFileCountZero(t *testing.T) {
	t.Parallel()
	r := testRouterForOnboarding(t)

	// No rows in foreign_files table -- Count() returns 0, so the baseline
	// sub-section must not appear.
	req := onboardingRequest()
	w := httptest.NewRecorder()
	r.handleOnboardingPage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	body := w.Body.String()
	if strings.Contains(body, "baseline-subsection") {
		t.Errorf("unexpected baseline-subsection in response body when no foreign files present")
	}
}
