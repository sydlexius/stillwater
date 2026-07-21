package maintenance

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
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
	} else if !errors.Is(err, sql.ErrNoRows) {
		s.logger.Warn("reading last_optimize_at", "error", err)
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

// confirmSlotOnDisk reports whether the file backing ONE specific image slot is
// present in dir. It is the slot-aware disk check RestoreExistsFlags needs, and
// the deliberate counterpart to ScanExistsFlags' type-level probe: clearing a
// flag may safely act on "no file of this type exists", but RESTORING one must
// prove the exact ordinal is on disk, or a present slot-0 fanart would wrongly
// resurrect the flags of empty slots 1..n.
//
// The three return shapes carry three distinct meanings, and the caller acts on
// only one of them:
//
//   - (true, nil):  the slot's file is confirmed present -- restore is allowed.
//   - (false, nil): the directory was read and the slot's file is definitively
//     absent -- leave the flag cleared.
//   - (false, err): the directory could not be examined (permission denied,
//     I/O error, unresolvable) -- UNVERIFIABLE, so skip rather than restore.
//     This mirrors ScanExistsFlags' refusal to act on a stat error, in the
//     conservative direction: an unverifiable slot is never restored.
//
// Fanart is resolved convention-agnostically via ResolveFanart over the default
// naming set (the same union ScanExistsFlags probes), because slot_index is a
// DiscoverFanart ORDINAL: the file backing slot N is the Nth resolved fanart
// path, whatever numbering convention the library uses. Single-slot types
// (thumb, logo, banner) occupy slot 0 only; a row claiming slot_index > 0 for
// such a type has no on-disk naming and is reported absent, never restored.
func confirmSlotOnDisk(dir, imageType string, slotIndex int) (bool, error) {
	if imageType == "fanart" {
		names, err := img.ResolveFanartNames(nil)
		if err != nil {
			// No fanart naming patterns at all: cannot verify, so do not restore.
			return false, err
		}
		_, paths, err := img.ResolveFanart(dir, names)
		if err != nil {
			// Directory unreadable/absent -- unverifiable, skip.
			return false, err
		}
		return slotIndex >= 0 && slotIndex < len(paths), nil
	}

	// Single-slot types live at slot 0. A row past slot 0 for one of them has no
	// filesystem naming to confirm against, so it is definitively unconfirmable
	// (not an error) and must stay cleared.
	if slotIndex != 0 {
		return false, nil
	}
	patterns := img.FileNamesForType(img.DefaultFileNames, imageType)
	if len(patterns) == 0 {
		// Unknown image type: no candidates to probe. FindExistingImageStrict
		// would report a clean miss without ever touching disk, so treat it as
		// definitively unconfirmable and leave the flag cleared.
		return false, nil
	}
	// Strict variant: a non-ENOENT stat error means "cannot tell", which must
	// NOT be read as "file present". Restore only on a confirmed hit.
	_, found, err := img.FindExistingImageStrict(dir, patterns)
	return found, err
}

// RestoreExistsFlags walks all artist_images rows where exists_flag=0, re-probes
// each slot's file on disk, and restores the flag to 1 ONLY for rows whose file
// is positively confirmed present. It is the exact inverse of ScanExistsFlags
// and the batch remediation for issue #2668, where a prior incident left
// surviving rows flagged missing while their files were still on disk, so the
// operator saw "no artwork" for images the platform was serving.
//
// It is deliberately conservative and monotone:
//
//   - It only ever sets 0 -> 1, via a guarded UPDATE (AND exists_flag = 0), and
//     never touches the locked column -- exists_flag is a physical fact, the
//     lock governs content, so a locked row whose file IS present is restored
//     while its lock is left untouched (design-lock #3).
//   - A pathless row with no cache-dir fallback, a row whose directory cannot be
//     examined, and a row whose file is genuinely absent are ALL left cleared.
//     Only a positive on-disk confirmation flips a flag; absence and
//     unverifiability never do. Inserting rows for files that have no registry
//     row at all is #2670 (already shipped) and out of scope here.
func (s *Service) RestoreExistsFlags(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT ai.artist_id, ai.image_type, ai.slot_index, a.path
		FROM artist_images ai
		JOIN artists a ON ai.artist_id = a.id
		WHERE ai.exists_flag = 0`)
	if err != nil {
		return fmt.Errorf("querying cleared exists_flag rows: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor, no actionable close error

	type presentRow struct {
		artistID  string
		imageType string
		slotIndex int
	}
	// Drain the cursor before issuing any UPDATE. modernc.org/sqlite is
	// single-writer, so holding this SELECT open across writes on the same
	// *sql.DB serializes badly or deadlocks. Two-phase is a correctness
	// requirement under the pure-Go driver, not an optimization (mirrors
	// ScanExistsFlags).
	var present []presentRow
	checked, skipped := 0, 0

	for rows.Next() {
		var artistID, imageType, artistPath string
		var slotIndex int
		if err := rows.Scan(&artistID, &imageType, &slotIndex, &artistPath); err != nil {
			return fmt.Errorf("scanning cleared exists_flag row: %w", err)
		}
		checked++

		dir := s.artistImageDir(artistPath, artistID)
		if dir == "" {
			// No resolvable path and no cache-dir fallback: cannot verify, so
			// leave the flag cleared rather than restoring on an assumption.
			s.logger.Warn("exists_flag restore: unresolvable image dir, skipping",
				slog.String("artist_id", artistID),
				slog.String("image_type", imageType))
			skipped++
			continue
		}

		found, confErr := confirmSlotOnDisk(dir, imageType, slotIndex)
		if confErr != nil {
			// Unverifiable (permission denied, I/O error, unreadable dir). Skip:
			// a flag is restored only on positive confirmation, never on a guess.
			s.logger.Warn("exists_flag restore: cannot verify slot on disk, skipping",
				slog.String("artist_id", artistID),
				slog.String("image_type", imageType),
				slog.Int("slot_index", slotIndex),
				slog.String("dir", dir),
				slog.Any("error", confErr))
			skipped++
			continue
		}
		if found {
			present = append(present, presentRow{artistID, imageType, slotIndex})
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating cleared exists_flag rows: %w", err)
	}

	restored, failed := 0, 0
	for _, r := range present {
		// Monotone, lock-preserving restore: the AND exists_flag = 0 guard keeps
		// the write idempotent against a concurrent change, and locked is absent
		// from the SET list so a locked row's content ownership is untouched.
		_, err := s.db.ExecContext(ctx, `
			UPDATE artist_images SET exists_flag = 1
			WHERE artist_id = ? AND image_type = ? AND slot_index = ? AND exists_flag = 0`,
			r.artistID, r.imageType, r.slotIndex)
		if err != nil {
			// A failed UPDATE leaves a confirmed-present slot still flagged
			// missing, which is the exact defect this scan exists to repair.
			// Surface at Error, not Warn (mirrors ScanExistsFlags).
			s.logger.Error("exists_flag restore: UPDATE failed, flag remains stale",
				slog.String("artist_id", r.artistID),
				slog.String("image_type", r.imageType),
				slog.Int("slot_index", r.slotIndex),
				slog.Any("error", err))
			failed++
			continue
		}
		restored++
	}

	s.logger.Info("exists_flag restore scan complete",
		slog.Int("checked", checked),
		slog.Int("restored", restored),
		slog.Int("skipped", skipped),
		slog.Int("failed", failed))
	return nil
}

// FanartPrimaryFn reports the primary fanart filename of the ACTIVE platform
// profile (e.g. "backdrop.jpg" for Emby, "fanart.jpg" for Kodi).
//
// It is a function parameter rather than a Service field, following
// StartForeignFileScanner's precedent of injecting a dependency at the call
// site, so this package keeps depending on nothing but the DB and the
// filesystem.
//
// It must not be substituted with the DEFAULT naming that ScanExistsFlags uses.
// ScanExistsFlags can afford defaults because it probes EVERY candidate name and
// only acts on a definitive miss. Fanart slot resolution cannot: DiscoverFanart
// keys off ONE primary name, so guessing "fanart.jpg" on an Emby library
// discovers zero files, backfills nothing, and reports a clean pass having done
// no work -- a silent no-op in the exact place a silent no-op is most expensive.
type FanartPrimaryFn func(ctx context.Context) string

// BackfillFanartHashes computes and stores the perceptual and content hashes of
// fanart slots whose phash column is empty.
//
// WHY THIS EXISTS. Provenance is recorded at SAVE time, so any fanart that was
// merely scanned rather than written by Stillwater -- and every fanart appended
// before #2564 fixed the append path -- has an artist_images row with an empty
// phash. UpsertAll deliberately preserves provenance columns on rescan, so that
// emptiness is permanent, not transient. A per-slot phash reader over such a row
// finds no data and reports the artist clean because it had nothing to judge.
// Existing libraries therefore need their starved rows healed before any
// detector built on those hashes can tell "no corruption" apart from "no data".
//
// WHY IT DOES NOT RE-HASH THE LIBRARY ON EVERY BOOT. The work-set is defined by
// the starvation itself: the empty-phash predicate selects only unfilled rows,
// and UpsertAll preserves the ones this pass fills, so every row this pass heals
// leaves the work-set for good.
//
// It does NOT converge to zero in every library. A row whose file cannot be
// decoded is never filled, so it stays selected and is re-hashed every pass,
// forever. That residue is bounded by the number of undecodable files and is
// benign -- a failed decode is cheap, logged, and costs one read per file per
// six hours -- but it is a steady-state floor, not a transient. What actually
// makes a run ledger unnecessary is that the DB itself records the progress:
// a filled row is durably distinguishable from a starved one, so no pass has to
// remember what an earlier pass did. That matters because this package has no
// ledger (the only last-run marker, db_maintenance.last_optimize_at, is
// display-only and gates nothing).
//
// FANART ONLY, deliberately. Fanart is the sole multi-slot image type, so it is
// the only type whose slots can starve while a sibling slot looks healthy, and
// DiscoverFanart gives an exact slot-to-path map to heal them with. The
// single-slot types have no equivalent authoritative mapping here and would have
// to guess at naming (see FanartPrimaryFn); healing them is a separate change
// rather than a guess bolted onto this one.
//
// maxPerPass bounds the work per pass. A row whose file cannot be decoded stays
// selected and is retried next pass; the bound is what stops a pocket of corrupt
// files from monopolising a run. Truncation is logged rather than silent.
func (s *Service) BackfillFanartHashes(ctx context.Context, fanartPrimary FanartPrimaryFn, maxPerPass int) error {
	if fanartPrimary == nil {
		return errors.New("backfilling fanart hashes: no fanart primary-name resolver supplied")
	}
	if maxPerPass <= 0 {
		maxPerPass = 500
	}
	primary := fanartPrimary(ctx)
	if primary == "" {
		return errors.New("backfilling fanart hashes: resolver returned an empty primary name")
	}

	// Select one extra row beyond the cap purely to detect truncation, so the
	// log can say the set was clipped instead of implying full coverage.
	rows, err := s.db.QueryContext(ctx, `
		SELECT ai.artist_id, ai.slot_index, a.path
		FROM artist_images ai
		JOIN artists a ON ai.artist_id = a.id
		WHERE ai.image_type = 'fanart' AND ai.phash = '' AND ai.exists_flag = 1
		ORDER BY ai.artist_id, ai.slot_index
		LIMIT ?`, maxPerPass+1)
	if err != nil {
		return fmt.Errorf("querying starved fanart rows: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor, no actionable close error

	type starved struct {
		artistID   string
		slotIndex  int
		artistPath string
	}
	// Drain the cursor before issuing any write. modernc.org/sqlite uses a
	// single-writer pool, so holding this SELECT open across writes on the same
	// *sql.DB serializes badly or deadlocks. Two-phase is a correctness
	// requirement under the pure-Go driver, not an optimization.
	var pending []starved
	for rows.Next() {
		var st starved
		if err := rows.Scan(&st.artistID, &st.slotIndex, &st.artistPath); err != nil {
			return fmt.Errorf("scanning starved fanart row: %w", err)
		}
		pending = append(pending, st)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating starved fanart rows: %w", err)
	}

	truncated := len(pending) > maxPerPass
	if truncated {
		pending = pending[:maxPerPass]
	}
	if len(pending) == 0 {
		return nil
	}

	// Resolve, hash and write ONE ROW AT A TIME.
	//
	// The hash and the UPDATE that stores it are deliberately interleaved rather
	// than batched into a hash-everything-then-write-everything pair of loops.
	// Batching leaves a window as long as the whole pass between reading a file
	// and writing its hash, and slot_index is a DiscoverFanart ORDINAL, not a
	// stable identifier: a concurrent scan that renumbers slots inside that
	// window makes the UPDATE attach file A's phash to the row that now
	// describes file B. That is worse than the starvation this task exists to
	// end -- a wrong phash is indistinguishable from a real one, so it
	// MANUFACTURES false cross-artist matches, while the true slot stays
	// starved. Interleaving shrinks the window to one file's hash.
	//
	// It NARROWS the window; it does not close it. The read and the write are
	// still separate operations against a filesystem and a DB that a scan can
	// mutate in between -- a genuine TOCTOU that only a scan/backfill lock could
	// eliminate, which is out of scope here. Interleaving is what is available
	// for ~no cost, not a proof of exclusion.
	//
	// Interleaving is free with respect to the two-phase drain above: that
	// requirement is about the SELECT CURSOR, and `pending` is fully drained by
	// this point, so the cursor is closed and these writes cannot contend with
	// it.
	//
	// DiscoverFanart reads a directory, so its result is cached per artist: the
	// rows are ordered by artist_id, but a map keeps that independent of the
	// ORDER BY rather than silently depending on it.
	discovered := make(map[string][]string)
	skipped := 0
	filled, failed := 0, 0

	for _, st := range pending {
		dir := s.artistImageDir(st.artistPath, st.artistID)
		if dir == "" {
			s.logger.Warn("fanart hash backfill: unresolvable image dir, skipping",
				slog.String("artist_id", st.artistID))
			skipped++
			continue
		}
		paths, ok := discovered[st.artistID]
		if !ok {
			p, discErr := img.DiscoverFanart(dir, primary)
			if discErr != nil {
				s.logger.Warn("fanart hash backfill: discovering fanart, skipping artist",
					slog.String("artist_id", st.artistID),
					slog.String("dir", dir),
					slog.Any("error", discErr))
				discovered[st.artistID] = nil
				skipped++
				continue
			}
			discovered[st.artistID] = p
			paths = p
		}
		// slot_index is the DiscoverFanart ORDINAL, so it indexes the slice
		// directly -- the same mapping imageDupRowPath uses. Matching it exactly
		// matters: reading the numeric filename suffix instead would drift the
		// moment a renumber closes a gap.
		if st.slotIndex < 0 || st.slotIndex >= len(paths) {
			// The row outlived its file, or a concurrent scan renumbered the
			// slots between the SELECT and here. Detection is unaffected; the
			// next pass re-derives.
			skipped++
			continue
		}
		path := paths[st.slotIndex]

		fh, hashErr := img.HashFile(path, true)
		if hashErr != nil || fh.Perceptual == 0 && fh.Content == "" {
			s.logger.Warn("fanart hash backfill: hashing file, skipping",
				slog.String("artist_id", st.artistID),
				slog.Int("slot_index", st.slotIndex),
				slog.String("path", path),
				slog.Any("error", hashErr))
			skipped++
			continue
		}
		// Store this row's hash NOW, before hashing the next file, so the
		// hash-to-write window stays one file long. Guarded by phash = '' so a
		// provenance write that landed between this pass's SELECT and now wins
		// instead of being overwritten by this slower, EXIF-less read. The save
		// path's value is the better one: it hashes the bytes it just wrote.
		_, err := s.db.ExecContext(ctx, `
			UPDATE artist_images SET phash = ?, content_hash = ?
			WHERE artist_id = ? AND image_type = 'fanart' AND slot_index = ? AND phash = ''`,
			img.HashHex(fh.Perceptual), fh.Content, st.artistID, st.slotIndex)
		if err != nil {
			// Filling these is the entire point of the task; a failed UPDATE
			// leaves a slot starved, which is the defect this exists to end.
			s.logger.Error("fanart hash backfill: UPDATE failed, slot remains starved",
				slog.String("artist_id", st.artistID),
				slog.Int("slot_index", st.slotIndex),
				slog.String("path", path),
				slog.Any("error", err))
			failed++
			continue
		}
		filled++
	}

	s.logger.Info("fanart hash backfill pass complete",
		slog.Int("selected", len(pending)),
		slog.Int("filled", filled),
		slog.Int("skipped", skipped),
		slog.Int("failed", failed),
		slog.Bool("truncated", truncated))
	return nil
}

// StartFanartHashBackfill runs BackfillFanartHashes after startupDelay and then
// on the given interval until the context is canceled.
//
// It keeps running rather than firing once at boot because starved rows are
// still created after boot: any fanart discovered by a scan (as opposed to
// written by Stillwater) arrives with no phash.
func (s *Service) StartFanartHashBackfill(ctx context.Context, fanartPrimary FanartPrimaryFn, interval, startupDelay time.Duration) {
	if fanartPrimary == nil {
		s.logger.Warn("fanart hash backfill not started: no primary-name resolver provided")
		return
	}
	if interval <= 0 {
		interval = 6 * time.Hour
	}
	if startupDelay <= 0 {
		startupDelay = 30 * time.Second
	}
	s.logger.Info("fanart hash backfill started",
		slog.String("interval", interval.String()),
		slog.String("startup_delay", startupDelay.String()))

	select {
	case <-ctx.Done():
		s.logger.Info("fanart hash backfill stopped")
		return
	case <-time.After(startupDelay):
	}
	if err := s.BackfillFanartHashes(ctx, fanartPrimary, 0); err != nil {
		s.logger.Error("initial fanart hash backfill failed", slog.Any("error", err))
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			s.logger.Info("fanart hash backfill stopped")
			return
		case <-ticker.C:
			if err := s.BackfillFanartHashes(ctx, fanartPrimary, 0); err != nil {
				s.logger.Error("fanart hash backfill failed", slog.Any("error", err))
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
// Returns the fallback value if the key does not exist or cannot be parsed.
// Logs a warning for genuine DB errors (i.e. anything other than a missing row).
func (s *Service) getBoolSetting(ctx context.Context, key string, fallback bool) bool {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			s.logger.Warn("reading bool setting", "key", key, "error", err)
		}
		return fallback
	}
	return v == "true" || v == "1"
}

// getIntSetting reads an integer setting from the key-value table.
// Returns the fallback value if the key does not exist or cannot be parsed.
// Logs a warning for genuine DB errors (i.e. anything other than a missing row).
// Logs a warning when a stored value is not a valid integer.
func (s *Service) getIntSetting(ctx context.Context, key string, fallback int) int {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			s.logger.Warn("reading int setting", "key", key, "error", err)
		}
		return fallback
	}
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		s.logger.Warn("int setting value is not a valid integer", "key", key, "stored_value", v, "fallback", fallback)
		return fallback
	}
	return n
}
