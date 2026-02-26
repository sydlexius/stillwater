package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/a-h/templ"
	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/version"
	"github.com/sydlexius/stillwater/web/components"
	"github.com/sydlexius/stillwater/web/templates"
)

func (r *Router) handleHealth(w http.ResponseWriter, req *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status":  "ok",
		"version": version.Version,
		"commit":  version.Commit,
		"time":    time.Now().UTC().Format(time.RFC3339),
	})
}

// assets returns cache-busted asset paths for templates.
func (r *Router) assets() templates.AssetPaths {
	return templates.AssetPaths{
		CSS:        r.staticAssets.Path("/css/styles.css"),
		HTMX:       r.staticAssets.Path("/js/htmx.min.js"),
		CropperJS:  r.staticAssets.Path("/js/cropper.min.js"),
		CropperCSS: r.staticAssets.Path("/css/cropper.min.css"),
		ChartJS:    r.staticAssets.Path("/js/chart.min.js"),
		SortableJS: r.staticAssets.Path("/js/Sortable.min.js"),
		LoginBG:    r.staticAssets.Path("/img/login-bg.jpg"),
	}
}

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
		body.Username = req.FormValue("username")
		body.Password = req.FormValue("password")
	}

	token, err := r.authService.Login(req.Context(), body.Username, body.Password)
	if err != nil {
		writeFormError(w, req, http.StatusUnauthorized, "Invalid username or password.")
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   req.TLS != nil || req.Header.Get("X-Forwarded-Proto") == "https",
		MaxAge:   86400,
	})

	w.Header().Set("HX-Redirect", "/")
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

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

func (r *Router) handleMe(w http.ResponseWriter, req *http.Request) {
	userID := middleware.UserIDFromContext(req.Context())
	if userID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"user_id": userID})
}

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
		Username string `json:"username"`
		Password string `json:"password"` //nolint:gosec // G117: not a hardcoded secret, this is a request field
	}
	if strings.HasPrefix(req.Header.Get("Content-Type"), "application/json") {
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			writeFormError(w, req, http.StatusBadRequest, "Invalid request body.")
			return
		}
	} else {
		body.Username = req.FormValue("username")
		body.Password = req.FormValue("password")
	}

	if body.Username == "" || body.Password == "" {
		writeFormError(w, req, http.StatusBadRequest, "Username and password are required.")
		return
	}

	if len(body.Password) < 8 {
		writeFormError(w, req, http.StatusBadRequest, "Password must be at least 8 characters.")
		return
	}

	created, err := r.authService.Setup(req.Context(), body.Username, body.Password)
	if err != nil {
		r.logger.Error("failed to create admin account", "error", err)
		writeFormError(w, req, http.StatusInternalServerError, "An internal error occurred. Please try again.")
		return
	}

	if !created {
		writeFormError(w, req, http.StatusConflict, "An admin account already exists.")
		return
	}

	w.Header().Set("HX-Redirect", "/")
	writeJSON(w, http.StatusCreated, map[string]string{"status": "admin account created"})
}

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
		renderTempl(w, req, templates.LoginPage(r.assets()))
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

	// Load current step from settings (default to 1)
	currentStep := 1
	var stepStr string
	err = r.db.QueryRowContext(req.Context(), //nolint:gosec // G701: query is a string literal
		`SELECT value FROM settings WHERE key = 'onboarding.step'`).Scan(&stepStr)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		r.logger.Error("loading onboarding step", "error", err)
	} else if err == nil {
		switch stepStr {
		case "2":
			currentStep = 2
		case "3":
			currentStep = 3
		case "4":
			currentStep = 4
		}
	}

	conns, err := r.connectionService.List(req.Context())
	if err != nil {
		r.logger.Error("listing connections for onboarding", "error", err)
	}

	libs, err := r.libraryService.List(req.Context())
	if err != nil {
		r.logger.Error("listing libraries for onboarding", "error", err)
	}

	data := templates.OnboardingData{
		Libraries:    libs,
		Profiles:     profiles,
		ProviderKeys: providerKeys,
		Connections:  conns,
		CurrentStep:  currentStep,
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
