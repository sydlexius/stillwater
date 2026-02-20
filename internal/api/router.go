package api

import (
	"log/slog"
	"net/http"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/auth"
	"github.com/sydlexius/stillwater/internal/platform"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/internal/rule"
	"github.com/sydlexius/stillwater/internal/scanner"
)

// Router sets up all HTTP routes for the application.
type Router struct {
	authService      *auth.Service
	artistService    *artist.Service
	scannerService   *scanner.Service
	platformService  *platform.Service
	providerSettings *provider.SettingsService
	providerRegistry *provider.Registry
	orchestrator     *provider.Orchestrator
	ruleService      *rule.Service
	ruleEngine       *rule.Engine
	pipeline         *rule.Pipeline
	logger           *slog.Logger
	basePath         string
	staticAssets     *StaticAssets
}

// NewRouter creates a new Router with all routes configured.
func NewRouter(authService *auth.Service, artistService *artist.Service, scannerService *scanner.Service, platformService *platform.Service, providerSettings *provider.SettingsService, providerRegistry *provider.Registry, orchestrator *provider.Orchestrator, ruleService *rule.Service, ruleEngine *rule.Engine, pipeline *rule.Pipeline, logger *slog.Logger, basePath string, staticDir string) *Router {
	return &Router{
		authService:      authService,
		artistService:    artistService,
		scannerService:   scannerService,
		platformService:  platformService,
		providerSettings: providerSettings,
		providerRegistry: providerRegistry,
		orchestrator:     orchestrator,
		ruleService:      ruleService,
		ruleEngine:       ruleEngine,
		pipeline:         pipeline,
		logger:           logger,
		basePath:         basePath,
		staticAssets:     NewStaticAssets(staticDir, logger),
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
	mux.HandleFunc("GET "+bp+"/api/v1/artists", wrapAuth(r.handleListArtists, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/artists/{id}", wrapAuth(r.handleGetArtist, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/scanner/run", wrapAuth(r.handleScannerRun, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/scanner/status", wrapAuth(r.handleScannerStatus, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/platforms", wrapAuth(r.handleListPlatforms, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/platforms/{id}", wrapAuth(r.handleGetPlatform, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/platforms", wrapAuth(r.handleCreatePlatform, authMw))
	mux.HandleFunc("PUT "+bp+"/api/v1/platforms/{id}", wrapAuth(r.handleUpdatePlatform, authMw))
	mux.HandleFunc("DELETE "+bp+"/api/v1/platforms/{id}", wrapAuth(r.handleDeletePlatform, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/platforms/{id}/activate", wrapAuth(r.handleSetActivePlatform, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/connections", wrapAuth(r.handleNotImplemented, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/connections", wrapAuth(r.handleNotImplemented, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/settings", wrapAuth(r.handleNotImplemented, authMw))
	mux.HandleFunc("PUT "+bp+"/api/v1/settings", wrapAuth(r.handleNotImplemented, authMw))

	// Provider routes
	mux.HandleFunc("GET "+bp+"/api/v1/providers", wrapAuth(r.handleListProviders, authMw))
	mux.HandleFunc("PUT "+bp+"/api/v1/providers/{name}/key", wrapAuth(r.handleSetProviderKey, authMw))
	mux.HandleFunc("DELETE "+bp+"/api/v1/providers/{name}/key", wrapAuth(r.handleDeleteProviderKey, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/providers/{name}/test", wrapAuth(r.handleTestProvider, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/providers/priorities", wrapAuth(r.handleGetPriorities, authMw))
	mux.HandleFunc("PUT "+bp+"/api/v1/providers/priorities", wrapAuth(r.handleSetPriorities, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/providers/search", wrapAuth(r.handleProviderSearch, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/providers/fetch", wrapAuth(r.handleProviderFetch, authMw))

	// Rule routes
	mux.HandleFunc("GET "+bp+"/api/v1/rules", wrapAuth(r.handleListRules, authMw))
	mux.HandleFunc("PUT "+bp+"/api/v1/rules/{id}", wrapAuth(r.handleUpdateRule, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/rules/{id}/run", wrapAuth(r.handleRunRule, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/rules/run-all", wrapAuth(r.handleRunAllRules, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/artists/{id}/health", wrapAuth(r.handleEvaluateArtist, authMw))

	// Image routes
	mux.HandleFunc("POST "+bp+"/api/v1/artists/{id}/images/upload", wrapAuth(r.handleImageUpload, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/artists/{id}/images/fetch", wrapAuth(r.handleImageFetch, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/artists/{id}/images/search", wrapAuth(r.handleImageSearch, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/artists/{id}/images/crop", wrapAuth(r.handleImageCrop, authMw))

	// Web routes (auth checked in handlers)
	mux.HandleFunc("GET "+bp+"/artists/{id}/images", r.handleArtistImagesPage)
	mux.HandleFunc("GET "+bp+"/artists/{id}", r.handleArtistDetailPage)
	mux.HandleFunc("GET "+bp+"/artists", r.handleArtistsPage)
	mux.HandleFunc("GET "+bp+"/settings", r.handleSettingsPage)

	// Apply logging to all requests
	return middleware.Logging(r.logger)(mux)
}

// wrapAuth wraps a handler function with auth middleware.
func wrapAuth(fn http.HandlerFunc, authMw func(http.Handler) http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authMw(fn).ServeHTTP(w, r)
	}
}
