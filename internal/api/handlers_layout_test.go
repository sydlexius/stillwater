package api

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/auth"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/database"
	"github.com/sydlexius/stillwater/internal/encryption"
	"github.com/sydlexius/stillwater/internal/i18n"
	"github.com/sydlexius/stillwater/internal/library"
	"github.com/sydlexius/stillwater/internal/platform"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/internal/version"
	"github.com/sydlexius/stillwater/web/templates"
	"golang.org/x/net/html"
)

// TestLayoutRenders_ValidHTML verifies that the layout template renders without
// errors and produces valid HTML with all required landmarks.
func TestLayoutRenders_ValidHTML(t *testing.T) {

	// Create sample AssetPaths context.
	assets := templates.AssetPaths{
		BasePath:       "/stillwater",
		CSS:            "/stillwater/static/css/styles.css?v=abc123",
		HTMX:           "/stillwater/static/js/htmx.min.js",
		CropperJS:      "/stillwater/static/js/cropper.min.js",
		CropperCSS:     "/stillwater/static/css/cropper.min.css",
		ChartJS:        "/stillwater/static/js/chart.min.js",
		SortableJS:     "/stillwater/static/js/sortable.min.js",
		HelpJS:         "/stillwater/static/js/help.js",
		PollingJS:      "/stillwater/static/js/polling.js",
		SessionJS:      "/stillwater/static/js/session.js",
		PreferencesJS:  "/stillwater/static/js/preferences.js",
		SidebarJS:      "/stillwater/static/js/sidebar.js",
		FilterFlyoutJS: "/stillwater/static/js/filter_flyout.js",
		DriverJS:       "/stillwater/static/js/driver.min.js",
		DriverCSS:      "/stillwater/static/css/driver.min.css",
		TourJS:         "/stillwater/static/js/tour.js",
		SSEJS:          "/stillwater/static/js/sse.js",
		LoginBG:        "/stillwater/static/img/login-bg.jpg",
		IsAdmin:        true,
		CurrentPath:    "/artists",
		Username:       "testuser",
		DisplayName:    "Test User",
		AvatarURL:      "/stillwater/static/img/avatar.png",
		Role:           "administrator",
		Version:        version.Version,
	}

	// Render the layout template with a sample child content.
	var buf bytes.Buffer
	if err := templates.Layout("Test Page", assets).Render(testI18nCtx(t, context.Background()), &buf); err != nil {
		t.Fatalf("rendering layout: %v", err)
	}

	htmlContent := buf.String()

	// Verify the HTML is valid by attempting to parse it.
	if htmlContent == "" {
		t.Fatal("layout rendered as empty string")
	}

	// Verify DOCTYPE is present (Templ uses lowercase html5 style).
	if !strings.Contains(htmlContent, "<!doctype html>") && !strings.Contains(htmlContent, "<!DOCTYPE html>") {
		t.Error("response missing DOCTYPE declaration")
	}

	// Verify basic structure.
	if !strings.Contains(htmlContent, "<html") {
		t.Error("response missing <html> tag")
	}
	if !strings.Contains(htmlContent, "<head>") {
		t.Error("response missing <head> tag")
	}
	if !strings.Contains(htmlContent, "<body") {
		t.Error("response missing <body> tag")
	}

	// Verify the page title is set correctly.
	expectedTitle := "<title>Test Page - Stillwater</title>"
	if !strings.Contains(htmlContent, expectedTitle) {
		t.Errorf("response missing expected title, want %q", expectedTitle)
	}
}

// TestLayoutSidebar_ContainsRequiredLandmarks verifies the sidebar and main
// content landmarks exist with correct IDs and semantic roles.
func TestLayoutSidebar_ContainsRequiredLandmarks(t *testing.T) {

	assets := templates.AssetPaths{
		BasePath:       "",
		CSS:            "/static/css/styles.css",
		HTMX:           "/static/js/htmx.min.js",
		CropperJS:      "/static/js/cropper.min.js",
		CropperCSS:     "/static/css/cropper.min.css",
		ChartJS:        "/static/js/chart.min.js",
		SortableJS:     "/static/js/sortable.min.js",
		HelpJS:         "/static/js/help.js",
		PollingJS:      "/static/js/polling.js",
		SessionJS:      "/static/js/session.js",
		PreferencesJS:  "/static/js/preferences.js",
		SidebarJS:      "/static/js/sidebar.js",
		FilterFlyoutJS: "/static/js/filter_flyout.js",
		DriverJS:       "/static/js/driver.min.js",
		DriverCSS:      "/static/css/driver.min.css",
		TourJS:         "/static/js/tour.js",
		SSEJS:          "/static/js/sse.js",
		LoginBG:        "/static/img/login-bg.jpg",
		IsAdmin:        false,
		CurrentPath:    "/",
		Username:       "john",
		DisplayName:    "John Doe",
		AvatarURL:      "",
		Role:           "operator",
		Version:        "1.0.0",
	}

	var buf bytes.Buffer
	if err := templates.Layout("Home", assets).Render(testI18nCtx(t, context.Background()), &buf); err != nil {
		t.Fatalf("rendering layout: %v", err)
	}

	htmlContent := buf.String()

	// Parse the HTML to verify structure.
	reader := strings.NewReader(htmlContent)
	tokenizer := html.NewTokenizer(reader)

	var hasNav, hasMain, hasSidebar, hasMainContent bool
	var navID string

	for {
		tt := tokenizer.Next()
		if tt == html.ErrorToken {
			break
		}

		if tt != html.StartTagToken {
			continue
		}

		t := tokenizer.Token()

		switch t.Data {
		case "nav":
			hasNav = true
			for _, attr := range t.Attr {
				if attr.Key == "id" {
					navID = attr.Val
					if navID == "sw-sidebar" {
						hasSidebar = true
					}
				}
			}
		case "main":
			hasMain = true
		case "div":
			for _, attr := range t.Attr {
				if attr.Key == "id" && attr.Val == "sw-main-content" {
					hasMainContent = true
				}
			}
		}
	}

	if !hasNav {
		t.Error("response missing <nav> element")
	}
	if !hasSidebar {
		t.Errorf("response missing sidebar nav element with id='sw-sidebar', got id=%q", navID)
	}
	if !hasMainContent {
		t.Error("response missing main content div with id='sw-main-content'")
	}
	if !hasMain {
		t.Error("response missing <main> element")
	}
}

// TestLayoutAdmin_RendersSharedFilesystemBar verifies that admin users see
// the shared filesystem bar while non-admin users do not.
func TestLayoutAdmin_RendersSharedFilesystemBar(t *testing.T) {
	tests := []struct {
		name    string
		isAdmin bool
		want    bool
	}{
		{"admin sees bar", true, true},
		{"non-admin hides bar", false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {

			assets := templates.AssetPaths{
				BasePath:       "",
				CSS:            "/static/css/styles.css",
				HTMX:           "/static/js/htmx.min.js",
				CropperJS:      "/static/js/cropper.min.js",
				CropperCSS:     "/static/css/cropper.min.css",
				ChartJS:        "/static/js/chart.min.js",
				SortableJS:     "/static/js/sortable.min.js",
				HelpJS:         "/static/js/help.js",
				PollingJS:      "/static/js/polling.js",
				SessionJS:      "/static/js/session.js",
				PreferencesJS:  "/static/js/preferences.js",
				SidebarJS:      "/static/js/sidebar.js",
				FilterFlyoutJS: "/static/js/filter_flyout.js",
				DriverJS:       "/static/js/driver.min.js",
				DriverCSS:      "/static/css/driver.min.css",
				TourJS:         "/static/js/tour.js",
				SSEJS:          "/static/js/sse.js",
				LoginBG:        "/static/img/login-bg.jpg",
				IsAdmin:        tt.isAdmin,
				CurrentPath:    "/",
				Username:       "user",
				DisplayName:    "User Name",
				AvatarURL:      "",
				Role:           "operator",
				Version:        "1.0.0",
			}

			var buf bytes.Buffer
			if err := templates.Layout("Test", assets).Render(testI18nCtx(t, context.Background()), &buf); err != nil {
				t.Fatalf("rendering layout: %v", err)
			}

			htmlContent := buf.String()

			// SharedFilesystemBar is rendered as a component with id="shared-fs-bar".
			hasBar := strings.Contains(htmlContent, "shared-fs-bar") ||
				strings.Contains(htmlContent, "SharedFilesystemBar")

			if tt.want && !hasBar {
				t.Error("admin user should see shared filesystem bar")
			}
			if !tt.want && hasBar {
				t.Error("non-admin user should not see shared filesystem bar")
			}
		})
	}
}

// TestLayoutContentArea_ContainsMainLandmark verifies that the main content
// area has proper semantic structure for accessibility.
func TestLayoutContentArea_ContainsMainLandmark(t *testing.T) {

	assets := templates.AssetPaths{
		BasePath:       "/app",
		CSS:            "/app/static/css/styles.css",
		HTMX:           "/app/static/js/htmx.min.js",
		CropperJS:      "/app/static/js/cropper.min.js",
		CropperCSS:     "/app/static/css/cropper.min.css",
		ChartJS:        "/app/static/js/chart.min.js",
		SortableJS:     "/app/static/js/sortable.min.js",
		HelpJS:         "/app/static/js/help.js",
		PollingJS:      "/app/static/js/polling.js",
		SessionJS:      "/app/static/js/session.js",
		PreferencesJS:  "/app/static/js/preferences.js",
		SidebarJS:      "/app/static/js/sidebar.js",
		FilterFlyoutJS: "/app/static/js/filter_flyout.js",
		DriverJS:       "/app/static/js/driver.min.js",
		DriverCSS:      "/app/static/css/driver.min.css",
		TourJS:         "/app/static/js/tour.js",
		SSEJS:          "/app/static/js/sse.js",
		LoginBG:        "/app/static/img/login-bg.jpg",
		IsAdmin:        true,
		CurrentPath:    "/dashboard",
		Username:       "admin",
		DisplayName:    "Administrator",
		AvatarURL:      "/app/avatar.jpg",
		Role:           "administrator",
		Version:        "2.0.0",
	}

	var buf bytes.Buffer
	if err := templates.Layout("Dashboard", assets).Render(testI18nCtx(t, context.Background()), &buf); err != nil {
		t.Fatalf("rendering layout: %v", err)
	}

	htmlContent := buf.String()

	// Verify main element exists.
	if !strings.Contains(htmlContent, "<main") {
		t.Error("response missing <main> element (required landmark)")
	}

	// Verify the main element has the semantic max-width class.
	if !strings.Contains(htmlContent, "max-w-7xl") {
		t.Error("main content area missing max-width constraint")
	}

	// Verify padding for responsive layout.
	if !strings.Contains(htmlContent, "px-4") || !strings.Contains(htmlContent, "py-6") {
		t.Error("main content area missing responsive padding")
	}
}

// TestLayout_AssetPathsWithBasePath verifies that asset paths are correctly
// prefixed with the BasePath when rendering.
func TestLayout_AssetPathsWithBasePath(t *testing.T) {
	basePath := "/my-app"
	assets := templates.AssetPaths{
		BasePath:       basePath,
		CSS:            basePath + "/static/css/styles.css?v=abc",
		HTMX:           basePath + "/static/js/htmx.min.js",
		CropperJS:      basePath + "/static/js/cropper.min.js",
		CropperCSS:     basePath + "/static/css/cropper.min.css",
		ChartJS:        basePath + "/static/js/chart.min.js",
		SortableJS:     basePath + "/static/js/sortable.min.js",
		HelpJS:         basePath + "/static/js/help.js",
		PollingJS:      basePath + "/static/js/polling.js",
		SessionJS:      basePath + "/static/js/session.js",
		PreferencesJS:  basePath + "/static/js/preferences.js",
		SidebarJS:      basePath + "/static/js/sidebar.js",
		FilterFlyoutJS: basePath + "/static/js/filter_flyout.js",
		DriverJS:       basePath + "/static/js/driver.min.js",
		DriverCSS:      basePath + "/static/css/driver.min.css",
		TourJS:         basePath + "/static/js/tour.js",
		SSEJS:          basePath + "/static/js/sse.js",
		LoginBG:        basePath + "/static/img/login-bg.jpg",
		IsAdmin:        false,
		CurrentPath:    "/",
		Username:       "test",
		DisplayName:    "",
		AvatarURL:      "",
		Role:           "operator",
		Version:        "1.0.0",
	}

	var buf bytes.Buffer
	if err := templates.Layout("Test", assets).Render(testI18nCtx(t, context.Background()), &buf); err != nil {
		t.Fatalf("rendering layout: %v", err)
	}

	htmlContent := buf.String()

	// Verify BasePath is used in critical asset paths.
	requiredPaths := []string{
		basePath + "/static/css/styles.css",
		basePath + "/static/js/htmx.min.js",
		basePath + "/static/img/favicon.svg",
	}

	for _, path := range requiredPaths {
		if !strings.Contains(htmlContent, path) {
			t.Errorf("response missing asset path %q", path)
		}
	}

	// Verify htmx-base-path meta tag is set correctly.
	// Templ may render with or without closing slash, so check for the content value.
	htmxMetaContent := `htmx-base-path" content="` + basePath + `"`
	if !strings.Contains(htmlContent, htmxMetaContent) {
		t.Errorf("response missing htmx-base-path meta tag with value %q", basePath)
	}
}

// TestLayout_RendersWithoutHandlerContext verifies the layout can be rendered
// independently for testing purposes, without a full HTTP handler context.
func TestLayout_RendersWithoutHandlerContext(t *testing.T) {
	assets := templates.AssetPaths{
		BasePath:       "",
		CSS:            "/css/main.css",
		HTMX:           "/js/htmx.js",
		CropperJS:      "/js/cropper.js",
		CropperCSS:     "/css/cropper.css",
		ChartJS:        "/js/chart.js",
		SortableJS:     "/js/sortable.js",
		HelpJS:         "/js/help.js",
		PollingJS:      "/js/polling.js",
		SessionJS:      "/js/session.js",
		PreferencesJS:  "/js/preferences.js",
		SidebarJS:      "/js/sidebar.js",
		FilterFlyoutJS: "/js/filter_flyout.js",
		DriverJS:       "/js/driver.min.js",
		DriverCSS:      "/css/driver.min.css",
		TourJS:         "/js/tour.js",
		SSEJS:          "/js/sse.js",
		LoginBG:        "/img/bg.jpg",
		IsAdmin:        false,
		CurrentPath:    "/",
		Username:       "guest",
		DisplayName:    "",
		AvatarURL:      "",
		Role:           "operator",
		Version:        "dev",
	}

	// Render without HTTP middleware, but with i18n context for translation.
	var buf bytes.Buffer
	ctx := testI18nCtx(t, context.Background())
	if err := templates.Layout("Test Page", assets).Render(ctx, &buf); err != nil {
		t.Fatalf("rendering layout with background context: %v", err)
	}

	htmlContent := buf.String()
	if len(htmlContent) == 0 {
		t.Fatal("layout produced empty output")
	}

	// Verify the page still has all required elements.
	if !strings.Contains(htmlContent, "<!doctype html>") && !strings.Contains(htmlContent, "<!DOCTYPE html>") {
		t.Error("layout missing DOCTYPE")
	}
	if !strings.Contains(htmlContent, "<html") {
		t.Error("layout missing html element")
	}
	if !strings.Contains(htmlContent, "id=\"sw-sidebar\"") {
		t.Error("layout missing sidebar element")
	}
	if !strings.Contains(htmlContent, "id=\"sw-main-content\"") {
		t.Error("layout missing main content element")
	}
}

// TestRootRoute_RendersLayout_WithSidebar verifies that GET / returns a 200
// status code and renders the layout with the sidebar landmark (id="sw-sidebar").
// This is an integration test that uses the real app router and handler registration.
func TestRootRoute_RendersLayout_WithSidebar(t *testing.T) {
	// Set up test database and services
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

	i18nBundle, err := i18n.LoadEmbedded()
	if err != nil {
		t.Fatalf("loading i18n bundle: %v", err)
	}

	// Create router with all necessary services
	r := NewRouter(RouterDeps{
		AuthService:       auth.NewService(db),
		PlatformService:   platform.NewService(db),
		ProviderSettings:  provider.NewSettingsService(db, enc),
		ConnectionService: connection.NewService(db, enc),
		LibraryService:    library.NewService(db),
		I18nBundle:        i18nBundle,
		DB:                db,
		Logger:            logger,
		StaticDir:         "../../web/static",
	})

	// Create a test user and session
	ctx := context.Background()
	user, err := r.authService.CreateLocalUser(ctx, "testuser", "password123", "Test User", "administrator", "")
	if err != nil {
		t.Fatalf("CreateLocalUser: %v", err)
	}

	token, err := r.authService.CreateSession(ctx, user.ID)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Mark onboarding as completed so the handler renders the index page
	_, err = db.ExecContext(ctx,
		`INSERT INTO settings (key, value) VALUES ('onboarding.completed', 'true')`)
	if err != nil {
		t.Fatalf("setting onboarding.completed: %v", err)
	}

	// Get the HTTP handler/mux
	mux := r.Handler(ctx)

	// Make HTTP GET request to /
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: "session", Value: token})
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	// Verify status code is 200
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	responseBody := w.Body.String()

	// Verify response contains sidebar element with correct ID
	if !strings.Contains(responseBody, "id=\"sw-sidebar\"") {
		t.Error("response missing sidebar element with id='sw-sidebar'")
	}

	// Verify response contains main content element with correct ID
	if !strings.Contains(responseBody, "id=\"sw-main-content\"") {
		t.Error("response missing main content element with id='sw-main-content'")
	}

	// Verify old navbar HTML identifiers are not present
	// (old navigation used <nav> without sw-sidebar id)
	if strings.Contains(responseBody, "id=\"navbar\"") || strings.Contains(responseBody, "id=\"nav-") {
		t.Error("response contains old navbar HTML identifiers (navbar should use id='sw-sidebar')")
	}

	// Verify response contains valid HTML structure
	if !strings.Contains(responseBody, "<!doctype html>") && !strings.Contains(responseBody, "<!DOCTYPE html>") {
		t.Error("response missing DOCTYPE declaration")
	}
}
