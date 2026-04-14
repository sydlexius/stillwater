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
	"net/http"
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
	"github.com/sydlexius/stillwater/internal/i18n"
	"github.com/sydlexius/stillwater/internal/imagebridge"
	"github.com/sydlexius/stillwater/internal/library"
	"github.com/sydlexius/stillwater/internal/logging"
	"github.com/sydlexius/stillwater/internal/maintenance"
	"github.com/sydlexius/stillwater/internal/nfo"
	"github.com/sydlexius/stillwater/internal/platform"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/internal/provider/allmusic"
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
	"github.com/sydlexius/stillwater/internal/settingsio"
	"github.com/sydlexius/stillwater/internal/version"
	"github.com/sydlexius/stillwater/internal/watcher"
	"github.com/sydlexius/stillwater/internal/webhook"
	"github.com/sydlexius/stillwater/web/static"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"
	"golang.org/x/time/rate"
)

func main() {
	// Handle subcommands before starting the server
	if len(os.Args) > 1 {
		switch os.Args[1] {
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

func run() error {
	// Load configuration
	configPath := os.Getenv("SW_CONFIG_PATH")
	if configPath == "" {
		configPath = "/config/config.yaml"
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	// Set up structured logging via the logging Manager
	logCfg := logging.Config{
		Level:  cfg.Logging.Level,
		Format: cfg.Logging.Format,
	}
	logManager, logger := logging.NewManager(logCfg)
	defer logManager.Close() //nolint:errcheck
	slog.SetDefault(logger)

	// Open database
	db, err := database.Open(cfg.Database.Path)
	if err != nil {
		return fmt.Errorf("opening database: %w", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			logger.Error("closing database", "error", err)
		}
	}()

	// Run migrations
	if err := database.Migrate(db); err != nil {
		return fmt.Errorf("running migrations: %w", err)
	}

	logger.Info("database ready", slog.String("path", cfg.Database.Path))

	// Reload logging settings from DB (overrides config file values if present)
	loadDBLoggingConfig(db, logManager, logger)

	// Initialize library service and backfill default library
	libraryService := library.NewService(db)
	defaultLibID := backfillDefaultLibrary(context.Background(), libraryService, cfg.Music.LibraryPath, db, logger)

	// Resolve encryption key: env var > file > generate new
	encKey, err := resolveEncryptionKey(cfg, logger)
	if err != nil {
		return fmt.Errorf("resolving encryption key: %w", err)
	}
	encryptor, _, err := encryption.NewEncryptor(encKey)
	if err != nil {
		return fmt.Errorf("creating encryptor: %w", err)
	}

	// Initialize services
	authService := auth.NewService(db)

	// Build the auth provider registry from the stored auth.method setting.
	// The local provider is always registered. Federated providers are added
	// when their corresponding auth.method is configured and a server URL is set.
	authRegistry := auth.NewRegistry()
	authRegistry.Register(auth.NewLocalProvider(db))
	{
		authMethod := getDBStringSetting(db, context.Background(), "auth.method", "local")
		authServerURL := getDBStringSetting(db, context.Background(), "auth.server_url", "")
		if authServerURL != "" {
			switch authMethod {
			case "emby":
				if p, err := auth.NewEmbyProvider(authServerURL, false, "admin", "operator"); err == nil {
					authRegistry.Register(p)
				} else {
					logger.Warn("failed to create emby auth provider", "error", err)
				}
			case "jellyfin":
				if p, err := auth.NewJellyfinProvider(authServerURL, false, "admin", "operator"); err == nil {
					authRegistry.Register(p)
				} else {
					logger.Warn("failed to create jellyfin auth provider", "error", err)
				}
			}
		}
	}

	artistService := artist.NewService(db)
	historyService := artist.NewHistoryService(db)
	artistService.SetHistoryService(historyService)
	platformService := platform.NewService(db)
	connectionService := connection.NewService(db, encryptor)

	// Initialize rule engine
	ruleService := rule.NewService(db)
	if err := ruleService.SeedDefaults(context.Background()); err != nil {
		return fmt.Errorf("seeding default rules: %w", err)
	}
	ruleEngine := rule.NewEngine(ruleService, db, platformService, libraryService, logger)
	ruleEngine.SetFSCache(rule.NewFSCache(0, 0, logger))

	// Wire platform image bridge so the logo_padding rule can check and fix
	// images for API-only artists that have no local filesystem path.
	imageBridge := imagebridge.New(connectionService, artistService, logger)
	ruleEngine.SetImageFetcher(imageBridge)

	// Initialize scanner (depends on rule engine for health scoring)
	scannerService := scanner.NewService(artistService, ruleEngine, ruleService, logger, cfg.Music.LibraryPath, cfg.Scanner.Exclusions)
	scannerService.SetDefaultLibraryID(defaultLibID)
	scannerService.SetLibraryLister(libraryService)

	// Initialize provider infrastructure
	rateLimiters := provider.NewRateLimiterMap()
	providerSettings := provider.NewSettingsService(db, encryptor)
	providerRegistry := provider.NewRegistry()

	mb := musicbrainz.New(rateLimiters, logger)
	baseURL, err := providerSettings.GetBaseURL(context.Background(), provider.NameMusicBrainz)
	if err != nil {
		logger.Warn("failed to load MusicBrainz mirror URL from database", "error", err)
	} else if baseURL != "" {
		mb.SetBaseURL(baseURL)
		logger.Info("loaded MusicBrainz mirror URL", slog.String("base_url", baseURL))
	}

	limit, err := providerSettings.GetRateLimit(context.Background(), provider.NameMusicBrainz)
	if err != nil {
		logger.Warn("failed to load MusicBrainz rate limit from database", "error", err)
	} else if limit > 0 {
		rateLimiters.SetLimit(provider.NameMusicBrainz, rate.Limit(limit))
		logger.Info("loaded MusicBrainz custom rate limit", slog.Float64("req_per_sec", limit))
	}
	providerRegistry.Register(mb)
	providerRegistry.Register(fanarttv.New(rateLimiters, providerSettings, logger))
	providerRegistry.Register(audiodb.New(rateLimiters, providerSettings, logger))
	providerRegistry.Register(discogs.New(rateLimiters, providerSettings, logger))
	providerRegistry.Register(lastfm.New(rateLimiters, providerSettings, logger))
	providerRegistry.Register(wikidata.New(rateLimiters, logger))
	providerRegistry.Register(deezer.New(rateLimiters, logger))
	providerRegistry.Register(wikipedia.New(rateLimiters, logger))
	providerRegistry.Register(genius.New(rateLimiters, providerSettings, logger))
	providerRegistry.Register(spotify.New(rateLimiters, providerSettings, logger))

	webSearchRegistry := provider.NewWebSearchRegistry()
	webSearchRegistry.Register(duckduckgo.New(rateLimiters, logger))

	webScraperRegistry := provider.NewWebScraperRegistry()
	webScraperRegistry.Register(allmusic.New(rateLimiters, logger))

	orchestrator := provider.NewOrchestrator(providerRegistry, providerSettings, logger)

	// Initialize scraper configuration and executor
	scraperService := scraper.NewService(db, logger)
	if err := scraperService.SeedDefaults(context.Background()); err != nil {
		return fmt.Errorf("seeding default scraper config: %w", err)
	}
	scraperExecutor := scraper.NewExecutor(scraperService, providerRegistry, providerSettings, logger)
	orchestrator.SetExecutor(scraperExecutor)

	// Initialize NFO snapshot and settings services
	nfoSnapshotService := nfo.NewSnapshotService(db)
	nfoSettingsService := nfo.NewNFOSettingsService(db, logger)

	// Initialize shared-filesystem check (used by fixers to skip writes on
	// libraries whose directories are also managed by a platform connection).
	fsCheck := rule.NewSharedFSCheck(libraryService, logger)

	// Create expected-writes tracker. The HTTP router and rule fixers register
	// paths they are about to write. The watcher service maintains this set
	// (pruning stale entries). External-write detection logic will consume
	// it when that filtering is enabled. Must be created before consumers.
	expectedWrites := watcher.NewExpectedWrites()

	// Create the publisher before the pipeline so it can be injected.
	publisher := publish.New(publish.Deps{
		ArtistService:      artistService,
		ConnectionService:  connectionService,
		NFOSnapshotService: nfoSnapshotService,
		NFOSettingsService: nfoSettingsService,
		PlatformService:    platformService,
		ExpectedWrites:     expectedWrites,
		ImageCacheDir:      filepath.Join(filepath.Dir(cfg.Database.Path), "cache", "images"),
		Logger:             logger,
	})

	// Initialize fix pipeline (depends on orchestrator and snapshot service)
	logoPaddingFixer := rule.NewLogoPaddingFixer(platformService, fsCheck, logger)
	logoPaddingFixer.SetImageFetcher(imageBridge, ruleEngine.ConsumeAPIImage)
	fixers := []rule.Fixer{
		rule.NewNFOFixer(nfoSnapshotService, nfoSettingsService, fsCheck, expectedWrites),
		rule.NewMetadataFixer(orchestrator, logger),
		rule.NewImageFixer(orchestrator, platformService, fsCheck, logger),
		rule.NewExtraneousImagesFixer(platformService, fsCheck, logger),
		logoPaddingFixer,
		rule.NewDirectoryRenameFixer(fsCheck, logger),
		rule.NewBackdropSequencingFixer(platformService, fsCheck, logger),
	}
	pipeline := rule.NewPipeline(ruleEngine, artistService, ruleService, fixers, publisher, logger)

	// Initialize bulk operations
	bulkService := rule.NewBulkService(db)
	bulkExecutor := rule.NewBulkExecutor(bulkService, artistService, orchestrator, pipeline, nfoSnapshotService, platformService, expectedWrites, publisher, logger)

	// Initialize event bus
	eventBus := event.NewBus(logger, 256)
	go eventBus.Start()
	defer eventBus.Stop()

	// Initialize webhook service and dispatcher
	webhookService := webhook.NewService(db)
	webhookDispatcher := webhook.NewDispatcher(webhookService, logger)

	// Initialize backup service
	backupDir := cfg.Backup.Path
	if backupDir == "" {
		backupDir = filepath.Join(filepath.Dir(cfg.Database.Path), "backups")
	}
	backupService := backup.NewService(db, backupDir, cfg.Backup.RetentionCount, logger)
	if dbRetention := getDBIntSetting(db, "backup_retention_count", 0); dbRetention > 0 {
		backupService.SetRetention(dbRetention)
	}
	if dbMaxAge := getDBIntSetting(db, "backup_max_age_days", -1); dbMaxAge >= 0 {
		backupService.SetMaxAgeDays(dbMaxAge)
	}

	// Initialize maintenance service
	maintenanceService := maintenance.NewService(db, cfg.Database.Path, logger)

	// Initialize settings export/import service
	settingsIOService := settingsio.NewService(db, providerSettings, connectionService, platformService, webhookService)

	// Subscribe dispatcher to all event types
	for _, eventType := range []event.Type{
		event.ArtistNew, event.MetadataFixed, event.ReviewNeeded,
		event.RuleViolation, event.BulkCompleted, event.ScanCompleted,
		event.LidarrArtistAdd, event.LidarrDownload,
		event.EmbyArtistUpdate, event.EmbyLibraryScan,
		event.JellyfinArtistUpdate, event.JellyfinLibraryScan,
		event.FSDirCreated, event.FSDirRemoved, event.FSUnexpectedWrite,
	} {
		eventBus.Subscribe(eventType, webhookDispatcher.HandleEvent)
	}

	// Wire event bus into scanner and bulk executor
	scannerService.SetEventBus(eventBus)
	bulkExecutor.SetEventBus(eventBus)

	// Subscribe to filesystem events so the rule engine's FSCache is
	// invalidated when directories are created or removed. The "path" field
	// in the event data identifies the affected directory.
	if fsCache := ruleEngine.FSCache(); fsCache != nil {
		for _, eventType := range []event.Type{event.FSDirCreated, event.FSDirRemoved, event.FSUnexpectedWrite} {
			eventBus.Subscribe(eventType, func(ev event.Event) {
				if p, ok := ev.Data["path"].(string); ok && p != "" {
					fsCache.InvalidatePath(p)
				}
			})
		}
	}

	// Health subscriber: re-evaluates per-artist health scores on mutations.
	// Subscription is registered now; Start/Bootstrap are called after the
	// shutdown context is created (below).
	healthSub := rule.NewHealthSubscriber(ruleEngine, artistService, logger)
	eventBus.Subscribe(event.ArtistUpdated, healthSub.HandleEvent)

	logger.Info("starting stillwater",
		slog.String("version", version.Version),
		slog.String("commit", version.Commit),
	)

	// Probe filesystem notification support for each library path.
	probeCache := watcher.NewProbeCache()
	{
		probLibs, probErr := libraryService.List(context.Background())
		if probErr != nil {
			logger.Error("listing libraries for probe", "error", probErr)
		} else {
			probeCache.ProbeAll(context.Background(), probLibs, logger)
		}
	}

	// Create rule scheduler before the router so it can be passed as a dependency.
	// Start() is called later, after the router is fully initialized.
	var ruleScheduler *rule.Scheduler
	var ruleScheduleMinutes int
	{
		ruleScheduleMinutes = getDBIntSetting(db, "rule_schedule.interval_minutes", 0)
		// Migrate legacy hours setting: if minutes key is absent, read hours and
		// persist the converted value so the legacy key can be ignored going forward.
		if ruleScheduleMinutes == 0 {
			if legacyHours := getDBIntSetting(db, "rule_schedule.interval_hours", 0); legacyHours > 0 {
				ruleScheduleMinutes = legacyHours * 60
				// Persist migrated value and remove legacy key.
				_, _ = db.ExecContext(context.Background(),
					`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
					 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
					"rule_schedule.interval_minutes", fmt.Sprintf("%d", ruleScheduleMinutes),
					time.Now().UTC().Format(time.RFC3339))
				_, _ = db.ExecContext(context.Background(),
					`DELETE FROM settings WHERE key = ?`, "rule_schedule.interval_hours")
				logger.Info("migrated rule schedule from hours to minutes",
					"minutes", ruleScheduleMinutes)
			}
		}
		if ruleScheduleMinutes >= 5 {
			ruleScheduler = rule.NewScheduler(pipeline, ruleService, artistService, logger)
		} else if ruleScheduleMinutes > 0 && ruleScheduleMinutes < 5 {
			logger.Warn("rule scheduler interval too short (minimum 5 minutes); scheduler not started",
				"minutes", ruleScheduleMinutes)
		}
	}

	// Load i18n translation bundle (embedded at compile time).
	i18nBundle, err := i18n.LoadEmbedded()
	if err != nil {
		return fmt.Errorf("loading i18n bundle: %w", err)
	}

	// Set up HTTP router
	router := api.NewRouter(api.RouterDeps{
		AuthService:        authService,
		AuthRegistry:       authRegistry,
		ArtistService:      artistService,
		HistoryService:     historyService,
		ScannerService:     scannerService,
		PlatformService:    platformService,
		ProviderSettings:   providerSettings,
		ProviderRegistry:   providerRegistry,
		WebSearchRegistry:  webSearchRegistry,
		WebScraperRegistry: webScraperRegistry,
		RateLimiters:       rateLimiters,
		Orchestrator:       orchestrator,
		RuleService:        ruleService,
		RuleEngine:         ruleEngine,
		Pipeline:           pipeline,
		BulkService:        bulkService,
		BulkExecutor:       bulkExecutor,
		NFOSnapshotService: nfoSnapshotService,
		NFOSettingsService: nfoSettingsService,
		ConnectionService:  connectionService,
		ScraperService:     scraperService,
		LibraryService:     libraryService,
		WebhookService:     webhookService,
		WebhookDispatcher:  webhookDispatcher,
		BackupService:      backupService,
		LogManager:         logManager,
		MaintenanceService: maintenanceService,
		SettingsIOService:  settingsIOService,
		ProbeCache:         probeCache,
		ExpectedWrites:     expectedWrites,
		EventBus:           eventBus,
		DB:                 db,
		Logger:             logger,
		BasePath:           cfg.Server.BasePath,
		BasePathFromEnv:    cfg.Server.BasePathFromEnv,
		StaticFS:           static.FS,
		ImageCacheDir:      filepath.Join(filepath.Dir(cfg.Database.Path), "cache", "images"),
		Publisher:          publisher,
		RuleScheduler:      ruleScheduler,
		I18nBundle:         i18nBundle,
	})

	// Graceful shutdown
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Start health subscriber background goroutine
	go healthSub.Start(ctx)
	defer healthSub.Stop()

	// Bootstrap stale health scores (zero-score artists) in the background
	// after a short delay to avoid competing with startup I/O.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Error("health bootstrap panicked", "panic", r)
			}
		}()
		select {
		case <-time.After(5 * time.Second):
			healthSub.Bootstrap(ctx)
		case <-ctx.Done():
		}
	}()

	// Create HTTP server
	addr := fmt.Sprintf(":%d", cfg.Server.Port)
	srv := &http.Server{
		Addr:         addr,
		Handler:      router.Handler(ctx),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Start backup scheduler
	if cfg.Backup.Enabled {
		go backupService.StartScheduler(ctx, time.Duration(cfg.Backup.IntervalHours)*time.Hour)
	}

	// Start maintenance scheduler (reads interval from DB settings, defaults to daily)
	{
		maintEnabled := getDBBoolSetting(db, "db_maintenance.enabled", true)
		maintHours := getDBIntSetting(db, "db_maintenance.interval_hours", 24)
		if maintHours <= 0 {
			maintHours = 24
		}
		if maintEnabled {
			go maintenanceService.StartScheduler(ctx, time.Duration(maintHours)*time.Hour)
		}
	}

	// Start session cleanup goroutine
	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := authService.CleanExpiredSessions(ctx); err != nil {
					logger.Error("session cleanup failed", "error", err)
				}
			}
		}
	}()

	// Start rule evaluation scheduler (created earlier, before the router)
	if ruleScheduler != nil {
		go ruleScheduler.Start(ctx, time.Duration(ruleScheduleMinutes)*time.Minute)
	}

	// Start filesystem watcher for libraries with fs_watch enabled
	{
		scanFn := func(ctx context.Context) error {
			_, err := scannerService.Run(ctx)
			return err
		}
		watcherService := watcher.NewService(scanFn, libraryService, eventBus, logger, probeCache, expectedWrites)
		go watcherService.Start(ctx)
	}

	go func() {
		logger.Info("server starting", slog.String("addr", addr), slog.String("base_path", cfg.Server.BasePath))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")

	// Shut down the HTTP server first to stop accepting new requests and
	// drain in-flight ones. This prevents new scan requests from racing
	// with the scanner's WaitGroup during shutdown.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	srvErr := srv.Shutdown(shutdownCtx)

	// Now stop the scanner -- no new Run() calls can arrive.
	scannerService.Shutdown()

	return srvErr
}

// resolveEncryptionKey determines the encryption key to use.
// Priority: SW_ENCRYPTION_KEY env var > encryption.key alongside DB > generate new.
func resolveEncryptionKey(cfg *config.Config, logger *slog.Logger) (string, error) {
	if cfg.Encryption.Key != "" {
		return cfg.Encryption.Key, nil
	}

	dataDir := filepath.Dir(cfg.Database.Path)
	keyFile := filepath.Join(dataDir, "encryption.key")

	// Try loading from file
	data, err := os.ReadFile(keyFile) //nolint:gosec // G304: path derived from trusted config
	if err == nil {
		key := strings.TrimSpace(string(data))
		if key != "" {
			logger.Debug("loaded encryption key from file", slog.String("path", keyFile))
			return key, nil
		}
	}

	// Generate a new key
	_, key, err := encryption.NewEncryptor("")
	if err != nil {
		return "", fmt.Errorf("generating encryption key: %w", err)
	}

	// Persist to file
	if err := os.MkdirAll(dataDir, 0o750); err != nil { //nolint:gosec // G304: dataDir derived from trusted config, not user input
		logger.Warn("could not create data directory for encryption key",
			slog.String("path", dataDir), slog.Any("error", err))
		return key, nil
	}

	if err := os.WriteFile(keyFile, []byte(key+"\n"), 0o600); err != nil { //nolint:gosec // G304: keyFile derived from trusted config, not user input
		logger.Warn("could not save encryption key to file",
			slog.String("path", keyFile), slog.Any("error", err))
	} else {
		logger.Warn("generated new encryption key -- back up this file",
			slog.String("path", keyFile))
	}

	return key, nil
}

// resetCredentials wipes all stored credentials from the database.
// This is an offline operation intended for recovery when the encryption key
// is lost or credentials need to be re-entered.
func resetCredentials() error {
	configPath := os.Getenv("SW_CONFIG_PATH")
	if configPath == "" {
		configPath = "/config/config.yaml"
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	db, err := database.Open(cfg.Database.Path)
	if err != nil {
		return fmt.Errorf("opening database: %w", err)
	}
	defer db.Close() //nolint:errcheck

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
		configPath = "/config/config.yaml"
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	db, err := database.Open(cfg.Database.Path)
	if err != nil {
		return fmt.Errorf("opening database: %w", err)
	}
	defer db.Close() //nolint:errcheck

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
	fd := int(os.Stdin.Fd()) //nolint:gosec // G115: safe for file descriptors

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
	fd := int(os.Stdin.Fd()) //nolint:gosec // G115: uintptr to int is safe for file descriptors

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

// backfillDefaultLibrary ensures at least one library exists and all artists
// have a library_id set. Returns the default library ID for the scanner.
func backfillDefaultLibrary(ctx context.Context, libService *library.Service, musicPath string, db *sql.DB, logger *slog.Logger) string {
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
		for _, lib := range libs {
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

	// Assign orphaned artists (library_id IS NULL) to the default library
	count := assignOrphanedArtists(ctx, db, defaultID, logger)
	if count > 0 {
		logger.Info("assigned orphaned artists to library",
			slog.String("library_id", defaultID),
			slog.Int64("count", count))
	}

	return defaultID
}

// assignOrphanedArtists sets library_id on all artists where it is currently NULL.
func assignOrphanedArtists(ctx context.Context, db *sql.DB, libraryID string, logger *slog.Logger) int64 {
	result, err := db.ExecContext(ctx,
		`UPDATE artists SET library_id = ? WHERE library_id IS NULL`, libraryID)
	if err != nil {
		logger.Error("assigning orphaned artists", "error", err)
		return 0
	}
	n, _ := result.RowsAffected()
	return n
}

// loadDBLoggingConfig reads logging settings from the DB and reconfigures the
// log manager if any are present. Called once after migrations.
func loadDBLoggingConfig(db *sql.DB, mgr *logging.Manager, logger *slog.Logger) {
	ctx := context.Background()
	level := getDBStringSetting(db, ctx, "logging.level", "")
	format := getDBStringSetting(db, ctx, "logging.format", "")
	if level == "" && format == "" {
		return // no DB overrides
	}

	cfg := mgr.Config()
	if level != "" && logging.ValidLevel(level) {
		cfg.Level = level
	}
	if format != "" && logging.ValidFormat(format) {
		cfg.Format = format
	}
	cfg.FilePath = getDBStringSetting(db, ctx, "logging.file_path", cfg.FilePath)
	if v := getDBIntSetting(db, "logging.file_max_size_mb", 0); v > 0 {
		cfg.FileMaxSizeMB = v
	}
	if v := getDBIntSetting(db, "logging.file_max_files", 0); v > 0 {
		cfg.FileMaxFiles = v
	}
	if v := getDBIntSetting(db, "logging.file_max_age_days", 0); v > 0 {
		cfg.FileMaxAgeDays = v
	}

	// Probe the log file path before handing it to the log manager. Containers
	// have /config/logs pre-created by the entrypoint, but native installs
	// (dev, Homebrew, bare systemd) often do not. Attempt to create the parent
	// directory and open the file for append. If either fails, drop the file
	// handler entirely and log a single WARN so the user sees the path that
	// was rejected without spamming stderr with per-log failures.
	if cfg.FilePath != "" && !logFilePathWritable(cfg.FilePath) {
		logger.Warn("log file path unwritable; using stdout only",
			slog.String("path", cfg.FilePath))
		cfg.FilePath = ""
	}

	mgr.Reconfigure(cfg)
	logger.Info("applied DB logging overrides", "config", cfg.String())
}

// logFilePathWritable reports whether path can be created and appended to.
// It creates the parent directory if missing and opens the file in
// O_APPEND|O_CREATE mode so a successful probe does not truncate an
// existing log. The file handle is closed before returning.
func logFilePathWritable(path string) bool {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return false
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644) //nolint:gosec // G304: operator-provided log path
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// getDBStringSetting reads a string setting directly from the database.
func getDBStringSetting(db *sql.DB, ctx context.Context, key, fallback string) string {
	var v string
	err := db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if err != nil || v == "" {
		return fallback
	}
	return v
}

// getDBBoolSetting reads a boolean setting directly from the database.
func getDBBoolSetting(db *sql.DB, key string, fallback bool) bool {
	var v string
	err := db.QueryRowContext(context.Background(), `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if err != nil {
		return fallback
	}
	return v == "true" || v == "1"
}

// getDBIntSetting reads an integer setting directly from the database.
func getDBIntSetting(db *sql.DB, key string, fallback int) int {
	var v string
	err := db.QueryRowContext(context.Background(), `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if err != nil || v == "" {
		return fallback
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
		return fallback
	}
	return n
}
