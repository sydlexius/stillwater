package main

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/sydlexius/stillwater/internal/api"
	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/auth"
	"github.com/sydlexius/stillwater/internal/backup"
	"github.com/sydlexius/stillwater/internal/config"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/database"
	"github.com/sydlexius/stillwater/internal/encryption"
	"github.com/sydlexius/stillwater/internal/event"
	"github.com/sydlexius/stillwater/internal/filesystem"
	"github.com/sydlexius/stillwater/internal/i18n"
	"github.com/sydlexius/stillwater/internal/imagebridge"
	"github.com/sydlexius/stillwater/internal/langpref"
	"github.com/sydlexius/stillwater/internal/library"
	"github.com/sydlexius/stillwater/internal/logging"
	"github.com/sydlexius/stillwater/internal/maintenance"
	"github.com/sydlexius/stillwater/internal/nfo"
	"github.com/sydlexius/stillwater/internal/platform"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/internal/provider/audiodb"
	"github.com/sydlexius/stillwater/internal/provider/deezer"
	"github.com/sydlexius/stillwater/internal/provider/discogs"
	"github.com/sydlexius/stillwater/internal/provider/duckduckgo"
	"github.com/sydlexius/stillwater/internal/provider/fanarttv"
	"github.com/sydlexius/stillwater/internal/provider/genius"
	"github.com/sydlexius/stillwater/internal/provider/lastfm"
	"github.com/sydlexius/stillwater/internal/provider/musicbrainz"
	"github.com/sydlexius/stillwater/internal/provider/spotify"
	"github.com/sydlexius/stillwater/internal/provider/wikidata"
	"github.com/sydlexius/stillwater/internal/provider/wikipedia"
	"github.com/sydlexius/stillwater/internal/publish"
	"github.com/sydlexius/stillwater/internal/rule"
	"github.com/sydlexius/stillwater/internal/scanner"
	"github.com/sydlexius/stillwater/internal/scraper"
	"github.com/sydlexius/stillwater/internal/server"
	"github.com/sydlexius/stillwater/internal/settingsio"
	"github.com/sydlexius/stillwater/internal/updater"
	"github.com/sydlexius/stillwater/internal/version"
	"github.com/sydlexius/stillwater/internal/watcher"
	"github.com/sydlexius/stillwater/internal/webhook"
	"github.com/sydlexius/stillwater/web/static"
	"github.com/sydlexius/stillwater/web/templates"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"
	"golang.org/x/time/rate"
)

func main() {
	// Handle subcommands before starting the server. The switch has only one
	// case today but is shaped for adding future subcommands (reset-config,
	// migrate-only, etc.) without rewriting the dispatch.
	if len(os.Args) > 1 {
		switch os.Args[1] { //nolint:gocritic // singleCaseSwitch: shaped for future subcommand cases, see comment above
		case "reset-credentials":
			if err := resetCredentials(); err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				os.Exit(1)
			}
			return
		}
	}

	// Parse global flags
	resetPwd := flag.Bool("reset-password", false, "Reset admin password and exit")
	username := flag.String("username", "", "Username for password reset")
	newPassword := flag.String("new-password", "", "New password (insecure: visible in process list; prefer interactive prompt)")
	flag.Parse()

	if *resetPwd {
		if *newPassword != "" {
			fmt.Fprintln(os.Stderr, "warning: --new-password exposes the password in process arguments; consider using the interactive prompt instead")
		}
		if err := resetPassword(*username, *newPassword); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// Application holds all initialized state for a Stillwater server instance.
// Fields are populated in sequence by the staged init phases and consumed by
// the listener and background-worker phases.
type Application struct {
	// Phase: loadConfig
	cfg         *config.Config
	configPath  string
	scaffolded  bool
	scaffoldErr error

	// Phase: setupLogging
	logManager *logging.Manager
	logger     *slog.Logger

	// Phase: openStorage
	db            *sql.DB
	imageCacheDir string

	// Phase: wireSecurity
	encryptor *encryption.Encryptor

	// Phase: buildServices
	authService         *auth.Service
	authRegistry        *auth.Registry
	artistService       *artist.Service
	historyService      *artist.HistoryService
	libraryService      *library.Service
	defaultLibID        string
	platformService     *platform.Service
	connectionService   *connection.Service
	ruleService         *rule.Service
	ruleEngine          *rule.Engine
	ruleScheduler       *rule.Scheduler
	ruleScheduleMinutes int
	imageBridge         *imagebridge.Bridge
	scannerService      *scanner.Service
	rateLimiters        *provider.RateLimiterMap
	providerSettings    *provider.SettingsService
	providerRegistry    *provider.Registry
	webSearchRegistry   *provider.WebSearchRegistry
	orchestrator        *provider.Orchestrator
	scraperService      *scraper.Service
	nfoSnapshotService  *nfo.SnapshotService
	nfoSettingsService  *nfo.NFOSettingsService
	fsCheck             *rule.SharedFSCheck
	expectedWrites      *watcher.ExpectedWrites
	publisher           *publish.Publisher
	pipeline            *rule.Pipeline
	bulkService         *rule.BulkService
	bulkExecutor        *rule.BulkExecutor
	eventBus            *event.Bus
	webhookService      *webhook.Service
	webhookDispatcher   *webhook.Dispatcher
	backupService       *backup.Service
	maintenanceService  *maintenance.Service
	settingsIOService   *settingsio.Service
	updaterService      *updater.Service
	probeCache          *watcher.ProbeCache
	healthSub           *rule.HealthSubscriber
	dirtySub            *rule.DirtySubscriber
	i18nBundle          *i18n.Bundle
	router              *api.Router

	// Testing seams: override these via functional options before calling run phases.
	encKeyResolver func(cfg *config.Config, logger *slog.Logger) (string, error)
	dbOpener       func(path string) (*sql.DB, error)
}

// Option is a functional option for Application.
type Option func(*Application)

// WithEncKeyResolver overrides the encryption key resolver (used in tests).
func WithEncKeyResolver(fn func(cfg *config.Config, logger *slog.Logger) (string, error)) Option {
	return func(a *Application) { a.encKeyResolver = fn }
}

// WithDBOpener overrides the database opener (used in tests).
func WithDBOpener(fn func(path string) (*sql.DB, error)) Option {
	return func(a *Application) { a.dbOpener = fn }
}

// newApplication creates an Application with production defaults.
func newApplication(opts ...Option) *Application {
	a := &Application{
		encKeyResolver: resolveEncryptionKey,
		dbOpener:       database.Open,
	}
	for _, o := range opts {
		o(a)
	}
	return a
}

// run is the top-level server lifecycle. It initializes all phases in order
// and blocks until the server exits.
//
// Each defer is registered immediately after the phase that acquires the
// resource, so cleanup fires even when a later phase fails. LIFO order:
// scanner shutdown -> webhook drains -> listener stop -> eventBus.Stop ->
// logManager.Close -> db.Close.
func run() error {
	a := newApplication()

	if err := a.loadConfig(); err != nil {
		return err
	}
	if err := a.setupLogging(); err != nil {
		return err
	}
	defer a.logManager.Close() //nolint:errcheck // Close error not actionable on cleanup

	if err := a.openStorage(); err != nil {
		return err
	}
	defer func() {
		if err := a.db.Close(); err != nil {
			a.logger.Error("closing database", "error", err)
		}
	}()

	if err := a.wireSecurity(); err != nil {
		return err
	}
	if err := a.buildServices(); err != nil {
		return err
	}
	// eventBus.Start() is launched inside buildServices; register Stop here so
	// cleanup fires if startListeners fails or returns early.
	defer a.eventBus.Stop()

	if err := a.startListeners(); err != nil {
		return err
	}
	return nil
}

// loadConfig reads configuration from the environment and config file.
// It also performs first-run scaffolding when SW_CONFIG_PATH is set.
func (a *Application) loadConfig() error {
	rawConfigPath, configPathSet := os.LookupEnv("SW_CONFIG_PATH")
	configPath := rawConfigPath
	if configPath == "" {
		configPath = "/config/config.toml"
	}

	// First-run scaffolding: create a commented config.toml at configPath when
	// the file is missing so admins have a documented starting point. A failure
	// here is non-fatal; in-code defaults plus env vars are sufficient to boot.
	//
	// Only scaffold when the operator has explicitly opted in via a non-empty
	// SW_CONFIG_PATH. Native binary installs that boot with only SW_DB_PATH
	// and SW_MUSIC_PATH would otherwise log a "could not write scaffold"
	// warning every startup just because the container default /config is
	// unwritable on a host filesystem. An explicit SW_CONFIG_PATH="" must be
	// treated the same as "not set" so the empty-string escape hatch is not
	// surprising. The container image sets SW_CONFIG_PATH to a real path, so
	// the Docker first-run experience is preserved.
	if configPathSet && rawConfigPath != "" {
		a.scaffolded, a.scaffoldErr = config.EnsureScaffold(configPath)
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	a.cfg = cfg
	a.configPath = configPath
	return nil
}

// setupLogging initializes the structured logging manager from the loaded
// configuration. After this phase, a.logger and a.logManager are available.
func (a *Application) setupLogging() error {
	if a.cfg == nil {
		return errors.New("setupLogging: loadConfig must run first")
	}
	logCfg := logging.Config{
		Level:  a.cfg.Logging.Level,
		Format: a.cfg.Logging.Format,
	}
	logManager, logger := logging.NewManager(logCfg)
	a.logManager = logManager
	a.logger = logger
	slog.SetDefault(logger)

	// Warn (but do not abort) if the version ldflags injection is malformed.
	if err := version.Validate(); err != nil {
		logger.Warn("version validation failed; auto-updater will be unable to compare versions. "+
			"Run a release build via goreleaser, or check the -ldflags injection in the build pipeline",
			"error", err)
	}

	if a.scaffoldErr != nil {
		logger.Warn("could not write first-run config scaffold",
			"path", a.configPath, "error", a.scaffoldErr)
	} else if a.scaffolded {
		logger.Info("wrote first-run config scaffold", "path", a.configPath)
	}
	return nil
}

// openStorage opens the SQLite database, enables foreign keys, runs migrations,
// reloads logging settings from DB, and derives the image cache directory.
func (a *Application) openStorage() error {
	if a.logger == nil {
		return errors.New("openStorage: setupLogging must run first")
	}
	db, err := a.dbOpener(a.cfg.Database.Path)
	if err != nil {
		return fmt.Errorf("opening database: %w", err)
	}
	a.db = db

	// Issue #1078: enable PRAGMA foreign_keys so ON DELETE CASCADE fires.
	if err := database.EnableForeignKeys(db); err != nil {
		return fmt.Errorf("enabling foreign keys: %w", err)
	}
	if err := database.Migrate(db); err != nil {
		return fmt.Errorf("running migrations: %w", err)
	}
	a.logger.Info("database ready", slog.String("path", a.cfg.Database.Path))

	// Reload logging settings from DB (overrides config file values if present).
	loadDBLoggingConfig(context.Background(), db, a.logManager, a.logger)

	// Derive the image cache directory once so the publisher, API router, and
	// maintenance service all agree on where cached artist images live.
	a.imageCacheDir = filepath.Join(filepath.Dir(a.cfg.Database.Path), "cache", "images")
	return nil
}

// wireSecurity resolves the encryption key and constructs the encryptor.
func (a *Application) wireSecurity() error {
	if a.db == nil {
		return errors.New("wireSecurity: openStorage must run first")
	}
	encKey, err := a.encKeyResolver(a.cfg, a.logger)
	if err != nil {
		return fmt.Errorf("resolving encryption key: %w", err)
	}
	encryptor, _, err := encryption.NewEncryptor(encKey)
	if err != nil {
		return fmt.Errorf("creating encryptor: %w", err)
	}
	a.encryptor = encryptor
	return nil
}

// buildServices constructs all domain services and wires their dependencies.
// This phase delegates to three cohesive sub-phases (wireAuth, wireProviders,
// wireRuleEngine) before wiring event subscriptions and building the HTTP router.
func (a *Application) buildServices() error {
	if a.encryptor == nil {
		return errors.New("buildServices: wireSecurity must run first")
	}
	db := a.db
	cfg := a.cfg
	logger := a.logger
	ctx := context.Background()

	a.wireAuth(ctx)
	if err := a.wireProviders(ctx); err != nil {
		return err
	}
	// wireEventBus must run before wireRuleEngine: publish.New stores the
	// Notifier adapter that wraps a.eventBus, so the bus pointer has to
	// exist at construction time. A nil bus there silently no-ops every
	// connection.push_failed event (busNotifier.NotifyConnectionPushFailed
	// returns early on n.bus == nil), so the operator never sees the toast.
	wireEventBus(a, logger)
	// Track the goroutine so we can stop it if any phase below fails.
	// run() registers its own Stop only after buildServices returns
	// successfully; without this guard a wireRuleEngine error would
	// orphan the event-bus goroutine until process exit.
	busOwned := true
	defer func() {
		if busOwned {
			a.eventBus.Stop()
		}
	}()
	if err := a.wireRuleEngine(ctx, logger); err != nil {
		return err
	}

	wireInfraServices(ctx, a, db, cfg, logger)
	applyPersistedBasePath(ctx, db, cfg, logger)
	wireEventSubscriptions(a)

	logger.Info("starting stillwater",
		slog.String("version", version.Version),
		slog.String("commit", version.Commit),
	)

	// Probe filesystem notification support for each library path.
	a.probeCache = watcher.NewProbeCache()
	if probLibs, probErr := a.libraryService.List(ctx); probErr != nil {
		logger.Error("listing libraries for probe", "error", probErr)
	} else {
		a.probeCache.ProbeAll(ctx, probLibs, logger)
	}

	resolveRuleSchedule(a, db, logger)

	// --- i18n ---
	i18nBundle, err := i18n.LoadEmbedded()
	if err != nil {
		return fmt.Errorf("loading i18n bundle: %w", err)
	}
	a.i18nBundle = i18nBundle

	// --- HTTP router ---
	a.router = api.NewRouter(api.RouterDeps{
		AuthService:        a.authService,
		AuthRegistry:       a.authRegistry,
		ArtistService:      a.artistService,
		HistoryService:     a.historyService,
		ScannerService:     a.scannerService,
		PlatformService:    a.platformService,
		ProviderSettings:   a.providerSettings,
		ProviderRegistry:   a.providerRegistry,
		WebSearchRegistry:  a.webSearchRegistry,
		RateLimiters:       a.rateLimiters,
		Orchestrator:       a.orchestrator,
		RuleService:        a.ruleService,
		RuleEngine:         a.ruleEngine,
		Pipeline:           a.pipeline,
		BulkService:        a.bulkService,
		BulkExecutor:       a.bulkExecutor,
		NFOSnapshotService: a.nfoSnapshotService,
		NFOSettingsService: a.nfoSettingsService,
		ConnectionService:  a.connectionService,
		ScraperService:     a.scraperService,
		LibraryService:     a.libraryService,
		WebhookService:     a.webhookService,
		WebhookDispatcher:  a.webhookDispatcher,
		BackupService:      a.backupService,
		LogManager:         a.logManager,
		MaintenanceService: a.maintenanceService,
		SettingsIOService:  a.settingsIOService,
		UpdaterService:     a.updaterService,
		ProbeCache:         a.probeCache,
		ExpectedWrites:     a.expectedWrites,
		EventBus:           a.eventBus,
		DB:                 db,
		Logger:             logger,
		BasePath:           cfg.Server.BasePath,
		BasePathFromEnv:    cfg.Server.BasePathFromEnv,
		TLSStatus:          buildTLSStatus(cfg),
		HTTP3Port:          server.EffectiveHTTP3Port(cfg),
		StaticFS:           static.FS,
		ImageCacheDir:      a.imageCacheDir,
		Publisher:          a.publisher,
		RuleScheduler:      a.ruleScheduler,
		I18nBundle:         a.i18nBundle,
		Encryptor:          a.encryptor,
	})

	// Hand ownership to run(): the caller's deferred Stop now owns the
	// bus lifecycle. Clearing the flag prevents the deferred Stop above
	// from firing on the success path. Must be the LAST thing before
	// `return nil` so every fallible step in buildServices (i18n bundle
	// load, router construction, etc.) is still guarded by the deferred
	// Stop if it errors.
	busOwned = false
	return nil
}

// wireEventBus initializes the event bus and starts it. The corresponding
// Stop is deferred in run() immediately after buildServices returns.
func wireEventBus(a *Application, logger *slog.Logger) {
	a.eventBus = event.NewBus(logger, 256)
	go a.eventBus.Start()
	a.webhookService = webhook.NewService(a.db)
	a.webhookDispatcher = webhook.NewDispatcher(a.webhookService, logger)
}

// wireInfraServices wires backup, maintenance, settingsIO, and updater
// services that depend only on db and cfg.
func wireInfraServices(ctx context.Context, a *Application, db *sql.DB, cfg *config.Config, logger *slog.Logger) {
	backupDir := cfg.Backup.Path
	if backupDir == "" {
		backupDir = filepath.Join(filepath.Dir(cfg.Database.Path), "backups")
	}
	a.backupService = backup.NewService(db, backupDir, cfg.Backup.RetentionCount, logger)
	if dbRetention := getDBIntSetting(ctx, db, "backup_retention_count", 0); dbRetention > 0 {
		a.backupService.SetRetention(dbRetention)
	}
	if dbMaxAge := getDBIntSetting(ctx, db, "backup_max_age_days", -1); dbMaxAge >= 0 {
		a.backupService.SetMaxAgeDays(dbMaxAge)
	}
	a.maintenanceService = maintenance.NewService(db, cfg.Database.Path, a.imageCacheDir, logger)
	a.settingsIOService = settingsio.NewService(db, a.providerSettings, a.connectionService, a.platformService, a.webhookService).
		WithRuleService(a.ruleService).
		WithScraperService(a.scraperService)
	a.updaterService = updater.NewService(db, logger)
}

// wireEventSubscriptions connects the event bus to the webhook dispatcher,
// scanner, bulk executor, FSCache invalidator, health subscriber, and dirty
// subscriber. All services wired here must be initialized before this call.
func wireEventSubscriptions(a *Application) {
	for _, eventType := range []event.Type{
		event.ArtistNew, event.MetadataFixed, event.ReviewNeeded,
		event.RuleViolation, event.BulkCompleted, event.ScanCompleted,
		event.LidarrArtistAdd, event.LidarrDownload,
		event.EmbyArtistUpdate, event.EmbyLibraryScan,
		event.JellyfinArtistUpdate, event.JellyfinLibraryScan,
		event.FSDirCreated, event.FSDirRemoved, event.FSUnexpectedWrite,
	} {
		a.eventBus.Subscribe(eventType, a.webhookDispatcher.HandleEvent)
	}
	a.scannerService.SetEventBus(a.eventBus)
	a.bulkExecutor.SetEventBus(a.eventBus)
	if fsCache := a.ruleEngine.FSCache(); fsCache != nil {
		for _, eventType := range []event.Type{event.FSDirCreated, event.FSDirRemoved, event.FSUnexpectedWrite} {
			a.eventBus.Subscribe(eventType, func(ev event.Event) {
				if p, ok := ev.Data["path"].(string); ok && p != "" {
					fsCache.InvalidatePath(p)
				}
			})
		}
	}
	a.healthSub = rule.NewHealthSubscriber(a.ruleEngine, a.artistService, a.logger)
	a.eventBus.Subscribe(event.ArtistUpdated, a.healthSub.HandleEvent)
	a.dirtySub = rule.NewDirtySubscriber(a.artistService, a.logger)
	a.eventBus.Subscribe(event.ArtistUpdated, a.dirtySub.HandleEvent)
}

// resolveRuleSchedule reads the rule schedule interval from the settings table,
// migrating legacy hours-to-minutes entries, and initializes the scheduler when
// the interval is at least 5 minutes.
func resolveRuleSchedule(a *Application, db *sql.DB, logger *slog.Logger) {
	ctx := context.Background()
	a.ruleScheduleMinutes = getDBIntSetting(ctx, db, "rule_schedule.interval_minutes", 0)
	if a.ruleScheduleMinutes == 0 {
		if legacyHours := getDBIntSetting(ctx, db, "rule_schedule.interval_hours", 0); legacyHours > 0 {
			a.ruleScheduleMinutes = legacyHours * 60
			_, _ = db.ExecContext(ctx,
				`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
				 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
				"rule_schedule.interval_minutes", fmt.Sprintf("%d", a.ruleScheduleMinutes),
				time.Now().UTC().Format(time.RFC3339))
			_, _ = db.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, "rule_schedule.interval_hours")
			logger.Info("migrated rule schedule from hours to minutes", "minutes", a.ruleScheduleMinutes)
		}
	}
	if a.ruleScheduleMinutes >= 5 {
		a.ruleScheduler = rule.NewScheduler(a.pipeline, a.ruleService, a.artistService, logger)
		langprefRepo := langpref.NewRepository(db)
		a.ruleScheduler.SetLangPrefProvider(langprefRepo.EffectiveForBackground)
	} else if a.ruleScheduleMinutes > 0 && a.ruleScheduleMinutes < 5 {
		logger.Warn("rule scheduler interval too short (minimum 5 minutes); scheduler not started",
			"minutes", a.ruleScheduleMinutes)
	}
}

// wireAuth wires library, artist, history, platform, connection, and the
// authentication service / registry (plus any external auth provider
// configured in the settings table -- emby / jellyfin). All failure modes
// today are log-and-degrade: external auth provider construction Warns on
// error and skips that provider, so the function never returns a non-nil
// error and the signature has no error return (per unparam).
func (a *Application) wireAuth(ctx context.Context) {
	db := a.db
	logger := a.logger
	cfg := a.cfg

	// --- Library ---
	a.libraryService = library.NewService(db)
	a.defaultLibID = backfillDefaultLibrary(ctx, a.libraryService, cfg.Music.LibraryPath, db, logger)

	// --- Artist / History ---
	a.artistService = artist.NewService(db)
	a.historyService = artist.NewHistoryService(db)
	a.artistService.SetHistoryService(a.historyService)

	// --- Platform / Connection ---
	a.platformService = platform.NewService(db)
	a.connectionService = connection.NewService(db, a.encryptor)

	// --- Auth ---
	a.authService = auth.NewService(db)
	a.authRegistry = auth.NewRegistry()
	a.authRegistry.Register(auth.NewLocalProvider(db))
	authMethod := getDBStringSetting(ctx, db, "auth.method", "local")
	authServerURL := getDBStringSetting(ctx, db, "auth.server_url", "")
	if authServerURL != "" {
		switch authMethod {
		case "emby":
			if p, err := auth.NewEmbyProvider(authServerURL, false, "admin", "operator"); err == nil {
				a.authRegistry.Register(p)
			} else {
				logger.Warn("failed to create emby auth provider", "error", err)
			}
		case "jellyfin":
			if p, err := auth.NewJellyfinProvider(authServerURL, false, "admin", "operator"); err == nil {
				a.authRegistry.Register(p)
			} else {
				logger.Warn("failed to create jellyfin auth provider", "error", err)
			}
		}
	}
}

// wireProviders wires the metadata provider registry (MusicBrainz, Fanart.tv,
// and the remaining nine adapters), the web-search registry, the orchestrator,
// and the scraper service that backs the orchestrator's executor.
func (a *Application) wireProviders(ctx context.Context) error {
	db := a.db
	logger := a.logger

	a.rateLimiters = provider.NewRateLimiterMap()
	a.providerSettings = provider.NewSettingsService(db, a.encryptor)
	a.providerRegistry = provider.NewRegistry()

	mb := musicbrainz.New(a.rateLimiters, logger)
	if baseURL, err := a.providerSettings.GetBaseURL(ctx, provider.NameMusicBrainz); err != nil {
		logger.Warn("failed to load MusicBrainz mirror URL from database", "error", err)
	} else if baseURL != "" {
		mb.SetBaseURL(baseURL)
		logger.Info("loaded MusicBrainz mirror URL", slog.String("base_url", baseURL))
	}
	if limit, err := a.providerSettings.GetRateLimit(ctx, provider.NameMusicBrainz); err != nil {
		logger.Warn("failed to load MusicBrainz rate limit from database", "error", err)
	} else if limit > 0 {
		a.rateLimiters.SetLimit(provider.NameMusicBrainz, rate.Limit(limit))
		logger.Info("loaded MusicBrainz custom rate limit", slog.Float64("req_per_sec", limit))
	}
	a.providerRegistry.Register(mb)
	a.providerRegistry.Register(fanarttv.New(a.rateLimiters, a.providerSettings, logger))
	a.providerRegistry.Register(audiodb.New(a.rateLimiters, a.providerSettings, logger))
	a.providerRegistry.Register(discogs.New(a.rateLimiters, a.providerSettings, logger))
	a.providerRegistry.Register(lastfm.New(a.rateLimiters, a.providerSettings, logger))
	a.providerRegistry.Register(wikidata.New(a.rateLimiters, logger))
	a.providerRegistry.Register(deezer.New(a.rateLimiters, logger))
	a.providerRegistry.Register(wikipedia.New(a.rateLimiters, a.providerSettings, logger))
	a.providerRegistry.Register(genius.New(a.rateLimiters, a.providerSettings, logger))
	a.providerRegistry.Register(spotify.New(a.rateLimiters, a.providerSettings, logger))

	a.webSearchRegistry = provider.NewWebSearchRegistry()
	a.webSearchRegistry.Register(duckduckgo.New(a.rateLimiters, logger))

	a.orchestrator = provider.NewOrchestrator(a.providerRegistry, a.providerSettings, logger)

	// --- Scraper ---
	a.scraperService = scraper.NewService(db, logger)
	if err := a.scraperService.SeedDefaults(ctx); err != nil {
		return fmt.Errorf("seeding default scraper config: %w", err)
	}
	scraperExecutor := scraper.NewExecutor(a.scraperService, a.providerRegistry, a.providerSettings, logger)
	a.orchestrator.SetExecutor(scraperExecutor)

	return nil
}

// wireRuleEngine wires the rule engine, fixers, pipeline, and bulk executor.
// wireProviders must run before this sub-phase so that a.orchestrator and
// a.platformService are available for fixer construction.
func (a *Application) wireRuleEngine(ctx context.Context, logger *slog.Logger) error {
	db := a.db

	// --- Rule engine ---
	a.ruleService = rule.NewService(db).WithLogger(logger)
	if err := a.ruleService.SeedDefaults(ctx); err != nil {
		return fmt.Errorf("seeding default rules: %w", err)
	}
	a.ruleEngine = rule.NewEngine(a.ruleService, db, a.platformService, a.libraryService, logger)
	a.ruleEngine.SetFSCache(rule.NewFSCache(0, 0, logger))
	a.ruleEngine.SetMetadataProvider(a.orchestrator)

	// Wire image bridge so logo_padding rule can check/fix API-only artists.
	a.imageBridge = imagebridge.New(a.connectionService, a.artistService, logger)
	a.ruleEngine.SetImageFetcher(a.imageBridge)

	// --- Scanner ---
	cfg := a.cfg
	a.scannerService = scanner.NewService(a.artistService, a.ruleEngine, a.ruleService, logger, cfg.Music.LibraryPath, cfg.Scanner.Exclusions)
	a.scannerService.SetDefaultLibraryID(a.defaultLibID)
	a.scannerService.SetLibraryLister(a.libraryService)
	a.scannerService.SetMtimeFastPath(cfg.Scanner.MtimeFastPath)

	// --- NFO ---
	a.nfoSnapshotService = nfo.NewSnapshotService(db)
	a.nfoSettingsService = nfo.NewNFOSettingsService(db, logger)

	// --- Rule fixers and pipeline ---
	a.fsCheck = rule.NewSharedFSCheck(a.libraryService, logger)
	a.expectedWrites = watcher.NewExpectedWrites()

	// Guard the ordering invariant: wireEventBus must run first so the
	// Notifier adapter captures a non-nil bus. Without the bus, every
	// connection.push_failed event silently no-ops in the notifier guard,
	// which we hit live during M52 PR6 UAT.
	if a.eventBus == nil {
		panic("wireRuleEngine: a.eventBus is nil; wireEventBus must run first (see main.go phase ordering)")
	}
	a.publisher = publish.New(publish.Deps{
		ArtistService:      a.artistService,
		ConnectionService:  a.connectionService,
		LibraryService:     a.libraryService,
		NFOSnapshotService: a.nfoSnapshotService,
		NFOSettingsService: a.nfoSettingsService,
		PlatformService:    a.platformService,
		ExpectedWrites:     a.expectedWrites,
		ImageCacheDir:      a.imageCacheDir,
		Logger:             logger,
		// Bridge per-connection push errors from detached goroutines onto
		// the event bus so the SSE hub can surface them as toasts.
		Notifier: publish.NewBusNotifier(a.eventBus),
	})
	// Wire the rename-time platform syncer so Service.RenameDirectory
	// re-issues the artist path on Emby/Jellyfin/Lidarr after a successful
	// directory rename (#1222, #1231). The publisher already owns
	// per-platform clients and connection-service access, so it is the
	// natural home for this orchestration.
	a.artistService.SetPlatformRenameSyncer(a.publisher)

	logoPaddingFixer := rule.NewLogoPaddingFixer(a.platformService, a.fsCheck, logger)
	logoPaddingFixer.SetImageFetcher(a.imageBridge, a.ruleEngine.ConsumeAPIImage)

	// Resolve the MusicBrainz release-group fetcher from the provider registry.
	// The MB adapter implements provider.ReleaseGroupFetcher; the discography
	// checker and fixer both need it. When MB is unregistered the rule degrades
	// gracefully (checker flags only empty discographies; fixer reports a
	// non-fatal "not available" result).
	releaseGroupFetcher := resolveReleaseGroupFetcher(a.providerRegistry)
	a.ruleEngine.SetReleaseGroupFetcher(releaseGroupFetcher)

	fixers := []rule.Fixer{
		rule.NewNFOFixer(a.nfoSnapshotService, a.nfoSettingsService, a.fsCheck, a.expectedWrites),
		rule.NewMetadataFixer(a.orchestrator, logger),
		rule.NewNameLanguageFixer(a.orchestrator, logger),
		rule.NewImageFixer(a.orchestrator, a.platformService, a.fsCheck, logger),
		rule.NewExtraneousImagesFixer(a.platformService, a.fsCheck, logger),
		logoPaddingFixer,
		rule.NewDirectoryRenameFixer(a.fsCheck, logger),
		rule.NewBackdropSequencingFixer(a.platformService, a.fsCheck, logger),
		rule.NewDiscographyFixer(releaseGroupFetcher, a.fsCheck, a.nfoSnapshotService, logger),
	}
	a.pipeline = rule.NewPipeline(a.ruleEngine, a.artistService, a.ruleService, fixers, a.publisher, logger)
	a.pipeline.SetHistoryService(a.historyService)

	a.bulkService = rule.NewBulkService(db)
	a.bulkExecutor = rule.NewBulkExecutor(a.bulkService, a.artistService, a.orchestrator, a.pipeline, a.nfoSnapshotService, a.platformService, a.expectedWrites, a.publisher, logger)

	return nil
}

// resolveReleaseGroupFetcher returns the MusicBrainz adapter from the provider
// registry when it implements provider.ReleaseGroupFetcher, or nil when the
// adapter is unregistered or does not support release-group fetching. The
// discography_populated checker and DiscographyFixer both accept a nil value
// and degrade gracefully, so a nil return is not a fatal wiring error.
func resolveReleaseGroupFetcher(registry *provider.Registry) provider.ReleaseGroupFetcher {
	if registry == nil {
		return nil
	}
	p := registry.Get(provider.NameMusicBrainz)
	if p == nil {
		return nil
	}
	// Type-assert via the interface so the MB concrete type is not imported
	// here purely for this check.
	fetcher, ok := p.(provider.ReleaseGroupFetcher)
	if !ok {
		return nil
	}
	return fetcher
}

// startListeners starts all background workers and the HTTP listener, then
// blocks until the server exits. It also performs the graceful shutdown
// sequence including webhook draining and scanner shutdown.
//
// logManager.Close, eventBus.Stop, and db.Close are deferred in run() so
// they fire even if this phase returns early.
func (a *Application) startListeners() error {
	if a.router == nil {
		return errors.New("startListeners: buildServices must run first")
	}
	cfg := a.cfg
	logger := a.logger
	db := a.db

	// Graceful shutdown signal handling.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Health subscriber.
	go a.healthSub.Start(ctx)
	defer a.healthSub.Stop()

	// Bootstrap stale health scores in the background after a short delay.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Error("health bootstrap panicked", "panic", r)
			}
		}()
		select {
		case <-time.After(5 * time.Second):
			a.healthSub.Bootstrap(ctx)
		case <-ctx.Done():
		}
	}()

	// Backup scheduler.
	if cfg.Backup.Enabled {
		go a.backupService.StartScheduler(ctx, time.Duration(cfg.Backup.IntervalHours)*time.Hour)
	}

	// Maintenance scheduler (interval from DB settings, defaults to daily).
	{
		maintEnabled := getDBBoolSetting(ctx, db, "db_maintenance.enabled", true)
		maintHours := getDBIntSetting(ctx, db, "db_maintenance.interval_hours", 24)
		if maintHours <= 0 {
			maintHours = 24
		}
		if maintEnabled {
			go a.maintenanceService.StartScheduler(ctx, time.Duration(maintHours)*time.Hour)
		}
	}

	// Proactive exists_flag consistency scanner.
	{
		existsFlagHours := getDBIntSetting(ctx, db, "exists_flag_scan.interval_hours", 1)
		if existsFlagHours <= 0 {
			existsFlagHours = 1
		}
		go a.maintenanceService.StartExistsFlagScanner(ctx, time.Duration(existsFlagHours)*time.Hour, 10*time.Second)
	}

	// Foreign-file scanner.
	{
		foreignHours := getDBIntSetting(ctx, db, "foreign_file_scan.interval_hours", 6)
		if foreignHours <= 0 {
			foreignHours = 6
		}
		go a.maintenanceService.StartForeignFileScanner(ctx, a.artistService, time.Duration(foreignHours)*time.Hour, 30*time.Second)
	}

	// Session cleanup.
	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := a.authService.CleanExpiredSessions(ctx); err != nil {
					logger.Error("session cleanup failed", "error", err)
				}
			}
		}
	}()

	// Rule scheduler.
	if a.ruleScheduler != nil {
		go a.ruleScheduler.Start(ctx, time.Duration(a.ruleScheduleMinutes)*time.Minute)
	}

	// Updater background scheduler.
	go a.updaterService.StartScheduler(ctx)

	// Filesystem watcher for libraries with fs_watch enabled.
	{
		scanFn := func(ctx context.Context) error {
			_, err := a.scannerService.Run(ctx)
			return err
		}
		watcherService := watcher.NewService(scanFn, a.libraryService, a.eventBus, logger, a.probeCache, a.expectedWrites)
		go watcherService.Start(ctx)
	}

	startAttrs := []any{
		slog.Int("port", cfg.Server.Port),
		slog.String("base_path", cfg.Server.BasePath),
		slog.Bool("tls", cfg.Server.TLS.Enabled()),
	}
	if cfg.Server.TLS.Enabled() && cfg.Server.TLS.Port != 0 && cfg.Server.TLS.Port != cfg.Server.Port {
		startAttrs = append(startAttrs, slog.Int("tls_port", cfg.Server.TLS.Port))
	}
	logger.Info("server starting", startAttrs...)

	// RunListeners blocks until ctx is canceled or a listener fails.
	srvErr := server.RunListeners(ctx, cfg, a.router.Handler(ctx), logger)

	logger.Info("shutting down")

	// Cancel the shared ctx so background goroutines stop before the scanner
	// shuts down. On the SIGTERM path stop() has already fired; on the
	// listener-failure path RunListeners returns without ctx being canceled.
	stop()

	// Drain in-flight INBOUND webhook handlers first. Each handler can spawn
	// outbound work (HandleEvent -> webhookDispatcher.Send), so we want them
	// finished before we start draining the dispatcher; otherwise the
	// dispatcher's deadline could expire while new outbound jobs are still
	// being enqueued by surviving handlers. The 5-minute bound matches the
	// per-handler context window and ensures shutdown cannot hang on a worker
	// stuck in non-context-aware code.
	inboundDrainCtx, inboundDrainCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer inboundDrainCancel()
	if err := a.router.DrainWebhooks(inboundDrainCtx); err != nil {
		logger.Warn("webhook drain did not complete cleanly", "error", err)
	}

	// Drain in-flight OUTBOUND webhook deliveries. A 10s deadline matches
	// requestTimeout and prevents a misbehaving external webhook target from
	// blocking shutdown indefinitely.
	outboundDrainCtx, outboundDrainCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer outboundDrainCancel()
	if err := a.webhookDispatcher.Drain(outboundDrainCtx); err != nil {
		logger.Warn("webhook drain timed out", slog.String("error", err.Error()))
	}

	// Stop the scanner -- the listener layer has drained, so no new scan
	// requests can race with the scanner's WaitGroup.
	a.scannerService.Shutdown()

	return srvErr
}

// applyPersistedBasePath reads the server.base_path override from the settings
// table and applies it to cfg when the env var was not the source of truth
// (cfg.Server.BasePathFromEnv == false). The HTTP mux is wired once at
// startup and cannot rebind on the fly; a corrupt persisted value is
// warn-and-ignored so operators are not locked out.
func applyPersistedBasePath(ctx context.Context, db *sql.DB, cfg *config.Config, logger *slog.Logger) {
	if cfg.Server.BasePathFromEnv {
		return
	}
	override := getDBStringSetting(ctx, db, "server.base_path", "")
	if override == "" {
		return
	}
	normalized := strings.TrimRight(override, "/")
	if normalized == "/" {
		normalized = ""
	}
	// Validate before applying. The HTTP mux composes routes as
	// basePath+"/api/v1/..." so a malformed override (missing
	// leading "/") would poison every route pattern and the
	// process would fail to start with an opaque mux error.
	// Warn-and-ignore so a corrupt persisted value cannot lock
	// operators out -- they can repair it via SW_BASE_PATH env or
	// by editing the settings table directly.
	//
	// The persisted value reaches mux pattern composition without a
	// second pass through the API handler's charset filter, so this
	// loader must reject the same things directly: a missing leading
	// "/", a leading "//" or "/\\" (CodeQL "bad redirect check" --
	// schema-relative URLs and Windows-style separators that could
	// be reflected back in router/redirect contexts), and any
	// character outside the API-validated set. The empty string is
	// the canonical "no override" sentinel and is allowed through.
	if normalized != "" && !isValidPersistedBasePath(normalized) {
		logger.Warn("ignoring invalid persisted base_path override",
			"override", override,
			"reason", "must start with single \"/\" and contain only letters, digits, hyphens, underscores, and slashes",
		)
		return
	}
	if normalized != cfg.Server.BasePath {
		logger.Info("applying persisted base_path override",
			"previous", cfg.Server.BasePath, "override", normalized)
		cfg.Server.BasePath = normalized
	}
}

// resolveEncryptionKey determines the encryption key to use.
// Priority: SW_ENCRYPTION_KEY env var > encryption.key alongside DB > generate new.
func resolveEncryptionKey(cfg *config.Config, logger *slog.Logger) (string, error) {
	if cfg.Encryption.Key != "" {
		return cfg.Encryption.Key, nil
	}

	dataDir := filepath.Dir(cfg.Database.Path)
	keyFile := filepath.Join(dataDir, "encryption.key")

	// Try loading from file. A read error other than "file not found" must be
	// fatal: silently falling through to key generation would orphan every
	// previously-encrypted secret if the existing file is unreadable due to
	// permissions, a filesystem fault, or transient IO failure.
	data, err := os.ReadFile(keyFile) //nolint:gosec // G304: path derived from trusted config
	switch {
	case err == nil:
		key := strings.TrimSpace(string(data))
		if key != "" {
			logger.Debug("loaded encryption key from file", slog.String("path", keyFile))
			return key, nil
		}
	case !errors.Is(err, os.ErrNotExist):
		return "", fmt.Errorf("reading encryption key file %s: %w", keyFile, err)
	}

	// Generate a new key
	_, key, err := encryption.NewEncryptor("")
	if err != nil {
		return "", fmt.Errorf("generating encryption key: %w", err)
	}

	// Persist to file atomically. Failure here is fatal: if the key is not
	// written, the next startup generates a different key and makes every
	// previously encrypted secret unrecoverable (provider API keys, connection
	// API keys). filesystem.WriteFileAtomic uses the tmp/bak/rename pattern so
	// a crash or disk-full mid-write cannot leave a truncated key on disk.
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return "", fmt.Errorf("creating data directory for encryption key: %w", err)
	}

	if err := filesystem.WriteFileAtomic(keyFile, []byte(key+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("saving encryption key to file %s: %w", keyFile, err)
	}
	logger.Warn("generated new encryption key -- back up this file",
		slog.String("path", keyFile))

	return key, nil
}

// resetCredentials wipes all stored credentials from the database.
// This is an offline operation intended for recovery when the encryption key
// is lost or credentials need to be re-entered.
func resetCredentials() error {
	configPath := os.Getenv("SW_CONFIG_PATH")
	if configPath == "" {
		configPath = "/config/config.toml"
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	db, err := database.Open(cfg.Database.Path)
	if err != nil {
		return fmt.Errorf("opening database: %w", err)
	}
	defer db.Close() //nolint:errcheck // Close error not actionable on cleanup

	if err := database.EnableForeignKeys(db); err != nil {
		return fmt.Errorf("enabling foreign keys: %w", err)
	}
	if err := database.Migrate(db); err != nil {
		return fmt.Errorf("running migrations: %w", err)
	}

	ctx := context.Background()

	// Clear provider API keys from settings
	if _, err := db.ExecContext(ctx, "DELETE FROM settings WHERE key LIKE 'provider.%.api_key'"); err != nil {
		return fmt.Errorf("clearing provider API keys: %w", err)
	}

	// Clear connection API keys
	if _, err := db.ExecContext(ctx, "UPDATE connections SET encrypted_api_key = ''"); err != nil {
		return fmt.Errorf("clearing connection API keys: %w", err)
	}

	// Clear user accounts (forces re-setup)
	if _, err := db.ExecContext(ctx, "DELETE FROM users"); err != nil {
		return fmt.Errorf("clearing user accounts: %w", err)
	}

	// Clear all sessions
	if _, err := db.ExecContext(ctx, "DELETE FROM sessions"); err != nil {
		return fmt.Errorf("clearing sessions: %w", err)
	}

	fmt.Println("Credentials reset successfully.")
	fmt.Println("All API keys, connection credentials, and user accounts have been cleared.")
	fmt.Println("The application will prompt for initial setup on next start.")
	return nil
}

// resetPassword updates the password for a user in the database.
// It opens the database, runs migrations, prompts for a password if needed,
// then delegates to resetPasswordDB.
func resetPassword(username, password string) error {
	configPath := os.Getenv("SW_CONFIG_PATH")
	if configPath == "" {
		configPath = "/config/config.toml"
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	db, err := database.Open(cfg.Database.Path)
	if err != nil {
		return fmt.Errorf("opening database: %w", err)
	}
	defer db.Close() //nolint:errcheck // Close error not actionable on cleanup

	if err := database.EnableForeignKeys(db); err != nil {
		return fmt.Errorf("enabling foreign keys: %w", err)
	}
	if err := database.Migrate(db); err != nil {
		return fmt.Errorf("running migrations: %w", err)
	}

	if password == "" {
		password, err = promptPassword()
		if err != nil {
			return fmt.Errorf("reading password: %w", err)
		}
	}

	return resetPasswordDB(context.Background(), db, username, password)
}

// resetPasswordDB performs the password reset against an already-open database.
// Accessible from tests in the same package.
func resetPasswordDB(ctx context.Context, db *sql.DB, username, password string) error {
	if username == "" {
		if err := db.QueryRowContext(ctx,
			"SELECT username FROM users WHERE role = 'admin' LIMIT 1").Scan(&username); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("no admin users found in database")
			}
			return fmt.Errorf("querying admin user: %w", err)
		}
	} else {
		var exists int
		if err := db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM users WHERE username = ?", username).Scan(&exists); err != nil {
			return fmt.Errorf("querying user: %w", err)
		}
		if exists == 0 {
			return fmt.Errorf("user not found: %s", username)
		}
	}

	hash, err := bcrypt.GenerateFromPassword(auth.PrehashPassword(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hashing password: %w", err)
	}

	result, err := db.ExecContext(ctx,
		"UPDATE users SET password_hash = ? WHERE username = ?", string(hash), username)
	if err != nil {
		return fmt.Errorf("updating password: %w", err)
	}

	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("user not found: %s", username)
	}

	fmt.Printf("Password for user '%s' has been reset successfully.\n", username)
	return nil
}

// promptPassword reads a password from stdin with TTY echo suppression.
// For TTY (interactive): prompts twice and confirms passwords match.
// For non-TTY (pipes/scripts): reads single line without confirmation.
func promptPassword() (string, error) {
	fd := int(os.Stdin.Fd())

	fmt.Fprint(os.Stderr, "Enter new password: ")
	password, err := readPasswordNoEcho()
	if err != nil {
		return "", err
	}
	fmt.Fprintln(os.Stderr)

	// Only prompt for confirmation on TTY (interactive mode)
	if term.IsTerminal(fd) {
		fmt.Fprint(os.Stderr, "Confirm password: ")
		confirm, err := readPasswordNoEcho()
		if err != nil {
			return "", err
		}
		fmt.Fprintln(os.Stderr)

		if password != confirm {
			return "", fmt.Errorf("passwords do not match")
		}
	}

	if password == "" {
		return "", fmt.Errorf("password cannot be empty")
	}

	return password, nil
}

// readPasswordNoEcho reads a password from stdin with echo suppression on TTY.
// If stdin is not a TTY, falls back to plain line reading (for scripts/pipes).
func readPasswordNoEcho() (string, error) {
	fd := int(os.Stdin.Fd())

	// Try to suppress echo on TTY
	if term.IsTerminal(fd) {
		password, err := term.ReadPassword(fd)
		if err != nil {
			return "", fmt.Errorf("reading password: %w", err)
		}
		return string(password), nil
	}

	// Fall back to plain reading for non-TTY (pipes, scripts)
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", fmt.Errorf("reading password from stdin: %w", err)
	}
	return strings.TrimSpace(line), nil
}

// backfillDefaultLibrary ensures at least one library exists. Returns the
// default library ID for the scanner. Assignment of "orphaned" artists
// (artists without a membership row) is no longer performed here: the
// legacy artists.library_id column was dropped in migration 004 and
// artist_libraries is the authoritative membership record. Newly
// scanned artists pick up a membership via Service.Create's
// AddDerivingSource path.
func backfillDefaultLibrary(ctx context.Context, libService *library.Service, musicPath string, db *sql.DB, logger *slog.Logger) string {
	_ = db // legacy parameter retained for call-site stability
	libs, err := libService.List(ctx)
	if err != nil {
		logger.Error("listing libraries for backfill", "error", err)
		return ""
	}

	var defaultID string
	if len(libs) > 0 {
		// Prefer a library named "Default", then one matching the legacy
		// musicPath, and fall back to the first listed library.
		var pathMatchID string
		cleanedMusic := filepath.Clean(musicPath)
		for i := range libs {
			lib := &libs[i]
			if lib.Name == "Default" {
				defaultID = lib.ID
				break
			}
			if musicPath != "" && pathMatchID == "" && filepath.Clean(lib.Path) == cleanedMusic {
				pathMatchID = lib.ID
			}
		}
		if defaultID == "" {
			if pathMatchID != "" {
				defaultID = pathMatchID
			} else {
				defaultID = libs[0].ID
			}
		}
	} else {
		// No libraries exist yet: create a Default library from SW_MUSIC_PATH
		lib := &library.Library{
			Name: "Default",
			Path: musicPath,
			Type: library.TypeRegular,
		}
		if err := libService.Create(ctx, lib); err != nil {
			logger.Error("creating default library", "error", err)
			return ""
		}
		logger.Info("created default library",
			slog.String("id", lib.ID),
			slog.String("path", musicPath))
		defaultID = lib.ID
	}

	return defaultID
}

// loadDBLoggingConfig reads logging settings from the DB and reconfigures the
// log manager if any are present. Called once after migrations.
func loadDBLoggingConfig(ctx context.Context, db *sql.DB, mgr *logging.Manager, logger *slog.Logger) {
	level := getDBStringSetting(ctx, db, "logging.level", "")
	format := getDBStringSetting(ctx, db, "logging.format", "")
	filePath := getDBStringSetting(ctx, db, "logging.file_path", "")
	fileMaxSize := getDBIntSetting(ctx, db, "logging.file_max_size_mb", 0)
	fileMaxFiles := getDBIntSetting(ctx, db, "logging.file_max_files", 0)
	fileMaxAge := getDBIntSetting(ctx, db, "logging.file_max_age_days", 0)
	if level == "" && format == "" && filePath == "" && fileMaxSize <= 0 && fileMaxFiles <= 0 && fileMaxAge <= 0 {
		return // no DB overrides
	}

	cfg := mgr.Config()
	if level != "" && logging.ValidLevel(level) {
		cfg.Level = level
	}
	if format != "" && logging.ValidFormat(format) {
		cfg.Format = format
	}
	if filePath != "" {
		cfg.FilePath = filePath
	}
	if fileMaxSize > 0 {
		cfg.FileMaxSizeMB = fileMaxSize
	}
	if fileMaxFiles > 0 {
		cfg.FileMaxFiles = fileMaxFiles
	}
	if fileMaxAge > 0 {
		cfg.FileMaxAgeDays = fileMaxAge
	}

	// Probe the log file path before handing it to the log manager. Containers
	// have /config/logs pre-created by the entrypoint, but native installs
	// (dev, Homebrew, bare systemd) often do not. Attempt to create the parent
	// directory and open the file for append. If either fails, drop the file
	// handler entirely and log a single WARN so the user sees the path that
	// was rejected without spamming stderr with per-log failures.
	if cfg.FilePath != "" {
		if err := logFilePathWritable(cfg.FilePath); err != nil {
			logger.Warn("log file path unwritable; using stdout only",
				slog.String("path", cfg.FilePath),
				slog.Any("error", err))
			cfg.FilePath = ""
		}
	}

	mgr.Reconfigure(cfg)
	logger.Info("applied DB logging overrides", "config", cfg.String())
}

// logFilePathWritable returns nil if path can be created and appended to, or
// the underlying error so callers can surface the reason in a warning. It
// creates the parent directory if missing and opens the file in
// O_APPEND|O_CREATE mode so a successful probe does not truncate an
// existing log. The file handle is closed before returning.
func logFilePathWritable(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("creating parent directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644) //nolint:gosec // G304: operator-provided log path
	if err != nil {
		return fmt.Errorf("opening log file for append: %w", err)
	}
	_ = f.Close()
	return nil
}

// getDBStringSetting reads a string setting directly from the database.
func getDBStringSetting(ctx context.Context, db *sql.DB, key, fallback string) string {
	var v string
	err := db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if err != nil || v == "" {
		return fallback
	}
	return v
}

// getDBBoolSetting reads a boolean setting directly from the database.
func getDBBoolSetting(ctx context.Context, db *sql.DB, key string, fallback bool) bool {
	var v string
	err := db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if err != nil {
		return fallback
	}
	return v == "true" || v == "1"
}

// isValidPersistedBasePath mirrors the API handler's server.base_path
// validation (handlers_settings.go) so a value loaded from the settings
// table at boot is held to the same rules a fresh PUT would have to pass.
// The persisted value is composed directly into mux route patterns and may
// surface in router-side redirect contexts, so a leading "//" or "/\\"
// (CodeQL "bad redirect check") and unexpected characters must be refused
// rather than warn-and-applied. Caller is responsible for stripping the
// trailing slash and treating "" as "no override"; this function assumes
// the input has at least one character.
func isValidPersistedBasePath(s string) bool {
	if !strings.HasPrefix(s, "/") {
		return false
	}
	if len(s) >= 2 && (s[1] == '/' || s[1] == '\\') {
		return false
	}
	for _, c := range s {
		ok := (c >= 'a' && c <= 'z') ||
			(c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '/'
		if !ok {
			return false
		}
	}
	return true
}

// getDBIntSetting reads an integer setting directly from the database.
func getDBIntSetting(ctx context.Context, db *sql.DB, key string, fallback int) int {
	var v string
	err := db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if err != nil || v == "" {
		return fallback
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
		return fallback
	}
	return n
}

// buildTLSStatus condenses the runtime TLS configuration into the read-only
// shape the Settings General tab renders. Three branches:
//   - off:  no cert configured        -- plain HTTP on Server.Port.
//   - byo:  cert and key set          -- HTTPS on TLS.Port, or on Server.Port
//     in collapse mode.
//   - acme: SW_ACME_DOMAIN set        -- HTTPS via autocert (Let's Encrypt /
//     Buypass). Surfaces the domain so the operator can confirm the
//     binding without parsing logs. Config validation guarantees that
//     ACME and BYO are mutually exclusive, so the order of branches
//     here is also their order of precedence.
//
// HTTPRedirectPort is forwarded as-is; the template renders the redirect
// listener row only when it is non-zero.
func buildTLSStatus(cfg *config.Config) templates.TLSStatusData {
	if cfg.ACME.Domain != "" {
		port := cfg.Server.TLS.Port
		if port == 0 {
			port = cfg.Server.Port
		}
		return templates.TLSStatusData{
			Mode:             "acme",
			AcmeDomain:       cfg.ACME.Domain,
			HTTPSPort:        port,
			HTTPRedirectPort: cfg.Server.HTTPRedirect.Port,
			HTTP3Port:        server.EffectiveHTTP3Port(cfg),
		}
	}
	if cfg.Server.TLS.Enabled() {
		port := cfg.Server.TLS.Port
		if port == 0 {
			port = cfg.Server.Port
		}
		return templates.TLSStatusData{
			Mode:             "byo",
			HTTPSPort:        port,
			HTTPRedirectPort: cfg.Server.HTTPRedirect.Port,
			HTTP3Port:        server.EffectiveHTTP3Port(cfg),
		}
	}
	return templates.TLSStatusData{
		Mode:             "off",
		HTTPPort:         cfg.Server.Port,
		HTTPRedirectPort: cfg.Server.HTTPRedirect.Port,
		// HTTP/3 requires TLS; off-mode never advertises a UDP listener.
	}
}
