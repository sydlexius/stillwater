package api

import (
	"log/slog"
	"net/http"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/auth"
)

// Router sets up all HTTP routes for the application.
type Router struct {
	authService  *auth.Service
	logger       *slog.Logger
	basePath     string
	staticAssets *StaticAssets
}

// NewRouter creates a new Router with all routes configured.
func NewRouter(authService *auth.Service, logger *slog.Logger, basePath string, staticDir string) *Router {
	return &Router{
		authService:  authService,
		logger:       logger,
		basePath:     basePath,
		staticAssets: NewStaticAssets(staticDir, logger),
	}
}

// Handler returns the fully configured HTTP handler with middleware applied.
func (r *Router) Handler() http.Handler {
	authMw := middleware.Auth(r.authService)
	mux := http.NewServeMux()
	bp := r.basePath

	// Public routes (no auth)
	mux.HandleFunc("GET "+bp+"/api/v1/health", r.handleHealth)
	mux.HandleFunc("POST "+bp+"/api/v1/auth/login", r.handleLogin)
	mux.HandleFunc("POST "+bp+"/api/v1/auth/setup", r.handleSetup)
	mux.Handle("GET "+bp+"/static/", r.staticAssets.Handler(bp))
	mux.HandleFunc("GET "+bp+"/", r.handleIndex)

	// Protected routes (auth required)
	mux.HandleFunc("POST "+bp+"/api/v1/auth/logout", wrapAuth(r.handleLogout, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/auth/me", wrapAuth(r.handleMe, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/artists", wrapAuth(r.handleNotImplemented, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/artists/{id}", wrapAuth(r.handleNotImplemented, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/connections", wrapAuth(r.handleNotImplemented, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/connections", wrapAuth(r.handleNotImplemented, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/settings", wrapAuth(r.handleNotImplemented, authMw))
	mux.HandleFunc("PUT "+bp+"/api/v1/settings", wrapAuth(r.handleNotImplemented, authMw))

	// Apply logging to all requests
	return middleware.Logging(r.logger)(mux)
}

// wrapAuth wraps a handler function with auth middleware.
func wrapAuth(fn http.HandlerFunc, authMw func(http.Handler) http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authMw(fn).ServeHTTP(w, r)
	}
}
