package api

import (
	"database/sql"
	"log/slog"
	"net/http"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/auth"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/nfo"
	"github.com/sydlexius/stillwater/internal/platform"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/internal/rule"
	"github.com/sydlexius/stillwater/internal/scanner"
	"github.com/sydlexius/stillwater/internal/webhook"
)

// RouterDeps bundles all dependencies needed by the HTTP router.
type RouterDeps struct {
	AuthService        *auth.Service
	ArtistService      *artist.Service
	ScannerService     *scanner.Service
	PlatformService    *platform.Service
	ProviderSettings   *provider.SettingsService
	ProviderRegistry   *provider.Registry
	Orchestrator       *provider.Orchestrator
	RuleService        *rule.Service
	RuleEngine         *rule.Engine
	Pipeline           *rule.Pipeline
	BulkService        *rule.BulkService
	BulkExecutor       *rule.BulkExecutor
	NFOSnapshotService *nfo.SnapshotService
	ConnectionService  *connection.Service
	WebhookService     *webhook.Service
	WebhookDispatcher  *webhook.Dispatcher
	DB                 *sql.DB
	Logger             *slog.Logger
	BasePath           string
	StaticDir          string
}

// Router sets up all HTTP routes for the application.
type Router struct {
	authService        *auth.Service
	artistService      *artist.Service
	scannerService     *scanner.Service
	platformService    *platform.Service
	providerSettings   *provider.SettingsService
	providerRegistry   *provider.Registry
	orchestrator       *provider.Orchestrator
	ruleService        *rule.Service
	ruleEngine         *rule.Engine
	pipeline           *rule.Pipeline
	bulkService        *rule.BulkService
	bulkExecutor       *rule.BulkExecutor
	nfoSnapshotService *nfo.SnapshotService
	connectionService  *connection.Service
	webhookService     *webhook.Service
	webhookDispatcher  *webhook.Dispatcher
	logger             *slog.Logger
	basePath           string
	staticAssets       *StaticAssets
	db                 *sql.DB
}

// NewRouter creates a new Router with all routes configured.
func NewRouter(deps RouterDeps) *Router {
	return &Router{
		authService:        deps.AuthService,
		artistService:      deps.ArtistService,
		scannerService:     deps.ScannerService,
		platformService:    deps.PlatformService,
		providerSettings:   deps.ProviderSettings,
		providerRegistry:   deps.ProviderRegistry,
		orchestrator:       deps.Orchestrator,
		ruleService:        deps.RuleService,
		ruleEngine:         deps.RuleEngine,
		pipeline:           deps.Pipeline,
		bulkService:        deps.BulkService,
		bulkExecutor:       deps.BulkExecutor,
		nfoSnapshotService: deps.NFOSnapshotService,
		connectionService:  deps.ConnectionService,
		webhookService:     deps.WebhookService,
		webhookDispatcher:  deps.WebhookDispatcher,
		db:                 deps.DB,
		logger:             deps.Logger,
		basePath:           deps.BasePath,
		staticAssets:       NewStaticAssets(deps.StaticDir, deps.Logger),
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
	mux.HandleFunc("GET "+bp+"/api/v1/artists/duplicates", wrapAuth(r.handleDuplicates, authMw))
	// Alias routes
	mux.HandleFunc("GET "+bp+"/api/v1/artists/{id}/aliases", wrapAuth(r.handleListAliases, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/artists/{id}/aliases", wrapAuth(r.handleAddAlias, authMw))
	mux.HandleFunc("DELETE "+bp+"/api/v1/artists/{id}/aliases/{aliasId}", wrapAuth(r.handleRemoveAlias, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/scanner/run", wrapAuth(r.handleScannerRun, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/scanner/status", wrapAuth(r.handleScannerStatus, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/platforms", wrapAuth(r.handleListPlatforms, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/platforms/{id}", wrapAuth(r.handleGetPlatform, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/platforms", wrapAuth(r.handleCreatePlatform, authMw))
	mux.HandleFunc("PUT "+bp+"/api/v1/platforms/{id}", wrapAuth(r.handleUpdatePlatform, authMw))
	mux.HandleFunc("DELETE "+bp+"/api/v1/platforms/{id}", wrapAuth(r.handleDeletePlatform, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/platforms/{id}/activate", wrapAuth(r.handleSetActivePlatform, authMw))
	// Connection routes
	mux.HandleFunc("GET "+bp+"/api/v1/connections", wrapAuth(r.handleListConnections, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/connections", wrapAuth(r.handleCreateConnection, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/connections/{id}", wrapAuth(r.handleGetConnection, authMw))
	mux.HandleFunc("PUT "+bp+"/api/v1/connections/{id}", wrapAuth(r.handleUpdateConnection, authMw))
	mux.HandleFunc("DELETE "+bp+"/api/v1/connections/{id}", wrapAuth(r.handleDeleteConnection, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/connections/{id}/test", wrapAuth(r.handleTestConnection, authMw))
	// Webhook routes
	mux.HandleFunc("GET "+bp+"/api/v1/webhooks", wrapAuth(r.handleListWebhooks, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/webhooks", wrapAuth(r.handleCreateWebhook, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/webhooks/{id}", wrapAuth(r.handleGetWebhook, authMw))
	mux.HandleFunc("PUT "+bp+"/api/v1/webhooks/{id}", wrapAuth(r.handleUpdateWebhook, authMw))
	mux.HandleFunc("DELETE "+bp+"/api/v1/webhooks/{id}", wrapAuth(r.handleDeleteWebhook, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/webhooks/{id}/test", wrapAuth(r.handleTestWebhook, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/settings", wrapAuth(r.handleGetSettings, authMw))
	mux.HandleFunc("PUT "+bp+"/api/v1/settings", wrapAuth(r.handleUpdateSettings, authMw))

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
	mux.HandleFunc("GET "+bp+"/api/v1/rules/classical-mode", wrapAuth(r.handleGetClassicalMode, authMw))
	mux.HandleFunc("PUT "+bp+"/api/v1/rules/classical-mode", wrapAuth(r.handleSetClassicalMode, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/artists/{id}/health", wrapAuth(r.handleEvaluateArtist, authMw))

	// Bulk operation routes
	mux.HandleFunc("POST "+bp+"/api/v1/bulk/fetch-metadata", wrapAuth(r.handleBulkFetchMetadata, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/bulk/fetch-images", wrapAuth(r.handleBulkFetchImages, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/bulk/jobs", wrapAuth(r.handleBulkJobList, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/bulk/jobs/{id}", wrapAuth(r.handleBulkJobStatus, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/bulk/jobs/{id}/cancel", wrapAuth(r.handleBulkJobCancel, authMw))

	// Report routes
	mux.HandleFunc("GET "+bp+"/api/v1/reports/health", wrapAuth(r.handleReportHealth, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/reports/health/history", wrapAuth(r.handleReportHealthHistory, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/reports/compliance", wrapAuth(r.handleReportCompliance, authMw))

	// NFO snapshot routes
	mux.HandleFunc("GET "+bp+"/api/v1/artists/{id}/nfo/diff", wrapAuth(r.handleNFODiff, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/artists/{id}/nfo/conflict", wrapAuth(r.handleNFOConflictCheck, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/artists/{id}/nfo/snapshots", wrapAuth(r.handleNFOSnapshotList, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/artists/{id}/nfo/snapshots/{snapshotId}/restore", wrapAuth(r.handleNFOSnapshotRestore, authMw))

	// Image routes
	mux.HandleFunc("POST "+bp+"/api/v1/artists/{id}/images/upload", wrapAuth(r.handleImageUpload, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/artists/{id}/images/fetch", wrapAuth(r.handleImageFetch, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/artists/{id}/images/search", wrapAuth(r.handleImageSearch, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/artists/{id}/images/crop", wrapAuth(r.handleImageCrop, authMw))

	// Push routes
	mux.HandleFunc("POST "+bp+"/api/v1/artists/{id}/push", wrapAuth(r.handlePushMetadata, authMw))

	// Web routes (auth checked in handlers)
	mux.HandleFunc("GET "+bp+"/artists/{id}/images", r.handleArtistImagesPage)
	mux.HandleFunc("GET "+bp+"/artists/{id}", r.handleArtistDetailPage)
	mux.HandleFunc("GET "+bp+"/artists", r.handleArtistsPage)
	mux.HandleFunc("GET "+bp+"/reports/compliance", r.handleCompliancePage)
	mux.HandleFunc("GET "+bp+"/artists/{id}/nfo", r.handleNFODiffPage)
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
