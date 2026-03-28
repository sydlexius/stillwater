package api

import (
	"crypto/subtle"
	"net/http"
	"time"

	"github.com/sydlexius/stillwater/internal/auth"
)

// handleOIDCLogin initiates the OIDC authorization code flow.
// It generates state and nonce parameters, stores them in HTTP-only cookies,
// and redirects the user to the identity provider's authorization endpoint.
// GET /api/v1/auth/oidc/login
func (r *Router) handleOIDCLogin(w http.ResponseWriter, req *http.Request) {
	if r.authRegistry == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "OIDC provider is not configured."})
		return
	}
	provider, ok := r.authRegistry.Get("oidc")
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "OIDC provider is not configured."})
		return
	}

	oidcProvider, ok := provider.(*auth.OIDCProvider)
	if !ok {
		r.logger.Error("oidc provider type assertion failed")
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "An internal error occurred. Please try again."})
		return
	}

	state, err := auth.GenerateState()
	if err != nil {
		r.logger.Error("failed to generate oidc state", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "An internal error occurred. Please try again."})
		return
	}

	nonce, err := auth.GenerateNonce()
	if err != nil {
		r.logger.Error("failed to generate oidc nonce", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "An internal error occurred. Please try again."})
		return
	}

	authURL, codeVerifier := oidcProvider.AuthURL(state, nonce)

	// Store state, nonce, and code verifier in HTTP-only cookies so they survive
	// the redirect round-trip. Each cookie expires after 10 minutes, which is
	// generous for an interactive login flow.
	isSecure := req.TLS != nil || req.Header.Get("X-Forwarded-Proto") == "https"
	cookieOpts := func(name, value string) *http.Cookie {
		return &http.Cookie{
			Name:     name,
			Value:    value,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			Secure:   isSecure,
			MaxAge:   int(10 * time.Minute / time.Second),
		}
	}

	http.SetCookie(w, cookieOpts("oidc_state", state))
	http.SetCookie(w, cookieOpts("oidc_nonce", nonce))
	http.SetCookie(w, cookieOpts("oidc_verifier", codeVerifier))

	http.Redirect(w, req, authURL, http.StatusFound)
}

// handleOIDCCallback handles the identity provider's redirect after authentication.
// It validates the state parameter, exchanges the authorization code for tokens,
// verifies the nonce, and then follows the standard login/provisioning flow.
// GET /api/v1/auth/oidc/callback
func (r *Router) handleOIDCCallback(w http.ResponseWriter, req *http.Request) {
	if r.authRegistry == nil {
		r.redirectWithError(w, req, "OIDC provider is not configured.")
		return
	}
	provider, ok := r.authRegistry.Get("oidc")
	if !ok {
		r.redirectWithError(w, req, "OIDC provider is not configured.")
		return
	}

	// Check for IdP-returned errors (e.g. user denied consent).
	if errParam := req.URL.Query().Get("error"); errParam != "" {
		desc := req.URL.Query().Get("error_description")
		r.logger.Warn("oidc callback received error from IdP", "error", errParam, "description", desc)
		r.redirectWithError(w, req, "Authentication was denied by the identity provider.")
		return
	}

	// Validate state parameter against the cookie to prevent CSRF.
	stateCookie, err := req.Cookie("oidc_state")
	if err != nil || stateCookie.Value == "" {
		r.redirectWithError(w, req, "Missing OIDC state. Please try logging in again.")
		return
	}

	stateParam := req.URL.Query().Get("state")
	if stateParam == "" || subtle.ConstantTimeCompare([]byte(stateParam), []byte(stateCookie.Value)) != 1 {
		r.logger.Warn("oidc callback: state mismatch", "expected_len", len(stateCookie.Value), "got_len", len(stateParam))
		r.redirectWithError(w, req, "Invalid OIDC state. Please try logging in again.")
		return
	}

	// Retrieve nonce and code verifier from cookies.
	nonceCookie, err := req.Cookie("oidc_nonce")
	if err != nil || nonceCookie.Value == "" {
		r.redirectWithError(w, req, "Missing OIDC nonce. Please try logging in again.")
		return
	}

	verifierCookie, err := req.Cookie("oidc_verifier")
	if err != nil || verifierCookie.Value == "" {
		r.redirectWithError(w, req, "Missing OIDC verifier. Please try logging in again.")
		return
	}

	// Clear the OIDC cookies now that we have consumed them.
	isSecure := req.TLS != nil || req.Header.Get("X-Forwarded-Proto") == "https"
	for _, name := range []string{"oidc_state", "oidc_nonce", "oidc_verifier"} {
		http.SetCookie(w, &http.Cookie{
			Name:     name,
			Value:    "",
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			Secure:   isSecure,
			MaxAge:   -1,
		})
	}

	code := req.URL.Query().Get("code")
	if code == "" {
		r.redirectWithError(w, req, "Missing authorization code. Please try logging in again.")
		return
	}

	// Exchange the authorization code for tokens and verify the ID token.
	creds := auth.Credentials{
		Code: code,
		Extra: map[string]string{
			"code_verifier": verifierCookie.Value,
		},
	}

	identity, err := provider.Authenticate(req.Context(), creds)
	if err != nil {
		r.logger.Error("oidc authentication failed", "error", err)
		r.redirectWithError(w, req, "OIDC authentication failed. Please try again.")
		return
	}

	// Validate the nonce from the ID token matches what we stored.
	tokenNonce, nonceOK := identity.Extra["nonce"]
	if !nonceOK || subtle.ConstantTimeCompare([]byte(tokenNonce), []byte(nonceCookie.Value)) != 1 {
		r.logger.Warn("oidc callback: nonce mismatch")
		r.redirectWithError(w, req, "Invalid OIDC nonce. Please try logging in again.")
		return
	}

	// Use the redirect-based login completion flow (browser navigation, not HTMX).
	r.completeLoginRedirect(w, req, provider, identity)
}
