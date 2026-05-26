package foreign

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	img "github.com/sydlexius/stillwater/internal/image"
)

// foreignNamePrefixes are the lowercased basename prefixes (without
// extension) that media servers use when persisting artwork to disk. The
// scanner checks each artist directory for files whose basename has one of
// these prefixes and a recognized image extension. Matching files lacking
// Stillwater EXIF provenance are recorded.
//
// "thumb" is included even though Stillwater also writes thumb*.jpg under
// some platforms; the provenance check disambiguates -- a Stillwater-written
// thumb.jpg has the EXIF tag and is silently skipped.
var foreignNamePrefixes = []string{
	"backdrop", "fanart", "poster", "logo", "banner",
	"thumb", "clearart", "disc", "landscape",
}

// imageExtensions are the file extensions the scanner considers when
// matching foreign image candidates.
var imageExtensions = map[string]struct{}{
	".jpg": {}, ".jpeg": {}, ".png": {}, ".webp": {},
}

// ArtistLister exposes only what the scanner needs from the artist service:
// page through every artist that has a filesystem path. Defined as an
// interface so the scanner is unit-testable with a stub.
type ArtistLister interface {
	List(ctx context.Context, params artist.ListParams) ([]artist.Artist, int, error)
}

// Scanner walks artist directories on a fixed cadence and records foreign
// files to the foreign_files ledger via Repository. It never deletes;
// per-file deletion is user-triggered through the API handler.
type Scanner struct {
	repo    *Repository
	artists ArtistLister
	logger  *slog.Logger
}

// NewScanner wires a Scanner with the repository it writes into and the
// artist lister it walks across.
func NewScanner(repo *Repository, artists ArtistLister, logger *slog.Logger) *Scanner {
	return &Scanner{
		repo:    repo,
		artists: artists,
		logger:  logger.With(slog.String("component", "foreign-scanner")),
	}
}

// isForeignCandidate returns true when the basename matches one of the
// media-server image-naming prefixes and has a recognized image extension.
// Case-insensitive on both name and extension to match how Emby / Jellyfin
// vary across deployments.
func isForeignCandidate(name string) bool {
	lower := strings.ToLower(name)
	ext := filepath.Ext(lower)
	if _, ok := imageExtensions[ext]; !ok {
		return false
	}
	stem := strings.TrimSuffix(lower, ext)
	for _, p := range foreignNamePrefixes {
		// Match either exact (e.g. "fanart.jpg") or prefix-with-suffix
		// (e.g. "fanart1.jpg", "backdrop-2.jpg"). Suffix is anything
		// the media server appended (digits, dashes) -- we don't try to
		// distinguish here; the provenance check below makes the final call.
		if stem == p || strings.HasPrefix(stem, p) {
			return true
		}
	}
	return false
}

// baselineKey is the settings KV row that records whether the first
// foreign-file scan has run to completion. When unset (or "false"), the
// next Scan runs in baseline mode: detections are recorded directly into
// the content-hash allowlist as the install's starting point, NOT into
// the foreign_files alert ledger. This avoids greeting a new operator
// with "325 incidents" on day-one of a fresh install when every one of
// those files predates Stillwater itself.
const baselineKey = "foreign_files.baseline_completed"

// baselineCountKey records the number of files admitted into the allowlist
// during the baseline scan. Surfaced by the OOBE summary panel and the
// foreign-files settings page as informational copy, not as an alert.
const baselineCountKey = "foreign_files.baseline_count"

// IsBaselined returns whether the first scan has run to completion. The
// caller is the OOBE wizard step that gates between "show baseline
// summary" vs "show empty-state" rendering.
func (s *Scanner) IsBaselined(ctx context.Context) (bool, error) {
	return readBaseline(ctx, s.repo.db)
}

// readBaseline is a package-internal helper so both the scanner and any
// future caller (e.g. an OOBE handler that wants to know whether the
// first scan has run) can share the same KV-probe logic.
func readBaseline(ctx context.Context, db interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}) (bool, error) {
	var v string
	err := db.QueryRowContext(ctx,
		`SELECT value FROM settings WHERE key = ?`, baselineKey).Scan(&v)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("reading %s: %w", baselineKey, err)
	}
	return v == "true", nil
}

// baselineWriter is the subset of *sql.DB that writeBaselineDone needs to
// open a transaction. Both *sql.DB and a test fake can satisfy it.
type baselineWriter interface {
	BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error)
}

// writeBaselineDone records that the first-scan baseline has completed,
// along with the count of files admitted into the global allowlist. Both
// rows are upserted inside a single transaction so a crash between the
// flag flip and the count write cannot leave baseline_completed=true with
// the count unrecorded; the next scan would otherwise believe the
// baseline is done while the OOBE summary panel renders a missing or
// stale count.
func writeBaselineDone(ctx context.Context, db baselineWriter, count int) error {
	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning baseline tx: %w", err)
	}
	defer func() {
		// Rollback is a no-op once Commit succeeds, so it is safe to always
		// schedule. Errors here would only fire on a driver-level failure
		// AFTER a successful commit, which is not actionable.
		_ = tx.Rollback()
	}()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO settings (key, value, updated_at)
		VALUES (?, 'true', ?)
		ON CONFLICT(key) DO UPDATE SET value = 'true', updated_at = excluded.updated_at
	`, baselineKey, now); err != nil {
		return fmt.Errorf("marking baseline completed: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO settings (key, value, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
	`, baselineCountKey, fmt.Sprintf("%d", count), now); err != nil {
		return fmt.Errorf("recording baseline count: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing baseline tx: %w", err)
	}
	return nil
}

// Scan walks every artist that has a filesystem path, records foreign image
// files in the ledger, and removes ledger rows whose underlying files have
// since been deleted or gained Stillwater provenance (so a successful crop
// or re-fetch flushes the entry without an explicit "resolved" action).
//
// On the FIRST scan of an install (foreign_files.baseline_completed != "true"
// in settings), every detection is admitted into the global content-hash
// allowlist instead of the alert ledger so the operator does not start at
// "325 incidents" against artwork that predates Stillwater. Once
// the baseline has been recorded, this method flips into the historical
// behavior: only NEW files (not on the allowlist) become alerts.
//
// The scan never deletes files. Allowlisted (artist, file_name) pairs and
// every globally-allowlisted file_name are skipped.
func (s *Scanner) Scan(ctx context.Context) error {
	const pageSize = 200
	params := artist.ListParams{Page: 1, PageSize: pageSize, Sort: "name"}

	// Baseline-mode probe runs once per Scan call. The setting is read
	// here rather than per-artist because flipping the flag mid-scan
	// would split detections across the ledger and the allowlist for the
	// same scan generation, which is harder to reason about than a clean
	// "this whole scan was baselined".
	baselineDone, err := readBaseline(ctx, s.repo.db)
	if err != nil {
		return fmt.Errorf("probing baseline state: %w", err)
	}
	runAsBaseline := !baselineDone

	first, total, err := s.artists.List(ctx, params)
	if err != nil {
		return fmt.Errorf("listing artists: %w", err)
	}

	scanned, recorded, cleared, skipped, baselined := 0, 0, 0, 0, 0
	process := func(artists []artist.Artist) {
		for i := range artists {
			if ctx.Err() != nil {
				return
			}
			a := &artists[i]
			if a.Path == "" {
				skipped++
				continue
			}
			rec, clr, sk, bl := s.scanArtist(ctx, *a, runAsBaseline)
			scanned++
			recorded += rec
			cleared += clr
			skipped += sk
			baselined += bl
		}
	}

	// abortErr distinguishes a clean completion from an early termination
	// caused by context cancellation or a pagination DB error. Reporting
	// "scan complete" with partial counts in those cases makes operator
	// logs misleading -- the scheduler then retries on the next tick
	// without any error-level signal that the prior run did not finish.
	var abortErr error
	process(first)
	for scanned+skipped < total {
		if cerr := ctx.Err(); cerr != nil {
			abortErr = cerr
			break
		}
		params.Page++
		more, _, err := s.artists.List(ctx, params)
		if err != nil {
			abortErr = fmt.Errorf("listing artists page %d: %w", params.Page, err)
			break
		}
		if len(more) == 0 {
			break
		}
		process(more)
	}

	if abortErr != nil {
		// Cancellation is the graceful-shutdown path; log at Info so a normal
		// stop does not generate Error noise. Any other abort (DB failure
		// mid-pagination, etc.) keeps Error severity so the operator notices.
		fields := []any{
			slog.Int("scanned_artists", scanned),
			slog.Int("recorded", recorded),
			slog.Int("cleared", cleared),
			slog.Int("skipped", skipped),
			slog.Int("baselined", baselined),
			slog.Bool("baseline_mode", runAsBaseline),
			slog.Any("error", abortErr),
		}
		if errors.Is(abortErr, context.Canceled) || errors.Is(abortErr, context.DeadlineExceeded) {
			s.logger.Info("foreign-file scan canceled; counts are partial", fields...)
			// A canceled baseline scan should NOT mark the baseline as
			// complete; the partial allowlist is fine to keep (subsequent
			// scans skip those hashes), but we want the next scan to also
			// run in baseline mode so any artist not yet visited still
			// gets admitted rather than alerted.
		} else {
			s.logger.Error("foreign-file scan aborted; counts are partial", fields...)
		}
		return abortErr
	}

	// Successful completion: flip the baseline flag so subsequent scans
	// run in alert mode. The count is informational only -- the OOBE
	// summary surfaces it as "Found N existing files; recorded as your
	// starting point" rather than as an alert count.
	if runAsBaseline {
		if err := writeBaselineDone(ctx, s.repo.db, baselined); err != nil {
			s.logger.Error("recording baseline completion", slog.Any("error", err))
			// Returning the error here would leave the ledger in an
			// otherwise-correct state; we surface the failure but do not
			// retry. The next scan will re-run in baseline mode and
			// re-admit the same hashes (allowlist insert is idempotent
			// via UNIQUE on content_hash + scope) so this is safe.
			return fmt.Errorf("recording baseline completion: %w", err)
		}
	}

	s.logger.Info("foreign-file scan complete",
		slog.Int("scanned_artists", scanned),
		slog.Int("recorded", recorded),
		slog.Int("cleared", cleared),
		slog.Int("skipped", skipped),
		slog.Int("baselined", baselined),
		slog.Bool("baseline_mode", runAsBaseline))
	return nil
}

// scanArtist examines a single artist directory and reconciles the ledger
// with on-disk reality. Returns (recorded, cleared, skipped, baselined)
// counts so the caller can roll up Scan-level metrics. When runAsBaseline
// is true, foreign detections are admitted into the global allowlist
// instead of being upserted into the foreign_files alert ledger.
//
//nolint:gocognit // Foreign-file reconciler (cog 50): reconciles on-disk files against the ledger with skip-don't-clear semantics (ambiguous reads -> skipped, not recorded/cleared, per the proactive-cron blast-radius safeguard). The bucket-selection ladder is essential to the safety policy but the per-file classification could split into a typed classifier helper to ease readability. Refactor tracked in #1549.
func (s *Scanner) scanArtist(ctx context.Context, a artist.Artist, runAsBaseline bool) (int, int, int, int) {
	entries, err := os.ReadDir(a.Path)
	if err != nil {
		// Skip-don't-clear: a transient read error must NOT clear ledger
		// entries. The user could lose history if a flaky NFS share blanks
		// out every artist's foreign-file list. (memory feedback_proactive_cron_blast_radius.md)
		s.logger.Warn("reading artist dir; skipping",
			slog.String("artist_id", a.ID),
			slog.String("path", a.Path),
			slog.Any("error", err))
		return 0, 0, 1, 0
	}

	// Build a set of the foreign candidates currently on disk so we can,
	// in a second pass, remove ledger rows whose file is gone.
	onDisk := map[string]os.DirEntry{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !isForeignCandidate(e.Name()) {
			continue
		}
		onDisk[e.Name()] = e
	}

	recorded := 0
	baselined := 0
	for name, de := range onDisk {
		if ctx.Err() != nil {
			return recorded, 0, 0, baselined
		}
		fullPath := filepath.Join(a.Path, name)

		// Provenance is checked before hashing: a Stillwater-managed
		// image is not foreign and never reaches the allowlist, so on a
		// steady-state library this skips a full-file read for the
		// common case.
		meta, err := img.ReadProvenance(fullPath)
		if err != nil {
			s.logger.Warn("read provenance failed; skipping",
				slog.String("path", fullPath),
				slog.Any("error", err))
			continue
		}
		if meta != nil {
			// Has Stillwater provenance -- not foreign.
			continue
		}

		// Hash is computed before the allowlist check because the
		// allowlist now keys on byte content rather than basename. If
		// hashing fails (permission, partial write) we skip the file
		// silently; re-detection on the next scan catches it once the
		// file is readable.
		hash, err := hashFile(fullPath)
		if err != nil {
			s.logger.Warn("hash file failed; skipping",
				slog.String("path", fullPath),
				slog.Any("error", err))
			continue
		}

		allowed, err := s.repo.IsAllowlisted(ctx, a.ID, hash)
		if err != nil {
			s.logger.Warn("allowlist check failed; skipping file",
				slog.String("artist_id", a.ID),
				slog.String("file", name),
				slog.Any("error", err))
			continue
		}
		if allowed {
			continue
		}

		var size int64
		if info, ierr := de.Info(); ierr == nil {
			size = info.Size()
		}

		// Baseline mode: admit the file into the global content-hash
		// allowlist instead of the alert ledger. The OOBE summary panel
		// will surface the per-scan count as informational copy
		// ("Found N existing files; recorded as your starting point")
		// so the user sees a neutral starting state rather than 325
		// red-banner incidents.
		if runAsBaseline {
			allow := AllowlistEntry{
				Scope:       ScopeGlobal,
				FileName:    name,
				ContentHash: hash,
				Note:        "Recorded during first foreign-file scan baseline",
			}
			if err := s.repo.AddAllowlist(ctx, allow); err != nil {
				s.logger.Warn("baseline: admitting file to allowlist failed",
					slog.String("artist_id", a.ID),
					slog.String("file", name),
					slog.Any("error", err))
				continue
			}
			baselined++
			continue
		}

		entry := Entry{
			ArtistID:    a.ID,
			FilePath:    fullPath,
			FileName:    name,
			ContentHash: hash,
			SizeBytes:   size,
			DetectedAt:  time.Now().UTC(),
		}
		if err := s.repo.Upsert(ctx, entry); err != nil {
			s.logger.Warn("upsert foreign-file entry",
				slog.String("artist_id", a.ID),
				slog.String("file", name),
				slog.Any("error", err))
			continue
		}
		recorded++
	}

	// Reconciliation pass: drop ledger rows whose file is no longer on disk
	// OR is now allowlisted. If listing fails we skip, again per the
	// skip-don't-clear policy.
	existing, err := s.listForArtist(ctx, a.ID)
	if err != nil {
		s.logger.Warn("listing existing entries for reconcile; skipping clear",
			slog.String("artist_id", a.ID),
			slog.Any("error", err))
		return recorded, 0, 0, baselined
	}
	cleared := 0
	for i := range existing {
		ex := &existing[i]
		if _, present := onDisk[ex.FileName]; present {
			// Still on disk; only clear if it has gained provenance OR is
			// now allowlisted. Both already filtered above (we would have
			// continued before upserting), but the row may pre-date the
			// fix, so re-evaluate here.
			//
			// Allowlist matching keys on content_hash. Pre-008 rows may
			// have an empty hash; backfill by rehashing on demand so the
			// allowlist check has a key to compare against. The next
			// upsert path (above) writes the hash back so subsequent
			// scans skip the rehash.
			hash := ex.ContentHash
			if hash == "" {
				h, herr := hashFile(ex.FilePath)
				if herr != nil {
					s.logger.Debug("rehash for reconcile failed; leaving row in place",
						slog.String("artist_id", a.ID),
						slog.String("file_path", ex.FilePath),
						slog.Any("error", herr))
					continue
				}
				hash = h
			}
			allowed, err := s.repo.IsAllowlisted(ctx, a.ID, hash)
			if err != nil {
				// Skip this row; leaving it in place is correct under the
				// skip-don't-clear policy, but the failure must be visible
				// so a chronic DB error does not silently freeze the
				// reconcile loop on every row.
				s.logger.Warn("checking allowlist for reconcile; leaving row in place",
					slog.String("artist_id", a.ID),
					slog.String("file_name", ex.FileName),
					slog.Any("error", err))
				continue
			}
			if allowed {
				if derr := s.repo.DeleteByPath(ctx, a.ID, ex.FilePath); derr != nil {
					s.logger.Warn("clearing allowlisted foreign-file row failed",
						slog.String("artist_id", a.ID),
						slog.String("file_path", ex.FilePath),
						slog.Any("error", derr))
				} else {
					cleared++
				}
				continue
			}
			meta, perr := img.ReadProvenance(ex.FilePath)
			if perr != nil {
				// Same skip-don't-clear policy: an unreadable file may be
				// transient (mid-write, perm flap). Surface the failure
				// rather than silently leaving the row stale.
				s.logger.Warn("reading provenance for reconcile; leaving row in place",
					slog.String("artist_id", a.ID),
					slog.String("file_path", ex.FilePath),
					slog.Any("error", perr))
				continue
			}
			if meta != nil {
				if derr := s.repo.DeleteByPath(ctx, a.ID, ex.FilePath); derr != nil {
					s.logger.Warn("clearing re-provenanced foreign-file row failed",
						slog.String("artist_id", a.ID),
						slog.String("file_path", ex.FilePath),
						slog.Any("error", derr))
				} else {
					cleared++
				}
			}
			continue
		}
		// File is gone from disk -- safe to clear.
		if derr := s.repo.DeleteByPath(ctx, a.ID, ex.FilePath); derr != nil {
			s.logger.Warn("clearing missing-file foreign-file row failed",
				slog.String("artist_id", a.ID),
				slog.String("file_path", ex.FilePath),
				slog.Any("error", derr))
		} else {
			cleared++
		}
	}

	return recorded, cleared, 0, baselined
}

// listForArtist returns the existing ledger rows for one artist, used by
// scanArtist's reconcile pass. Defined here (rather than on Repository) so
// the listing predicate stays close to the only caller that needs it.
func (s *Scanner) listForArtist(ctx context.Context, artistID string) ([]Entry, error) {
	rows, err := s.repo.db.QueryContext(ctx,
		`SELECT id, artist_id, file_path, file_name, COALESCE(content_hash, ''), size_bytes, detected_at
		   FROM foreign_files WHERE artist_id = ?`, artistID)
	if err != nil {
		return nil, fmt.Errorf("listing artist foreign files: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor
	var out []Entry
	for rows.Next() {
		var e Entry
		var detected string
		if err := rows.Scan(&e.ID, &e.ArtistID, &e.FilePath, &e.FileName, &e.ContentHash, &e.SizeBytes, &detected); err != nil {
			return nil, fmt.Errorf("scanning artist foreign file row: %w", err)
		}
		// Mirror Repository.GetByID/List parsing so DetectedAt is populated
		// consistently regardless of which read path produced the Entry.
		if t, perr := time.Parse(time.RFC3339, detected); perr == nil {
			e.DetectedAt = t
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// hashFile returns the lowercase hex sha256 of the file at path. The
// scanner uses it to key allowlist matching on byte content rather than
// basename, and to rehash legacy rows that predate migration 008 during
// reconciliation. Reading is streamed so very large foreign files do not
// balloon scanner memory.
func hashFile(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // G304: path is built from artist directory + DirEntry name, both server-controlled
	if err != nil {
		return "", fmt.Errorf("hash open: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only handle
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash read: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// StartScheduler runs the scan on a fixed interval (after startupDelay) until
// ctx is canceled. Mirrors maintenance.StartExistsFlagScanner so the operator
// model is consistent: one-shot at boot then ticker-driven.
func (s *Scanner) StartScheduler(ctx context.Context, interval, startupDelay time.Duration) {
	s.logger.Info("foreign-file scanner started",
		slog.String("interval", interval.String()),
		slog.String("startup_delay", startupDelay.String()))

	select {
	case <-ctx.Done():
		s.logger.Info("foreign-file scanner stopped before initial run")
		return
	case <-time.After(startupDelay):
	}
	// Scan logs its own abort detail at Info (cancel) or Error (other);
	// suppress the wrapper Error log on cancellation so graceful shutdown
	// is quiet. Other error types are double-logged: once with counts
	// inside Scan, once here as a summary; the summary is intentional so
	// the operator sees both the per-iteration record and the scheduler-
	// level "scan failed" hook in any log filter that excludes Scan's
	// internal lines.
	if err := s.Scan(ctx); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		s.logger.Error("initial foreign-file scan failed", slog.Any("error", err))
	}

	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			s.logger.Info("foreign-file scanner stopped")
			return
		case <-t.C:
			if err := s.Scan(ctx); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				s.logger.Error("foreign-file scan failed", slog.Any("error", err))
			}
		}
	}
}
