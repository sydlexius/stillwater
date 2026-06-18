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
	"github.com/sydlexius/stillwater/internal/conflict"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/encryption"
	"github.com/sydlexius/stillwater/internal/event"
	"github.com/sydlexius/stillwater/internal/foreign"
	"github.com/sydlexius/stillwater/internal/httpsafe"
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
	// UX is the SW_UX UI-channel mode: "stable", "next", or "dual". Drives the
	// UX middleware (X-Stillwater-UX header + ux= log field) and the /next/*
	// lane. Empty is treated as "stable".
	UX        string
	TLSStatus templates.TLSStatusData
	// HTTP3Port is the UDP port advertised in the Alt-Svc response header.
	// Zero disables the header (HTTP/3 not enabled). Set to the effective
	// HTTP/3 listener port when SW_HTTP3_ENABLED is true.
	HTTP3Port     int
	StaticFS      fs.FS
	ImageCacheDir string
	Publisher     *publish.Publisher
	SSEHub        *SSEHub
	I18nBundle    *i18n.Bundle
	// Encryptor is used to decrypt inbound webhook HMAC secrets stored
	// encrypted-at-rest in the settings table. Nil disables HMAC verification
	// (secrets are never read and all requests pass through unchecked).
	Encryptor *encryption.Encryptor
	// SessionSecret is used to sign CSRF tokens. It must be at least 32 bytes;
	// the CSRF middleware panics at startup if it is empty or too short.
	SessionSecret string
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
	ux                 string
	tlsStatus          templates.TLSStatusData
	http3Port          int
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
	// discographyFetchInFlight holds the artist IDs that currently have a
	// discography fetch running. A concurrent fetch for the same artist is
	// rejected with 409: the fetch is a read-modify-write of artist.nfo, so
	// two interleaved cycles could each merge against the pre-fetch album
	// list and the later write would silently drop the earlier fetch's
	// additions. Guarded by discographyFetchMu.
	discographyFetchInFlight map[string]bool
	discographyFetchMu       sync.Mutex
	// reIdentifyWizardStore backs the interactive re-identify review flow.
	// Sessions are in-memory, TTL-bounded, and never persisted across
	// restarts; the flow is an interactive user task so lossy restart
	// semantics are acceptable and avoid a schema change for this RC fix.
	reIdentifyWizardStore *reIdentifyWizardStore
	undoStore             *rule.UndoStore
	sseHub                *SSEHub
	i18nBundle            *i18n.Bundle
	conflictDetector      *conflict.Detector
	conflictGate          *conflict.Gate
	// stillwaterManagedMu serializes read-modify-write inside
	// handleSetStillwaterManaged on a per-connection basis. Without it,
	// two concurrent enable=true requests for the same connection could
	// both observe FeatureManageServerFiles=false on the snapshot loaded
	// at the top of the handler, both fall through the idempotency guard,
	// and both run applyStillwaterManaged -- the second snapshot of an
	// already-managed peer would clobber pre_stillwater_config_json with
	// the already-cleared library options, making restore unable to
	// recover the real pre-Stillwater settings (issue #1190).
	//
	// Map values are *sync.Mutex pulled via LoadOrStore; entries
	// accumulate for the lifetime of the process. Cardinality is bounded
	// by the number of connections (small for realistic deployments) so
	// we accept the leak rather than racing removal against late-arriving
	// requests.
	stillwaterManagedMu sync.Map
	// foreignRepo persists foreign-file ledger rows and the allowlist
	// (#1185). Always non-nil after NewRouter when DB is provided so the
	// foreign-files settings page never has to special-case a missing dep.
	foreignRepo *foreign.Repository

	// setupRestoreMu serializes handleSetupRestore. The handler runs a
	// read-modify-write across the users table and the onboarding
	// settings row: HasUsers probe, onboarding flag probe, envelope
	// Import, then onboarding flag flip. Without serialization two
	// simultaneous unauthenticated POSTs to /api/v1/setup/restore would
	// each see HasUsers=false at the top, both fall through the gate,
	// and the second call would either insert duplicate user rows or
	// fail mid-import depending on which transaction ordering the engine
	// happens to pick. The handler is rate-limited by loginRL but rate
	// limits are coarse; a coincident pair within the same window is
	// possible and the mutex closes that TOCTOU window. Held only for
	// the duration of one request, so contention is bounded by the
	// rate-limit cap.
	setupRestoreMu sync.Mutex

	// webhookWg tracks in-flight inbound webhook processing goroutines so
	// DrainWebhooks can wait for them to finish before the DB is closed.
	webhookWg sync.WaitGroup
	// webhookShutdownCtx is canceled by DrainWebhooks to signal in-flight
	// webhook goroutines that a shutdown is in progress. Goroutines derive
	// their processing context from this so they stop work promptly when
	// the application is going down.
	webhookShutdownCtx    context.Context
	webhookShutdownCancel context.CancelFunc
	// encryptor decrypts inbound webhook HMAC secrets stored encrypted-at-rest
	// in the settings table. Nil means HMAC verification is disabled.
	encryptor *encryption.Encryptor
	// sessionSecret is the HMAC key used to sign and verify CSRF tokens.
	sessionSecret string
}

// NewRouter creates a new Router with all routes configured.
func NewRouter(deps RouterDeps) *Router {
	webhookCtx, webhookCancel := context.WithCancel(context.Background())
	r := &Router{
		authService:              deps.AuthService,
		authRegistry:             deps.AuthRegistry,
		artistService:            deps.ArtistService,
		historyService:           deps.HistoryService,
		scannerService:           deps.ScannerService,
		platformService:          deps.PlatformService,
		providerSettings:         deps.ProviderSettings,
		providerRegistry:         deps.ProviderRegistry,
		webSearchRegistry:        deps.WebSearchRegistry,
		rateLimiters:             deps.RateLimiters,
		orchestrator:             deps.Orchestrator,
		ruleService:              deps.RuleService,
		ruleEngine:               deps.RuleEngine,
		pipeline:                 deps.Pipeline,
		bulkService:              deps.BulkService,
		bulkExecutor:             deps.BulkExecutor,
		ruleScheduler:            deps.RuleScheduler,
		nfoSnapshotService:       deps.NFOSnapshotService,
		nfoSettingsService:       deps.NFOSettingsService,
		connectionService:        deps.ConnectionService,
		scraperService:           deps.ScraperService,
		libraryService:           deps.LibraryService,
		webhookService:           deps.WebhookService,
		webhookDispatcher:        deps.WebhookDispatcher,
		backupService:            deps.BackupService,
		logManager:               deps.LogManager,
		maintenanceService:       deps.MaintenanceService,
		settingsIOService:        deps.SettingsIOService,
		updaterService:           deps.UpdaterService,
		probeCache:               deps.ProbeCache,
		expectedWrites:           deps.ExpectedWrites,
		eventBus:                 deps.EventBus,
		db:                       deps.DB,
		logger:                   deps.Logger,
		basePath:                 deps.BasePath,
		basePathFromEnv:          deps.BasePathFromEnv,
		ux:                       deps.UX,
		tlsStatus:                deps.TLSStatus,
		http3Port:                deps.HTTP3Port,
		imageCacheDir:            deps.ImageCacheDir,
		publisher:                deps.Publisher,
		sseHub:                   deps.SSEHub,
		staticAssets:             NewStaticAssets(deps.StaticFS, deps.Logger),
		ssrfClient:               httpsafe.SafeClient(fetchTimeout),
		fileRemover:              osRemover{},
		libraryOps:               make(map[string]*LibraryOpResult),
		discographyFetchInFlight: make(map[string]bool),
		undoStore:                rule.NewUndoStore(),
		i18nBundle:               deps.I18nBundle,
		reIdentifyWizardStore:    newReIdentifyWizardStore(),
		webhookShutdownCtx:       webhookCtx,
		webhookShutdownCancel:    webhookCancel,
		encryptor:                deps.Encryptor,
		sessionSecret:            deps.SessionSecret,
	}

	// Auto-init the SSE hub if not provided by the caller, so the /events/stream
	// endpoint is always functional even when main.go does not wire one.
	if r.sseHub == nil {
		r.sseHub = NewSSEHub(deps.Logger)
	}
	if deps.EventBus != nil {
		r.sseHub.SubscribeToEventBus(deps.EventBus)
	}

	// Conflict detector and gate are owned by the router so every write
	// handler shares the same cached ledger. We construct them here (not via
	// RouterDeps) so main.go doesn't have to know about the dependency:
	// the only inputs are the connection service and event bus, which are
	// already in scope.
	if deps.ConnectionService != nil {
		r.conflictDetector = conflict.NewDetector(deps.ConnectionService, deps.EventBus, deps.Logger)
		r.conflictGate = conflict.NewGate(r.conflictDetector)
		// Teach the rule pipeline to refuse auto-mode image/NFO fixes while
		// a conflict is active. Without this the pipeline would write
		// artwork/NFO files that the peer immediately duplicates under its
		// own filenames, defeating the whole detection effort.
		if deps.Pipeline != nil {
			if setter, ok := deps.Pipeline.(interface {
				SetWriteGate(g rule.WriteGate)
			}); ok {
				setter.SetWriteGate(r.conflictGate)
			}
		}
	}

	// Foreign-file ledger repository (#1185). The scanner that fills it is
	// owned by main.go (not the router) so the goroutine is started once on
	// boot and shut down with the rest of the app; the router only needs
	// read/write access for the settings handlers and the banner count.
	if deps.DB != nil {
		r.foreignRepo = foreign.NewRepository(deps.DB)
	}

	// Configure the static asset base path used by template helpers (logoSrc, etc.)
	// so that sub-path deployments produce correct URLs.
	templates.SetBasePath(deps.BasePath)

	return r
}

// DrainWebhooks signals in-flight inbound webhook goroutines to stop and waits
// for them to finish. Call this after the HTTP server has drained (no new
// inbound webhook requests can arrive) but before the database is closed, so
// goroutines that are still processing can write their results.
//
// The provided ctx bounds the wait: if it is canceled before all goroutines
// finish, DrainWebhooks returns immediately with ctx.Err() so the shutdown
// sequence can continue. In practice, main.go passes the already-canceled
// shutdown context; the 5-minute per-goroutine processing timeout is the
// effective bound.
func (r *Router) DrainWebhooks(ctx context.Context) error {
	// Cancel the webhook-scoped context so goroutines that are still running
	// know a shutdown is in progress and can stop early if possible.
	r.webhookShutdownCancel()

	done := make(chan struct{})
	go func() {
		r.webhookWg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Handler returns the fully configured HTTP handler with middleware applied.
// The provided context controls the lifecycle of background goroutines (e.g. rate limiter cleanup).
func (r *Router) Handler(ctx context.Context) http.Handler {
	authMw := middleware.Auth(r.authService)
	optAuthMw := middleware.OptionalAuth(r.authService)
	csrf := middleware.NewCSRF(r.sessionSecret)
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
	// Pre-admin restore-from-backup (#1114). Same rate-limiter as login/setup
	// so brute-forcing the passphrase is throttled identically. The handler
	// internally checks HasUsers AND onboarding.completed so the route is
	// only reachable on a truly fresh install -- before any admin exists.
	// CSRF-exempt for the same reason as /auth/setup: no session to attach
	// a token to.
	mux.Handle("POST "+bp+"/api/v1/setup/restore", loginRL.Middleware(http.HandlerFunc(r.handleSetupRestore)))
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
	mux.HandleFunc("PATCH "+bp+"/api/v1/preferences", wrapAuth(r.handlePatchPreferences, authMw))
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
	// Permanent delete -- distinct sub-resource (4 segments) so it cannot
	// be confused with the deactivation route above NOR collide with the
	// existing 3-segment `/users/invites/{id}` (mirrors the
	// `/auth/tokens/{id}/permanent` precedent for revoke-vs-delete). See
	// issue #1170.
	mux.HandleFunc("DELETE "+bp+"/api/v1/users/{id}/account/permanent", wrapAuth(requireMultiUser(middleware.RequireAdmin(r.handleDeleteUser)), authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/artists", wrapAuth(r.handleListArtists, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/artists/badge", wrapAuth(r.handleArtistsBadge, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/artists/locked", wrapAuth(r.handleListLockedArtists, authMw))
	// Grouped with the other literal /artists/* routes.
	mux.HandleFunc("GET "+bp+"/api/v1/artists/matching-ids", wrapAuth(r.handleArtistMatchingIDs, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/artists/{id}", wrapAuth(r.handleGetArtist, authMw))
	// Near-duplicate detection report. Canonical path is
	// /api/v1/reports/duplicates so it groups with the other report
	// endpoints (health, compliance, metadata-completeness). The old
	// /api/v1/artists/duplicates alias is kept for one release per the
	// deprecation note in #1615; remove in M53.
	mux.HandleFunc("GET "+bp+"/api/v1/reports/duplicates", wrapAuth(r.handleDuplicates, authMw))
	// Sidebar count badge for the Duplicates child link (#1665). Returns an
	// HTML fragment so the sidebar's hx-swap="innerHTML" placeholder can
	// drop the child entirely when no duplicates remain. Admin-only via
	// the in-handler role check.
	mux.HandleFunc("GET "+bp+"/api/v1/reports/duplicates/count", wrapAuth(r.handleArtistDuplicatesCount, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/artists/duplicates", wrapAuth(r.handleDuplicates, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/artists/merge", wrapAuth(r.handleArtistsMerge, authMw))
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
	mux.HandleFunc("PATCH "+bp+"/api/v1/libraries/{id}", wrapAuth(middleware.RequireAdmin(r.handlePatchLibrary), authMw))
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
	mux.HandleFunc("POST "+bp+"/api/v1/connections/{id}/stillwater-managed", wrapAuth(middleware.RequireAdmin(r.handleSetStillwaterManaged), authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/connections/{id}/conflict-detail", wrapAuth(r.handleGetConnectionConflictDetail, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/conflicts", wrapAuth(r.handleGetConflicts, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/config/conflict-banner", wrapAuth(r.handleGetConflictBanner, authMw))
	// Foreign-file routes (#1185). Read access is admin-only because the
	// list page exposes filesystem paths; writes are admin-only because they
	// mutate disk state (Delete) or change scanner behavior (allowlist).
	// Sidebar count badge for the Foreign Files child link (#1778). Returns an
	// HTML fragment so the sidebar's hx-swap="innerHTML" placeholder can drop
	// the child entirely when no foreign files remain. Admin-only via the
	// in-handler role check. ?ch=next emits the /next/ href + glyph.
	mux.HandleFunc("GET "+bp+"/api/v1/foreign-files/count", wrapAuth(r.handleForeignFilesCount, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/foreign-files", wrapAuth(middleware.RequireAdmin(r.handleForeignFilesList), authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/foreign-files/dismiss", wrapAuth(middleware.RequireAdmin(r.handleForeignFilesDismiss), authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/foreign-files/{id}/allowlist", wrapAuth(middleware.RequireAdmin(r.handleForeignFileAllowlist), authMw))
	mux.HandleFunc("DELETE "+bp+"/api/v1/foreign-files/{id}/file", wrapAuth(middleware.RequireAdmin(r.handleForeignFileDelete), authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/foreign-file-allowlist", wrapAuth(middleware.RequireAdmin(r.handleForeignAllowlistList), authMw))
	mux.HandleFunc("DELETE "+bp+"/api/v1/foreign-file-allowlist/{id}", wrapAuth(middleware.RequireAdmin(r.handleForeignAllowlistRemove), authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/onboarding/conflict-step", wrapAuth(r.handlePostOnboardingConflictStep, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/onboarding/reset", wrapAuth(middleware.RequireAdmin(r.handlePostOnboardingReset), authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/connections/{id}/platform-summary", wrapAuth(r.handleGetPlatformSummary, authMw))
	// Connection library discovery/import routes
	mux.HandleFunc("GET "+bp+"/api/v1/connections/{id}/libraries", wrapAuth(r.handleDiscoverLibraries, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/connections/{id}/libraries/import", wrapAuth(middleware.RequireAdmin(r.handleImportLibraries), authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/connections/{id}/libraries/{libId}/populate", wrapAuth(middleware.RequireAdmin(r.handlePopulateLibrary), authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/connections/{id}/libraries/{libId}/scan", wrapAuth(middleware.RequireAdmin(r.handleScanLibrary), authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/libraries/{libId}/operation/status", wrapAuth(r.handleLibraryOpStatus, authMw))
	// Aggregate populate state for the ProgressPill reconnect-rehydrate
	// path (#1641). Returns every in-flight populate as a ProgressPill
	// envelope so the JS client can replay them through swProgressPill.push
	// without needing per-library status fan-out.
	mux.HandleFunc("GET "+bp+"/api/v1/connections/populate/in-flight", wrapAuth(r.handlePopulateInFlight, authMw))
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
	// Vocab (metadata tag exclude list + count caps) settings routes (admin only)
	mux.HandleFunc("GET "+bp+"/api/v1/settings/vocab", wrapAuth(middleware.RequireAdmin(r.handleGetVocab), authMw))
	mux.HandleFunc("PUT "+bp+"/api/v1/settings/vocab", wrapAuth(middleware.RequireAdmin(r.handlePutVocab), authMw))
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
	// Updater routes (all admin only; POST /updates/check is mutating -- it
	// refreshes the cached last_checked timestamp and latest-release metadata).
	mux.HandleFunc("POST "+bp+"/api/v1/updates/check", wrapAuth(middleware.RequireAdmin(r.handlePostUpdateCheck), authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/updates/status", wrapAuth(middleware.RequireAdmin(r.handleGetUpdateStatus), authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/updates/apply", wrapAuth(middleware.RequireAdmin(r.handlePostUpdateApply), authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/updates/config", wrapAuth(middleware.RequireAdmin(r.handleGetUpdateConfig), authMw))
	mux.HandleFunc("PUT "+bp+"/api/v1/updates/config", wrapAuth(middleware.RequireAdmin(r.handlePutUpdateConfig), authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/updates/skips", wrapAuth(middleware.RequireAdmin(r.handleGetUpdateSkips), authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/updates/skips", wrapAuth(middleware.RequireAdmin(r.handlePostUpdateSkips), authMw))
	mux.HandleFunc("DELETE "+bp+"/api/v1/updates/skips/{version}", wrapAuth(middleware.RequireAdmin(r.handleDeleteUpdateSkip), authMw))
	// Shared-filesystem detection routes (admin only)
	mux.HandleFunc("GET "+bp+"/api/v1/shared-filesystem/status", wrapAuth(middleware.RequireAdmin(r.handleSharedFilesystemStatus), authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/shared-filesystem/dismiss", wrapAuth(middleware.RequireAdmin(r.handleSharedFilesystemDismiss), authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/shared-filesystem/recheck", wrapAuth(middleware.RequireAdmin(r.handleSharedFilesystemRecheck), authMw))
	// Filesystem browse route (admin only) -- used by the path picker modal
	mux.HandleFunc("GET "+bp+"/api/v1/filesystem/browse", wrapAuth(middleware.RequireAdmin(r.handleFilesystemBrowse), authMw))

	// Provider routes (key config requires admin; search/fetch are operator-accessible)
	mux.HandleFunc("GET "+bp+"/api/v1/providers", wrapAuth(middleware.RequireAdmin(r.handleListProviders), authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/providers/{name}/config", wrapAuth(middleware.RequireAdmin(r.handleGetProviderConfig), authMw))
	mux.HandleFunc("PUT "+bp+"/api/v1/providers/{name}/config", wrapAuth(middleware.RequireAdmin(r.handleSetProviderConfig), authMw))
	mux.HandleFunc("PUT "+bp+"/api/v1/providers/{name}/key", wrapAuth(middleware.RequireAdmin(r.handleSetProviderKey), authMw))
	mux.HandleFunc("DELETE "+bp+"/api/v1/providers/{name}/key", wrapAuth(middleware.RequireAdmin(r.handleDeleteProviderKey), authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/providers/{name}/test", wrapAuth(middleware.RequireAdmin(r.handleTestProvider), authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/providers/priorities", wrapAuth(middleware.RequireAdmin(r.handleGetPriorities), authMw))
	mux.HandleFunc("PUT "+bp+"/api/v1/providers/priorities", wrapAuth(middleware.RequireAdmin(r.handleSetPriorities), authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/providers/priorities/reset", wrapAuth(middleware.RequireAdmin(r.handleResetPriorities), authMw))
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
	mux.HandleFunc("GET "+bp+"/api/v1/rules/{id}/results", wrapAuth(r.handleRuleResults, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/rules/{id}/run", wrapAuth(r.handleRunRule, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/rules/run-all", wrapAuth(r.handleRunAllRules, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/rules/run-all/status", wrapAuth(r.handleRunAllRulesStatus, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/rules/status", wrapAuth(r.handleRulesStatus, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/artists/{id}/health", wrapAuth(r.handleEvaluateArtist, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/artists/{id}/rule-results", wrapAuth(r.handleArtistRuleResults, authMw))
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
	mux.HandleFunc("GET "+bp+"/artists/re-identify/wizard/{sid}/step/{idx}", wrapOptionalAuth(r.handleReIdentifyWizardStep, optAuthMw))
	mux.HandleFunc("POST "+bp+"/api/v1/artists/re-identify/wizard/{sid}/step/{idx}/accept", wrapAuth(r.handleReIdentifyWizardAccept, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/artists/re-identify/wizard/{sid}/step/{idx}/skip", wrapAuth(r.handleReIdentifyWizardSkip, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/artists/re-identify/wizard/{sid}/step/{idx}/decline", wrapAuth(r.handleReIdentifyWizardDecline, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/artists/re-identify/wizard/{sid}/step/{idx}/retry", wrapAuth(r.handleReIdentifyWizardRetry, authMw))
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
	mux.HandleFunc("GET "+bp+"/api/v1/reports/compliance/count", wrapAuth(r.handleComplianceCount, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/reports/compliance/export", wrapAuth(r.handleReportComplianceExport, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/reports/metadata-completeness", wrapAuth(r.handleReportMetadataCompleteness, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/reports/rule-pass-rates", wrapAuth(r.handleReportRulePassRates, authMw))

	// History routes
	mux.HandleFunc("GET "+bp+"/api/v1/artists/{id}/history", wrapAuth(r.handleListArtistHistory, authMw))
	mux.HandleFunc("GET "+bp+"/artists/{id}/history/tab", wrapOptionalAuth(r.handleArtistHistoryTab, optAuthMw))
	mux.HandleFunc("GET "+bp+"/artists/{id}/violations/tab", wrapOptionalAuth(r.handleArtistViolationsTab, optAuthMw))
	mux.HandleFunc("GET "+bp+"/artists/{id}/discography/tab", wrapOptionalAuth(r.handleArtistDiscographyTab, optAuthMw))
	mux.HandleFunc("POST "+bp+"/api/v1/artists/{id}/discography/fetch", wrapAuth(middleware.RequireAdmin(r.handleFetchDiscography), authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/history/{id}/revert", wrapAuth(r.handleRevertHistory, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/history", wrapAuth(r.handleListGlobalHistory, authMw))

	// NFO conflict check route (field-level undo via /history; diff view retired)
	mux.HandleFunc("GET "+bp+"/api/v1/artists/{id}/nfo/conflict", wrapAuth(r.handleNFOConflictCheck, authMw))

	// Field-level edit routes
	mux.HandleFunc("GET "+bp+"/api/v1/artists/{id}/fields/{field}/display", wrapAuth(r.handleFieldDisplay, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/artists/{id}/fields/{field}/edit", wrapAuth(r.handleFieldEdit, authMw))
	// Batch edit: returns all field edit fragments in one OOB response,
	// eliminating the N+1 history queries from the next/ "Edit All" flow.
	mux.HandleFunc("GET "+bp+"/api/v1/artists/{id}/fields/edit-all", wrapAuth(r.handleFieldsEditAll, authMw))
	mux.HandleFunc("PATCH "+bp+"/api/v1/artists/{id}/fields/{field}", wrapAuth(r.handleFieldUpdate, authMw))
	mux.HandleFunc("DELETE "+bp+"/api/v1/artists/{id}/fields/{field}", wrapAuth(r.handleFieldClear, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/artists/{id}/fields/{field}/history/fragment", wrapAuth(r.handleFieldHistoryFragment, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/artists/{id}/fields/{field}/providers", wrapAuth(r.handleFieldProviders, authMw))
	mux.HandleFunc("DELETE "+bp+"/api/v1/artists/{id}/members", wrapAuth(r.handleClearMembers, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/artists/{id}/members/from-provider", wrapAuth(r.handleSaveMembers, authMw))
	// Refresh and disambiguation routes
	mux.HandleFunc("POST "+bp+"/api/v1/artists/{id}/rename-directory", wrapAuth(r.handleArtistRenameDirectory, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/artists/{id}/refresh", wrapAuth(r.handleArtistRefresh, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/artists/{id}/refresh/search", wrapAuth(r.handleRefreshSearch, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/artists/{id}/refresh/link", wrapAuth(r.handleRefreshLink, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/artists/{id}/reidentify", wrapAuth(r.handleReidentify, authMw))
	// Deezer match-by-name (mirrors the MusicBrainz identify flow, keyed on the
	// Deezer provider ID). search returns scored candidates; link persists the
	// chosen Deezer ID and refreshes.
	mux.HandleFunc("GET "+bp+"/api/v1/artists/{id}/deezer/identify", wrapAuth(r.handleDeezerIdentify, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/artists/{id}/deezer/search", wrapAuth(r.handleDeezerSearch, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/artists/{id}/deezer/link", wrapAuth(r.handleDeezerLink, authMw))

	// Discogs match-by-name identify (next/ artist-detail per-row affordance;
	// mirrors the Deezer identify flow, scoped to Discogs).
	mux.HandleFunc("GET "+bp+"/api/v1/artists/{id}/discogs/identify", wrapAuth(r.handleDiscogsIdentify, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/artists/{id}/discogs/search", wrapAuth(r.handleDiscogsSearch, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/artists/{id}/discogs/link", wrapAuth(r.handleDiscogsLink, authMw))

	// MusicBrainz contribution routes
	mux.HandleFunc("GET "+bp+"/api/v1/artists/{id}/musicbrainz/diffs", wrapAuth(r.handleGetMBDiffs, authMw))

	// Image routes
	mux.HandleFunc("POST "+bp+"/api/v1/artists/{id}/images/upload", wrapAuth(r.handleImageUpload, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/artists/{id}/images/fetch", wrapAuth(r.handleImageFetch, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/artists/{id}/images/search", wrapAuth(r.handleImageSearch, authMw))
	mux.HandleFunc("GET "+bp+"/api/v1/artists/{id}/images/websearch", wrapAuth(r.handleWebImageSearch, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/artists/{id}/images/crop", wrapAuth(r.handleImageCrop, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/artists/{id}/images/logo/trim", wrapAuth(r.handleLogoTrim, authMw))
	mux.HandleFunc("POST "+bp+"/api/v1/artists/{id}/images/{type}/revert", wrapAuth(r.handleImageRevert, authMw))
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
		// gosec G710: target's path is server-controlled (r.basePath +
		// /reports/compliance); only the query string is carried through from
		// the request, which cannot redirect off-origin.
		http.Redirect(w, req, target, http.StatusMovedPermanently) //nolint:gosec // G710: path is server-built; only the query string flows from req.
	}, optAuthMw))
	mux.HandleFunc("GET "+bp+"/activity", wrapOptionalAuth(r.handleActivityPage, optAuthMw))
	mux.HandleFunc("GET "+bp+"/activity/content", wrapOptionalAuth(r.handleActivityContent, optAuthMw))
	mux.HandleFunc("GET "+bp+"/dashboard/actions", wrapOptionalAuth(r.handleDashboardActionQueue, optAuthMw))
	mux.HandleFunc("GET "+bp+"/dashboard/activity", wrapOptionalAuth(r.handleDashboardActivityFeed, optAuthMw))
	mux.HandleFunc("GET "+bp+"/settings", wrapOptionalAuth(r.handleSettingsPage, optAuthMw))
	// Foreign-file management pages (#1185). Registered before the catch-all
	// /settings/{section} redirect so the more-specific routes win.
	mux.HandleFunc("GET "+bp+"/settings/foreign-files", wrapOptionalAuth(r.handleForeignFilesPage, optAuthMw))
	mux.HandleFunc("GET "+bp+"/settings/foreign-files/allowlist", wrapOptionalAuth(r.handleForeignAllowlistPage, optAuthMw))
	// Near-duplicate artist detection page (#1614). Canonical path is
	// /reports/duplicates so it sits alongside /reports/compliance under
	// the Reports hub. The old /settings/artist-duplicates path 301s to
	// the new one so bookmarks and external links still resolve (#1615 IA
	// move). Registered before the catch-all so the specific paths win
	// over the /settings/{section} section redirect.
	mux.HandleFunc("GET "+bp+"/reports/duplicates", wrapOptionalAuth(r.handleArtistDuplicatesPage, optAuthMw))
	mux.HandleFunc("GET "+bp+"/settings/artist-duplicates", wrapOptionalAuth(func(w http.ResponseWriter, req *http.Request) {
		target := r.basePath + "/reports/duplicates"
		if raw := req.URL.RawQuery; raw != "" {
			target += "?" + raw
		}
		// gosec G710: path is server-built (r.basePath + /reports/duplicates);
		// only the query string flows from req, which cannot redirect off-origin.
		http.Redirect(w, req, target, http.StatusMovedPermanently) //nolint:gosec // G710: path is server-built; only the query string flows from req.
	}, optAuthMw))
	mux.HandleFunc("GET "+bp+"/settings/{section}", wrapOptionalAuth(func(w http.ResponseWriter, req *http.Request) {
		q := req.URL.Query()
		q.Set("tab", req.PathValue("section"))
		target := r.basePath + "/settings"
		if encoded := q.Encode(); encoded != "" {
			target += "?" + encoded
		}
		// gosec G710: path is server-built (r.basePath + /settings); query
		// originates from the request but cannot redirect off-origin.
		http.Redirect(w, req, target, http.StatusMovedPermanently) //nolint:gosec // G710: path is server-built; only the encoded query flows from req.
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

	// next/ UI channel lane (M55 #1340). Until a screen's next template lands,
	// every /next/* path falls back to its stable counterpart so navigation
	// never breaks and no /next path 404s. The handler strips the /next prefix
	// and re-dispatches through the mux to the existing v1 handler; the UX
	// middleware has already set the X-Stillwater-UX: next header. Registered
	// before the catch-all; each screen issue replaces this generic fallback
	// with a flag-aware handler as its next template lands. No per-route auth
	// wrapper here: the re-dispatched stable route applies its own auth.
	// Per-screen next/ routes land as their templates ship; each is more
	// specific than the /next/{path...} fallback so Go's mux prefers it, and
	// each renders the next template only when the resolved channel is "next"
	// (otherwise it falls back to the stable page itself). M55 #1335: artists.
	mux.HandleFunc("GET "+bp+"/next/artists", wrapOptionalAuth(r.handleNextArtistsPage, optAuthMw))
	// M55 #1334: the next/ dashboard is the next channel's INDEX, mirroring how
	// the stable dashboard is the site root ("/"). It is served at the next root
	// "/next/" (and the no-slash "/next"), not a "/next/dashboard" sub-path, so
	// there are no redirect shenanigans between the two. {$} matches the next
	// root exactly; both registrations sit before the catch-all wildcard so Go's
	// mux prefers them over /next/{path...}.
	mux.HandleFunc("GET "+bp+"/next/{$}", wrapOptionalAuth(r.handleNextDashboardPage, optAuthMw))
	mux.HandleFunc("GET "+bp+"/next", wrapOptionalAuth(r.handleNextDashboardPage, optAuthMw))
	// M55 #1336: the next/ artist-detail page. More specific than the
	// /next/{path...} fallback so Go's mux prefers it; renders the next template
	// only when the resolved channel is "next" (otherwise it delegates to the
	// stable tabbed detail page).
	mux.HandleFunc("GET "+bp+"/next/artists/{id}", wrapOptionalAuth(r.handleNextArtistDetailPage, optAuthMw))
	// Manage-artwork modal body fragment (M55 #1336, 4B): more specific than the
	// /next/{path...} fallback, so Go's mux prefers it. Renders ArtworkManageEditor
	// scoped to ?kind= for the in-page modal's active kind.
	mux.HandleFunc("GET "+bp+"/next/artists/{id}/artwork-modal", wrapOptionalAuth(r.handleNextArtworkModal, optAuthMw))
	// M55 #1774: preferences flyout drawer. Two routes:
	//   /next/preferences        - standalone page for direct-URL / bookmark access.
	//   /next/preferences-drawer - HTMX fragment; returns only the drawer body so
	//                             LayoutNext can lazy-load it without passing prefs
	//                             data through every handler that calls LayoutNext.
	mux.HandleFunc("GET "+bp+"/next/preferences", wrapOptionalAuth(r.handleNextPreferencesPage, optAuthMw))
	mux.HandleFunc("GET "+bp+"/next/preferences-drawer", wrapOptionalAuth(r.handleNextPreferencesDrawer, optAuthMw))
	// M55 #1773: foreign-files management in the next/ chrome. wrapOptionalAuth
	// (not wrapAuth) so the handler's requireForeignAdmin gate runs: it renders
	// the login page for unauthenticated visitors rather than returning 401 JSON
	// (which is correct for API routes but wrong for browser page requests).
	mux.HandleFunc("GET "+bp+"/next/reports/foreign-files", wrapOptionalAuth(r.handleNextForeignFilesPage, optAuthMw))
	mux.HandleFunc("GET "+bp+"/next/reports/foreign-files/allowlist", wrapOptionalAuth(r.handleNextForeignAllowlistPage, optAuthMw))
	mux.HandleFunc("GET "+bp+"/next/{path...}", r.nextFallback(mux))

	// Catch-all: unmatched routes render the custom 404 page. Registered last
	// so all explicit routes above take precedence. Uses optional auth so the
	// sidebar can show the logged-in state when the user is authenticated.
	mux.HandleFunc(bp+"/{path...}", wrapOptionalAuth(r.handle404, optAuthMw))

	// Apply middleware chain: security headers > i18n > logging > CSRF
	// Login and setup are exempt from CSRF (registered with rate limiter above).
	// /setup/restore shares the entry-point treatment because the UI that
	// posts to it (setup.templ) is rendered before any user exists, so there
	// is no session to attach a CSRF token to. Brute-force protection comes
	// from loginRL on the route.
	csrfExempt := []string{
		bp + "/api/v1/auth/login",
		bp + "/api/v1/auth/setup",
		bp + "/api/v1/setup/restore",
	}
	var handler http.Handler = mux
	handler = csrfWithExemptions(csrf, handler, csrfExempt)
	handler = middleware.Logging(r.logger, bp)(handler)
	// UX wraps Logging (outside it) so the resolved channel is in the request
	// context when Logging emits the per-request ux= field. It also sets the
	// X-Stillwater-UX response header and drives the /next/* lane.
	handler = middleware.UX(r.ux, bp)(handler)
	if r.i18nBundle != nil {
		handler = i18n.Middleware(r.i18nBundle)(handler)
	}
	handler = middleware.SecurityHeaders(handler)
	// Alt-Svc advertises HTTP/3 (QUIC) on the configured UDP port. When
	// HTTP/3 is disabled (http3Port == 0) AltSvc is a pass-through, so
	// composing it unconditionally keeps the middleware chain stable.
	handler = middleware.AltSvc(r.http3Port)(handler)
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
