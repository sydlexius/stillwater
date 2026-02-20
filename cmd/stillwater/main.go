package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sydlexius/stillwater/internal/api"
	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/auth"
	"github.com/sydlexius/stillwater/internal/config"
	"github.com/sydlexius/stillwater/internal/database"
	"github.com/sydlexius/stillwater/internal/encryption"
	"github.com/sydlexius/stillwater/internal/platform"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/internal/provider/audiodb"
	"github.com/sydlexius/stillwater/internal/provider/discogs"
	"github.com/sydlexius/stillwater/internal/provider/fanarttv"
	"github.com/sydlexius/stillwater/internal/provider/lastfm"
	"github.com/sydlexius/stillwater/internal/provider/musicbrainz"
	"github.com/sydlexius/stillwater/internal/provider/wikidata"
	"github.com/sydlexius/stillwater/internal/rule"
	"github.com/sydlexius/stillwater/internal/scanner"
	"github.com/sydlexius/stillwater/internal/version"
)

func main() {
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

	// Set up structured logging
	var logLevel slog.Level
	switch cfg.Logging.Level {
	case "debug":
		logLevel = slog.LevelDebug
	case "warn":
		logLevel = slog.LevelWarn
	case "error":
		logLevel = slog.LevelError
	default:
		logLevel = slog.LevelInfo
	}

	var handler slog.Handler
	if cfg.Logging.Format == "text" {
		handler = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel})
	} else {
		handler = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel})
	}
	logger := slog.New(handler)
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

	// Initialize encryptor
	encryptor, encKey, err := encryption.NewEncryptor(cfg.Encryption.Key)
	if err != nil {
		return fmt.Errorf("creating encryptor: %w", err)
	}
	if cfg.Encryption.Key == "" {
		logger.Warn("no encryption key configured, generated a new one -- set SW_ENCRYPTION_KEY to persist",
			slog.String("key", encKey))
	}

	// Initialize services
	authService := auth.NewService(db)
	artistService := artist.NewService(db)
	platformService := platform.NewService(db)

	// Initialize rule engine
	ruleService := rule.NewService(db)
	if err := ruleService.SeedDefaults(context.Background()); err != nil {
		return fmt.Errorf("seeding default rules: %w", err)
	}
	ruleEngine := rule.NewEngine(ruleService, logger)

	// Initialize scanner (depends on rule engine for health scoring)
	scannerService := scanner.NewService(artistService, ruleEngine, ruleService, logger, cfg.Music.LibraryPath, cfg.Scanner.Exclusions)

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

	orchestrator := provider.NewOrchestrator(providerRegistry, providerSettings, logger)

	// Initialize fix pipeline (depends on orchestrator)
	fixers := []rule.Fixer{
		&rule.NFOFixer{},
		rule.NewMetadataFixer(orchestrator),
		rule.NewImageFixer(orchestrator, logger),
	}
	pipeline := rule.NewPipeline(ruleEngine, artistService, fixers, logger)

	// Initialize bulk operations
	bulkService := rule.NewBulkService(db)
	bulkExecutor := rule.NewBulkExecutor(bulkService, artistService, orchestrator, pipeline, logger)

	logger.Info("starting stillwater",
		slog.String("version", version.Version),
		slog.String("commit", version.Commit),
	)

	// Set up HTTP router
	router := api.NewRouter(authService, artistService, scannerService, platformService, providerSettings, providerRegistry, orchestrator, ruleService, ruleEngine, pipeline, bulkService, bulkExecutor, logger, cfg.Server.BasePath, "web/static")

	// Create HTTP server
	addr := fmt.Sprintf(":%d", cfg.Server.Port)
	srv := &http.Server{
		Addr:         addr,
		Handler:      router.Handler(),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Graceful shutdown
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

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
