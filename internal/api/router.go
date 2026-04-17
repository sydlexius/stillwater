package api

import (
	"context"
	"database/sql"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/auth"
	"github.com/sydlexius/stillwater/internal/backup"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/event"
	"github.com/sydlexius/stillwater/internal/i18n"
	img "github.com/sydlexius/stillwater/internal/image"
	"github.com/sydlexius/stillwater/internal/library"
	"github.com/sydlexius/stillwater/internal/logging"
	"github.com/sydlexius/stillwater/internal/maintenance"
	"github.com/sydlexius/stillwater/internal/nfo"
	"github.com/sydlexius/stillwater/internal/platform"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/internal/publish"
	"github.com/sydlexius/stillwater/internal/rule"
	"github.com/sydlexius/stillwater/internal/scanner"
	"github.com/sydlexius/stillwater/internal/scraper"
	"github.com/sydlexius/stillwater/internal/settingsio"
	"github.com/sydlexius/stillwater/internal/updater"
	"github.com/sydlexius/stillwater/internal/watcher"
	"github.com/sydlexius/stillwater/internal/webhook"
	"github.com/sydlexius/stillwater/web/templates"
)

// RouterDeps bundles all dependencies needed by the HTTP router.
type RouterDeps struct {
	AuthService        *auth.Service
	AuthRegistry       *auth.Registry
	ArtistService      *artist.Service
	HistoryService     *artist.HistoryService
	ScannerService     *scanner.Service
	PlatformService    *platform.Service
	ProviderSettings   *provider.SettingsService
	ProviderRegistry   *provider.Registry
	WebSearchRegistry  *provider.WebSearchRegistry
	WebScraperRegistry *provider.WebScraperRegistry
	RateLimiters       *provider.RateLimiterMap
	Orchestrator       *provider.Orchestrator
	RuleService        *rule.Service
	RuleEngine         *rule.Engine
	Pipeline           rule.PipelineRunner
	BulkService        *rule.BulkService
	BulkExecutor       *rule.BulkExecutor
	RuleScheduler      *rule.Scheduler
	NFOSnapshotService *nfo.SnapshotService
	NFOSettingsService *nfo.NFOSettingsService
	ConnectionService  *connection.Service
	ScraperService     *scraper.Service
	LibraryService     *library.Service
	WebhookService     *webhook.Service
	WebhookDispatcher  *webhook.Dispatcher
	BackupService      *backup.Service
	LogManager         *logging.Manager
	MaintenanceService *maintenance.Service
	SettingsIOService  *settingsio.Service
	UpdaterService     *updater.Service
	ProbeCache         *watcher.ProbeCache
	ExpectedWrites     *watcher.ExpectedWrites
	EventBus           *event.Bus
	DB                 *sql.DB
	Logger             *slog.Logger
	BasePath           string
	BasePathFromEnv    bool
	StaticFS           fs.FS
	ImageCacheDir      string
	Publisher          *publish.Publisher
	SSEHub             *SSEHub
	I18nBundle         *i18n.Bundle
}

// Router sets up all HTTP routes for the application.
type Router struct {
	authService        *auth.Service
	authRegistry       *auth.Registry
	artistService      *artist.Service
	historyService     *artist.HistoryService
	scannerService     *scanner.Service
	platformService    *platform.Service
	providerSettings   *provider.SettingsService
	providerRegistry   *provider.Registry
	webSearchRegistry  *provider.WebSearchRegistry
	webScraperRegistry *provider.WebScraperRegistry
	rateLimiters       *provider.RateLimiterMap
	orchestrator       *provider.Orchestrator
	ruleService        *rule.Service
	ruleEngine         *rule.Engine
	pipeline           rule.PipelineRunner
	bulkService        *rule.BulkService
	bulkExecutor       *rule.BulkExecutor
	ruleScheduler      *rule.Scheduler
	nfoSnapshotService *nfo.SnapshotService
	nfoSettingsService *nfo.NFOSettingsService
	connectionService  *connection.Service
	scraperService     *scraper.Service
	libraryService     *library.Service
	webhookService     *webhook.Service
	webhookDispatcher  *webhook.Dispatcher
	backupService      *backup.Service
	logManager         *logging.Manager
	maintenanceService *maintenance.Service
	settingsIOService  *settingsio.Service
	updaterService     *updater.Service
	probeCache         *watcher.ProbeCache
	expectedWrites     *watcher.ExpectedWrites
	eventBus           *event.Bus
	publisher          *publish.Publisher
	logger             *slog.Logger
	basePath           string
	basePathFromEnv    bool
	imageCacheDir      string
	staticAssets       *StaticAssets
	db                 *sql.DB
	fileRemover        FileRemover
	ssrfClient         *http.Client
	libraryOps         map[string]*LibraryOpResult
	libraryOpsMu       sync.Mutex
	ruleRun            *ruleRunStatus
	ruleRunMu          sync.Mutex
	fixAllProgress     *FixAllProgress
	fixAllMu           sync.RWMutex
	identifyProgress   *IdentifyProgress
	identifyMu         sync.RWMutex
	bulkActionProgress *BulkActionProgress
	bulkActionMu       sync.RWMutex
	// reIdentifyWizardStore backs the interactive re-identify review flow.
	// Sessions are in-memory, TTL-bounded, and never persisted across
	// restarts; the flow is an interactive user task so lossy restart
	// semantics are acceptable and avoid a schema change for this RC fix.
	reIdentifyWizardStore *reIdentifyWizardStore
	undoStore             *rule.UndoStore
	sseHub                *SSEHub
	i18nBundle            *i18n.Bundle
}

// NewRouter creates a new Router with all routes configured.
func NewRouter(deps RouterDeps) *Router {
	r := &Router{
		authService:        deps.AuthService,
		authRegistry:       deps.AuthRegistry,
		artistService:      deps.ArtistService,
		historyService:     deps.HistoryService,
		scannerService:     deps.ScannerService,
		platformService:    deps.PlatformService,
		providerSettings:   deps.ProviderSettings,
		providerRegistry:   deps.ProviderRegistry,
		webSearchRegistry:  deps.WebSearchRegistry,
		webScraperRegistry: deps.WebScraperRegistry,
		rateLimiters:       deps.RateLimiters,
		orchestrator:       deps.Orchestrator,
		ruleService:        deps.RuleService,
		ruleEngine:         deps.RuleEngine,
		pipeline:           deps.Pipeline,
		bulkService:        deps.BulkService,
		bulkExecutor:       deps.BulkExecutor,
		ruleScheduler:      deps.RuleScheduler,
		nfoSnapshotService: deps.NFOSnapshotService,
		nfoSettingsService: deps.NFOSettingsService,
		connectionService:  deps.ConnectionService,
		scraperService:     deps.ScraperService,
		libraryService:     deps.LibraryService,
		webhookService:     deps.WebhookService,
		webhookDispatcher:  deps.WebhookDispatcher,
		backupService:      deps.BackupService,
		logManager:         deps.LogManager,
		maintenanceService: deps.MaintenanceService,
		settingsIOService:  deps.SettingsIOService,
		updaterService:     deps.UpdaterService,
		probeCache:         deps.ProbeCache,
		expectedWrites:     deps.ExpectedWrites,
		eventBus:           deps.EventBus,
		db:                 deps.DB,
		logger:             deps.Logger,
		basePath:           deps.BasePath,
		basePathFromEnv:    deps.BasePathFromEnv,
		imageCacheDir:      deps.ImageCacheDir,
		publisher:          deps.Publisher,
		sseHub:             deps.SSEHub,
		staticAssets:       NewStaticAssets(deps.StaticFS, deps.Logger),
		ssrfClient: &http.Client{
			Timeout:   fetchTimeout,
			Transport: ssrfSafeTransport(),
		},
		fileRemover:           osRemover{},
		libraryOps:            make(map[string]*LibraryOpResult),
		undoStore:             rule.NewUndoStore(),
		i18nBundle:            deps.I18nBundle,
		reIdentifyWizardStore: newReIdentifyWizardStore(),
	}

	// Auto-init the SSE hub if not provided by the caller, so the /events/stream
	// endpoint is always functional even when main.go does not wire one.
	if r.sseHub == nil {
		r.sseHub = NewSSEHub(deps.Logger)
	}
	if deps.EventBus != nil {
		r.sseHub.SubscribeToEventBus(deps.EventBus)
	}

	// Configure the static asset base path used by template helpers (logoSrc, etc.)
	// so that sub-path deployments produce correct URLs.
	templates.SetBasePath(deps.BasePath)

	return r
}

// Handler returns the fully configured HTTP handler with middleware applied.
// The provided context controls the lifecycle of background goroutines (e.g. rate limiter cleanup).
func (r *Router) Handler(ctx context.Context) http.Handler {
	authMw := middleware.Auth(r.authService)
	optAuthMw := middleware.OptionalAuth(r.authService)
	csrf := middleware.NewCSRF()
	loginRL := middleware.NewLoginRateLimiter(ctx)
	requireMultiUser := middleware.RequireMultiUser(r.getStringSetting)

	// Start periodic expiry of stale undo entries so the store does not grow unbounded.
	if r.undoStore != nil {
		r.undoStore.StartCleanup(ctx)
	}
	mux := http.NewServeMux()
	bp := r.basePath

	// Public routes (no auth)
	// Login and setup are exempt from CSRF (entry points) but rate-limited
	mux.HandleFunc("GET "+bp+"/api/v1/health", r.handleHealth)
	mux.HandleFunc("GET "+bp+"/api/v1/docs", r.handleAPIDocs)
	mux.HandleFunc("GET "+bp+"/api/v1/docs/openapi.yaml", r.handleOpenAPISpec)
	mux.Handle("POST "+bp+"/api/v1/auth/login", loginRL.Middleware(http.HandlerFunc(r.handleLogin)))
	mux.Handle("POST "+bp+"/api/v1/auth/setup", loginRL.Middleware(http.HandlerFunc(r.handleSetup)))
	mux.Handle("POST "+bp+"/api/v1/users/register", loginRL.Middleware(requireMultiUser(r.handleRegister)))
	// OIDC authentication flow (public, rate-limited)
	mux.Handle("GET "+bp+"/api/v1/auth/oidc/login", loginRL.Middleware(http.HandlerFunc(r.handleOIDCLogin)))
	mux.Handle("GET "+bp+"/api/v1/auth/oidc/callback", loginRL.Middleware(http.HandlerFunc(r.handleOIDCCallback)))
	mux.HandleFunc("GET "+bp+"/register", requireMultiUser(r.handleRegisterPage))
	mux.Handle("GET "+bp+"/static/", r.staticAssets.Handler(bp))
	mux.HandleFunc("GET "+bp+"/", wrapOptionalAuth(r.handleIndex, optAuthMw))

	// Protected routes (auth required)
	mux.HandleFunc("POST "+bp+"/api/v1/auth/logout", wrapAuth(r.handleLogout, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/auth/me", wrapAuth(r.handleMe, authMw))
	// API token routes
	mux.HandleFunc("POST "+bp+"/api/v1/auth/tokens", wrapAuth(r.handleCreateAPIToken, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/auth/tokens", wrapAuth(r.handleListAPITokens, authMw))
	mux.HandleFunc("DELETE "+bp+"/api/v1/auth/tokens/{id}", wrapAuth(r.handleRevokeAPIToken, authMw))
	mux.HandleFunc("DELETE "+bp+"/api/v1/auth/tokens/{id}/permanent", wrapAuth(r.handleDeleteAPIToken, authMw))
	// User preferences routes (per-user, no admin required)
	mux.HandleFunc("GET "+bp+"/api/v1/preferences", wrapAuth(r.handleGetPreferences, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/preferences/{key}", wrapAuth(r.handleGetPreference, authMw))
	mux.HandleFunc("PUT "+bp+"/api/v1/preferences/{key}", wrapAuth(r.handleUpdatePreference, authMw))
	// User management routes (multi-user gate + admin role required)
	mux.HandleFunc("POST "+bp+"/api/v1/users/invites", wrapAuth(requireMultiUser(middleware.RequireAdmin(r.handleCreateInvite)), authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/users/invites", wrapAuth(requireMultiUser(middleware.RequireAdmin(r.handleListInvites)), authMw))
	mux.HandleFunc("DELETE "+bp+"/api/v1/users/invites/{id}", wrapAuth(requireMultiUser(middleware.RequireAdmin(r.handleRevokeInvite)), authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/users", wrapAuth(requireMultiUser(middleware.RequireAdmin(r.handleListUsers)), authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/users/{id}", wrapAuth(requireMultiUser(r.handleGetUser), authMw))
	mux.HandleFunc("PATCH "+bp+"/api/v1/users/{id}", wrapAuth(requireMultiUser(middleware.RequireAdmin(r.handleUpdateUser)), authMw))
	mux.HandleFunc("DELETE "+bp+"/api/v1/users/{id}", wrapAuth(requireMultiUser(middleware.RequireAdmin(r.handleDeactivateUser)), authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/artists", wrapAuth(r.handleListArtists, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/artists/badge", wrapAuth(r.handleArtistsBadge, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/artists/locked", wrapAuth(r.handleListLockedArtists, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/artists/{id}", wrapAuth(r.handleGetArtist, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/artists/duplicates", wrapAuth(r.handleDuplicates, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/artists/{id}/lock", wrapAuth(r.handleLockArtist, authMw))
	mux.HandleFunc("DELETE "+bp+"/api/v1/artists/{id}/lock", wrapAuth(r.handleUnlockArtist, authMw))
	// Field-level and per-image lock toggles for platforms that support
	// granular lock semantics (Emby LockedFields, image LockData).
	mux.HandleFunc("POST "+bp+"/api/v1/artists/{id}/field-locks/{field}", wrapAuth(r.handleLockArtistField, authMw))
	mux.HandleFunc("DELETE "+bp+"/api/v1/artists/{id}/field-locks/{field}", wrapAuth(r.handleUnlockArtistField, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/artists/{id}/image-locks/{imageId}", wrapAuth(r.handleLockArtistImage, authMw))
	mux.HandleFunc("DELETE "+bp+"/api/v1/artists/{id}/image-locks/{imageId}", wrapAuth(r.handleUnlockArtistImage, authMw))
	// Alias routes
	mux.HandleFunc("GET "+bp+"/api/v1/artists/{id}/aliases", wrapAuth(r.handleListAliases, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/artists/{id}/aliases", wrapAuth(r.handleAddAlias, authMw))
	mux.HandleFunc("DELETE "+bp+"/api/v1/artists/{id}/aliases/{aliasId}", wrapAuth(r.handleRemoveAlias, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/scanner/run", wrapAuth(r.handleScannerRun, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/scanner/status", wrapAuth(r.handleScannerStatus, authMw))
	// Library routes (create/update/delete require admin)
	mux.HandleFunc("GET "+bp+"/api/v1/libraries", wrapAuth(r.handleListLibraries, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/libraries/{id}", wrapAuth(r.handleGetLibrary, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/libraries", wrapAuth(middleware.RequireAdmin(r.handleCreateLibrary), authMw))
	mux.HandleFunc("PUT "+bp+"/api/v1/libraries/{id}", wrapAuth(middleware.RequireAdmin(r.handleUpdateLibrary), authMw))
	mux.HandleFunc("DELETE "+bp+"/api/v1/libraries/{id}", wrapAuth(middleware.RequireAdmin(r.handleDeleteLibrary), authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/platforms", wrapAuth(r.handleListPlatforms, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/platforms/{id}", wrapAuth(r.handleGetPlatform, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/platforms", wrapAuth(r.handleCreatePlatform, authMw))
	mux.HandleFunc("PUT "+bp+"/api/v1/platforms/{id}", wrapAuth(r.handleUpdatePlatform, authMw))
	mux.HandleFunc("DELETE "+bp+"/api/v1/platforms/{id}", wrapAuth(r.handleDeletePlatform, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/platforms/{id}/activate", wrapAuth(r.handleSetActivePlatform, authMw))
	// Connection routes (create/update/delete require admin; read and test are operator-accessible)
	mux.HandleFunc("GET "+bp+"/api/v1/connections", wrapAuth(r.handleListConnections, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/connections", wrapAuth(middleware.RequireAdmin(r.handleCreateConnection), authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/connections/clobber-check", wrapAuth(r.handleClobberCheck, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/connections/{id}", wrapAuth(r.handleGetConnection, authMw))
	mux.HandleFunc("PUT "+bp+"/api/v1/connections/{id}", wrapAuth(middleware.RequireAdmin(r.handleUpdateConnection), authMw))
	mux.HandleFunc("DELETE "+bp+"/api/v1/connections/{id}", wrapAuth(middleware.RequireAdmin(r.handleDeleteConnection), authMw))
	mux.HandleFunc("PATCH "+bp+"/api/v1/connections/{id}/features", wrapAuth(middleware.RequireAdmin(r.handleUpdateConnectionFeatures), authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/connections/{id}/test", wrapAuth(r.handleTestConnection, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/connections/{id}/platform-settings", wrapAuth(r.handleGetPlatformSettings, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/connections/{id}/platform-settings/disable", wrapAuth(middleware.RequireAdmin(r.handleDisablePlatformSettings), authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/connections/{id}/platform-summary", wrapAuth(r.handleGetPlatformSummary, authMw))
	// Connection library discovery/import routes
	mux.HandleFunc("GET "+bp+"/api/v1/connections/{id}/libraries", wrapAuth(r.handleDiscoverLibraries, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/connections/{id}/libraries/import", wrapAuth(middleware.RequireAdmin(r.handleImportLibraries), authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/connections/{id}/libraries/{libId}/populate", wrapAuth(middleware.RequireAdmin(r.handlePopulateLibrary), authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/connections/{id}/libraries/{libId}/scan", wrapAuth(middleware.RequireAdmin(r.handleScanLibrary), authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/libraries/{libId}/operation/status", wrapAuth(r.handleLibraryOpStatus, authMw))
	// Inbound webhook routes (API token with webhook scope)
	mux.HandleFunc("POST "+bp+"/api/v1/webhooks/inbound/lidarr",
		wrapAuth(middleware.RequireScope("webhook")(r.handleLidarrWebhook), authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/webhooks/inbound/emby",
		wrapAuth(middleware.RequireScope("webhook")(r.handleEmbyWebhook), authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/webhooks/inbound/jellyfin",
		wrapAuth(middleware.RequireScope("webhook")(r.handleJellyfinWebhook), authMw))
	// Webhook routes (admin required for mutating operations)
	mux.HandleFunc("GET "+bp+"/api/v1/webhooks", wrapAuth(middleware.RequireAdmin(r.handleListWebhooks), authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/webhooks", wrapAuth(middleware.RequireAdmin(r.handleCreateWebhook), authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/webhooks/{id}", wrapAuth(middleware.RequireAdmin(r.handleGetWebhook), authMw))
	mux.HandleFunc("PUT "+bp+"/api/v1/webhooks/{id}", wrapAuth(middleware.RequireAdmin(r.handleUpdateWebhook), authMw))
	mux.HandleFunc("DELETE "+bp+"/api/v1/webhooks/{id}", wrapAuth(middleware.RequireAdmin(r.handleDeleteWebhook), authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/webhooks/{id}/test", wrapAuth(middleware.RequireAdmin(r.handleTestWebhook), authMw))
	// Settings routes (all require admin)
	mux.HandleFunc("GET "+bp+"/api/v1/settings", wrapAuth(middleware.RequireAdmin(r.handleGetSettings), authMw))
	mux.HandleFunc("PUT "+bp+"/api/v1/settings", wrapAuth(middleware.RequireAdmin(r.handleUpdateSettings), authMw))
	// Backup routes (admin only)
	mux.HandleFunc("POST "+bp+"/api/v1/settings/backup", wrapAuth(middleware.RequireAdmin(r.handleBackupCreate), authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/settings/backup/history", wrapAuth(middleware.RequireAdmin(r.handleBackupHistory), authMw))
	mux.HandleFunc("DELETE "+bp+"/api/v1/settings/backup/{filename}", wrapAuth(middleware.RequireAdmin(r.handleBackupDelete), authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/settings/backup/{filename}", wrapAuth(middleware.RequireAdmin(r.handleBackupDownload), authMw))
	// Logging routes (admin only)
	mux.HandleFunc("GET "+bp+"/api/v1/settings/logging", wrapAuth(middleware.RequireAdmin(r.handleGetLogging), authMw))
	mux.HandleFunc("PUT "+bp+"/api/v1/settings/logging", wrapAuth(middleware.RequireAdmin(r.handleUpdateLogging), authMw))
	// Log viewer routes (admin only)
	mux.HandleFunc("GET "+bp+"/api/v1/logs", wrapAuth(middleware.RequireAdmin(r.handleGetLogs), authMw))
	mux.HandleFunc("DELETE "+bp+"/api/v1/logs", wrapAuth(middleware.RequireAdmin(r.handleClearLogs), authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/logs/files", wrapAuth(middleware.RequireAdmin(r.handleListLogFiles), authMw))
	mux.HandleFunc("DELETE "+bp+"/api/v1/logs/files", wrapAuth(middleware.RequireAdmin(r.handleDeleteLogFiles), authMw))
	// Maintenance routes (admin only)
	mux.HandleFunc("GET "+bp+"/api/v1/settings/maintenance/status", wrapAuth(middleware.RequireAdmin(r.handleMaintenanceStatus), authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/settings/maintenance/optimize", wrapAuth(middleware.RequireAdmin(r.handleMaintenanceOptimize), authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/settings/maintenance/vacuum", wrapAuth(middleware.RequireAdmin(r.handleMaintenanceVacuum), authMw))
	mux.HandleFunc("PUT "+bp+"/api/v1/settings/maintenance/schedule", wrapAuth(middleware.RequireAdmin(r.handleMaintenanceSchedule), authMw))
	// Cache routes (admin only)
	mux.HandleFunc("GET "+bp+"/api/v1/settings/cache/stats", wrapAuth(middleware.RequireAdmin(r.handleCacheStats), authMw))
	mux.HandleFunc("DELETE "+bp+"/api/v1/settings/cache", wrapAuth(middleware.RequireAdmin(r.handleCacheClear), authMw))
	// Settings export/import routes (admin only)
	mux.HandleFunc("POST "+bp+"/api/v1/settings/export", wrapAuth(middleware.RequireAdmin(r.handleSettingsExport), authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/settings/import", wrapAuth(middleware.RequireAdmin(r.handleSettingsImport), authMw))
	// NFO output settings routes (admin only)
	mux.HandleFunc("GET "+bp+"/api/v1/settings/nfo-output", wrapAuth(middleware.RequireAdmin(r.handleGetNFOOutput), authMw))
	mux.HandleFunc("PUT "+bp+"/api/v1/settings/nfo-output", wrapAuth(middleware.RequireAdmin(r.handleUpdateNFOOutput), authMw))
	// Updater routes (admin only for config and apply; check/status are read-only)
	mux.HandleFunc("GET "+bp+"/api/v1/updates/check", wrapAuth(middleware.RequireAdmin(r.handleGetUpdateCheck), authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/updates/status", wrapAuth(middleware.RequireAdmin(r.handleGetUpdateStatus), authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/updates/apply", wrapAuth(middleware.RequireAdmin(r.handlePostUpdateApply), authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/updates/config", wrapAuth(middleware.RequireAdmin(r.handleGetUpdateConfig), authMw))
	mux.HandleFunc("PUT "+bp+"/api/v1/updates/config", wrapAuth(middleware.RequireAdmin(r.handlePutUpdateConfig), authMw))
	// Shared-filesystem detection routes (admin only)
	mux.HandleFunc("GET "+bp+"/api/v1/shared-filesystem/status", wrapAuth(middleware.RequireAdmin(r.handleSharedFilesystemStatus), authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/shared-filesystem/dismiss", wrapAuth(middleware.RequireAdmin(r.handleSharedFilesystemDismiss), authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/shared-filesystem/recheck", wrapAuth(middleware.RequireAdmin(r.handleSharedFilesystemRecheck), authMw))
	// Filesystem browse route (admin only) -- used by the path picker modal
	mux.HandleFunc("GET "+bp+"/api/v1/filesystem/browse", wrapAuth(middleware.RequireAdmin(r.handleFilesystemBrowse), authMw))

	// Provider routes (key config requires admin; search/fetch are operator-accessible)
	mux.HandleFunc("GET "+bp+"/api/v1/providers", wrapAuth(middleware.RequireAdmin(r.handleListProviders), authMw))
	mux.HandleFunc("PUT "+bp+"/api/v1/providers/{name}/key", wrapAuth(middleware.RequireAdmin(r.handleSetProviderKey), authMw))
	mux.HandleFunc("DELETE "+bp+"/api/v1/providers/{name}/key", wrapAuth(middleware.RequireAdmin(r.handleDeleteProviderKey), authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/providers/{name}/test", wrapAuth(middleware.RequireAdmin(r.handleTestProvider), authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/providers/priorities", wrapAuth(middleware.RequireAdmin(r.handleGetPriorities), authMw))
	mux.HandleFunc("PUT "+bp+"/api/v1/providers/priorities", wrapAuth(middleware.RequireAdmin(r.handleSetPriorities), authMw))
	mux.HandleFunc("PUT "+bp+"/api/v1/providers/priorities/{field}/{provider}/toggle", wrapAuth(middleware.RequireAdmin(r.handleToggleFieldProvider), authMw))
	mux.HandleFunc("PUT "+bp+"/api/v1/providers/{name}/mirror", wrapAuth(middleware.RequireAdmin(r.handleSetMirror), authMw))
	mux.HandleFunc("DELETE "+bp+"/api/v1/providers/{name}/mirror", wrapAuth(middleware.RequireAdmin(r.handleDeleteMirror), authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/providers/search", wrapAuth(r.handleProviderSearch, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/providers/fetch", wrapAuth(r.handleProviderFetch, authMw))
	// Web search provider routes (toggle requires admin)
	mux.HandleFunc("GET "+bp+"/api/v1/providers/websearch", wrapAuth(middleware.RequireAdmin(r.handleGetWebSearchProviders), authMw))
	mux.HandleFunc("PUT "+bp+"/api/v1/providers/websearch/{name}/toggle", wrapAuth(middleware.RequireAdmin(r.handleSetWebSearchEnabled), authMw))

	// Scraper config routes (config mutations require admin; provider list is operator-accessible)
	mux.HandleFunc("GET "+bp+"/api/v1/scraper/config", wrapAuth(middleware.RequireAdmin(r.handleGetScraperConfig), authMw))
	mux.HandleFunc("PUT "+bp+"/api/v1/scraper/config", wrapAuth(middleware.RequireAdmin(r.handleUpdateScraperConfig), authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/scraper/config/connections/{id}", wrapAuth(middleware.RequireAdmin(r.handleGetConnectionScraperConfig), authMw))
	mux.HandleFunc("PUT "+bp+"/api/v1/scraper/config/connections/{id}", wrapAuth(middleware.RequireAdmin(r.handleUpdateConnectionScraperConfig), authMw))
	mux.HandleFunc("DELETE "+bp+"/api/v1/scraper/config/connections/{id}", wrapAuth(middleware.RequireAdmin(r.handleResetConnectionScraperConfig), authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/scraper/providers", wrapAuth(r.handleListScraperProviders, authMw))

	// Rule routes (config/enable requires admin; execution/evaluate/fix are operator-accessible)
	mux.HandleFunc("GET "+bp+"/api/v1/rules", wrapAuth(r.handleListRules, authMw))
	mux.HandleFunc("PUT "+bp+"/api/v1/rules/{id}", wrapAuth(middleware.RequireAdmin(r.handleUpdateRule), authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/rules/{id}/run", wrapAuth(r.handleRunRule, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/rules/run-all", wrapAuth(r.handleRunAllRules, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/rules/run-all/status", wrapAuth(r.handleRunAllRulesStatus, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/rules/status", wrapAuth(r.handleRulesStatus, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/rules/classical-mode", wrapAuth(r.handleGetClassicalMode, authMw))
	mux.HandleFunc("PUT "+bp+"/api/v1/rules/classical-mode", wrapAuth(middleware.RequireAdmin(r.handleSetClassicalMode), authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/artists/{id}/health", wrapAuth(r.handleEvaluateArtist, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/artists/{id}/run-rules", wrapAuth(r.handleRunArtistRules, authMw))

	// Notifications (rule violations) routes
	mux.HandleFunc("GET "+bp+"/api/v1/notifications/counts", wrapAuth(r.handleNotificationCounts, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/notifications/badge", wrapAuth(r.handleNotificationBadge, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/notifications/fix-all/status", wrapAuth(r.handleFixAllStatus, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/notifications/fix-all", wrapAuth(r.handleFixAll, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/notifications/export", wrapAuth(r.handleNotificationsExport, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/notifications", wrapAuth(r.handleListNotifications, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/notifications/bulk-dismiss", wrapAuth(r.handleBulkDismissViolations, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/notifications/{id}/dismiss", wrapAuth(r.handleDismissViolation, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/notifications/{id}/resolve", wrapAuth(r.handleResolveViolation, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/notifications/{id}/fix", wrapAuth(r.handleFixViolation, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/notifications/{id}/apply-candidate", wrapAuth(r.handleApplyViolationCandidate, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/fix-undo/{undoId}", wrapAuth(r.handleUndoFix, authMw))
	mux.HandleFunc("DELETE "+bp+"/api/v1/notifications/resolved", wrapAuth(r.handleClearResolvedViolations, authMw))

	// Bulk operation routes
	mux.HandleFunc("POST "+bp+"/api/v1/bulk/fetch-metadata", wrapAuth(r.handleBulkFetchMetadata, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/bulk/fetch-images", wrapAuth(r.handleBulkFetchImages, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/bulk/jobs", wrapAuth(r.handleBulkJobList, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/bulk/jobs/{id}", wrapAuth(r.handleBulkJobStatus, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/bulk/jobs/{id}/cancel", wrapAuth(r.handleBulkJobCancel, authMw))

	// Bulk identify routes
	mux.HandleFunc("POST "+bp+"/api/v1/artists/bulk-identify", wrapAuth(r.handleBulkIdentify, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/artists/bulk-identify", wrapAuth(r.handleBulkIdentifyProgress, authMw))
	mux.HandleFunc("DELETE "+bp+"/api/v1/artists/bulk-identify", wrapAuth(r.handleBulkIdentifyCancel, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/artists/bulk-identify/link", wrapAuth(r.handleBulkIdentifyLink, authMw))

	// Bulk actions over an explicit artist ID list (run rules, re-identify,
	// scan, fetch images). Singleton with 409 on concurrent start.
	mux.HandleFunc("POST "+bp+"/api/v1/artists/bulk-actions", wrapAuth(r.handleBulkAction, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/artists/bulk-actions/status", wrapAuth(r.handleBulkActionStatus, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/artists/bulk-actions/cancel", wrapAuth(r.handleBulkActionCancel, authMw))

	// Re-identify review wizard. Interactive per-artist stepper over the
	// same artist IDs that /bulk-actions accepts; see
	// handlers_reidentify_wizard.go for the session-store design.
	mux.HandleFunc("POST "+bp+"/api/v1/artists/re-identify/wizard", wrapAuth(r.handleReIdentifyWizardStart, authMw))
	mux.HandleFunc("GET "+bp+"/artists/re-identify/wizard/{sid}/step/{idx}", wrapAuth(r.handleReIdentifyWizardStep, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/artists/re-identify/wizard/{sid}/step/{idx}/accept", wrapAuth(r.handleReIdentifyWizardAccept, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/artists/re-identify/wizard/{sid}/step/{idx}/skip", wrapAuth(r.handleReIdentifyWizardSkip, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/artists/re-identify/wizard/{sid}/step/{idx}/decline", wrapAuth(r.handleReIdentifyWizardDecline, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/artists/re-identify/wizard/{sid}/save-exit", wrapAuth(r.handleReIdentifyWizardSaveExit, authMw))

	// SSE event stream
	mux.HandleFunc("GET "+bp+"/api/v1/events/stream", wrapAuth(r.handleSSEStream, authMw))

	// Violation trend
	mux.HandleFunc("GET "+bp+"/api/v1/violations/trend", wrapAuth(r.handleViolationTrend, authMw))

	// Report routes
	mux.HandleFunc("GET "+bp+"/api/v1/reports/health", wrapAuth(r.handleReportHealth, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/reports/health/history", wrapAuth(r.handleReportHealthHistory, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/reports/health/by-library", wrapAuth(r.handleReportHealthByLibrary, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/reports/compliance", wrapAuth(r.handleReportCompliance, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/reports/compliance/export", wrapAuth(r.handleReportComplianceExport, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/reports/metadata-completeness", wrapAuth(r.handleReportMetadataCompleteness, authMw))

	// History routes
	mux.HandleFunc("GET "+bp+"/api/v1/artists/{id}/history", wrapAuth(r.handleListArtistHistory, authMw))
	mux.HandleFunc("GET "+bp+"/artists/{id}/history/tab", wrapOptionalAuth(r.handleArtistHistoryTab, optAuthMw))
	mux.HandleFunc("GET "+bp+"/artists/{id}/violations/tab", wrapOptionalAuth(r.handleArtistViolationsTab, optAuthMw))
	mux.HandleFunc("GET "+bp+"/artists/{id}/discography/tab", wrapOptionalAuth(r.handleArtistDiscographyTab, optAuthMw))
	mux.HandleFunc("POST "+bp+"/api/v1/history/{id}/revert", wrapAuth(r.handleRevertHistory, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/history", wrapAuth(r.handleListGlobalHistory, authMw))

	// NFO diff routes (snapshot routes removed -- field-level undo via /history)
	mux.HandleFunc("GET "+bp+"/api/v1/artists/{id}/nfo/diff", wrapAuth(r.handleNFODiff, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/artists/{id}/nfo/conflict", wrapAuth(r.handleNFOConflictCheck, authMw))

	// Field-level edit routes
	mux.HandleFunc("GET "+bp+"/api/v1/artists/{id}/fields/{field}/display", wrapAuth(r.handleFieldDisplay, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/artists/{id}/fields/{field}/edit", wrapAuth(r.handleFieldEdit, authMw))
	mux.HandleFunc("PATCH "+bp+"/api/v1/artists/{id}/fields/{field}", wrapAuth(r.handleFieldUpdate, authMw))
	mux.HandleFunc("DELETE "+bp+"/api/v1/artists/{id}/fields/{field}", wrapAuth(r.handleFieldClear, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/artists/{id}/fields/{field}/providers", wrapAuth(r.handleFieldProviders, authMw))
	mux.HandleFunc("DELETE "+bp+"/api/v1/artists/{id}/members", wrapAuth(r.handleClearMembers, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/artists/{id}/members/from-provider", wrapAuth(r.handleSaveMembers, authMw))
	// Refresh and disambiguation routes
	mux.HandleFunc("POST "+bp+"/api/v1/artists/{id}/refresh", wrapAuth(r.handleArtistRefresh, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/artists/{id}/refresh/search", wrapAuth(r.handleRefreshSearch, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/artists/{id}/refresh/link", wrapAuth(r.handleRefreshLink, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/artists/{id}/reidentify", wrapAuth(r.handleReidentify, authMw))

	// MusicBrainz contribution routes
	mux.HandleFunc("GET "+bp+"/api/v1/artists/{id}/musicbrainz/diffs", wrapAuth(r.handleGetMBDiffs, authMw))

	// Image routes
	mux.HandleFunc("POST "+bp+"/api/v1/artists/{id}/images/upload", wrapAuth(r.handleImageUpload, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/artists/{id}/images/fetch", wrapAuth(r.handleImageFetch, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/artists/{id}/images/search", wrapAuth(r.handleImageSearch, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/artists/{id}/images/websearch", wrapAuth(r.handleWebImageSearch, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/artists/{id}/images/crop", wrapAuth(r.handleImageCrop, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/artists/{id}/images/logo/trim", wrapAuth(r.handleLogoTrim, authMw))
	// Multi-fanart routes
	mux.HandleFunc("GET "+bp+"/api/v1/artists/{id}/images/fanart/list", wrapAuth(r.handleFanartList, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/artists/{id}/images/fanart/{index}/file", wrapAuth(r.handleServeFanartByIndex, authMw))
	mux.HandleFunc("DELETE "+bp+"/api/v1/artists/{id}/images/fanart/batch", wrapAuth(r.handleFanartBatchDelete, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/artists/{id}/images/fanart/fetch-batch", wrapAuth(r.handleFanartBatchFetch, authMw))
	// Fanart slot management routes
	mux.HandleFunc("POST "+bp+"/api/v1/artists/{id}/images/fanart/reorder", wrapAuth(r.handleFanartReorder, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/artists/{id}/images/fanart/{slot}/assign", wrapAuth(r.handleFanartSlotAssign, authMw))
	mux.HandleFunc("DELETE "+bp+"/api/v1/artists/{id}/images/fanart/{slot}", wrapAuth(r.handleFanartSlotDelete, authMw))
	// Platform backdrop routes
	mux.HandleFunc("GET "+bp+"/api/v1/artists/{id}/platform-backdrops", wrapAuth(r.handlePlatformBackdrops, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/artists/{id}/platform-backdrops/{connectionId}/{index}/thumbnail", wrapAuth(r.handlePlatformBackdropThumbnail, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/artists/{id}/fanart-sync-state", wrapAuth(r.handleFanartSyncState, authMw))
	// Ambient backdrop (random artist fanart for layout background)
	mux.HandleFunc("GET "+bp+"/api/v1/images/random-backdrop", wrapAuth(r.handleRandomBackdrop, authMw))
	// Generic image routes
	mux.HandleFunc("GET "+bp+"/api/v1/artists/{id}/images/{type}/file", wrapAuth(r.handleServeImage, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/artists/{id}/images/{type}/info", wrapAuth(r.handleImageInfo, authMw))
	mux.HandleFunc("DELETE "+bp+"/api/v1/artists/{id}/images/{type}", wrapAuth(r.handleDeleteImage, authMw))

	// Platform ID routes
	mux.HandleFunc("GET "+bp+"/api/v1/artists/{id}/platform-ids", wrapAuth(r.handleGetPlatformIDs, authMw))
	mux.HandleFunc("PUT "+bp+"/api/v1/artists/{id}/platform-ids/{connectionId}", wrapAuth(r.handleSetPlatformID, authMw))
	mux.HandleFunc("DELETE "+bp+"/api/v1/artists/{id}/platform-ids/{connectionId}", wrapAuth(r.handleDeletePlatformID, authMw))

	// Push routes
	mux.HandleFunc("POST "+bp+"/api/v1/artists/{id}/push", wrapAuth(r.handlePushMetadata, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/artists/{id}/push/images", wrapAuth(r.handlePushImages, authMw))
	mux.HandleFunc("DELETE "+bp+"/api/v1/artists/{id}/push/images/{type}", wrapAuth(r.handleDeletePushImage, authMw))

	// Platform state routes
	mux.HandleFunc("GET "+bp+"/api/v1/artists/{id}/platform-state", wrapAuth(r.handleGetPlatformState, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/artists/{id}/pull", wrapAuth(r.handlePullMetadata, authMw))

	// Web routes (optional auth populates user context for login redirect)
	mux.HandleFunc("GET "+bp+"/artists/{id}/images", wrapOptionalAuth(r.handleArtistImagesPage, optAuthMw))
	mux.HandleFunc("GET "+bp+"/artists/{id}", wrapOptionalAuth(r.handleArtistDetailPage, optAuthMw))
	mux.HandleFunc("GET "+bp+"/artists", wrapOptionalAuth(r.handleArtistsPage, optAuthMw))
	mux.HandleFunc("GET "+bp+"/reports/compliance", wrapOptionalAuth(r.handleCompliancePage, optAuthMw))
	mux.HandleFunc("GET "+bp+"/reports", wrapOptionalAuth(func(w http.ResponseWriter, req *http.Request) {
		target := r.basePath + "/reports/compliance"
		if req.URL.RawQuery != "" {
			target = target + "?" + req.URL.RawQuery
		}
		http.Redirect(w, req, target, http.StatusMovedPermanently)
	}, optAuthMw))
	mux.HandleFunc("GET "+bp+"/artists/{id}/nfo", wrapOptionalAuth(r.handleNFODiffPage, optAuthMw))
	mux.HandleFunc("GET "+bp+"/activity", wrapOptionalAuth(r.handleActivityPage, optAuthMw))
	mux.HandleFunc("GET "+bp+"/activity/content", wrapOptionalAuth(r.handleActivityContent, optAuthMw))
	mux.HandleFunc("GET "+bp+"/dashboard/actions", wrapOptionalAuth(r.handleDashboardActionQueue, optAuthMw))
	mux.HandleFunc("GET "+bp+"/dashboard/activity", wrapOptionalAuth(r.handleDashboardActivityFeed, optAuthMw))
	mux.HandleFunc("GET "+bp+"/settings", wrapOptionalAuth(r.handleSettingsPage, optAuthMw))
	mux.HandleFunc("GET "+bp+"/settings/{section}", wrapOptionalAuth(func(w http.ResponseWriter, req *http.Request) {
		q := req.URL.Query()
		q.Set("tab", req.PathValue("section"))
		target := r.basePath + "/settings"
		if encoded := q.Encode(); encoded != "" {
			target += "?" + encoded
		}
		http.Redirect(w, req, target, http.StatusMovedPermanently)
	}, optAuthMw))
	mux.HandleFunc("GET "+bp+"/preferences", wrapOptionalAuth(r.handleUserPreferencesPage, optAuthMw))
	mux.HandleFunc("GET "+bp+"/guide", wrapOptionalAuth(r.handleGuidePage, optAuthMw))
	mux.HandleFunc("GET "+bp+"/setup/wizard", wrapOptionalAuth(r.handleOnboardingPage, optAuthMw))
	mux.HandleFunc("GET "+bp+"/notifications", wrapAuth(func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, r.basePath+"/", http.StatusMovedPermanently)
	}, authMw))

	// Start the pprof listener on a dedicated localhost port when SW_PPROF=1 or
	// SW_PPROF=true. Using a separate listener ensures pprof is never reachable
	// via the public-facing port even if accidentally enabled in a container.
	if pprofEnabled() {
		pprofOnce.Do(func() { registerPprof(ctx, r.logger) })
	} else if v := os.Getenv("SW_PPROF"); v != "" {
		r.logger.Warn("SW_PPROF is set but not recognized; use '1' or 'true' to enable", "value", v)
	}

	// Catch-all: unmatched routes render the custom 404 page. Registered last
	// so all explicit routes above take precedence. Uses optional auth so the
	// sidebar can show the logged-in state when the user is authenticated.
	mux.HandleFunc(bp+"/{path...}", wrapOptionalAuth(r.handle404, optAuthMw))

	// Apply middleware chain: security headers > i18n > logging > CSRF
	// Login and setup are exempt from CSRF (registered with rate limiter above)
	csrfExempt := []string{
		bp + "/api/v1/auth/login",
		bp + "/api/v1/auth/setup",
	}
	var handler http.Handler = mux
	handler = csrfWithExemptions(csrf, handler, csrfExempt)
	handler = middleware.Logging(r.logger, bp)(handler)
	if r.i18nBundle != nil {
		handler = i18n.Middleware(r.i18nBundle)(handler)
	}
	handler = middleware.SecurityHeaders(handler)
	return handler
}

// csrfWithExemptions wraps CSRF middleware but skips validation for exempt paths
// and for requests authenticated with API tokens (sw_ prefix).
func csrfWithExemptions(csrf *middleware.CSRF, next http.Handler, exemptPaths []string) http.Handler {
	csrfHandler := csrf.Middleware(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip CSRF for API token requests
		if isAPITokenRequest(r) {
			next.ServeHTTP(w, r)
			return
		}
		for _, path := range exemptPaths {
			if r.URL.Path == path {
				next.ServeHTTP(w, r)
				return
			}
		}
		csrfHandler.ServeHTTP(w, r)
	})
}

// isAPITokenRequest returns true if the request carries a sw_ API token
// and is not also using cookie-based authentication. This ensures CSRF
// is only bypassed for true token-based API requests, not for browser
// requests that happen to include a spoofed apikey parameter.
func isAPITokenRequest(r *http.Request) bool {
	header := r.Header.Get("Authorization")
	hasToken := strings.HasPrefix(header, "Bearer "+auth.APITokenPrefix)
	if !hasToken {
		hasToken = strings.HasPrefix(r.URL.Query().Get("apikey"), auth.APITokenPrefix)
	}
	if !hasToken {
		return false
	}
	// If a session cookie is present, treat as a browser request and
	// do not bypass CSRF based on a potentially unvalidated token param.
	if _, err := r.Cookie("session"); err == nil {
		return false
	}
	return true
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

// enforceCacheLimitIfNeeded checks whether the artist uses the image cache
// (pathless) and if so, enforces the configured cache size limit. Errors are
// logged but do not fail the calling operation.
func (r *Router) enforceCacheLimitIfNeeded(ctx context.Context, a *artist.Artist) {
	if a.Path != "" || r.imageCacheDir == "" {
		return
	}
	maxMB := r.getIntSetting(ctx, "cache.image.max_size_mb", 0)
	if maxMB <= 0 {
		return
	}
	if err := img.EnforceCacheLimit(r.imageCacheDir, int64(maxMB)*1024*1024, r.logger); err != nil {
		r.logger.Warn("image cache eviction failed", "error", err)
	}
}
