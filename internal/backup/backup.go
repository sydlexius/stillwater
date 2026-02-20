package backup

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// backupPattern matches backup filenames: stillwater-YYYYMMDD-HHMMSS.db
var backupPattern = regexp.MustCompile(`^stillwater-\d{8}-\d{6}\.db$`)

// BackupInfo describes a backup file.
type BackupInfo struct {
	Filename  string    `json:"filename"`
	Size      int64     `json:"size"`
	CreatedAt time.Time `json:"created_at"`
}

// Service manages database backups.
type Service struct {
	db        *sql.DB
	backupDir string
	retention int
	logger    *slog.Logger
}

// NewService creates a backup service.
func NewService(db *sql.DB, backupDir string, retention int, logger *slog.Logger) *Service {
	return &Service{
		db:        db,
		backupDir: backupDir,
		retention: retention,
		logger:    logger.With(slog.String("component", "backup")),
	}
}

// Backup creates a snapshot of the database using VACUUM INTO.
func (s *Service) Backup(ctx context.Context) (*BackupInfo, error) {
	if err := os.MkdirAll(s.backupDir, 0o750); err != nil {
		return nil, fmt.Errorf("creating backup directory: %w", err)
	}

	now := time.Now().UTC()
	filename := fmt.Sprintf("stillwater-%s.db", now.Format("20060102-150405"))
	dest := filepath.Join(s.backupDir, filename)

	s.logger.Info("starting backup", slog.String("dest", dest))

	_, err := s.db.ExecContext(ctx, "VACUUM INTO ?", dest)
	if err != nil {
		return nil, fmt.Errorf("VACUUM INTO: %w", err)
	}

	info, err := os.Stat(dest)
	if err != nil {
		return nil, fmt.Errorf("stat backup file: %w", err)
	}

	s.logger.Info("backup complete",
		slog.String("filename", filename),
		slog.Int64("size", info.Size()))

	return &BackupInfo{
		Filename:  filename,
		Size:      info.Size(),
		CreatedAt: now,
	}, nil
}

// ListBackups returns all backup files sorted by date descending.
func (s *Service) ListBackups() ([]BackupInfo, error) {
	entries, err := os.ReadDir(s.backupDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading backup directory: %w", err)
	}

	var backups []BackupInfo
	for _, entry := range entries {
		if entry.IsDir() || !backupPattern.MatchString(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}

		// Parse timestamp from filename: stillwater-YYYYMMDD-HHMMSS.db
		name := strings.TrimSuffix(strings.TrimPrefix(entry.Name(), "stillwater-"), ".db")
		ts, err := time.Parse("20060102-150405", name)
		if err != nil {
			ts = info.ModTime()
		}

		backups = append(backups, BackupInfo{
			Filename:  entry.Name(),
			Size:      info.Size(),
			CreatedAt: ts,
		})
	}

	sort.Slice(backups, func(i, j int) bool {
		return backups[i].CreatedAt.After(backups[j].CreatedAt)
	})

	return backups, nil
}

// Prune deletes the oldest backups exceeding the retention count.
func (s *Service) Prune() error {
	backups, err := s.ListBackups()
	if err != nil {
		return err
	}

	if len(backups) <= s.retention {
		return nil
	}

	for _, b := range backups[s.retention:] {
		path := filepath.Join(s.backupDir, b.Filename)
		if err := os.Remove(path); err != nil {
			s.logger.Warn("failed to remove old backup",
				slog.String("filename", b.Filename),
				slog.Any("error", err))
			continue
		}
		s.logger.Info("pruned old backup", slog.String("filename", b.Filename))
	}

	return nil
}

// BackupDir returns the configured backup directory path.
func (s *Service) BackupDir() string {
	return s.backupDir
}

// StartScheduler runs backups on a fixed interval until the context is cancelled.
func (s *Service) StartScheduler(ctx context.Context, interval time.Duration) {
	s.logger.Info("backup scheduler started",
		slog.String("interval", interval.String()),
		slog.Int("retention", s.retention))

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			s.logger.Info("backup scheduler stopped")
			return
		case <-ticker.C:
			if _, err := s.Backup(ctx); err != nil {
				s.logger.Error("scheduled backup failed", slog.Any("error", err))
				continue
			}
			if err := s.Prune(); err != nil {
				s.logger.Error("backup prune failed", slog.Any("error", err))
			}
		}
	}
}

// IsValidBackupFilename checks if a filename matches the expected backup pattern
// and does not contain path traversal characters.
func IsValidBackupFilename(filename string) bool {
	if strings.Contains(filename, "/") || strings.Contains(filename, "\\") || strings.Contains(filename, "..") {
		return false
	}
	return backupPattern.MatchString(filename)
}
