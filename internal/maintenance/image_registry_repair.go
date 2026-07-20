// Image registry repair: rebuild missing artist_images rows from the files
// actually on disk, for recovery after rows were destroyed while their files
// survived.
//
// This file is INSERT-ONLY. It holds exactly one write statement, an
// INSERT ... ON CONFLICT DO NOTHING that restores a row only where none
// exists. There is no UPDATE and no DELETE: an existing row is never modified,
// not even to correct a stale exists_flag (that is a separate opt-in pass), and
// no row is ever removed. TestRepairIsStructurallyInsertOnly enforces that
// mechanically by rejecting DELETE, UPDATE, os.Remove, and os.Rename in this
// file.
//
// The incident this repairs was caused by code that asserted a fact it had not
// measured: a failed directory read became "zero files exist", which licensed
// deletion. Every read here is three-valued -- present, absent, or unknown --
// and unknown always means SKIP AND REPORT, never absent. An unreadable
// directory skips its artist; an undecodable file skips its slot; neither is
// ever counted as clean. The same principle scales up in the mount-down guard
// (see ErrLibraryUnreachable): a library-wide pass that could read nothing yet
// saw absences refuses to call that clean.
package maintenance

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	img "github.com/sydlexius/stillwater/internal/image"
)

// singleSlotTypes are the image types that occupy only ordinal 0.
var singleSlotTypes = []string{"thumb", "logo", "banner"}

// ImageRepairOpts controls a repair pass.
//
// There is no DryRun field on purpose: a Go bool zero-values to false, so a
// DryRun field would mean a request that omitted it WRITES. Commit is
// affirmative, so a malformed body or a dropped field previews instead.
type ImageRepairOpts struct {
	// Commit must be true for anything to be written. False runs identical
	// discovery and verification and reports the plan.
	Commit bool
	// ArtistID scopes the pass to one artist; empty means every artist. The
	// empty (library-wide) case is also what arms the mount-visibility guard
	// in RepairImageRegistry -- see ErrLibraryUnreachable.
	ArtistID string
}

// ImageRepairOutcome is one artist/type/slot decision, recorded whether or not
// anything was written. Action is one of would_insert, inserted, skipped,
// failed.
type ImageRepairOutcome struct {
	ArtistID  string `json:"artist_id"`
	ImageType string `json:"image_type"`
	SlotIndex int    `json:"slot_index"`
	FileName  string `json:"file_name"`
	Action    string `json:"action"`
	Reason    string `json:"reason"`
}

// ImageRepairResult reports what a pass measured. Every counter is established
// independently; none is derived from another. RowsInserted in particular is
// neither RowsPlanned nor a RowsAffected tally -- it comes from re-reading the
// table after the writes, so a pass whose inserts all silently no-op reports
// RowsPlanned=9, RowsInserted=0 and says so.
type ImageRepairResult struct {
	OpID   string `json:"op_id"`
	DryRun bool   `json:"dry_run"`
	// ArtistsScanned counts artists whose directory was read successfully;
	// skipped, absent, and failed artists are NOT included.
	ArtistsScanned int `json:"artists_scanned"`
	// ArtistsSkipped counts artists with no resolvable image directory at all
	// (pathless, no cache dir).
	ArtistsSkipped int `json:"artists_skipped"`
	// ArtistsAbsent counts artists whose directory is definitively absent
	// (ENOENT). A clean, expected no-op for an additive repair -- kept apart
	// from ArtistsFailed so a genuinely-missing folder is never reported as an
	// error the operator should investigate.
	ArtistsAbsent int `json:"artists_absent"`
	// ArtistsFailed counts artists whose directory could not be read for a
	// reason that is NOT definitive absence (EACCES, ESTALE, unmounted share):
	// we cannot tell what is on disk, so we touch nothing.
	ArtistsFailed int                  `json:"artists_failed"`
	RowsPlanned   int                  `json:"rows_planned"`
	RowsInserted  int                  `json:"rows_inserted"`
	FilesSkipped  int                  `json:"files_skipped"`
	Outcomes      []ImageRepairOutcome `json:"outcomes"`
}

// ErrLibraryUnreachable is returned when a library-wide pass could not read a
// single artist directory yet saw at least one report as absent (ENOENT).
//
// This is the mount-down guard. When the media mount is gone, every artist
// directory returns ENOENT, and per-artist that reads as "definitively absent,
// nothing to restore". Left alone, a total outage would be reported as a clean
// no-op -- 0 inserted, all absent -- which is the exact catastrophe this whole
// feature exists to prevent, just one layer up. So the pass FAILS CLOSED: if it
// scanned nothing across the whole library and saw absences, it refuses to
// call that clean and returns this error. Nothing was written (inserts happen
// only after a successful scan, and there were none). A single genuinely-absent
// artist under a healthy library does not trip it -- other artists scan, so
// ArtistsScanned > 0 -- nor does a single-artist scoped pass.
var ErrLibraryUnreachable = errors.New("image repair: no artist directory was readable across the whole library; refusing to treat as clean (mount likely down)")

// slotKey identifies a registry row within one artist.
type slotKey struct {
	imageType string
	slotIndex int
}

// candidate is a verified file on disk that the registry should describe.
type candidate struct {
	key         slotKey
	fileName    string
	width       int
	height      int
	placeholder string
	lowRes      bool
}

// RepairImageRegistry rebuilds missing artist_images rows from files on disk.
//
// It is insert-only: it never deletes a row and never overwrites an existing
// one, including its exists_flag. A row that is already present is left exactly
// as it is. (Restoring a cleared exists_flag on a surviving row is a separate,
// opt-in concern handled elsewhere.)
//
// The error return is reserved for failures that make the whole pass
// meaningless: a failed artist query, a canceled context, or the mount-down
// guard (ErrLibraryUnreachable). Per-artist and per-file failures are reported
// in the result and do not abort the pass: one unreadable directory must not
// stop the rest of the library from being repaired.
func (s *Service) RepairImageRegistry(ctx context.Context, opts ImageRepairOpts) (*ImageRepairResult, error) {
	res := &ImageRepairResult{OpID: uuid.New().String(), DryRun: !opts.Commit}
	log := s.logger.With(
		slog.String("component", "image-repair"),
		slog.String("op_id", res.OpID),
		slog.Bool("dry_run", res.DryRun))

	artists, err := s.repairArtists(ctx, opts.ArtistID)
	if err != nil {
		return nil, err
	}
	for _, a := range artists {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		s.repairOneArtist(ctx, log, opts, a.id, a.path, res)
	}

	// Mount-down guard. On a library-wide pass, if not one artist directory
	// was readable yet at least one reported absent, the library itself is not
	// visible (a missing mount makes every directory ENOENT). Refuse to call
	// that clean. Scoped single-artist passes are exempt: one absent artist is
	// not evidence about the mount, and asking about one artist is not the
	// catastrophe case. Nothing was written -- inserts require a successful
	// scan, of which there were none -- so failing closed here rolls back
	// nothing.
	if opts.ArtistID == "" && len(artists) > 0 && res.ArtistsScanned == 0 && res.ArtistsAbsent > 0 {
		log.Error("image registry repair: library not visible; refusing to report clean-absent",
			slog.Int("artists_in_scope", len(artists)),
			slog.Int("artists_absent", res.ArtistsAbsent),
			slog.Int("artists_failed", res.ArtistsFailed))
		return res, ErrLibraryUnreachable
	}

	log.Info("image registry repair complete",
		slog.Int("artists_scanned", res.ArtistsScanned),
		slog.Int("artists_skipped", res.ArtistsSkipped),
		slog.Int("artists_absent", res.ArtistsAbsent),
		slog.Int("artists_failed", res.ArtistsFailed),
		slog.Int("rows_planned", res.RowsPlanned),
		slog.Int("rows_inserted", res.RowsInserted),
		slog.Int("files_skipped", res.FilesSkipped))
	return res, nil
}

type repairArtist struct{ id, path string }

// repairArtists loads the artists in scope up front so the read cursor is
// closed before any write runs: modernc.org/sqlite is single-writer, and
// holding a SELECT open across writes on the same *sql.DB serializes badly.
// Same two-phase reasoning as ScanExistsFlags.
func (s *Service) repairArtists(ctx context.Context, artistID string) ([]repairArtist, error) {
	query, args := `SELECT id, path FROM artists ORDER BY id`, []any(nil)
	if artistID != "" {
		query, args = `SELECT id, path FROM artists WHERE id = ?`, []any{artistID}
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying artists for image repair: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor

	var out []repairArtist
	for rows.Next() {
		var a repairArtist
		if err := rows.Scan(&a.id, &a.path); err != nil {
			return nil, fmt.Errorf("scanning artist row for image repair: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating artists for image repair: %w", err)
	}
	return out, nil
}

// repairOneArtist runs discover -> verify -> plan -> write -> confirm for one
// artist, accumulating into res.
func (s *Service) repairOneArtist(ctx context.Context, log *slog.Logger, opts ImageRepairOpts, artistID, artistPath string, res *ImageRepairResult) {
	log = log.With(slog.String("artist_id", artistID))

	dir := s.artistImageDir(artistPath, artistID)
	if dir == "" {
		log.Warn("image repair: unresolvable image dir, skipping artist")
		res.ArtistsSkipped++
		res.Outcomes = append(res.Outcomes, ImageRepairOutcome{
			ArtistID: artistID, Action: "skipped", Reason: "no_image_dir"})
		return
	}

	existing, err := s.existingSlots(ctx, artistID)
	if err != nil {
		log.Error("image repair: reading existing rows failed, skipping artist", slog.Any("error", err))
		res.ArtistsFailed++
		res.Outcomes = append(res.Outcomes, ImageRepairOutcome{
			ArtistID: artistID, Action: "failed", Reason: "registry_read_failed"})
		return
	}

	found, err := s.discover(log, artistID, dir, res)
	if err != nil {
		// Two different facts, deliberately kept apart. Conflating them is the
		// exact mistake that caused the incident, so the repair must not repeat
		// it in its own reporting.
		if errors.Is(err, fs.ErrNotExist) {
			// ENOENT: the directory is definitively absent. For an additive
			// repair that is a clean, expected outcome -- there is nothing on
			// disk to rebuild from, and nothing to do. It is NOT a failure.
			log.Info("image repair: artist directory absent, nothing to restore",
				slog.String("dir", dir))
			res.ArtistsAbsent++
			res.Outcomes = append(res.Outcomes, ImageRepairOutcome{
				ArtistID: artistID, Action: "skipped", Reason: "dir_absent"})
			return
		}
		// EACCES, ESTALE, an unmounted share. We genuinely cannot tell what is
		// in this directory, so we touch nothing. Downgrading this to "zero
		// files" is the bug being repaired.
		log.Warn("image repair: directory unreadable, skipping artist",
			slog.String("dir", dir), slog.Any("error", err))
		res.ArtistsFailed++
		res.Outcomes = append(res.Outcomes, ImageRepairOutcome{
			ArtistID: artistID, Action: "failed", Reason: "dir_unreadable"})
		return
	}
	res.ArtistsScanned++

	inserts := plan(artistID, found, existing, opts, res)
	if !opts.Commit || len(inserts) == 0 {
		return
	}
	s.applyInserts(ctx, log, artistID, inserts)
	s.confirm(ctx, log, artistID, inserts, existing, res)
}

// existingSlots reads the artist's current rows, mapping each key to its
// exists_flag. Key presence is row existence. This pre-state decides what is
// inserted and is what the post-write re-read is diffed against, since
// artist_images has no created_at column to identify this run's rows by time.
func (s *Service) existingSlots(ctx context.Context, artistID string) (map[slotKey]bool, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT image_type, slot_index, exists_flag FROM artist_images WHERE artist_id = ?`, artistID)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // read-only cursor

	out := map[slotKey]bool{}
	for rows.Next() {
		var k slotKey
		var flag int
		if err := rows.Scan(&k.imageType, &k.slotIndex, &flag); err != nil {
			return nil, err
		}
		out[k] = flag == 1
	}
	return out, rows.Err()
}

// discover enumerates the verified image files in dir. An error means the
// directory could not be read, which the caller must treat as unknown rather
// than empty. Files present but failing verification are counted in
// res.FilesSkipped and omitted.
func (s *Service) discover(log *slog.Logger, artistID, dir string, res *ImageRepairResult) ([]candidate, error) {
	fanart, err := img.ResolveFanartFiles(dir, img.DefaultFileNames["fanart"])
	if err != nil {
		return nil, err
	}

	var out []candidate
	for i, p := range fanart {
		// Fanart ordinals >= 1 are existence-only in the registry, matching
		// extractImageMetadata, so their dimensions stay zero. They are still
		// fully decoded: verification is about whether the file is real, not
		// about what gets stored.
		out = appendVerified(log, out, artistID, slotKey{"fanart", i}, p, i == 0, res)
	}

	for _, t := range singleSlotTypes {
		// Strict: a stat error means "cannot tell" and must not read as
		// "absent" (issue #1161). Absent is a no-op here anyway; the point is
		// that a flaky mount cannot produce a silently clean report.
		path, ok, statErr := img.FindExistingImageStrict(dir, img.FileNamesForType(img.DefaultFileNames, t))
		if statErr != nil {
			log.Warn("image repair: stat error probing image type, skipping type",
				slog.String("image_type", t), slog.Any("error", statErr))
			res.FilesSkipped++
			res.Outcomes = append(res.Outcomes, ImageRepairOutcome{
				ArtistID: artistID, ImageType: t, Action: "skipped", Reason: "stat_failed"})
			continue
		}
		if ok {
			out = appendVerified(log, out, artistID, slotKey{t, 0}, path, true, res)
		}
	}
	return out, nil
}

// appendVerified decodes the file at path and appends it to out, or records a
// skip. withMetadata controls whether dimensions and a placeholder are kept.
func appendVerified(log *slog.Logger, out []candidate, artistID string, key slotKey, path string, withMetadata bool, res *ImageRepairResult) []candidate {
	c, err := verifyImageFile(key, path, withMetadata)
	if err != nil {
		log.Warn("image repair: file failed verification, skipping slot",
			slog.String("image_type", key.imageType),
			slog.Int("slot_index", key.slotIndex),
			slog.String("path", path), slog.Any("error", err))
		res.FilesSkipped++
		res.Outcomes = append(res.Outcomes, ImageRepairOutcome{
			ArtistID: artistID, ImageType: key.imageType, SlotIndex: key.slotIndex,
			FileName: filepath.Base(path), Action: "skipped", Reason: "decode_failed"})
		return out
	}
	return append(out, c)
}

// verifyImageFile proves a file is a displayable image before any row is
// written for it.
//
// The gate is GeneratePlaceholder, which runs a FULL PIXEL DECODE. Not
// GetDimensions: that is image.DecodeConfig, header-only, and a JPEG truncated
// mid-scan passes it while reporting entirely plausible geometry, so a
// header-only check writes a correct-looking row for a file that cannot be
// displayed. Not HashFile either: it returns a usable content hash ALONGSIDE a
// perceptual-hash error, so a caller checking the value rather than the error
// would accept an undecodable file.
func verifyImageFile(key slotKey, path string, withMetadata bool) (candidate, error) {
	c := candidate{key: key, fileName: filepath.Base(path)}

	f, err := os.Open(path) //nolint:gosec // path comes from a listing of the artist's own image dir
	if err != nil {
		return c, fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close() //nolint:errcheck // read-only handle

	placeholder, err := img.GeneratePlaceholder(f, key.imageType)
	if err != nil {
		return c, fmt.Errorf("decoding %s: %w", path, err)
	}
	if !withMetadata {
		return c, nil
	}
	if _, err := f.Seek(0, 0); err != nil {
		return c, fmt.Errorf("rewinding %s: %w", path, err)
	}
	w, h, err := img.GetDimensions(f)
	if err != nil {
		return c, fmt.Errorf("reading dimensions of %s: %w", path, err)
	}
	c.width, c.height, c.placeholder = w, h, placeholder
	c.lowRes = img.IsLowResolution(w, h, key.imageType)
	return c, nil
}

// plan selects the verified files whose registry row is missing, recording a
// preview outcome for each. Discovery and verification have already run
// identically in both modes by this point, so the dry-run report IS the commit
// plan rather than an approximation of it.
//
// A file whose row already exists is left entirely alone. Re-creating a missing
// row is recoverable; overwriting an existing one -- which may hold a value a
// user set -- is not, so this pass never touches it.
func plan(artistID string, found []candidate, existing map[slotKey]bool, opts ImageRepairOpts, res *ImageRepairResult) (inserts []candidate) {
	for _, c := range found {
		if _, rowExists := existing[c.key]; rowExists {
			continue
		}
		inserts = append(inserts, c)
		res.RowsPlanned++
		res.Outcomes = append(res.Outcomes, outcomeFor(artistID, c, opts.Commit, "inserted", "would_insert", "row_missing"))
	}
	return inserts
}

func outcomeFor(artistID string, c candidate, commit bool, committed, preview, reason string) ImageRepairOutcome {
	action := preview
	if commit {
		action = committed
	}
	return ImageRepairOutcome{
		ArtistID: artistID, ImageType: c.key.imageType, SlotIndex: c.key.slotIndex,
		FileName: c.fileName, Action: action, Reason: reason,
	}
}

// applyInserts writes the missing rows. ON CONFLICT DO NOTHING carries two
// requirements in one clause: idempotency (a second pass writes nothing) and
// lock safety (on a conflict `locked` is untouched because nothing is). Both
// are enforced by the database, not by a Go branch a later edit could bypass.
func (s *Service) applyInserts(ctx context.Context, log *slog.Logger, artistID string, inserts []candidate) {
	now := time.Now().UTC().Format(time.RFC3339)
	for _, c := range inserts {
		_, err := s.db.ExecContext(ctx, `
			INSERT INTO artist_images
			  (id, artist_id, image_type, slot_index, exists_flag, low_res, placeholder,
			   width, height, phash, content_hash, file_format, source, last_written_at, locked)
			VALUES (?, ?, ?, ?, 1, ?, ?, ?, ?, '', '', '', 'repair', ?, 0)
			ON CONFLICT(artist_id, image_type, slot_index) DO NOTHING`,
			uuid.New().String(), artistID, c.key.imageType, c.key.slotIndex,
			boolToInt(c.lowRes), c.placeholder, c.width, c.height, now)
		if err != nil {
			log.Error("image repair: insert failed, row remains missing",
				slog.String("image_type", c.key.imageType),
				slog.Int("slot_index", c.key.slotIndex), slog.Any("error", err))
		}
	}
}

// confirm re-reads the artist's rows and counts what actually landed. This is
// the defense against "reports success while doing nothing": RowsInserted
// counts planned keys present now and absent before -- it is NOT derived from
// the plan size or from RowsAffected, so a pass whose inserts all silently
// no-op reports the divergence instead of hiding it.
func (s *Service) confirm(ctx context.Context, log *slog.Logger, artistID string, inserts []candidate, before map[slotKey]bool, res *ImageRepairResult) {
	after, err := s.existingSlots(ctx, artistID)
	if err != nil {
		// We wrote, but cannot prove what landed. Say that rather than
		// crediting the writes.
		log.Error("image repair: post-write re-read failed; writes unconfirmed", slog.Any("error", err))
		return
	}

	inserted := 0
	for _, c := range inserts {
		if _, wasThere := before[c.key]; wasThere {
			continue
		}
		if _, isThere := after[c.key]; isThere {
			inserted++
		}
	}
	res.RowsInserted += inserted

	if inserted != len(inserts) {
		log.Warn("image repair: planned and confirmed insert counts diverge",
			slog.Int("rows_planned", len(inserts)), slog.Int("rows_inserted", inserted))
	}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
