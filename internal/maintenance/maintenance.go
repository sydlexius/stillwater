package maintenance

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/sydlexius/stillwater/internal/foreign"
	img "github.com/sydlexius/stillwater/internal/image"
)

// ForeignArtistLister mirrors the small slice of internal/artist that the
// foreign-file scanner needs. Defined here (and aliased onto foreign.Scanner)
// so wiring stays inside this package and main.go does not import foreign.
type ForeignArtistLister = foreign.ArtistLister

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
	db            *sql.DB
	dbPath        string
	imageCacheDir string
	logger        *slog.Logger
}

// NewService creates a maintenance service. imageCacheDir is the directory
// where platform-sourced artist images are cached for artists without a
// filesystem path. It is derived once in cmd/stillwater/main.go and shared
// with publish.New and api.NewRouter so all three consumers agree on where
// cached images live -- passing a different value here would silently diverge
// the scanner from the writers.
func NewService(db *sql.DB, dbPath string, imageCacheDir string, logger *slog.Logger) *Service {
	return &Service{
		db:            db,
		dbPath:        dbPath,
		imageCacheDir: imageCacheDir,
		logger:        logger.With(slog.String("component", "maintenance")),
	}
}

// artistImageDir returns the directory where images for an artist are stored,
// using the same resolution as Router.imageDir (internal/api/handlers_image.go)
// and Publisher.ImageDir (internal/publish/publisher.go): prefer the artist's
// library path, otherwise fall back to <imageCacheDir>/<artistID>. Returns ""
// when neither resolves; the scanner treats that as "cannot verify" and skips
// the row rather than clearing, so a misconfigured cache dir does not wipe
// flags for every cache-only artist.
func (s *Service) artistImageDir(artistPath, artistID string) string {
	if artistPath != "" {
		return artistPath
	}
	if s.imageCacheDir != "" && artistID != "" {
		return filepath.Join(s.imageCacheDir, artistID)
	}
	return ""
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

// ScanExistsFlags walks all artist_images rows where exists_flag=1, checks
// each row's image directory on disk, and clears the flag for rows whose
// files have genuinely vanished. Rows where the directory cannot be examined
// reliably (permission denied, I/O error, stale NFS handle, unresolvable
// path) are skipped rather than cleared, so a transient filesystem flake
// cannot wipe flags for thousands of artists at once.
//
// The scan uses the default image naming patterns. It is intentionally
// conservative: it only clears a flag when the directory is confirmed
// reachable AND none of the naming-pattern candidates exist under it.
func (s *Service) ScanExistsFlags(ctx context.Context) error {
	// Query all rows where exists_flag=1, joining artists to get the path and ID
	// so we can reconstruct the image directory without an external dependency.
	// Close errors on read-only cursors are not actionable -- the query is
	// already done by the time we close -- so we suppress the lint here.
	rows, err := s.db.QueryContext(ctx, `
		SELECT ai.artist_id, ai.image_type, ai.slot_index, a.path
		FROM artist_images ai
		JOIN artists a ON ai.artist_id = a.id
		WHERE ai.exists_flag = 1`)
	if err != nil {
		return fmt.Errorf("querying exists_flag rows: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor, no actionable close error

	type staleRow struct {
		artistID  string
		imageType string
		slotIndex int
	}
	// Drain the cursor into a slice before issuing any update statement.
	// modernc.org/sqlite uses a single-writer pool, so holding this SELECT
	// cursor open while executing writes on the same *sql.DB would serialize
	// badly (or deadlock in pathological cases). Two-phase is not an
	// optimization here; it is a correctness requirement under the pure-Go
	// driver.
	var stale []staleRow
	checked, skipped := 0, 0

	for rows.Next() {
		var artistID, imageType, artistPath string
		var slotIndex int
		if err := rows.Scan(&artistID, &imageType, &slotIndex, &artistPath); err != nil {
			return fmt.Errorf("scanning exists_flag row: %w", err)
		}
		checked++

		dir := s.artistImageDir(artistPath, artistID)
		if dir == "" {
			// No resolvable path and no cache-dir fallback (misconfigured or
			// both inputs empty). Can't verify either way -- skip rather than
			// clear, so configuration gaps do not corrupt flags.
			s.logger.Warn("exists_flag scan: unresolvable image dir, skipping",
				slog.String("artist_id", artistID),
				slog.String("image_type", imageType))
			skipped++
			continue
		}

		// Use the strict variant: a transient stat error (EACCES on a
		// permission-denied dir, EIO/ESTALE on an unmounted NFS share) means
		// "we don't know whether the file is absent" and must NOT be treated
		// as a clean miss. Without this distinction, a single flaky filesystem
		// could clear every exists_flag under it. See issue #1161.
		patterns := img.FileNamesForType(img.DefaultFileNames, imageType)
		if len(patterns) == 0 {
			// Unknown imageType: FindExistingImageStrict(dir, nil) reports
			// found=false, err=nil, which would clear exists_flag without ever
			// probing the filesystem. Skip so the "only clear on definitive
			// absence" guarantee is preserved.
			s.logger.Warn("exists_flag scan: unknown image type, skipping",
				slog.String("artist_id", artistID),
				slog.String("image_type", imageType))
			skipped++
			continue
		}
		_, found, statErr := img.FindExistingImageStrict(dir, patterns)
		if statErr != nil {
			s.logger.Warn("exists_flag scan: stat error probing artist dir, skipping",
				slog.String("artist_id", artistID),
				slog.String("dir", dir),
				slog.Any("error", statErr))
			skipped++
			continue
		}
		if !found {
			stale = append(stale, staleRow{artistID, imageType, slotIndex})
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating exists_flag rows: %w", err)
	}

	cleared, failed := 0, 0
	for _, r := range stale {
		_, err := s.db.ExecContext(ctx, `
			UPDATE artist_images SET exists_flag = 0
			WHERE artist_id = ? AND image_type = ? AND slot_index = ?`,
			r.artistID, r.imageType, r.slotIndex)
		if err != nil {
			// The whole point of the scanner is to clear these flags; a failed
			// UPDATE means a stale flag persists, which is the exact defect
			// this scanner exists to prevent. Surface at Error, not Warn.
			s.logger.Error("exists_flag scan: UPDATE failed, flag remains stale",
				slog.String("artist_id", r.artistID),
				slog.String("image_type", r.imageType),
				slog.Int("slot_index", r.slotIndex),
				slog.Any("error", err))
			failed++
			continue
		}
		cleared++
	}

	s.logger.Info("exists_flag consistency scan complete",
		slog.Int("checked", checked),
		slog.Int("cleared", cleared),
		slog.Int("skipped", skipped),
		slog.Int("failed", failed))
	return nil
}

// StartExistsFlagScanner runs ScanExistsFlags once at startup (after
// startupDelay, so DB migrations and other boot-time I/O don't contend with
// it) and then on a fixed interval until the context is canceled.
//
// The startup scan matters because stale exists_flag=1 rows manifest as
// broken image icons and backdrop 404s on the very first page load after a
// restart; waiting a full interval to catch up leaves the UI broken in the
// interim.
//
// startupDelay is a parameter (not a constant) so tests can drive it in
// milliseconds rather than waiting 10 seconds per test.
func (s *Service) StartExistsFlagScanner(ctx context.Context, interval, startupDelay time.Duration) {
	s.logger.Info("exists_flag scanner started",
		slog.String("interval", interval.String()),
		slog.String("startup_delay", startupDelay.String()))

	select {
	case <-ctx.Done():
		s.logger.Info("exists_flag scanner stopped")
		return
	case <-time.After(startupDelay):
	}
	if err := s.ScanExistsFlags(ctx); err != nil {
		s.logger.Error("initial exists_flag scan failed", slog.Any("error", err))
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			s.logger.Info("exists_flag scanner stopped")
			return
		case <-ticker.C:
			if err := s.ScanExistsFlags(ctx); err != nil {
				s.logger.Error("exists_flag scan failed", slog.Any("error", err))
			}
		}
	}
}

// StartForeignFileScanner constructs a foreign-file scanner against the
// service's *sql.DB and starts it on the given cadence. Owns no scanner
// state of its own; this method exists so cmd/stillwater/main.go can stand
// up the scheduler in one call without repeating the wiring.
//
// interval defaults to 6 hours when zero is passed; startupDelay defaults
// to 30 seconds. Both are settable so tests can drive the scanner in
// milliseconds.
func (s *Service) StartForeignFileScanner(ctx context.Context, artists ForeignArtistLister, interval, startupDelay time.Duration) {
	if artists == nil {
		s.logger.Warn("foreign-file scanner not started: no artist lister provided")
		return
	}
	if interval <= 0 {
		interval = 6 * time.Hour
	}
	if startupDelay <= 0 {
		startupDelay = 30 * time.Second
	}
	repo := foreign.NewRepository(s.db)
	scanner := foreign.NewScanner(repo, artists, s.logger)
	scanner.StartScheduler(ctx, interval, startupDelay)
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
