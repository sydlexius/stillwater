package maintenance

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"time"
)

// Status holds database maintenance status information.
type Status struct {
	DBFileSize       int64  `json:"db_file_size"`
	WALFileSize      int64  `json:"wal_file_size"`
	PageCount        int64  `json:"page_count"`
	PageSize         int64  `json:"page_size"`
	LastOptimizeAt   string `json:"last_optimize_at,omitempty"`
	ScheduleEnabled  bool   `json:"schedule_enabled"`
	ScheduleInterval int    `json:"schedule_interval_hours"`
}

// ScheduleConfig holds the maintenance schedule settings.
type ScheduleConfig struct {
	Enabled       bool `json:"enabled"`
	IntervalHours int  `json:"interval_hours"`
}

// Service provides database maintenance operations.
type Service struct {
	db     *sql.DB
	dbPath string
	logger *slog.Logger
}

// NewService creates a maintenance service.
func NewService(db *sql.DB, dbPath string, logger *slog.Logger) *Service {
	return &Service{
		db:     db,
		dbPath: dbPath,
		logger: logger.With(slog.String("component", "maintenance")),
	}
}

// Status returns current database maintenance status.
func (s *Service) Status(ctx context.Context) (*Status, error) {
	st := &Status{}

	// DB file size
	if info, err := os.Stat(s.dbPath); err == nil {
		st.DBFileSize = info.Size()
	}

	// WAL file size
	if info, err := os.Stat(s.dbPath + "-wal"); err == nil {
		st.WALFileSize = info.Size()
	}

	// Page count and size
	if err := s.db.QueryRowContext(ctx, "PRAGMA page_count").Scan(&st.PageCount); err != nil {
		s.logger.Warn("reading page_count", "error", err)
	}
	if err := s.db.QueryRowContext(ctx, "PRAGMA page_size").Scan(&st.PageSize); err != nil {
		s.logger.Warn("reading page_size", "error", err)
	}

	// Last optimize time from settings
	var lastOpt string
	err := s.db.QueryRowContext(ctx,
		`SELECT value FROM settings WHERE key = 'db_maintenance.last_optimize_at'`).Scan(&lastOpt)
	if err == nil {
		st.LastOptimizeAt = lastOpt
	}

	// Schedule config
	st.ScheduleEnabled = s.getBoolSetting(ctx, "db_maintenance.enabled", true)
	st.ScheduleInterval = s.getIntSetting(ctx, "db_maintenance.interval_hours", 24)

	return st, nil
}

// Optimize runs PRAGMA optimize followed by a WAL checkpoint.
func (s *Service) Optimize(ctx context.Context) error {
	s.logger.Info("running PRAGMA optimize")
	if _, err := s.db.ExecContext(ctx, "PRAGMA optimize"); err != nil {
		return fmt.Errorf("PRAGMA optimize: %w", err)
	}

	s.logger.Info("running WAL checkpoint")
	if _, err := s.db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return fmt.Errorf("WAL checkpoint: %w", err)
	}

	// Record the timestamp
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO settings (key, value, updated_at) VALUES ('db_maintenance.last_optimize_at', ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		now, now)
	if err != nil {
		s.logger.Warn("recording optimize timestamp", "error", err)
	}

	s.logger.Info("optimize complete")
	return nil
}

// Vacuum runs VACUUM to rebuild the database file.
func (s *Service) Vacuum(ctx context.Context) error {
	s.logger.Info("running VACUUM")
	if _, err := s.db.ExecContext(ctx, "VACUUM"); err != nil {
		return fmt.Errorf("VACUUM: %w", err)
	}
	s.logger.Info("vacuum complete")
	return nil
}

// StartScheduler runs optimize on a fixed interval until the context is canceled.
func (s *Service) StartScheduler(ctx context.Context, interval time.Duration) {
	s.logger.Info("maintenance scheduler started",
		slog.String("interval", interval.String()))

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			s.logger.Info("maintenance scheduler stopped")
			return
		case <-ticker.C:
			if err := s.Optimize(ctx); err != nil {
				s.logger.Error("scheduled optimize failed", slog.Any("error", err))
			}
		}
	}
}

// getBoolSetting reads a boolean setting from the key-value table.
func (s *Service) getBoolSetting(ctx context.Context, key string, fallback bool) bool {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if err != nil {
		return fallback
	}
	return v == "true" || v == "1"
}

// getIntSetting reads an integer setting from the key-value table.
func (s *Service) getIntSetting(ctx context.Context, key string, fallback int) int {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if err != nil || v == "" {
		return fallback
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
		return fallback
	}
	return n
}
