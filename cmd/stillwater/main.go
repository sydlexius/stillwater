package main

import (
	"context"
	"database/sql"
	"fmt"
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
	"github.com/sydlexius/stillwater/internal/provider/lastfm"
	"github.com/sydlexius/stillwater/internal/provider/musicbrainz"
	"github.com/sydlexius/stillwater/internal/provider/wikidata"
	"github.com/sydlexius/stillwater/internal/rule"
	"github.com/sydlexius/stillwater/internal/scanner"
	"github.com/sydlexius/stillwater/internal/scraper"
	"github.com/sydlexius/stillwater/internal/settingsio"
	"github.com/sydlexius/stillwater/internal/version"
	"github.com/sydlexius/stillwater/internal/watcher"
	"github.com/sydlexius/stillwater/internal/webhook"
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

	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// Load configuration
	configPath := os.Getenv("SW_CONFIG_PATH")
	if configPath == "" {
		configPath = "/data/config.yaml"
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
	artistService := artist.NewService(db)
	platformService := platform.NewService(db)
	connectionService := connection.NewService(db, encryptor)

	// Initialize rule engine
	ruleService := rule.NewService(db)
	if err := ruleService.SeedDefaults(context.Background()); err != nil {
		return fmt.Errorf("seeding default rules: %w", err)
	}
	ruleEngine := rule.NewEngine(ruleService, db, platformService, logger)

	// Initialize scanner (depends on rule engine for health scoring)
	scannerService := scanner.NewService(artistService, ruleEngine, ruleService, logger, cfg.Music.LibraryPath, cfg.Scanner.Exclusions)
	scannerService.SetDefaultLibraryID(defaultLibID)
	scannerService.SetLibraryLister(libraryService)

	// Initialize provider infrastructure
	rateLimiters := provider.NewRateLimiterMap()
	providerSettings := provider.NewSettingsService(db, encryptor)
	providerRegistry := provider.NewRegistry()

	providerRegistry.Register(musicbrainz.New(rateLimiters, logger))
	providerRegistry.Register(fanarttv.New(rateLimiters, providerSettings, logger))
	providerRegistry.Register(audiodb.New(rateLimiters, providerSettings, logger))
	providerRegistry.Register(discogs.New(rateLimiters, providerSettings, logger))
	providerRegistry.Register(lastfm.New(rateLimiters, providerSettings, logger))
	providerRegistry.Register(wikidata.New(rateLimiters, logger))
	providerRegistry.Register(deezer.New(rateLimiters, logger))

	webSearchRegistry := provider.NewWebSearchRegistry()
	webSearchRegistry.Register(duckduckgo.New(rateLimiters, logger))

	orchestrator := provider.NewOrchestrator(providerRegistry, providerSettings, logger)

	// Initialize scraper configuration and executor
	scraperService := scraper.NewService(db, logger)
	if err := scraperService.SeedDefaults(context.Background()); err != nil {
		return fmt.Errorf("seeding default scraper config: %w", err)
	}
	scraperExecutor := scraper.NewExecutor(scraperService, providerRegistry, providerSettings, logger)
	orchestrator.SetExecutor(scraperExecutor)

	// Initialize NFO snapshot service
	nfoSnapshotService := nfo.NewSnapshotService(db)

	// Initialize fix pipeline (depends on orchestrator and snapshot service)
	fixers := []rule.Fixer{
		&rule.NFOFixer{SnapshotService: nfoSnapshotService},
		rule.NewMetadataFixer(orchestrator, nfoSnapshotService),
		rule.NewImageFixer(orchestrator, platformService, logger),
		rule.NewExtraneousImagesFixer(platformService, logger),
	}
	pipeline := rule.NewPipeline(ruleEngine, artistService, ruleService, fixers, logger)

	// Initialize bulk operations
	bulkService := rule.NewBulkService(db)
	bulkExecutor := rule.NewBulkExecutor(bulkService, artistService, orchestrator, pipeline, nfoSnapshotService, logger)

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

	// Initialize maintenance service
	maintenanceService := maintenance.NewService(db, cfg.Database.Path, logger)

	// Initialize settings export/import service
	settingsIOService := settingsio.NewService(db, providerSettings, connectionService, platformService, webhookService)

	// Subscribe dispatcher to all event types
	for _, eventType := range []event.Type{
		event.ArtistNew, event.MetadataFixed, event.ReviewNeeded,
		event.RuleViolation, event.BulkCompleted, event.ScanCompleted,
		event.LidarrArtistAdd, event.LidarrDownload,
		event.FSDirCreated, event.FSDirRemoved,
	} {
		eventBus.Subscribe(eventType, webhookDispatcher.HandleEvent)
	}

	// Wire event bus into scanner and bulk executor
	scannerService.SetEventBus(eventBus)
	bulkExecutor.SetEventBus(eventBus)

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

	// Set up HTTP router
	router := api.NewRouter(api.RouterDeps{
		AuthService:        authService,
		ArtistService:      artistService,
		ScannerService:     scannerService,
		PlatformService:    platformService,
		ProviderSettings:   providerSettings,
		ProviderRegistry:   providerRegistry,
		WebSearchRegistry:  webSearchRegistry,
		Orchestrator:       orchestrator,
		RuleService:        ruleService,
		RuleEngine:         ruleEngine,
		Pipeline:           pipeline,
		BulkService:        bulkService,
		BulkExecutor:       bulkExecutor,
		NFOSnapshotService: nfoSnapshotService,
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
		EventBus:           eventBus,
		DB:                 db,
		Logger:             logger,
		BasePath:           cfg.Server.BasePath,
		StaticDir:          "web/static",
	})

	// Graceful shutdown
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

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

	// Start rule evaluation scheduler (opt-in via settings)
	{
		ruleScheduleHours := getDBIntSetting(db, "rule_schedule.interval_hours", 0)
		switch ruleScheduleHours {
		case 0:
			// scheduler disabled
		case 6, 12, 24, 48:
			ruleScheduler := rule.NewScheduler(pipeline, ruleService, logger)
			go ruleScheduler.Start(ctx, time.Duration(ruleScheduleHours)*time.Hour)
		default:
			logger.Warn("invalid rule scheduler interval; scheduler not started",
				"hours", ruleScheduleHours)
		}
	}

	// Start filesystem watcher for libraries with fs_watch enabled
	{
		scanFn := func(ctx context.Context) error {
			_, err := scannerService.Run(ctx)
			return err
		}
		watcherService := watcher.NewService(scanFn, libraryService, eventBus, logger, probeCache)
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

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	return srv.Shutdown(shutdownCtx)
}

// resolveEncryptionKey determines the encryption key to use.
// Priority: SW_ENCRYPTION_KEY env var > /data/encryption.key file > generate new.
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
		configPath = "/data/config.yaml"
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

	mgr.Reconfigure(cfg)
	logger.Info("applied DB logging overrides", "config", cfg.String())
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
