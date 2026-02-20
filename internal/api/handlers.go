package api

import (
	"encoding/json"
	"net/http"
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
	}
}

func (r *Router) handleLogin(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"` //nolint:gosec // G117: not a hardcoded secret, this is a request field
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	token, err := r.authService.Login(req.Context(), body.Username, body.Password)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid credentials"})
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   true,
		MaxAge:   86400,
	})

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
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if hasUsers {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "admin account already exists"})
		return
	}

	var body struct {
		Username string `json:"username"`
		Password string `json:"password"` //nolint:gosec // G117: not a hardcoded secret, this is a request field
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	if body.Username == "" || body.Password == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "username and password are required"})
		return
	}

	if len(body.Password) < 8 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "password must be at least 8 characters"})
		return
	}

	created, err := r.authService.Setup(req.Context(), body.Username, body.Password)
	if err != nil {
		r.logger.Error("failed to create admin account", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	if !created {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "admin account already exists"})
		return
	}

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

	// Check auth
	userID := middleware.UserIDFromContext(req.Context())
	if userID == "" {
		renderTempl(w, req, templates.LoginPage(r.assets()))
		return
	}

	renderTempl(w, req, templates.IndexPage(r.assets()))
}

func renderTempl(w http.ResponseWriter, r *http.Request, component templ.Component) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := component.Render(r.Context(), w); err != nil {
		http.Error(w, "render error", http.StatusInternalServerError)
	}
}

func (r *Router) handleNotImplemented(w http.ResponseWriter, req *http.Request) {
	writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "not implemented"})
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

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, "encode error", http.StatusInternalServerError)
	}
}
