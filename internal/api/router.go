package api

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/auth"
	"github.com/sydlexius/stillwater/internal/backup"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/nfo"
	"github.com/sydlexius/stillwater/internal/platform"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/internal/rule"
	"github.com/sydlexius/stillwater/internal/scanner"
	"github.com/sydlexius/stillwater/internal/scraper"
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
	WebSearchRegistry  *provider.WebSearchRegistry
	Orchestrator       *provider.Orchestrator
	RuleService        *rule.Service
	RuleEngine         *rule.Engine
	Pipeline           *rule.Pipeline
	BulkService        *rule.BulkService
	BulkExecutor       *rule.BulkExecutor
	NFOSnapshotService *nfo.SnapshotService
	ConnectionService  *connection.Service
	ScraperService     *scraper.Service
	WebhookService     *webhook.Service
	WebhookDispatcher  *webhook.Dispatcher
	BackupService      *backup.Service
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
	webSearchRegistry  *provider.WebSearchRegistry
	orchestrator       *provider.Orchestrator
	ruleService        *rule.Service
	ruleEngine         *rule.Engine
	pipeline           *rule.Pipeline
	bulkService        *rule.BulkService
	bulkExecutor       *rule.BulkExecutor
	nfoSnapshotService *nfo.SnapshotService
	connectionService  *connection.Service
	scraperService     *scraper.Service
	webhookService     *webhook.Service
	webhookDispatcher  *webhook.Dispatcher
	backupService      *backup.Service
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
		webSearchRegistry:  deps.WebSearchRegistry,
		orchestrator:       deps.Orchestrator,
		ruleService:        deps.RuleService,
		ruleEngine:         deps.RuleEngine,
		pipeline:           deps.Pipeline,
		bulkService:        deps.BulkService,
		bulkExecutor:       deps.BulkExecutor,
		nfoSnapshotService: deps.NFOSnapshotService,
		connectionService:  deps.ConnectionService,
		scraperService:     deps.ScraperService,
		webhookService:     deps.WebhookService,
		webhookDispatcher:  deps.WebhookDispatcher,
		backupService:      deps.BackupService,
		db:                 deps.DB,
		logger:             deps.Logger,
		basePath:           deps.BasePath,
		staticAssets:       NewStaticAssets(deps.StaticDir, deps.Logger),
	}
}

// Handler returns the fully configured HTTP handler with middleware applied.
// The provided context controls the lifecycle of background goroutines (e.g. rate limiter cleanup).
func (r *Router) Handler(ctx context.Context) http.Handler {
	authMw := middleware.Auth(r.authService)
	optAuthMw := middleware.OptionalAuth(r.authService)
	csrf := middleware.NewCSRF()
	loginRL := middleware.NewLoginRateLimiter(ctx)
	mux := http.NewServeMux()
	bp := r.basePath

	// Public routes (no auth)
	// Login and setup are exempt from CSRF (entry points) but rate-limited
	mux.HandleFunc("GET "+bp+"/api/v1/health", r.handleHealth)
	mux.HandleFunc("GET "+bp+"/api/v1/docs", r.handleAPIDocs)
	mux.HandleFunc("GET "+bp+"/api/v1/docs/openapi.yaml", r.handleOpenAPISpec)
	mux.Handle("POST "+bp+"/api/v1/auth/login", loginRL.Middleware(http.HandlerFunc(r.handleLogin)))
	mux.Handle("POST "+bp+"/api/v1/auth/setup", loginRL.Middleware(http.HandlerFunc(r.handleSetup)))
	mux.Handle("GET "+bp+"/static/", r.staticAssets.Handler(bp))
	mux.HandleFunc("GET "+bp+"/", wrapOptionalAuth(r.handleIndex, optAuthMw))

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
	mux.HandleFunc("GET "+bp+"/api/v1/connections/clobber-check", wrapAuth(r.handleClobberCheck, authMw))
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
	// Backup routes
	mux.HandleFunc("POST "+bp+"/api/v1/settings/backup", wrapAuth(r.handleBackupCreate, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/settings/backup/history", wrapAuth(r.handleBackupHistory, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/settings/backup/{filename}", wrapAuth(r.handleBackupDownload, authMw))

	// Provider routes
	mux.HandleFunc("GET "+bp+"/api/v1/providers", wrapAuth(r.handleListProviders, authMw))
	mux.HandleFunc("PUT "+bp+"/api/v1/providers/{name}/key", wrapAuth(r.handleSetProviderKey, authMw))
	mux.HandleFunc("DELETE "+bp+"/api/v1/providers/{name}/key", wrapAuth(r.handleDeleteProviderKey, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/providers/{name}/test", wrapAuth(r.handleTestProvider, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/providers/priorities", wrapAuth(r.handleGetPriorities, authMw))
	mux.HandleFunc("PUT "+bp+"/api/v1/providers/priorities", wrapAuth(r.handleSetPriorities, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/providers/search", wrapAuth(r.handleProviderSearch, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/providers/fetch", wrapAuth(r.handleProviderFetch, authMw))
	// Web search provider routes
	mux.HandleFunc("GET "+bp+"/api/v1/providers/websearch", wrapAuth(r.handleGetWebSearchProviders, authMw))
	mux.HandleFunc("PUT "+bp+"/api/v1/providers/websearch/{name}/toggle", wrapAuth(r.handleSetWebSearchEnabled, authMw))

	// Scraper config routes
	mux.HandleFunc("GET "+bp+"/api/v1/scraper/config", wrapAuth(r.handleGetScraperConfig, authMw))
	mux.HandleFunc("PUT "+bp+"/api/v1/scraper/config", wrapAuth(r.handleUpdateScraperConfig, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/scraper/config/connections/{id}", wrapAuth(r.handleGetConnectionScraperConfig, authMw))
	mux.HandleFunc("PUT "+bp+"/api/v1/scraper/config/connections/{id}", wrapAuth(r.handleUpdateConnectionScraperConfig, authMw))
	mux.HandleFunc("DELETE "+bp+"/api/v1/scraper/config/connections/{id}", wrapAuth(r.handleResetConnectionScraperConfig, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/scraper/providers", wrapAuth(r.handleListScraperProviders, authMw))

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

	// Field-level edit routes
	mux.HandleFunc("GET "+bp+"/api/v1/artists/{id}/fields/{field}/display", wrapAuth(r.handleFieldDisplay, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/artists/{id}/fields/{field}/edit", wrapAuth(r.handleFieldEdit, authMw))
	mux.HandleFunc("PATCH "+bp+"/api/v1/artists/{id}/fields/{field}", wrapAuth(r.handleFieldUpdate, authMw))
	mux.HandleFunc("DELETE "+bp+"/api/v1/artists/{id}/fields/{field}", wrapAuth(r.handleFieldClear, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/artists/{id}/fields/{field}/providers", wrapAuth(r.handleFieldProviders, authMw))
	// Refresh and disambiguation routes
	mux.HandleFunc("POST "+bp+"/api/v1/artists/{id}/refresh", wrapAuth(r.handleArtistRefresh, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/artists/{id}/refresh/search", wrapAuth(r.handleRefreshSearch, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/artists/{id}/refresh/link", wrapAuth(r.handleRefreshLink, authMw))

	// Image routes
	mux.HandleFunc("POST "+bp+"/api/v1/artists/{id}/images/upload", wrapAuth(r.handleImageUpload, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/artists/{id}/images/fetch", wrapAuth(r.handleImageFetch, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/artists/{id}/images/search", wrapAuth(r.handleImageSearch, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/artists/{id}/images/websearch", wrapAuth(r.handleWebImageSearch, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/artists/{id}/images/crop", wrapAuth(r.handleImageCrop, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/artists/{id}/images/{type}/file", wrapAuth(r.handleServeImage, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/artists/{id}/images/{type}/info", wrapAuth(r.handleImageInfo, authMw))
	mux.HandleFunc("DELETE "+bp+"/api/v1/artists/{id}/images/{type}", wrapAuth(r.handleDeleteImage, authMw))

	// Push routes
	mux.HandleFunc("POST "+bp+"/api/v1/artists/{id}/push", wrapAuth(r.handlePushMetadata, authMw))

	// Web routes (optional auth populates user context for login redirect)
	mux.HandleFunc("GET "+bp+"/artists/{id}/images", wrapOptionalAuth(r.handleArtistImagesPage, optAuthMw))
	mux.HandleFunc("GET "+bp+"/artists/{id}", wrapOptionalAuth(r.handleArtistDetailPage, optAuthMw))
	mux.HandleFunc("GET "+bp+"/artists", wrapOptionalAuth(r.handleArtistsPage, optAuthMw))
	mux.HandleFunc("GET "+bp+"/reports/compliance", wrapOptionalAuth(r.handleCompliancePage, optAuthMw))
	mux.HandleFunc("GET "+bp+"/artists/{id}/nfo", wrapOptionalAuth(r.handleNFODiffPage, optAuthMw))
	mux.HandleFunc("GET "+bp+"/settings", wrapOptionalAuth(r.handleSettingsPage, optAuthMw))
	mux.HandleFunc("GET "+bp+"/setup/wizard", wrapOptionalAuth(r.handleOnboardingPage, optAuthMw))

	// Apply middleware chain: security headers > logging > CSRF
	// Login and setup are exempt from CSRF (registered with rate limiter above)
	csrfExempt := []string{
		bp + "/api/v1/auth/login",
		bp + "/api/v1/auth/setup",
	}
	var handler http.Handler = mux
	handler = csrfWithExemptions(csrf, handler, csrfExempt)
	handler = middleware.Logging(r.logger)(handler)
	handler = middleware.SecurityHeaders(handler)
	return handler
}

// csrfWithExemptions wraps CSRF middleware but skips validation for exempt paths.
func csrfWithExemptions(csrf *middleware.CSRF, next http.Handler, exemptPaths []string) http.Handler {
	csrfHandler := csrf.Middleware(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, path := range exemptPaths {
			if r.URL.Path == path {
				next.ServeHTTP(w, r)
				return
			}
		}
		csrfHandler.ServeHTTP(w, r)
	})
}

// wrapAuth wraps a handler function with auth middleware.
func wrapAuth(fn http.HandlerFunc, authMw func(http.Handler) http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authMw(fn).ServeHTTP(w, r)
	}
}

// wrapOptionalAuth wraps a handler function with optional auth middleware.
func wrapOptionalAuth(fn http.HandlerFunc, mw func(http.Handler) http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		mw(fn).ServeHTTP(w, r)
	}
}
