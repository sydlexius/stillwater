package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/a-h/templ"
	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/auth"
	"github.com/sydlexius/stillwater/internal/connection"
	connEmby "github.com/sydlexius/stillwater/internal/connection/emby"
	connJellyfin "github.com/sydlexius/stillwater/internal/connection/jellyfin"
	"github.com/sydlexius/stillwater/internal/library"
	"github.com/sydlexius/stillwater/internal/version"
	"github.com/sydlexius/stillwater/web/components"
	"github.com/sydlexius/stillwater/web/templates"
)

// handleHealth returns a simple health check response with version info.
// GET /api/v1/health
func (r *Router) handleHealth(w http.ResponseWriter, req *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status":  "ok",
		"version": version.Version,
		"commit":  version.Commit,
		"time":    time.Now().UTC().Format(time.RFC3339),
	})
}

// assets returns cache-busted asset paths and server configuration for templates.
// BasePath is the configured server.base_path prefix (e.g. "/stillwater") so
// templates can construct correct absolute URLs in sub-path deployments.
func (r *Router) assets() templates.AssetPaths {
	return templates.AssetPaths{
		CSS:        r.basePath + r.staticAssets.Path("/css/styles.css"),
		HTMX:       r.basePath + r.staticAssets.Path("/js/htmx.min.js"),
		CropperJS:  r.basePath + r.staticAssets.Path("/js/cropper.min.js"),
		CropperCSS: r.basePath + r.staticAssets.Path("/css/cropper.min.css"),
		ChartJS:    r.basePath + r.staticAssets.Path("/js/chart.min.js"),
		SortableJS: r.basePath + r.staticAssets.Path("/js/Sortable.min.js"),
		HelpJS:     r.basePath + r.staticAssets.Path("/js/help.js"),
		PollingJS:  r.basePath + r.staticAssets.Path("/js/polling.js"),
		LoginBG:    r.basePath + r.staticAssets.Path("/img/login-bg.jpg"),
		BasePath:   r.basePath,
	}
}

// handleLogin authenticates a user and sets a session cookie.
// POST /api/v1/auth/login
func (r *Router) handleLogin(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"` //nolint:gosec // G117: not a hardcoded secret, this is a request field
	}
	if strings.HasPrefix(req.Header.Get("Content-Type"), "application/json") {
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			writeFormError(w, req, http.StatusBadRequest, "Invalid request body.")
			return
		}
	} else {
		req.Body = http.MaxBytesReader(w, req.Body, 1<<20)
		body.Username = req.FormValue("username")
		body.Password = req.FormValue("password")
	}

	authMethod := r.getStringSetting(req.Context(), "auth.method", "local")

	switch authMethod {
	case "emby", "jellyfin":
		r.handleLoginFederated(w, req, body.Username, body.Password, authMethod)
	default:
		r.handleLoginLocal(w, req, body.Username, body.Password)
	}
}

// handleLoginLocal performs local username/password authentication.
func (r *Router) handleLoginLocal(w http.ResponseWriter, req *http.Request, username, password string) {
	token, err := r.authService.Login(req.Context(), username, password)
	if err != nil {
		r.logger.Warn("local login failed", "username", username)
		writeFormError(w, req, http.StatusUnauthorized, "Invalid username or password.")
		return
	}

	r.setSessionCookie(w, req, token)
	w.Header().Set("HX-Redirect", r.basePath+"/")
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleLoginFederated authenticates against an Emby or Jellyfin server and
// creates a local Stillwater session.
func (r *Router) handleLoginFederated(w http.ResponseWriter, req *http.Request, username, password, authMethod string) {
	serverURL := r.getStringSetting(req.Context(), "auth.server_url", "")
	if serverURL == "" {
		writeFormError(w, req, http.StatusInternalServerError, "Auth server URL not configured.")
		return
	}

	result, err := r.authenticateByName(req.Context(), authMethod, serverURL, username, password)
	if err != nil {
		r.logger.Warn("federated login failed", "method", authMethod, "error", err)
		if errors.Is(err, connEmby.ErrInvalidCredentials) || errors.Is(err, connJellyfin.ErrInvalidCredentials) {
			writeFormError(w, req, http.StatusUnauthorized, fmt.Sprintf("Invalid %s credentials.", authMethodDisplayName(authMethod)))
			return
		}
		writeFormError(w, req, http.StatusBadGateway, fmt.Sprintf("Cannot connect to %s server. Please verify the server is running.", authMethodDisplayName(authMethod)))
		return
	}

	fedResult := auth.FederatedAuthResult{
		AccessToken: result.AccessToken,
		UserID:      result.User.ID,
		UserName:    result.User.Name,
		IsAdmin:     result.User.Policy.IsAdministrator,
	}

	token, err := r.authService.LoginFederated(req.Context(), fedResult, authMethod)
	if err != nil {
		r.logger.Warn("federated session creation failed", "method", authMethod, "error", err)
		writeFormError(w, req, http.StatusUnauthorized, "This account is not authorized for this Stillwater instance.")
		return
	}

	// Update the connection API key if the server issued a new access token.
	r.updateConnectionToken(req.Context(), authMethod, serverURL, result.AccessToken)

	r.setSessionCookie(w, req, token)
	w.Header().Set("HX-Redirect", r.basePath+"/")
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// setSessionCookie writes the Stillwater session cookie to the response.
func (r *Router) setSessionCookie(w http.ResponseWriter, req *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   req.TLS != nil || req.Header.Get("X-Forwarded-Proto") == "https",
		MaxAge:   86400,
	})
}

// renderLoginPage renders the login page with the configured auth method.
func (r *Router) renderLoginPage(w http.ResponseWriter, req *http.Request) {
	authMethod := r.getStringSetting(req.Context(), "auth.method", "local")
	renderTempl(w, req, templates.LoginPage(r.assets(), authMethod))
}

// authMethodDisplayName returns a human-readable name for the auth method.
func authMethodDisplayName(method string) string {
	switch method {
	case "emby":
		return "Emby"
	case "jellyfin":
		return "Jellyfin"
	default:
		return method
	}
}

// federatedAuthResult is a unified wrapper over emby.AuthResult and jellyfin.AuthResult.
type federatedAuthResult struct {
	AccessToken string
	User        struct {
		ID     string
		Name   string
		Policy struct {
			IsAdministrator bool
		}
	}
}

// authenticateByName calls the appropriate media server's AuthenticateByName API.
func (r *Router) authenticateByName(ctx context.Context, authMethod, serverURL, username, password string) (*federatedAuthResult, error) {
	switch authMethod {
	case "emby":
		res, err := connEmby.AuthenticateByName(ctx, serverURL, username, password, r.logger)
		if err != nil {
			return nil, err
		}
		return &federatedAuthResult{
			AccessToken: res.AccessToken,
			User: struct {
				ID     string
				Name   string
				Policy struct{ IsAdministrator bool }
			}{
				ID:   res.User.ID,
				Name: res.User.Name,
				Policy: struct{ IsAdministrator bool }{
					IsAdministrator: res.User.Policy.IsAdministrator,
				},
			},
		}, nil
	case "jellyfin":
		res, err := connJellyfin.AuthenticateByName(ctx, serverURL, username, password, r.logger)
		if err != nil {
			return nil, err
		}
		return &federatedAuthResult{
			AccessToken: res.AccessToken,
			User: struct {
				ID     string
				Name   string
				Policy struct{ IsAdministrator bool }
			}{
				ID:   res.User.ID,
				Name: res.User.Name,
				Policy: struct{ IsAdministrator bool }{
					IsAdministrator: res.User.Policy.IsAdministrator,
				},
			},
		}, nil
	default:
		return nil, fmt.Errorf("unsupported auth method: %s", authMethod)
	}
}

// updateConnectionToken updates the stored API key for a federated connection
// if the media server issued a new access token during login.
func (r *Router) updateConnectionToken(ctx context.Context, authMethod, serverURL, newToken string) {
	conn, err := r.connectionService.GetByTypeAndURL(ctx, authMethod, serverURL)
	if err != nil {
		r.logger.Warn("failed to look up connection for token update", "method", authMethod, "error", err)
		return
	}
	if conn == nil {
		return
	}
	// The service decrypts the stored key on read; compare with the new token.
	if conn.APIKey == newToken {
		return
	}
	conn.APIKey = newToken
	if err := r.connectionService.Update(ctx, conn); err != nil {
		r.logger.Warn("failed to update connection token after federated login", "error", err)
	}
}

// handleLogout destroys the current session and clears the session cookie.
// POST /api/v1/auth/logout
func (r *Router) handleLogout(w http.ResponseWriter, req *http.Request) {
	if cookie, err := req.Cookie("session"); err == nil {
		if logoutErr := r.authService.Logout(req.Context(), cookie.Value); logoutErr != nil {
			r.logger.Warn("failed to delete session", "error", logoutErr)
		}
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   req.TLS != nil || req.Header.Get("X-Forwarded-Proto") == "https",
		MaxAge:   -1,
	})

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleMe returns the currently authenticated user's identity.
// GET /api/v1/auth/me
func (r *Router) handleMe(w http.ResponseWriter, req *http.Request) {
	userID := middleware.UserIDFromContext(req.Context())
	if userID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"user_id": userID})
}

// handleSetup creates the initial admin user during first-time setup.
// Supports both local account creation and federated auth via Emby/Jellyfin.
// POST /api/v1/auth/setup
func (r *Router) handleSetup(w http.ResponseWriter, req *http.Request) {
	hasUsers, err := r.authService.HasUsers(req.Context())
	if err != nil {
		writeFormError(w, req, http.StatusInternalServerError, "An internal error occurred. Please try again.")
		return
	}
	if hasUsers {
		writeFormError(w, req, http.StatusConflict, "An admin account already exists.")
		return
	}

	var body struct {
		AuthMethod string `json:"auth_method"`
		Username   string `json:"username"`
		Password   string `json:"password"`   //nolint:gosec // G117: not a hardcoded secret, this is a request field
		ServerURL  string `json:"server_url"` //nolint:gosec // G117: not a secret, this is a server address
	}
	if strings.HasPrefix(req.Header.Get("Content-Type"), "application/json") {
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			writeFormError(w, req, http.StatusBadRequest, "Invalid request body.")
			return
		}
	} else {
		req.Body = http.MaxBytesReader(w, req.Body, 1<<20)
		body.AuthMethod = req.FormValue("auth_method")
		body.Username = req.FormValue("username")
		body.Password = req.FormValue("password")
		body.ServerURL = req.FormValue("server_url")
	}

	switch body.AuthMethod {
	case "emby", "jellyfin":
		r.handleSetupFederated(w, req, body.AuthMethod, body.Username, body.Password, body.ServerURL)
	default:
		r.handleSetupLocal(w, req, body.Username, body.Password)
	}
}

// handleSetupLocal creates the initial admin account with a local username/password.
func (r *Router) handleSetupLocal(w http.ResponseWriter, req *http.Request, username, password string) {
	if username == "" || password == "" {
		writeFormError(w, req, http.StatusBadRequest, "Username and password are required.")
		return
	}

	if len(password) < 8 {
		writeFormError(w, req, http.StatusBadRequest, "Password must be at least 8 characters.")
		return
	}

	created, err := r.authService.Setup(req.Context(), username, password)
	if err != nil {
		r.logger.Error("failed to create admin account", "error", err)
		writeFormError(w, req, http.StatusInternalServerError, "An internal error occurred. Please try again.")
		return
	}

	if !created {
		writeFormError(w, req, http.StatusConflict, "An admin account already exists.")
		return
	}

	w.Header().Set("HX-Redirect", r.basePath+"/")
	writeJSON(w, http.StatusCreated, map[string]string{"status": "admin account created"})
}

// handleSetupFederated creates the initial admin account by authenticating against
// an Emby or Jellyfin server. On success it also stores the auth settings and
// auto-creates the first server connection.
func (r *Router) handleSetupFederated(w http.ResponseWriter, req *http.Request, authMethod, username, password, serverURL string) {
	if username == "" || password == "" {
		writeFormError(w, req, http.StatusBadRequest, "Username and password are required.")
		return
	}

	if serverURL == "" {
		writeFormError(w, req, http.StatusBadRequest, "Server URL is required for federated authentication.")
		return
	}

	cleanedURL, err := connection.ValidateBaseURL(serverURL)
	if err != nil {
		writeFormError(w, req, http.StatusBadRequest, "Invalid server URL.")
		return
	}

	// Authenticate against the media server.
	result, err := r.authenticateByName(req.Context(), authMethod, cleanedURL, username, password)
	if err != nil {
		r.logger.Warn("federated setup auth failed", "method", authMethod, "error", err)
		if errors.Is(err, connEmby.ErrInvalidCredentials) || errors.Is(err, connJellyfin.ErrInvalidCredentials) {
			writeFormError(w, req, http.StatusUnauthorized, fmt.Sprintf("Invalid %s credentials.", authMethodDisplayName(authMethod)))
			return
		}
		writeFormError(w, req, http.StatusBadGateway, fmt.Sprintf("Cannot connect to %s server. Please verify the server is running and the URL is correct.", authMethodDisplayName(authMethod)))
		return
	}

	// Only media server administrators can set up Stillwater.
	if !result.User.Policy.IsAdministrator {
		writeFormError(w, req, http.StatusForbidden, fmt.Sprintf("Only %s administrator accounts can set up Stillwater.", authMethodDisplayName(authMethod)))
		return
	}

	fedResult := auth.FederatedAuthResult{
		AccessToken: result.AccessToken,
		UserID:      result.User.ID,
		UserName:    result.User.Name,
		IsAdmin:     result.User.Policy.IsAdministrator,
	}

	// Create the local user record.
	created, err := r.authService.SetupFederated(req.Context(), fedResult, authMethod)
	if err != nil {
		r.logger.Error("failed to create federated admin account", "error", err)
		writeFormError(w, req, http.StatusInternalServerError, "An internal error occurred. Please try again.")
		return
	}
	if !created {
		writeFormError(w, req, http.StatusConflict, "An admin account already exists.")
		return
	}

	// Store auth settings. This is critical: if it fails, the login page will
	// default to local auth and the federated user cannot log in.
	now := time.Now().UTC().Format(time.RFC3339)
	for k, v := range map[string]string{"auth.method": authMethod, "auth.server_url": cleanedURL} {
		if _, err := r.db.ExecContext(req.Context(),
			`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
			k, v, now); err != nil {
			r.logger.Error("failed to store auth setting", "key", k, "error", err)
			writeFormError(w, req, http.StatusInternalServerError, "An internal error occurred. Please try again.")
			return
		}
	}

	// Auto-create the first server connection.
	connName := authMethodDisplayName(authMethod)
	conn := &connection.Connection{
		Name:                 connName,
		Type:                 authMethod,
		URL:                  cleanedURL,
		APIKey:               result.AccessToken,
		Enabled:              true,
		FeatureLibraryImport: true,
		FeatureNFOWrite:      true,
		FeatureImageWrite:    true,
		PlatformUserID:       result.User.ID,
	}
	if err := r.connectionService.Create(req.Context(), conn); err != nil {
		r.logger.Error("failed to auto-create connection during federated setup", "error", err)
		// Non-fatal: the user can add the connection manually later.
	} else if err := r.connectionService.UpdateStatus(req.Context(), conn.ID, "ok", ""); err != nil {
		r.logger.Warn("connection created but status update failed", "connection_id", conn.ID, "error", err)
	}

	// Auto-login the user.
	token, err := r.authService.LoginFederated(req.Context(), fedResult, authMethod)
	if err != nil {
		r.logger.Error("failed to auto-login after federated setup", "error", err)
		// Redirect to root so the user lands on the login page.
		w.Header().Set("HX-Redirect", r.basePath+"/")
		writeJSON(w, http.StatusCreated, map[string]string{"status": "admin account created"})
		return
	}

	r.setSessionCookie(w, req, token)
	w.Header().Set("HX-Redirect", r.basePath+"/")
	writeJSON(w, http.StatusCreated, map[string]string{"status": "admin account created"})
}

// handleIndex serves the main web application page, redirecting to setup or
// login if needed.
// GET /
func (r *Router) handleIndex(w http.ResponseWriter, req *http.Request) {
	// Check if setup is needed
	hasUsers, err := r.authService.HasUsers(req.Context())
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !hasUsers {
		renderTempl(w, req, templates.SetupPage(r.assets()))
		return
	}

	// Check auth (populated by OptionalAuth middleware)
	userID := middleware.UserIDFromContext(req.Context())
	if userID == "" {
		r.renderLoginPage(w, req)
		return
	}

	// Check if onboarding wizard needs to run
	var completed string
	err = r.db.QueryRowContext(req.Context(), //nolint:gosec // G701: query is a string literal
		`SELECT value FROM settings WHERE key = 'onboarding.completed'`).Scan(&completed)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		r.logger.Error("checking onboarding status", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if completed != "true" {
		http.Redirect(w, req, r.basePath+"/setup/wizard", http.StatusSeeOther)
		return
	}

	renderTempl(w, req, templates.IndexPage(r.assets()))
}

// handleOnboardingPage serves the first-time setup wizard page.
// GET /setup/wizard
func (r *Router) handleOnboardingPage(w http.ResponseWriter, req *http.Request) {
	// Require auth
	userID := middleware.UserIDFromContext(req.Context())
	if userID == "" {
		http.Redirect(w, req, r.basePath+"/", http.StatusSeeOther)
		return
	}

	// If onboarding already completed, redirect to dashboard
	var completed string
	err := r.db.QueryRowContext(req.Context(), //nolint:gosec // G701: query is a string literal
		`SELECT value FROM settings WHERE key = 'onboarding.completed'`).Scan(&completed)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		r.logger.Error("checking onboarding status", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if completed == "true" {
		http.Redirect(w, req, r.basePath+"/", http.StatusSeeOther)
		return
	}

	// Load wizard data
	profiles, err := r.platformService.List(req.Context())
	if err != nil {
		r.logger.Error("listing platforms for onboarding", "error", err)
	}

	providerKeys, err := r.providerSettings.ListProviderKeyStatuses(req.Context())
	if err != nil {
		r.logger.Error("listing provider keys for onboarding", "error", err)
	}

	// Load current step from settings (default to 0 = intro).
	currentStep := 0
	var stepStr string
	err = r.db.QueryRowContext(req.Context(), //nolint:gosec // G701: query is a string literal
		`SELECT value FROM settings WHERE key = 'onboarding.step'`).Scan(&stepStr)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		r.logger.Error("loading onboarding step", "error", err)
	} else if err == nil {
		switch stepStr {
		case "1":
			currentStep = 1
		case "2":
			currentStep = 2
		case "3":
			currentStep = 3
		case "4":
			currentStep = 4
		case "5":
			currentStep = 5
		}
	}

	conns, err := r.connectionService.List(req.Context())
	if err != nil {
		r.logger.Error("listing connections for onboarding", "error", err)
	}

	var libs []library.Library
	if r.libraryService != nil {
		libs, err = r.libraryService.List(req.Context())
		if err != nil {
			r.logger.Error("listing libraries for onboarding", "error", err)
		}
	}

	webSearchProviders, err := r.providerSettings.ListWebSearchStatuses(req.Context())
	if err != nil {
		r.logger.Error("listing web search providers for onboarding", "error", err)
	}

	unidentifiedCount := -1
	err = r.db.QueryRowContext(req.Context(), //nolint:gosec // G701: query is a string literal
		`SELECT COUNT(*) FROM artists WHERE is_excluded = 0 AND locked = 0
		 AND NOT EXISTS (
		    SELECT 1 FROM artist_provider_ids
		    WHERE artist_id = artists.id AND provider = 'musicbrainz'
		 )`).Scan(&unidentifiedCount)
	if err != nil {
		r.logger.Error("counting unidentified artists for onboarding", "error", err)
		unidentifiedCount = -1
	}

	data := templates.OnboardingData{
		Libraries:          libs,
		Profiles:           profiles,
		ProviderKeys:       providerKeys,
		WebSearchProviders: webSearchProviders,
		Connections:        conns,
		CurrentStep:        currentStep,
		UnidentifiedCount:  unidentifiedCount,
	}
	renderTempl(w, req, templates.OnboardingPage(r.assets(), data))
}

func renderTempl(w http.ResponseWriter, r *http.Request, component templ.Component) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := component.Render(r.Context(), w); err != nil {
		http.Error(w, "render error", http.StatusInternalServerError)
	}
}

// writeError sends an error response. For HTMX requests, it renders an error
// toast HTML fragment. For API requests, it returns JSON.
func writeError(w http.ResponseWriter, req *http.Request, status int, message string) {
	if req.Header.Get("HX-Request") == "true" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(status)
		component := components.ErrorToast("error", message)
		_ = component.Render(req.Context(), w)
		return
	}
	writeJSON(w, status, map[string]string{"error": message})
}

// writeFormError sends an error response suitable for inline form display.
// For HTMX requests, it returns 200 with a styled inline message fragment
// (HTMX does not swap content on 4xx/5xx by default). For API requests, it
// returns JSON with the proper HTTP status code.
func writeFormError(w http.ResponseWriter, req *http.Request, status int, message string) {
	if req.Header.Get("HX-Request") == "true" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		component := components.InlineMessage("error", message)
		_ = component.Render(req.Context(), w)
		return
	}
	writeJSON(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, "encode error", http.StatusInternalServerError)
	}
}
