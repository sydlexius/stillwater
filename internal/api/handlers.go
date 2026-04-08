package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
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
		CSS:            r.basePath + r.staticAssets.Path("/css/styles.css"),
		HTMX:           r.basePath + r.staticAssets.Path("/js/htmx.min.js"),
		CropperJS:      r.basePath + r.staticAssets.Path("/js/cropper.min.js"),
		CropperCSS:     r.basePath + r.staticAssets.Path("/css/cropper.min.css"),
		ChartJS:        r.basePath + r.staticAssets.Path("/js/chart.min.js"),
		SortableJS:     r.basePath + r.staticAssets.Path("/js/Sortable.min.js"),
		HelpJS:         r.basePath + r.staticAssets.Path("/js/help.js"),
		PollingJS:      r.basePath + r.staticAssets.Path("/js/polling.js"),
		SessionJS:      r.basePath + r.staticAssets.Path("/js/session.js"),
		PreferencesJS:  r.basePath + r.staticAssets.Path("/js/preferences.js"),
		SidebarJS:      r.basePath + r.staticAssets.Path("/js/sidebar.js"),
		FilterFlyoutJS: r.basePath + r.staticAssets.Path("/js/filter-flyout.js"),
		// DriverJS, DriverCSS, and TourJS are intentionally omitted here.
		// They are conditionally set in assetsFor() based on the request path
		// so pages that do not use the guided tour avoid the extra JS/CSS.
		SSEJS:    r.basePath + r.staticAssets.Path("/js/sse.js"),
		LoginBG:  r.basePath + r.staticAssets.Path("/img/login-bg.jpg"),
		BasePath: r.basePath,
	}
}

// assetsFor returns AssetPaths with the IsAdmin flag set based on the
// authenticated user's role from the request context. Use this instead of
// assets() when rendering pages that include the Layout (which conditionally
// shows admin-only elements like Settings nav and SharedFilesystemBar).
func (r *Router) assetsFor(req *http.Request) templates.AssetPaths {
	a := r.assets()
	ctx := req.Context()
	role := middleware.RoleFromContext(ctx)
	a.IsAdmin = role == "administrator"
	a.Role = role
	a.Version = version.Version

	// Look up the full user record to get display name and avatar info.
	// Username and DisplayName default to empty; only populated from the
	// fetched user record so we never leak the raw UUID to the UI.
	userID := middleware.UserIDFromContext(ctx)
	if userID != "" {
		if user, err := r.authService.GetUserByID(ctx, userID); err == nil {
			a.Username = user.Username
			a.DisplayName = user.DisplayName
			a.AvatarURL = r.buildAvatarURL(ctx, user)
		} else {
			slog.Warn("failed to look up user for sidebar", "user_id", userID, "error", err)
		}
	}

	a.ShowViolations = r.getBoolSetting(ctx, "sidebar_show_violations", true)

	// Strip the base path prefix so the bottom tab bar can match the active
	// route without knowing the deployment prefix.
	path := req.URL.Path
	if r.basePath != "" {
		path = strings.TrimPrefix(path, r.basePath)
	}
	if path == "" {
		path = "/"
	}
	a.CurrentPath = path

	// Include Driver.js tour assets only on pages where the guided tour
	// may auto-start, be manually triggered, or set a pending flag.
	switch {
	case path == "/artists" || path == "/artists/",
		strings.HasPrefix(path, "/guide"),
		strings.HasPrefix(path, "/onboarding"),
		strings.HasPrefix(path, "/setup/wizard"):
		a.DriverJS = r.basePath + r.staticAssets.Path("/js/driver.min.js")
		a.DriverCSS = r.basePath + r.staticAssets.Path("/css/driver.min.css")
		a.TourJS = r.basePath + r.staticAssets.Path("/js/tour.js")
	}

	return a
}

// buildAvatarURL constructs a profile image URL for federated users (Emby/Jellyfin).
// Returns an empty string for local users or when the server URL is not configured.
// Emby: {serverURL}/Users/{providerID}/Images/Primary
// Jellyfin: {serverURL}/Users/{providerID}/Images/Primary
func (r *Router) buildAvatarURL(ctx context.Context, user *auth.User) string {
	if user.AuthProvider != "emby" && user.AuthProvider != "jellyfin" {
		return ""
	}
	if user.ProviderID == "" {
		return ""
	}
	// Try provider-specific server URL first, fall back to the legacy global key.
	var serverURL string
	switch user.AuthProvider {
	case "emby":
		serverURL = r.getStringSetting(ctx, "auth.providers.emby.server_url", "")
	case "jellyfin":
		serverURL = r.getStringSetting(ctx, "auth.providers.jellyfin.server_url", "")
	}
	if serverURL == "" {
		serverURL = r.getStringSetting(ctx, "auth.server_url", "")
	}
	if serverURL == "" {
		return ""
	}
	// Both Emby and Jellyfin use the same endpoint for user profile images.
	return serverURL + "/Users/" + url.PathEscape(user.ProviderID) + "/Images/Primary"
}

// handleLogin authenticates a user and sets a session cookie.
// If the auth registry is configured, it dispatches to the appropriate provider.
// For backward compatibility, falls back to the legacy auth.method setting path
// when the registry is not wired or the requested provider is not registered.
// POST /api/v1/auth/login
func (r *Router) handleLogin(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"` //nolint:gosec // G117: not a hardcoded secret, this is a request field
		Provider string `json:"provider"` // optional; overrides the auth.method setting
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
		body.Provider = req.FormValue("provider")
	}

	// Determine provider type: explicit field takes priority, then auth.method setting.
	providerType := body.Provider
	if providerType == "" {
		providerType = r.getStringSetting(req.Context(), "auth.method", "local")
	}

	// If the registry is configured and the provider is registered, use the new path.
	if r.authRegistry != nil {
		if provider, ok := r.authRegistry.Get(providerType); ok {
			creds := auth.Credentials{
				Username: body.Username,
				Password: body.Password,
			}
			identity, err := provider.Authenticate(req.Context(), creds)
			if err != nil {
				r.logger.Warn("authentication failed", "provider", providerType, "error", err)
				if providerType == "local" {
					writeFormError(w, req, http.StatusUnauthorized, "Invalid username or password.")
				} else if errors.Is(err, auth.ErrInvalidCredentials) {
					writeFormError(w, req, http.StatusUnauthorized, fmt.Sprintf("Invalid %s credentials.", authMethodDisplayName(providerType)))
				} else {
					// Network errors, non-200 responses, and malformed provider
					// responses are upstream failures, not authentication rejections.
					writeFormError(w, req, http.StatusBadGateway, fmt.Sprintf("Cannot connect to %s server. Please verify the server is running.", authMethodDisplayName(providerType)))
				}
				return
			}
			r.completeLogin(w, req, provider, identity)
			return
		}
	}

	// Legacy fallback: use the old branching logic when the registry is not
	// wired or the provider is not yet registered.
	switch providerType {
	case "emby", "jellyfin":
		r.handleLoginFederated(w, req, body.Username, body.Password, providerType)
	default:
		r.handleLoginLocal(w, req, body.Username, body.Password)
	}
}

// completeLogin is the post-authentication flow shared by all providers.
// It looks up or auto-provisions the user, then creates a session.
func (r *Router) completeLogin(w http.ResponseWriter, req *http.Request, provider auth.Authenticator, identity *auth.Identity) {
	ctx := req.Context()

	var user *auth.User
	var lookupErr error

	if identity.ProviderType == "local" {
		// LocalProvider sets ProviderID to the user's database row ID.
		user, lookupErr = r.authService.GetUserByID(ctx, identity.ProviderID)
	} else {
		// Federated providers: look up by auth_provider + provider_id columns.
		user, lookupErr = r.authService.GetUserByProvider(ctx, identity.ProviderType, identity.ProviderID)
	}

	if lookupErr != nil {
		if !errors.Is(lookupErr, sql.ErrNoRows) {
			r.logger.Error("failed to look up user during login",
				"provider", identity.ProviderType, "provider_id", identity.ProviderID, "error", lookupErr)
			writeFormError(w, req, http.StatusInternalServerError, "An internal error occurred. Please try again.")
			return
		}

		// User not found -- check if auto-provisioning is allowed.
		if !provider.CanAutoProvision(identity) {
			r.logger.Warn("login: user not found and auto-provision disabled",
				"provider", identity.ProviderType, "provider_id", identity.ProviderID)
			writeFormError(w, req, http.StatusUnauthorized, "This account is not authorized for this Stillwater instance. Contact your administrator.")
			return
		}

		// Auto-provision the user with the role determined by the provider.
		role := provider.MapRole(identity)
		if role == "" {
			role = "operator"
		}
		var createErr error
		user, createErr = r.authService.CreateFederatedUser(ctx, identity, role, "")
		if createErr != nil {
			r.logger.Error("failed to auto-provision federated user",
				"provider", identity.ProviderType, "provider_id", identity.ProviderID, "error", createErr)
			writeFormError(w, req, http.StatusInternalServerError, "An internal error occurred. Please try again.")
			return
		}
	}

	if !user.IsActive {
		r.logger.Warn("login: inactive user attempted login", "user_id", user.ID)
		writeFormError(w, req, http.StatusUnauthorized, "Your account has been deactivated. Contact your administrator.")
		return
	}

	// Sync display name for federated users if it changed on the provider.
	if identity.ProviderType != "local" && identity.DisplayName != "" && identity.DisplayName != user.DisplayName {
		if syncErr := r.authService.SyncDisplayName(ctx, user.ID, identity.DisplayName); syncErr != nil {
			// Non-fatal: log and continue.
			r.logger.Warn("failed to sync display name", "user_id", user.ID, "error", syncErr)
		}
	}

	// Update the connection API key if the provider issued a new access token.
	if identity.RawToken != "" && identity.ProviderType != "local" {
		serverURL := r.getStringSetting(ctx, "auth.server_url", "")
		if serverURL != "" {
			r.updateConnectionToken(ctx, identity.ProviderType, serverURL, identity.ProviderID, identity.RawToken)
		}
	}

	token, err := r.authService.CreateSession(ctx, user.ID)
	if err != nil {
		r.logger.Error("failed to create session", "user_id", user.ID, "error", err)
		writeFormError(w, req, http.StatusInternalServerError, "An internal error occurred. Please try again.")
		return
	}

	r.setSessionCookie(w, req, token)

	// If the login form included a return URL (from session timeout redirect),
	// send the user back to that page instead of the default root.
	// Strip basePath prefix if already present to avoid double-prefixing.
	dest := buildSafeRedirect(r.basePath, validateReturnURL(req.FormValue("return_url")))
	w.Header().Set("HX-Redirect", dest)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// completeLoginRedirect performs the same user lookup/provision/session logic as
// completeLogin but finishes with a browser redirect (302) instead of JSON.
// Used for the OIDC callback which arrives as a full-page browser navigation.
func (r *Router) completeLoginRedirect(w http.ResponseWriter, req *http.Request, provider auth.Authenticator, identity *auth.Identity) {
	ctx := req.Context()

	var user *auth.User
	var lookupErr error

	if identity.ProviderType == "local" {
		user, lookupErr = r.authService.GetUserByID(ctx, identity.ProviderID)
	} else {
		user, lookupErr = r.authService.GetUserByProvider(ctx, identity.ProviderType, identity.ProviderID)
	}

	if lookupErr != nil {
		if !errors.Is(lookupErr, sql.ErrNoRows) {
			r.logger.Error("failed to look up user during login",
				"provider", identity.ProviderType, "provider_id", identity.ProviderID, "error", lookupErr)
			r.redirectWithError(w, req, "An internal error occurred. Please try again.")
			return
		}

		if !provider.CanAutoProvision(identity) {
			r.logger.Warn("login: user not found and auto-provision disabled",
				"provider", identity.ProviderType, "provider_id", identity.ProviderID)
			r.redirectWithError(w, req, "This account is not authorized for this Stillwater instance. Contact your administrator.")
			return
		}

		role := provider.MapRole(identity)
		if role == "" {
			role = "operator"
		}
		var createErr error
		user, createErr = r.authService.CreateFederatedUser(ctx, identity, role, "")
		if createErr != nil {
			r.logger.Error("failed to auto-provision federated user",
				"provider", identity.ProviderType, "provider_id", identity.ProviderID, "error", createErr)
			r.redirectWithError(w, req, "An internal error occurred. Please try again.")
			return
		}
	}

	if !user.IsActive {
		r.logger.Warn("login: inactive user attempted login", "user_id", user.ID)
		r.redirectWithError(w, req, "Your account has been deactivated. Contact your administrator.")
		return
	}

	if identity.ProviderType != "local" && identity.DisplayName != "" && identity.DisplayName != user.DisplayName {
		if syncErr := r.authService.SyncDisplayName(ctx, user.ID, identity.DisplayName); syncErr != nil {
			r.logger.Warn("failed to sync display name", "user_id", user.ID, "error", syncErr)
		}
	}

	if identity.RawToken != "" && identity.ProviderType != "local" {
		serverURL := r.getStringSetting(ctx, "auth.server_url", "")
		if serverURL != "" {
			r.updateConnectionToken(ctx, identity.ProviderType, serverURL, identity.ProviderID, identity.RawToken)
		}
	}

	token, err := r.authService.CreateSession(ctx, user.ID)
	if err != nil {
		r.logger.Error("failed to create session", "user_id", user.ID, "error", err)
		r.redirectWithError(w, req, "An internal error occurred. Please try again.")
		return
	}

	r.setSessionCookie(w, req, token)

	// Honor a return URL from the form, query parameter, or OIDC cookie.
	var rawReturn string
	if v := validateReturnURL(req.FormValue("return_url")); v != "" {
		rawReturn = v
	} else if c, err := req.Cookie("oidc_return"); err == nil && c.Value != "" {
		rawReturn = validateReturnURL(c.Value)
		// Clear the cookie after use. Match the Secure/SameSite attributes from
		// the original cookie set in handleOIDCLogin so browsers delete it.
		http.SetCookie(w, &http.Cookie{
			Name:     "oidc_return",
			Value:    "",
			Path:     "/",
			MaxAge:   -1,
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			Secure:   req.TLS != nil || req.Header.Get("X-Forwarded-Proto") == "https",
		})
	}
	dest := buildSafeRedirect(r.basePath, rawReturn)
	http.Redirect(w, req, dest, http.StatusFound)
}

// buildSafeRedirect constructs a redirect URL from a validated return path.
// The return path is re-parsed with url.Parse and only the Path component is
// used, which breaks any taint chain from user input to the redirect target.
// This satisfies CodeQL's go/unvalidated-url-redirection rule.
func buildSafeRedirect(basePath, returnPath string) string {
	dest := basePath + "/"
	if returnPath == "" {
		return dest
	}
	parsed, err := url.Parse(returnPath)
	if err != nil || parsed.Host != "" || parsed.Scheme != "" {
		return dest
	}
	safePath := parsed.Path
	if basePath != "" && strings.HasPrefix(safePath, basePath+"/") {
		safePath = strings.TrimPrefix(safePath, basePath)
	}
	dest = basePath + safePath
	if parsed.RawQuery != "" {
		dest += "?" + parsed.RawQuery
	}
	if parsed.Fragment != "" {
		dest += "#" + parsed.Fragment
	}
	return dest
}

// redirectWithError redirects the user to the login page with an error message
// as a query parameter. Used for OIDC and other browser-navigated flows where
// JSON responses are not appropriate.
func (r *Router) redirectWithError(w http.ResponseWriter, req *http.Request, msg string) {
	http.Redirect(w, req, r.basePath+"/?error="+url.QueryEscape(msg), http.StatusFound)
}

// validateReturnURL checks that a return URL is a safe relative path.
// It rejects absolute URLs, protocol-relative URLs, and other open-redirect
// vectors. Returns the validated path or empty string if the input is invalid.
func validateReturnURL(rawURL string) string {
	if rawURL == "" {
		return ""
	}

	// Reject URLs containing ASCII control characters (CR, LF, null, etc.)
	// to prevent header injection via Location or HX-Redirect headers.
	for _, c := range rawURL {
		if c < 0x20 || c == 0x7f {
			slog.Debug("rejected return URL: contains control character", "raw", rawURL) //nolint:gosec // G706: debug audit log
			return ""
		}
	}

	// Strip backslashes to prevent bypass via "\" which some browsers
	// normalize to "/", turning "\evil.com" into "//evil.com".
	cleaned := strings.ReplaceAll(rawURL, "\\", "")
	if cleaned == "" {
		slog.Debug("rejected return URL: empty after backslash strip", "raw", rawURL) //nolint:gosec // G706: debug audit log; slog escapes values in structured output
		return ""
	}

	// Must start with a single forward slash (relative path).
	if !strings.HasPrefix(cleaned, "/") {
		slog.Debug("rejected return URL: no leading slash", "raw", rawURL) //nolint:gosec // G706: debug audit log
		return ""
	}

	// Reject protocol-relative URLs ("//evil.com").
	if strings.HasPrefix(cleaned, "//") {
		slog.Debug("rejected return URL: protocol-relative", "raw", rawURL) //nolint:gosec // G706: debug audit log
		return ""
	}

	// Reject any URL with a scheme (e.g. "javascript:", "data:", "http:").
	parsed, err := url.Parse(cleaned)
	if err != nil {
		slog.Debug("rejected return URL: parse error", "raw", rawURL, "error", err) //nolint:gosec // G706: debug audit log
		return ""
	}
	if parsed.Scheme != "" || parsed.Host != "" {
		slog.Debug("rejected return URL: has scheme or host", "raw", rawURL) //nolint:gosec // G706: debug audit log
		return ""
	}

	return cleaned
}

// handleLoginLocal performs local username/password authentication.
// Used as a legacy fallback when the auth registry is not configured.
func (r *Router) handleLoginLocal(w http.ResponseWriter, req *http.Request, username, password string) {
	token, err := r.authService.Login(req.Context(), username, password)
	if err != nil {
		r.logger.Warn("local login failed", "username", username)
		writeFormError(w, req, http.StatusUnauthorized, "Invalid username or password.")
		return
	}

	r.setSessionCookie(w, req, token)
	dest := buildSafeRedirect(r.basePath, validateReturnURL(req.FormValue("return_url")))
	w.Header().Set("HX-Redirect", dest)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleLoginFederated authenticates against an Emby or Jellyfin server and
// creates a local Stillwater session. Used as a legacy fallback when the auth
// registry is not configured.
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
		if errors.Is(err, auth.ErrUserNotConfigured) {
			r.logger.Warn("federated login: user not found", "method", authMethod, "provider_id", result.User.ID)
			writeFormError(w, req, http.StatusUnauthorized, "This account is not authorized for this Stillwater instance.")
		} else {
			r.logger.Error("federated session creation failed", "method", authMethod, "error", err)
			writeFormError(w, req, http.StatusInternalServerError, "An internal error occurred. Please try again.")
		}
		return
	}

	// Update the connection API key if the server issued a new access token.
	r.updateConnectionToken(req.Context(), authMethod, serverURL, result.User.ID, result.AccessToken)

	r.setSessionCookie(w, req, token)
	dest := buildSafeRedirect(r.basePath, validateReturnURL(req.FormValue("return_url")))
	w.Header().Set("HX-Redirect", dest)
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

// renderLoginPage renders the login page with all enabled auth providers.
// Reads the enabled state of each provider from the database settings so that
// toggling a provider in Settings is reflected on the login page immediately
// without a server restart.
func (r *Router) renderLoginPage(w http.ResponseWriter, req *http.Request) {
	providers := r.enabledAuthProviders(req.Context())
	oidcInfo := templates.OIDCLoginInfo{
		DisplayName: r.getStringSetting(req.Context(), "auth.providers.oidc.display_name", ""),
		LogoURL:     r.getStringSetting(req.Context(), "auth.providers.oidc.logo_url", ""),
	}
	renderTempl(w, req, templates.LoginPage(r.assets(), providers, oidcInfo))
}

// enabledAuthProviders builds the provider list for login and registration
// pages by reading the auth.providers.*.enabled settings from the database.
// This ensures that toggling a provider in Settings takes effect immediately.
func (r *Router) enabledAuthProviders(ctx context.Context) []auth.Authenticator {
	var providers []auth.Authenticator

	// Local authentication is always included: it provides break-glass access
	// when all federated providers are misconfigured.
	providers = append(providers, syntheticProvider{providerType: "local"})
	if r.getBoolSetting(ctx, "auth.providers.emby.enabled", false) {
		providers = append(providers, syntheticProvider{providerType: "emby"})
	}
	if r.getBoolSetting(ctx, "auth.providers.jellyfin.enabled", false) {
		providers = append(providers, syntheticProvider{providerType: "jellyfin"})
	}
	if r.getBoolSetting(ctx, "auth.providers.oidc.enabled", false) &&
		r.getStringSetting(ctx, "auth.providers.oidc.issuer_url", "") != "" &&
		r.getStringSetting(ctx, "auth.providers.oidc.client_id", "") != "" {
		providers = append(providers, syntheticProvider{providerType: "oidc"})
	}

	return providers
}

// handleRegisterPage serves the invite redemption page.
// If a ?code= query parameter is present and valid it pre-validates the invite
// and shows the registration form immediately; otherwise the user must enter
// the code manually.
// GET /register
func (r *Router) handleRegisterPage(w http.ResponseWriter, req *http.Request) {
	code := req.URL.Query().Get("code")

	data := templates.RegisterPageData{
		Code: code,
	}

	// Populate providers from database settings (same source as the login page).
	data.Providers = r.enabledAuthProviders(req.Context())

	// If a code was provided, validate it to show the invite info banner or an
	// error message without requiring a form submission.
	if code != "" && r.authService != nil {
		invite, err := r.authService.GetInviteByCode(req.Context(), code)
		switch {
		case err == nil:
			data.Invite = invite
			if inviter, lookupErr := r.authService.GetUserByID(req.Context(), invite.CreatedBy); lookupErr == nil {
				if inviter.DisplayName != "" {
					data.InviterName = inviter.DisplayName
				} else {
					data.InviterName = inviter.Username
				}
			} else {
				r.logger.Warn("inviter lookup failed, using generic label",
					"invite_id", invite.ID, "created_by", invite.CreatedBy, "error", lookupErr)
				data.InviterName = "an administrator"
			}
		case errors.Is(err, auth.ErrInviteNotFound):
			data.InviteError = "This invite code is invalid."
		case errors.Is(err, auth.ErrInviteRedeemed):
			data.InviteError = "This invite has already been used."
		case errors.Is(err, auth.ErrInviteExpired):
			data.InviteError = "This invite has expired."
		default:
			r.logger.Error("validating invite code for register page", "error", err)
			data.InviteError = "Unable to validate invite code. Please try again."
		}
	}

	renderTempl(w, req, templates.RegisterPage(r.assets(), data))
}

// syntheticProvider is a minimal auth.Authenticator implementation used as a
// fallback when the auth registry is not configured. It satisfies the interface
// so the login and register templates can iterate over a provider list without
// nil-checking the registry.
type syntheticProvider struct{ providerType string }

func (s syntheticProvider) Type() string { return s.providerType }
func (s syntheticProvider) Authenticate(_ context.Context, _ auth.Credentials) (*auth.Identity, error) {
	return nil, fmt.Errorf("synthetic provider does not support authentication")
}
func (s syntheticProvider) CanAutoProvision(_ *auth.Identity) bool { return false }
func (s syntheticProvider) MapRole(_ *auth.Identity) string        { return "" }

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
func (r *Router) updateConnectionToken(ctx context.Context, authMethod, serverURL, platformUserID, newToken string) {
	conn, err := r.connectionService.GetByTypeAndURL(ctx, authMethod, serverURL)
	if err != nil {
		r.logger.Warn("failed to look up connection for token update", "method", authMethod, "error", err)
		return
	}
	if conn == nil || conn.PlatformUserID != platformUserID {
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

	w.Header().Set("HX-Redirect", strings.TrimRight(r.basePath, "/")+"/")
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

	authMethod := body.AuthMethod
	if authMethod == "" {
		authMethod = "local"
	}

	// For federated providers, use the registry path when available. This ensures
	// the first user is always Administrator regardless of the provider's MapRole.
	// Local setup must skip the registry because there is no existing user to
	// authenticate against yet -- handleSetupLocal creates the user directly.
	if r.authRegistry != nil && authMethod != "local" {
		if provider, ok := r.authRegistry.Get(authMethod); ok {
			creds := auth.Credentials{
				Username: body.Username,
				Password: body.Password,
			}
			identity, err := provider.Authenticate(req.Context(), creds)
			if err != nil {
				r.logger.Warn("setup authentication failed", "provider", authMethod, "error", err)
				if errors.Is(err, auth.ErrInvalidCredentials) {
					writeFormError(w, req, http.StatusUnauthorized, fmt.Sprintf("Invalid %s credentials.", authMethodDisplayName(authMethod)))
				} else {
					writeFormError(w, req, http.StatusBadGateway, fmt.Sprintf("Cannot connect to %s server. Please verify the server is running.", authMethodDisplayName(authMethod)))
				}
				return
			}
			r.handleSetupWithIdentity(w, req, identity, authMethod, body.ServerURL)
			return
		}
	}

	// Legacy fallback when the registry is not configured.
	switch authMethod {
	case "local":
		r.handleSetupLocal(w, req, body.Username, body.Password)
	case "emby", "jellyfin":
		r.handleSetupFederated(w, req, authMethod, body.Username, body.Password, body.ServerURL)
	default:
		writeFormError(w, req, http.StatusBadRequest, "Unsupported auth method.")
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
	// default to local auth and the federated user cannot log in. On failure,
	// delete the user so setup can be retried.
	now := time.Now().UTC().Format(time.RFC3339)
	for k, v := range map[string]string{"auth.method": authMethod, "auth.server_url": cleanedURL} {
		if _, err := r.db.ExecContext(req.Context(),
			`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
			k, v, now); err != nil {
			r.logger.Error("failed to store auth setting, rolling back user", "key", k, "error", err)
			_, _ = r.db.ExecContext(req.Context(), `DELETE FROM users WHERE auth_provider = ? AND provider_id = ?`, authMethod, result.User.ID) //nolint:errcheck
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

// handleSetupWithIdentity completes the setup flow when the provider registry
// is configured. The first user is always created as Administrator regardless
// of what the provider's MapRole returns.
func (r *Router) handleSetupWithIdentity(w http.ResponseWriter, req *http.Request, identity *auth.Identity, authMethod, serverURL string) {
	ctx := req.Context()

	// Emby and Jellyfin require a server URL and admin flag on the remote account.
	requiresServerURL := authMethod == "emby" || authMethod == "jellyfin"
	if requiresServerURL {
		if serverURL == "" {
			writeFormError(w, req, http.StatusBadRequest, "Server URL is required for federated authentication.")
			return
		}
		cleanedURL, err := connection.ValidateBaseURL(serverURL)
		if err != nil {
			writeFormError(w, req, http.StatusBadRequest, "Invalid server URL.")
			return
		}
		serverURL = cleanedURL

		// Only media server administrators may complete the setup flow.
		if !identity.IsAdmin {
			writeFormError(w, req, http.StatusForbidden, fmt.Sprintf("Only %s administrator accounts can set up Stillwater.", authMethodDisplayName(authMethod)))
			return
		}
	}

	// The first user is always Administrator, regardless of MapRole.
	// This method is only called for federated providers (local setup uses
	// handleSetupLocal directly), so we always create a federated user.
	user, createErr := r.authService.CreateFederatedUser(ctx, identity, "administrator", "")
	if createErr != nil {
		r.logger.Error("failed to create federated admin account during setup",
			"provider", authMethod, "error", createErr)
		writeFormError(w, req, http.StatusInternalServerError, "An internal error occurred. Please try again.")
		return
	}

	// For media server providers (emby/jellyfin), persist auth settings and
	// auto-create the first server connection so the login page can redirect
	// correctly. Other federated provider types manage this separately.
	if requiresServerURL {
		// Store auth settings so the login page uses the correct provider.
		now := time.Now().UTC().Format(time.RFC3339)
		for k, v := range map[string]string{"auth.method": authMethod, "auth.server_url": serverURL} {
			if _, err := r.db.ExecContext(ctx,
				`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
				ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
				k, v, now); err != nil {
				r.logger.Error("failed to store auth setting, rolling back user", "key", k, "error", err)
				_, _ = r.db.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, user.ID) //nolint:errcheck
				writeFormError(w, req, http.StatusInternalServerError, "An internal error occurred. Please try again.")
				return
			}
		}

		// Auto-create the first server connection.
		connName := authMethodDisplayName(authMethod)
		conn := &connection.Connection{
			Name:                 connName,
			Type:                 authMethod,
			URL:                  serverURL,
			APIKey:               identity.RawToken,
			Enabled:              true,
			FeatureLibraryImport: true,
			FeatureNFOWrite:      true,
			FeatureImageWrite:    true,
			PlatformUserID:       identity.ProviderID,
		}
		if err := r.connectionService.Create(ctx, conn); err != nil {
			r.logger.Error("failed to auto-create connection during federated setup", "error", err)
			// Non-fatal: the user can add the connection manually later.
		} else if err := r.connectionService.UpdateStatus(ctx, conn.ID, "ok", ""); err != nil {
			r.logger.Warn("connection created but status update failed", "connection_id", conn.ID, "error", err)
		}
	}

	// Create a session to auto-login the new administrator.
	sessionToken, err := r.authService.CreateSession(ctx, user.ID)
	if err != nil {
		r.logger.Error("failed to create session after setup", "user_id", user.ID, "error", err)
		// Non-fatal: redirect to login page.
		w.Header().Set("HX-Redirect", r.basePath+"/")
		writeJSON(w, http.StatusCreated, map[string]string{"status": "admin account created"})
		return
	}

	r.setSessionCookie(w, req, sessionToken)
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

	renderTempl(w, req, templates.IndexPage(r.assetsFor(req)))
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

	// Fetch user's auth provider for OOBE auto-select
	user, err := r.authService.GetUserByID(req.Context(), userID)
	userAuthProvider := ""
	if err == nil && user != nil {
		userAuthProvider = user.AuthProvider
	} else if err != nil {
		r.logger.Warn("failed to lookup user for onboarding auth provider auto-select", "user_id", userID, "error", err)
	}

	data := templates.OnboardingData{
		Libraries:          libs,
		Profiles:           profiles,
		ProviderKeys:       providerKeys,
		WebSearchProviders: webSearchProviders,
		Connections:        conns,
		CurrentStep:        currentStep,
		UnidentifiedCount:  unidentifiedCount,
		UserAuthProvider:   userAuthProvider,
	}
	renderTempl(w, req, templates.OnboardingPage(r.assetsFor(req), data))
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

// handle404 serves the custom 404 error page for unmatched routes.
// JSON API clients (Accept: application/json or /api/ path prefix) and HTMX
// partial requests (HX-Request: true) receive a JSON error body so they are
// not served a full HTML document in a swap target.
func (r *Router) handle404(w http.ResponseWriter, req *http.Request) {
	isJSON := strings.Contains(req.Header.Get("Accept"), "application/json")
	isHTMX := req.Header.Get("HX-Request") == "true"
	isAPI := strings.HasPrefix(req.URL.Path, r.basePath+"/api/")
	if isJSON || isHTMX || isAPI {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	if err := templates.Error404Page(r.assetsFor(req)).Render(req.Context(), w); err != nil {
		r.logger.Error("rendering 404 page", "error", err)
		// Headers already sent; cannot write a different status.
	}
}

// ServeError500 renders the custom 500 error page. Other handlers can call
// this helper instead of http.Error when an unexpected internal error occurs
// and the request was a browser navigation (not a JSON API call).
// JSON API clients (Accept: application/json or /api/ path prefix) and HTMX
// partial requests (HX-Request: true) receive a JSON error body.
func (r *Router) ServeError500(w http.ResponseWriter, req *http.Request) {
	isJSON := strings.Contains(req.Header.Get("Accept"), "application/json")
	isHTMX := req.Header.Get("HX-Request") == "true"
	isAPI := strings.HasPrefix(req.URL.Path, r.basePath+"/api/")
	if isJSON || isHTMX || isAPI {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal server error"})
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusInternalServerError)
	if err := templates.Error500Page(r.assetsFor(req)).Render(req.Context(), w); err != nil {
		r.logger.Error("rendering 500 page", "error", err)
		// Headers already sent; cannot write a different status.
	}
}
