package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// backupPattern matches backup filenames: stillwater-YYYYMMDD-HHMMSS.db, plus
// an optional "-N" disambiguation suffix appended when two backups land in the
// same wall-clock second (see linkIntoPlace).
var backupPattern = regexp.MustCompile(`^stillwater-\d{8}-\d{6}(-\d+)?\.db$`)

// tsLayoutLen is the length of the "20060102-150405" timestamp embedded in a
// backup filename (8-digit date + '-' + 6-digit time = 15 chars). Anything
// after it is the optional "-N" collision suffix.
const tsLayoutLen = len("20060102-150405")

// maxCollisionSuffix bounds the disambiguation search in linkIntoPlace. It is a
// runaway backstop only: reaching it means thousands of backups share one
// second, which never happens in practice.
const maxCollisionSuffix = 10000

// osChmod is the chmod function used to restrict a snapshot's permissions.
// Overridable in tests to simulate a failure, following the same
// injectable-hook pattern as osRename in internal/filesystem.
var osChmod = os.Chmod

// osMkdirTemp creates the per-backup staging directory. Overridable in tests
// to simulate a failure.
var osMkdirTemp = os.MkdirTemp

// osLink hard-links the finished, permission-restricted snapshot into the
// backup directory. It replaces a plain os.Rename because os.Link never
// overwrites an existing file -- it fails with os.ErrExist if the target is
// taken -- which makes same-second collision detection race-free between
// concurrent Backup calls (a scheduled backup coinciding with a manual one).
// A plain rename would silently clobber the earlier snapshot. Overridable in
// tests to simulate a failure. The staging file lives inside backupDir, so the
// link is always same-filesystem.
var osLink = os.Link

// BackupInfo describes a backup file.
type BackupInfo struct {
	Filename  string    `json:"filename"`
	Size      int64     `json:"size"`
	CreatedAt time.Time `json:"created_at"`
}

// Clock is the time source used by Service for backup filename timestamps.
// The default implementation delegates to time.Now. Tests inject a fake clock
// to generate unique filenames without sleeping.
type Clock interface {
	Now() time.Time
}

// realClock is the production Clock implementation.
type realClock struct{}

func (realClock) Now() time.Time { return time.Now().UTC() }

// Service manages database backups.
type Service struct {
	db         *sql.DB
	backupDir  string
	retention  int
	maxAgeDays int
	clock      Clock
	mu         sync.RWMutex
	logger     *slog.Logger
}

// NewService creates a backup service.
func NewService(db *sql.DB, backupDir string, retention int, logger *slog.Logger) *Service {
	return &Service{
		db:        db,
		backupDir: backupDir,
		retention: retention,
		clock:     realClock{},
		logger:    logger.With(slog.String("component", "backup")),
	}
}

// WithClock attaches a clock to the service. Intended for tests that need to
// generate unique backup filenames without sleeping.
func (s *Service) WithClock(c Clock) *Service {
	if c != nil {
		s.clock = c
	}
	return s
}

// Backup creates a snapshot of the database using VACUUM INTO.
//
// The snapshot is written to a staging directory created 0700 (owner-only)
// so it is never group/other-readable at rest, not even for the duration of
// the VACUUM itself: VACUUM INTO creates its output file at the process
// umask (typically 0644), so writing directly into backupDir and chmod-ing
// afterward would leave the full database -- including encrypted secrets --
// world/group-readable while the VACUUM runs. Once the snapshot is
// permission-restricted, it is moved into backupDir via a no-clobber hard link
// (see linkIntoPlace) so an earlier same-second snapshot is never silently
// destroyed; the final chmod is a belt-and-suspenders check in case the move
// ever lands on a filesystem that doesn't preserve the mode.
func (s *Service) Backup(ctx context.Context) (*BackupInfo, error) {
	if err := os.MkdirAll(s.backupDir, 0o750); err != nil {
		return nil, fmt.Errorf("creating backup directory: %w", err)
	}

	now := s.clock.Now()
	baseFilename := fmt.Sprintf("stillwater-%s.db", now.Format("20060102-150405"))

	s.logger.Info("starting backup", slog.String("dest", filepath.Join(s.backupDir, baseFilename)))

	stagingDir, err := osMkdirTemp(s.backupDir, ".vacuum-*")
	if err != nil {
		return nil, fmt.Errorf("creating staging directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(stagingDir) }()

	stagingPath := filepath.Join(stagingDir, baseFilename)

	if _, err := s.db.ExecContext(ctx, "VACUUM INTO ?", stagingPath); err != nil {
		return nil, fmt.Errorf("VACUUM INTO: %w", err)
	}

	if err := osChmod(stagingPath, 0o600); err != nil {
		return nil, fmt.Errorf("restricting backup permissions: %w", err)
	}

	// Move the snapshot into backupDir without ever overwriting an existing
	// file. On a same-second collision this returns a distinct, disambiguated
	// filename so both snapshots survive.
	filename, err := linkIntoPlace(stagingPath, s.backupDir, baseFilename)
	if err != nil {
		return nil, err
	}
	dest := filepath.Join(s.backupDir, filename)

	if err := osChmod(dest, 0o600); err != nil {
		return nil, fmt.Errorf("restricting backup permissions: %w", err)
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

// linkIntoPlace moves stagingPath into backupDir under baseFilename (or a
// disambiguated variant), then removes the staging link. It never overwrites
// an existing snapshot: osLink fails with os.ErrExist when the target name is
// already taken -- which is race-free against a concurrent Backup writing the
// same second, unlike a stat-then-rename that both callers can pass before
// either renames. On collision it appends an incrementing "-N" suffix
// (stillwater-YYYYMMDD-HHMMSS-1.db, -2.db, ...) until it finds a free name, so
// two backups in the same second both survive and neither call fails. Returns
// the final filename actually used.
func linkIntoPlace(stagingPath, backupDir, baseFilename string) (string, error) {
	base := strings.TrimSuffix(baseFilename, ".db")
	for i := 0; i <= maxCollisionSuffix; i++ {
		name := baseFilename
		if i > 0 {
			name = fmt.Sprintf("%s-%d.db", base, i)
		}
		dest := filepath.Join(backupDir, name)
		err := osLink(stagingPath, dest)
		if err == nil {
			if rmErr := os.Remove(stagingPath); rmErr != nil {
				return "", fmt.Errorf("removing staging file after link: %w", rmErr)
			}
			return name, nil
		}
		if errors.Is(err, os.ErrExist) {
			continue // name taken (possibly by a concurrent backup); try the next suffix
		}
		return "", fmt.Errorf("moving backup into place: %w", err)
	}
	return "", fmt.Errorf("moving backup into place: exhausted %d collision suffixes for %q", maxCollisionSuffix, baseFilename)
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

		// Parse timestamp from filename: stillwater-YYYYMMDD-HHMMSS[-N].db.
		// The timestamp is always the first tsLayoutLen chars; a trailing
		// "-N" collision suffix (added by linkIntoPlace on a same-second
		// collision) is ignored so those snapshots still sort by their second.
		name := strings.TrimSuffix(strings.TrimPrefix(entry.Name(), "stillwater-"), ".db")
		if len(name) > tsLayoutLen {
			name = name[:tsLayoutLen]
		}
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

// Delete removes a single backup file by filename.
func (s *Service) Delete(filename string) error {
	if !IsValidBackupFilename(filename) {
		return fmt.Errorf("invalid backup filename")
	}
	path := filepath.Join(s.backupDir, filename)
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("removing backup: %w", err)
	}
	s.logger.Info("backup deleted", slog.String("filename", filename))
	return nil
}

// SetRetention updates the retention count for pruning.
func (s *Service) SetRetention(count int) {
	if count < 0 {
		count = 0
	}
	s.mu.Lock()
	s.retention = count
	s.mu.Unlock()
	s.logger.Info("backup retention updated", slog.Int("count", count))
}

// SetMaxAgeDays updates the max age in days for pruning.
func (s *Service) SetMaxAgeDays(days int) {
	if days < 0 {
		days = 0
	}
	s.mu.Lock()
	s.maxAgeDays = days
	s.mu.Unlock()
	s.logger.Info("backup max age updated", slog.Int("days", days))
}

// Retention returns the current retention count.
func (s *Service) Retention() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.retention
}

// MaxAgeDays returns the current max age in days.
func (s *Service) MaxAgeDays() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.maxAgeDays
}

// Prune deletes backups exceeding the retention count and older than max age.
func (s *Service) Prune() error {
	// Snapshot settings under lock
	s.mu.RLock()
	retention := s.retention
	maxAge := s.maxAgeDays
	s.mu.RUnlock()

	backups, err := s.ListBackups()
	if err != nil {
		return err
	}

	// Count-based pruning
	if len(backups) > retention {
		for _, b := range backups[retention:] {
			path := filepath.Join(s.backupDir, b.Filename)
			if err := os.Remove(path); err != nil {
				s.logger.Warn("failed to remove old backup",
					slog.String("filename", b.Filename),
					slog.Any("error", err))
				continue
			}
			s.logger.Info("pruned old backup", slog.String("filename", b.Filename))
		}
	}

	// Age-based pruning
	if maxAge > 0 {
		cutoff := s.clock.Now().AddDate(0, 0, -maxAge)
		// Re-read after count-based pruning may have removed some
		backups, err = s.ListBackups()
		if err != nil {
			return err
		}
		for _, b := range backups {
			if b.CreatedAt.Before(cutoff) {
				path := filepath.Join(s.backupDir, b.Filename)
				if err := os.Remove(path); err != nil {
					s.logger.Warn("failed to remove aged backup",
						slog.String("filename", b.Filename),
						slog.Any("error", err))
					continue
				}
				s.logger.Info("pruned aged backup",
					slog.String("filename", b.Filename),
					slog.Int("max_age_days", maxAge))
			}
		}
	}

	return nil
}

// BackupDir returns the configured backup directory path.
func (s *Service) BackupDir() string {
	return s.backupDir
}

// StartScheduler runs backups on a fixed interval until the context is canceled.
func (s *Service) StartScheduler(ctx context.Context, interval time.Duration) {
	s.logger.Info("backup scheduler started",
		slog.String("interval", interval.String()),
		slog.Int("retention", s.Retention()),
		slog.Int("max_age_days", s.MaxAgeDays()))

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
