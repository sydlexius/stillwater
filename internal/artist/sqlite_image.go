package artist

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/sydlexius/stillwater/internal/dbutil"
)

type sqliteImageRepo struct {
	db *sql.DB
}

func newSQLiteImageRepo(db *sql.DB) *sqliteImageRepo {
	return &sqliteImageRepo{db: db}
}

func (r *sqliteImageRepo) GetForArtist(ctx context.Context, artistID string) ([]ArtistImage, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, artist_id, image_type, slot_index, exists_flag, low_res, placeholder,
			width, height, phash, content_hash, file_format, source, last_written_at, locked
		FROM artist_images WHERE artist_id = ? ORDER BY image_type, slot_index`,
		artistID)
	if err != nil {
		return nil, fmt.Errorf("getting images for artist %s: %w", artistID, err)
	}
	defer rows.Close() //nolint:errcheck // Close error not actionable on cleanup

	return scanImageRows(rows)
}

func (r *sqliteImageRepo) GetForArtists(ctx context.Context, artistIDs []string) (map[string][]ArtistImage, error) {
	if len(artistIDs) == 0 {
		return nil, nil
	}

	placeholders := make([]string, len(artistIDs))
	args := make([]any, len(artistIDs))
	for i, id := range artistIDs {
		placeholders[i] = "?"
		args[i] = id
	}

	query := `SELECT id, artist_id, image_type, slot_index, exists_flag, low_res, placeholder, ` + //nolint:gosec // G202: placeholders are "?" literals
		`width, height, phash, content_hash, file_format, source, last_written_at, locked ` +
		`FROM artist_images ` +
		`WHERE artist_id IN (` + strings.Join(placeholders, ",") + `) ` +
		`ORDER BY artist_id, image_type, slot_index`
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("batch getting images: %w", err)
	}
	defer rows.Close() //nolint:errcheck // Close error not actionable on cleanup

	result := make(map[string][]ArtistImage, len(artistIDs))
	for rows.Next() {
		var img ArtistImage
		var existsFlag, lowRes, locked int
		if err := rows.Scan(
			&img.ID, &img.ArtistID, &img.ImageType, &img.SlotIndex,
			&existsFlag, &lowRes, &img.Placeholder,
			&img.Width, &img.Height, &img.PHash, &img.ContentHash, &img.FileFormat, &img.Source,
			&img.LastWrittenAt, &locked,
		); err != nil {
			return nil, fmt.Errorf("scanning image row: %w", err)
		}
		img.Exists = existsFlag == 1
		img.LowRes = lowRes == 1
		img.Locked = locked == 1
		result[img.ArtistID] = append(result[img.ArtistID], img)
	}
	return result, rows.Err()
}

func (r *sqliteImageRepo) Upsert(ctx context.Context, img *ArtistImage) error {
	if img.ID == "" {
		img.ID = uuid.New().String()
	}

	// The INSERT persists the caller-supplied locked value for a brand-new row,
	// but ON CONFLICT deliberately omits locked from the SET list: a lock is
	// operator intent, changed only via SetLock. Without that omission any
	// refresh-shaped upsert whose caller left Locked at its zero value would
	// silently clear an operator's lock, exposing pinned artwork to the
	// auto-fix rules that delete files. The omission cuts both ways: a caller
	// passing Locked: true against an existing row also gets no lock, and no
	// error. Callers that mean to change a lock in either direction call
	// SetLock.
	//
	// Every other SET column except id is a full overwrite by design, because
	// the singular Upsert is a full-write path (unlike MergeAll/ReconcileAll,
	// which are fed by scans carrying only display fields). If a future change
	// needs to guard provenance or dimensions here, mirror their approach: exclude the
	// column outright, or gate it with a CASE WHEN that keeps the stored value.
	//
	// id = excluded.id below is NOT part of that design and is a known defect:
	// because an empty img.ID is filled with a fresh UUID above, a
	// refresh-shaped Upsert rotates an existing row's primary key. Any ID a
	// caller still holds then goes stale, including the one SetLock matches on,
	// so a lock toggle issued against the pre-refresh ID fails with ErrNotFound.
	// Tracked as its own issue; left in place here to keep this change scoped to
	// the locked column.
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO artist_images (id, artist_id, image_type, slot_index, exists_flag, low_res, placeholder,
			width, height, phash, content_hash, file_format, source, last_written_at, locked)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(artist_id, image_type, slot_index) DO UPDATE SET
			id = excluded.id,
			exists_flag = excluded.exists_flag,
			low_res = excluded.low_res,
			placeholder = excluded.placeholder,
			width = excluded.width,
			height = excluded.height,
			phash = excluded.phash,
			content_hash = excluded.content_hash,
			file_format = excluded.file_format,
			source = excluded.source,
			last_written_at = excluded.last_written_at`,
		img.ID, img.ArtistID, img.ImageType, img.SlotIndex,
		dbutil.BoolToInt(img.Exists), dbutil.BoolToInt(img.LowRes), img.Placeholder,
		img.Width, img.Height, img.PHash, img.ContentHash, img.FileFormat, img.Source, img.LastWrittenAt,
		dbutil.BoolToInt(img.Locked),
	)
	if err != nil {
		return fmt.Errorf("upserting image %s/%s: %w", img.ArtistID, img.ImageType, err)
	}
	return nil
}

// slotKey identifies one artist_images row within a single artist: the
// (image_type, slot_index) pair that the table is uniquely keyed on alongside
// artist_id.
type slotKey struct {
	imageType string
	slotIndex int
}

// destructiveImageRecord is one buffered attribution record describing an
// artist_images row deletion or an exists_flag clear. writeAll accumulates
// these while its transaction is open and emits them only once that
// transaction has committed, so a rolled-back transaction leaves no record
// behind (issue #2636).
type destructiveImageRecord struct {
	msg       string
	imageType string
	slotIndex int
}

// unattributedSource is the marker recorded when the calling context carries
// no explicit source tag. It is deliberately NOT the "manual" default that
// sourceFromContext applies for history rows: many automated callers reach
// the image write path through Service.Update without tagging a source, and recording
// those as "manual" would be a positive false claim that a human did the
// destroying. For an incident investigation, an explicit "unknown" is strictly
// more useful than a confident wrong answer (issue #2636).
const unattributedSource = "unattributed"

// logSourceFromContext returns the source tag to record on destructive image
// records: the explicitly-tagged value when the context carries one, and
// unattributedSource otherwise.
//
// This intentionally does NOT reuse sourceFromContext. That helper's "manual"
// default is load-bearing for the metadata_changes history table, whose
// accepted-source validation (history.go) does not include "unattributed";
// widening it there would corrupt history rows and fail validation. The two
// consumers want different answers for the same missing value, so they get
// different helpers.
func logSourceFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(sourceKey).(string); ok && v != "" {
		return v
	}
	return unattributedSource
}

// deleteStaleSlots removes the rows the caller's own filesystem enumeration
// proves cannot exist. It deletes stale rows individually rather than using a
// broad DELETE that could race with UpdateProvenance.
//
// Removal requires BOTH forms of evidence to agree (issue #2635):
//
//   - the caller enumerated the row's image type, and its count of files found
//     on disk puts the row's ordinal past the end (slot_index >= FoundSlots);
//   - and the incoming slice does not name the slot either.
//
// The first clause is the load-bearing one. Bounding by image type alone was
// not enough: within an enumerated type the incoming slice is re-derived from
// an Artist's flat fields and can under-report independently of disk, which is
// how an artist holding only fanart1.jpg lost every fanart row. Counting is a
// claim the caller can only make by having walked the directory, and it is
// exact, because slot_index is a DiscoverFanart ordinal.
//
// The second clause is deliberate redundancy rather than a second opinion:
// a slot the caller just wrote is never destroyed in the same call, whatever
// the count says.
//
// It returns one buffered record per deleted row for the caller to emit AFTER
// the transaction commits; it deliberately emits nothing itself, so a caller
// that rolls back leaves no phantom record behind (issue #2636).
func deleteStaleSlots(
	ctx context.Context,
	tx *sql.Tx,
	artistID string,
	priorExists map[slotKey]bool,
	incoming map[slotKey]struct{},
	enumerated map[string]int,
) ([]destructiveImageRecord, error) {
	var toRemove []slotKey
	for k := range priorExists {
		if _, ok := incoming[k]; ok {
			continue
		}
		found, probed := enumerated[k.imageType]
		if !probed {
			// The caller never looked at this type. Absence here is silence,
			// not evidence.
			continue
		}
		if k.slotIndex < found {
			// The caller counted a file at this ordinal. The incoming slice
			// failing to mention it is the slice being lossy, not the file
			// being gone -- keep the row.
			continue
		}
		toRemove = append(toRemove, k)
	}
	if len(toRemove) == 0 {
		return nil, nil
	}

	// Map iteration order is randomized; sort so deletions (and the records
	// they produce) are ordered stably and readably.
	sort.Slice(toRemove, func(i, j int) bool {
		if toRemove[i].imageType != toRemove[j].imageType {
			return toRemove[i].imageType < toRemove[j].imageType
		}
		return toRemove[i].slotIndex < toRemove[j].slotIndex
	})

	delStmt, err := tx.PrepareContext(ctx,
		`DELETE FROM artist_images WHERE artist_id = ? AND image_type = ? AND slot_index = ?`)
	if err != nil {
		return nil, fmt.Errorf("preparing delete for removed slots: %w", err)
	}
	defer delStmt.Close() //nolint:errcheck // Close error not actionable on cleanup

	records := make([]destructiveImageRecord, 0, len(toRemove))
	for _, k := range toRemove {
		if _, err := delStmt.ExecContext(ctx, artistID, k.imageType, k.slotIndex); err != nil {
			return nil, fmt.Errorf("deleting removed slot %s/%d: %w", k.imageType, k.slotIndex, err)
		}
		// One record per deleted slot. Deletions are rare, so the volume is
		// negligible, and per-row attributability is the whole point: the
		// #2636 incident destroyed rows that no log could account for.
		records = append(records, destructiveImageRecord{
			msg:       "artist image row deleted",
			imageType: k.imageType,
			slotIndex: k.slotIndex,
		})
	}
	return records, nil
}

// ErrNoImageEnumeration is returned by ReconcileAll when the caller supplied
// no filesystem enumeration, or supplied a malformed one.
//
// Deleting a row asserts that a file is gone. That assertion is only ever
// backed by a caller that walked the filesystem and looked, so ReconcileAll
// makes the caller state what it looked at and what it found. A caller that
// states nothing has looked at nothing, and its empty slot set carries no
// information -- which is exactly the shape of the #2635 incident, where an
// Artist struct nobody populated produced an empty slice and wiped the
// registry.
//
// The check is UNCONDITIONAL and runs before the transaction opens. An earlier
// form only fired when the artist happened to be holding rows that would have
// been deleted, which made the contract data-dependent: a caller that forgot
// its enumeration got a silent nil against a clean registry and only found out
// later, against an unlucky artist, in production. It also fired after every
// upsert in the transaction had already executed, so the rollback discarded
// writes that were perfectly legitimate. Validating at the entry point makes
// the refusal a property of the CALL rather than of the data it happens to
// meet.
var ErrNoImageEnumeration = errors.New("refusing to delete artist image rows: caller supplied no filesystem enumeration")

// CanonicalImageTypes are the image types the scanner's filesystem detection
// covers. A caller holding a fully detected Artist enumerates exactly these.
//
// Types outside this list (e.g. "poster") are written by other paths that the
// artist-directory walk does not probe for, so a scanner-driven reconcile has
// no evidence about them and must leave their rows alone. It is a list of
// TYPES, not a ready-made enumeration: an enumeration also needs the count of
// files found for each type, which only the code that walked the directory
// can supply.
var CanonicalImageTypes = []string{"banner", "fanart", "logo", "thumb"}

// MergeAll implements ImageRepository.MergeAll: it writes the named slots and
// leaves every absent slot untouched.
func (r *sqliteImageRepo) MergeAll(ctx context.Context, artistID string, images []ArtistImage) error {
	return r.writeAll(ctx, artistID, images, nil)
}

// ReconcileAll implements ImageRepository.ReconcileAll: it writes the named
// slots and deletes stored slots the caller's enumeration proves are gone.
func (r *sqliteImageRepo) ReconcileAll(ctx context.Context, artistID string, images []ArtistImage, enumerated []ImageEnumeration) error {
	scope, err := validateEnumeration(artistID, enumerated)
	if err != nil {
		return err
	}
	return r.writeAll(ctx, artistID, images, scope)
}

// validateEnumeration converts the caller's enumeration into the type -> count
// map writeAll consumes, rejecting anything that cannot bound a delete.
//
// Every rejection here is ErrNoImageEnumeration because they are all the same
// caller error wearing different clothes: an enumeration that does not
// describe a real filesystem walk. It is checked before any transaction opens
// so the refusal costs nothing and discards nothing (issue #2635).
func validateEnumeration(artistID string, enumerated []ImageEnumeration) (map[string]int, error) {
	if len(enumerated) == 0 {
		// Unlike the rejections below, this one cannot describe itself from the
		// enumeration -- there is no entry to name. The artist ID is the only
		// handle an operator has on which reconcile went wrong, and this is the
		// rejection most likely to actually fire in production, so it is the one
		// that most needs to say who asked and for what.
		return nil, fmt.Errorf("%w: artist %q supplied an empty enumeration", ErrNoImageEnumeration, artistID)
	}
	scope := make(map[string]int, len(enumerated))
	for _, e := range enumerated {
		if e.ImageType == "" {
			return nil, fmt.Errorf("%w: entry with empty image type", ErrNoImageEnumeration)
		}
		if e.FoundSlots < 0 {
			return nil, fmt.Errorf("%w: image type %q reports %d found slots", ErrNoImageEnumeration, e.ImageType, e.FoundSlots)
		}
		if prior, dup := scope[e.ImageType]; dup {
			// Two counts for one type means the caller walked the directory
			// twice and got different answers, or assembled the slice wrongly.
			// Either way there is no single count to bound a delete with, and
			// silently picking one would delete on an arbitrary choice.
			if prior != e.FoundSlots {
				return nil, fmt.Errorf("%w: image type %q enumerated twice with conflicting counts (%d and %d)",
					ErrNoImageEnumeration, e.ImageType, prior, e.FoundSlots)
			}
			continue
		}
		scope[e.ImageType] = e.FoundSlots
	}
	return scope, nil
}

// writeAll is the shared body of MergeAll and ReconcileAll. A nil enumeration
// selects the declarative contract (absence means nothing, delete nothing); a
// non-nil one selects the destructive contract, bounded by the per-type counts
// the caller measured on disk. MergeAll passes nil; ReconcileAll passes a map
// that validateEnumeration has already proved non-empty and well-formed, so
// "declared nothing" can never reach this far as an empty map.
func (r *sqliteImageRepo) writeAll(ctx context.Context, artistID string, images []ArtistImage, enumerated map[string]int) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // Rollback after commit success is a no-op; on error path the original error is what callers act on

	// Build a set of (image_type, slot_index) keys present in the incoming data
	// so we can mark absent slots as not-existing afterward.
	incoming := make(map[slotKey]struct{}, len(images))

	// Read the pre-upsert state of every row this artist already has. This read
	// MUST happen before the upsert loop below: the upsert overwrites
	// exists_flag unconditionally, so once it has run a 1 -> 0 transition is no
	// longer detectable. The same snapshot also drives the stale-row delete diff
	// further down. Moving the read earlier does not change which rows the diff
	// selects: every row the upsert loop inserts or updates has its key in
	// `incoming` by construction, so such rows could never have been selected
	// for removal by a post-upsert read either.
	priorExists := make(map[slotKey]bool)
	priorRows, err := tx.QueryContext(ctx,
		`SELECT image_type, slot_index, exists_flag FROM artist_images WHERE artist_id = ?`, artistID)
	if err != nil {
		return fmt.Errorf("querying existing image slots: %w", err)
	}
	defer priorRows.Close() //nolint:errcheck // Close error not actionable on cleanup
	for priorRows.Next() {
		var k slotKey
		var existsFlag int
		if err := priorRows.Scan(&k.imageType, &k.slotIndex, &existsFlag); err != nil {
			return fmt.Errorf("scanning existing image slot: %w", err)
		}
		priorExists[k] = existsFlag == 1
	}
	if err := priorRows.Err(); err != nil {
		return fmt.Errorf("iterating existing image slots: %w", err)
	}
	if err := priorRows.Close(); err != nil {
		return fmt.Errorf("closing existing image slot rows: %w", err)
	}

	// source identifies the calling path (scan, manual, rule:<id>, ...). It is
	// attached to every destructive record below so a row deletion or an
	// exists_flag clear can be attributed to the code path that decided it
	// (issue #2636). Note this is logSourceFromContext, NOT sourceFromContext:
	// an untagged context must record "unattributed" rather than inherit the
	// history layer's "manual" default, which would falsely blame a human.
	source := logSourceFromContext(ctx)

	// Destructive records are buffered, not emitted inline, and flushed only
	// after the transaction commits (issue #2636). Logging is a side effect of
	// the destruction, so it has to share the transaction's atomicity: any
	// failure below (a later Exec, a ctx cancellation, a Commit that hits
	// SQLITE_BUSY or a full disk) rolls the whole transaction back, and a
	// record already written to the log would then be a phantom claiming a
	// destruction that never happened. A phantom is worse than silence,
	// because the next incident investigation will trust it.
	var pending []destructiveImageRecord

	// Upsert each incoming image row. ON CONFLICT updates only display fields,
	// leaving provenance columns (phash, content_hash, source, file_format,
	// last_written_at) untouched so that UpdateProvenance and UpdateHashes data
	// survives. This is what makes the lazy hash backfill durable: a rescan
	// re-syncs the display fields without wiping the hashes it just computed,
	// so hashing stays a once-per-file cost rather than a once-per-scan one.
	upsertStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO artist_images (id, artist_id, image_type, slot_index, exists_flag, low_res, placeholder,
			width, height, phash, content_hash, file_format, source, last_written_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, '', '', '', '', '')
		ON CONFLICT(artist_id, image_type, slot_index) DO UPDATE SET
			exists_flag = excluded.exists_flag,
			low_res     = excluded.low_res,
			placeholder = excluded.placeholder,
			width  = CASE WHEN excluded.width  > 0 THEN excluded.width  ELSE artist_images.width  END,
			height = CASE WHEN excluded.height > 0 THEN excluded.height ELSE artist_images.height END`)
	if err != nil {
		return fmt.Errorf("preparing upsert: %w", err)
	}
	defer upsertStmt.Close() //nolint:errcheck // Close error not actionable on cleanup

	for i := range images {
		img := &images[i]
		id := img.ID
		if id == "" {
			id = uuid.New().String()
		}
		key := slotKey{img.ImageType, img.SlotIndex}
		incoming[key] = struct{}{}
		// Record every exists_flag 1 -> 0 transition. A cleared flag hides the
		// image in the UI and makes the artist look unarted, so the deciding
		// path has to be attributable after the fact (issue #2636).
		if priorExists[key] && !img.Exists {
			pending = append(pending, destructiveImageRecord{
				msg:       "artist image exists flag cleared",
				imageType: img.ImageType,
				slotIndex: img.SlotIndex,
			})
		}
		if _, err := upsertStmt.ExecContext(ctx,
			id, artistID, img.ImageType, img.SlotIndex,
			dbutil.BoolToInt(img.Exists), dbutil.BoolToInt(img.LowRes), img.Placeholder,
			img.Width, img.Height,
		); err != nil {
			return fmt.Errorf("upserting image %s/%d: %w", img.ImageType, img.SlotIndex, err)
		}
	}

	// Absence is only actionable under the reconcile contract, where the
	// caller has enumerated the filesystem. Under MergeAll (enumerated == nil)
	// an absent slot means the caller said nothing about it, so the row stands.
	//
	// The delete runs inside the transaction, against the snapshot read above,
	// so the rows it acts on are exactly the rows that were there when the
	// diff was computed.
	if enumerated != nil {
		deleted, err := deleteStaleSlots(ctx, tx, artistID, priorExists, incoming, enumerated)
		if err != nil {
			return err
		}
		pending = append(pending, deleted...)
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	// Past this point the destruction is durable, so the records are true.
	// They are emitted at Warn, not Info: the deployed default level is Info
	// and the live level is operator-adjustable from the Logs settings tab, so
	// a routine noise-reduction bump to Warn would silently disable the entire
	// attribution mechanism -- exactly when the added per-row volume makes
	// that bump most tempting. These are rare destructive events that exist
	// only for post-hoc forensics, so they are emitted at the level that
	// survives.
	for _, rec := range pending {
		slog.Warn(rec.msg,
			"artist_id", artistID,
			"image_type", rec.imageType,
			"slot_index", rec.slotIndex,
			"source", source)
	}
	return nil
}

// UpdateProvenance updates only the provenance-related fields (phash,
// content_hash, source, file_format, last_written_at) on an existing
// artist_images row, identified by artist_id + image_type + slot_index. This is
// a targeted update that does not touch display fields (exists_flag, low_res,
// placeholder, dimensions). Returns an error if no matching row exists.
func (r *sqliteImageRepo) UpdateProvenance(ctx context.Context, artistID, imageType string, slotIndex int, phash, contentHash, source, fileFormat, lastWrittenAt string) error {
	result, err := r.db.ExecContext(ctx, `
		UPDATE artist_images
		SET phash = ?, content_hash = ?, source = ?, file_format = ?, last_written_at = ?
		WHERE artist_id = ? AND image_type = ? AND slot_index = ?`,
		phash, contentHash, source, fileFormat, lastWrittenAt,
		artistID, imageType, slotIndex,
	)
	if err != nil {
		return fmt.Errorf("updating image provenance %s/%s/%d: %w", artistID, imageType, slotIndex, err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking rows affected for provenance %s/%s/%d: %w", artistID, imageType, slotIndex, err)
	}
	if n == 0 {
		return fmt.Errorf("no image row found for %s/%s/%d", artistID, imageType, slotIndex)
	}
	return nil
}

// UpdateHashes writes only the two hash columns for an existing artist_images
// row. It exists alongside UpdateProvenance for the lazy-backfill path, which
// hashes a file that Stillwater did not necessarily write and therefore knows
// nothing about its source: routing that through UpdateProvenance would blank
// the source, file_format, and last_written_at of an already-provenanced row.
//
// A zero-row update means the slot was removed or renumbered by a concurrent
// scan between detection and persistence. That is a benign race, not a
// corruption, so it is reported as ErrNotFound for the caller to log and skip
// rather than treated as a failure of the evaluation.
func (r *sqliteImageRepo) UpdateHashes(ctx context.Context, artistID, imageType string, slotIndex int, phash, contentHash string) error {
	result, err := r.db.ExecContext(ctx, `
		UPDATE artist_images
		SET phash = ?, content_hash = ?
		WHERE artist_id = ? AND image_type = ? AND slot_index = ?`,
		phash, contentHash, artistID, imageType, slotIndex,
	)
	if err != nil {
		return fmt.Errorf("updating image hashes %s/%s/%d: %w", artistID, imageType, slotIndex, err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking rows affected for hashes %s/%s/%d: %w", artistID, imageType, slotIndex, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: image row %s/%s/%d", ErrNotFound, artistID, imageType, slotIndex)
	}
	return nil
}

// ClearHashesForType blanks phash and content_hash for every slot of one image
// type belonging to one artist, returning them to the "not yet hashed" state
// that a fresh row starts in. The next duplicate evaluation re-derives them from
// the files on disk.
//
// It is deliberately whole-type rather than per-slot. The operations that
// require it -- renumbering, reordering, deleting a slot -- shift files ACROSS
// slots, so the set of rows whose file changed identity is precisely the set
// this cannot cheaply enumerate. Clearing the type costs one re-read per file on
// the next evaluation; getting the enumeration subtly wrong costs a file.
//
// A zero-row update is not an error: an artist with no rows of this type has no
// stale hashes by definition.
func (r *sqliteImageRepo) ClearHashesForType(ctx context.Context, artistID, imageType string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE artist_images SET phash = '', content_hash = ''
		WHERE artist_id = ? AND image_type = ?`,
		artistID, imageType,
	)
	if err != nil {
		return fmt.Errorf("clearing image hashes for %s/%s: %w", artistID, imageType, err)
	}
	return nil
}

// ClearExistsFlag sets exists_flag=0 for the given artist/image_type/slot.
// This is a best-effort update used when a previously existing image file is
// confirmed missing on disk, so that subsequent UI renders show a placeholder
// instead of a broken image.
//
// A 1 -> 0 transition emits the same attributed record the writeAll path emits
// for the same event (issue #2636). It has to, because the two paths are
// reachable from ONE operator action: persistImageFlag routes an exists=false
// write here, but routes it through writeAll instead whenever the artist
// happens to carry a non-empty placeholder, since that makes
// extractImageMetadata emit a row. Without this the attribution for "the UI
// stopped showing this artwork" depended on whether a placeholder string was
// set -- present for some artists, absent for others, for reasons unrelated to
// the deletion. An incident timeline with holes of unpredictable shape is
// worse than one with none, because the holes read as evidence of absence.
//
// The read and the write share a transaction so the transition is decided on
// the same row the update lands on, and the record is emitted only after the
// commit makes it true.
func (r *sqliteImageRepo) ClearExistsFlag(ctx context.Context, artistID, imageType string, slotIndex int) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx clearing exists flag for %s/%s/%d: %w", artistID, imageType, slotIndex, err)
	}
	defer tx.Rollback() //nolint:errcheck // Rollback after a successful commit is a no-op; on the error path the original error is what callers act on

	var priorExists int
	err = tx.QueryRowContext(ctx, `
		SELECT exists_flag FROM artist_images
		WHERE artist_id = ? AND image_type = ? AND slot_index = ?`,
		artistID, imageType, slotIndex,
	).Scan(&priorExists)
	if errors.Is(err, sql.ErrNoRows) {
		// No row to clear. Not an error: the caller is asserting a file is
		// missing, and no row already agrees with that.
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading exists flag for %s/%s/%d: %w", artistID, imageType, slotIndex, err)
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE artist_images SET exists_flag = 0
		WHERE artist_id = ? AND image_type = ? AND slot_index = ?`,
		artistID, imageType, slotIndex,
	); err != nil {
		return fmt.Errorf("clearing exists flag for %s/%s/%d: %w", artistID, imageType, slotIndex, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing exists flag clear for %s/%s/%d: %w", artistID, imageType, slotIndex, err)
	}

	if priorExists == 1 {
		slog.Warn("artist image exists flag cleared",
			"artist_id", artistID,
			"image_type", imageType,
			"slot_index", slotIndex,
			"source", logSourceFromContext(ctx))
	}
	return nil
}

// RestoreExistsFlag sets exists_flag=1 for the given artist/image_type/slot,
// but ONLY when the row currently reads exists_flag=0. It is the monotone
// inverse of ClearExistsFlag (issue #2668): the caller has confirmed the slot's
// file is present on disk, so a stale exists_flag=0 -- which hides live artwork
// behind a placeholder -- is corrected to 1.
//
// The restore is deliberately monotone (0 -> 1 only, via the AND exists_flag = 0
// clause) and locked is deliberately ABSENT from the SET list. exists_flag is a
// PHYSICAL fact about the filesystem; a lock governs a row's CONTENT, not its
// physical existence. Restoring the physical fact on a locked row is therefore
// correct and required -- the lock is not evidence the file is gone -- and the
// restore must fire on a locked row while never touching the locked column or
// any other field (design-lock #3). Leaving locked out of the write is what
// keeps those two concerns separate.
//
// A genuine 0 -> 1 transition emits an attributed record mirroring the clear
// path's forensic record (issue #2636). RowsAffected -- not the pre-read value
// -- gates the record: the monotone WHERE clause means the UPDATE changes a row
// only when it was 0, so exactly one row affected IS the transition. Nothing is
// emitted when the row already reads 1 (the guard makes the UPDATE a no-op) or
// when no such row exists.
//
// The read and the write share a transaction so the decision lands on the same
// row the update mutates, and the record is emitted only after the commit makes
// it true. sql.ErrNoRows is a no-op returning nil: the caller is asserting a
// file is present, and there is no row to carry that fact (inserting one is
// #2670, out of scope here).
func (r *sqliteImageRepo) RestoreExistsFlag(ctx context.Context, artistID, imageType string, slotIndex int) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx restoring exists flag for %s/%s/%d: %w", artistID, imageType, slotIndex, err)
	}
	defer tx.Rollback() //nolint:errcheck // Rollback after a successful commit is a no-op; on the error path the original error is what callers act on

	// SELECT first so a missing row is a clean no-op rather than an error, and
	// so the decision is made against the same row the UPDATE lands on. Mirrors
	// ClearExistsFlag.
	var priorExists int
	err = tx.QueryRowContext(ctx, `
		SELECT exists_flag FROM artist_images
		WHERE artist_id = ? AND image_type = ? AND slot_index = ?`,
		artistID, imageType, slotIndex,
	).Scan(&priorExists)
	if errors.Is(err, sql.ErrNoRows) {
		// No row to restore. Not an error: the caller is asserting a file is
		// present, and no row exists to carry that fact.
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading exists flag for %s/%s/%d: %w", artistID, imageType, slotIndex, err)
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE artist_images SET exists_flag = 1
		WHERE artist_id = ? AND image_type = ? AND slot_index = ? AND exists_flag = 0`,
		artistID, imageType, slotIndex,
	)
	if err != nil {
		return fmt.Errorf("restoring exists flag for %s/%s/%d: %w", artistID, imageType, slotIndex, err)
	}
	restored, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("reading rows affected restoring exists flag for %s/%s/%d: %w", artistID, imageType, slotIndex, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing exists flag restore for %s/%s/%d: %w", artistID, imageType, slotIndex, err)
	}

	// restored == 1 is the genuine 0 -> 1 transition (the monotone guard cannot
	// affect a row that was already 1), so the record describes a real change.
	if restored == 1 {
		slog.Info("artist image exists flag restored",
			"artist_id", artistID,
			"image_type", imageType,
			"slot_index", slotIndex,
			"source", logSourceFromContext(ctx))
	}
	return nil
}

// SetLock toggles the lock flag for a single image row identified by its
// primary key. Returns an error if no matching row exists.
func (r *sqliteImageRepo) SetLock(ctx context.Context, imageID string, locked bool) error {
	result, err := r.db.ExecContext(ctx,
		`UPDATE artist_images SET locked = ? WHERE id = ?`,
		dbutil.BoolToInt(locked), imageID,
	)
	if err != nil {
		return fmt.Errorf("setting image lock %s: %w", imageID, err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("reading image lock rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: image %s", ErrNotFound, imageID)
	}
	return nil
}

func (r *sqliteImageRepo) DeleteByArtistID(ctx context.Context, artistID string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM artist_images WHERE artist_id = ?`, artistID)
	if err != nil {
		return fmt.Errorf("deleting images for artist %s: %w", artistID, err)
	}
	return nil
}

// NewestWriteTimesByArtist returns a map of artist_id to their most recent
// last_written_at timestamp string for all artists in the given library.
// Only artists with at least one non-empty last_written_at are included.
func (r *sqliteImageRepo) NewestWriteTimesByArtist(ctx context.Context, libraryID string) (map[string]string, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT a.id, MAX(ai.last_written_at)
		FROM artist_images ai
		JOIN artists a ON ai.artist_id = a.id
		JOIN artist_libraries al ON al.artist_id = a.id
		WHERE al.library_id = ?
		AND ai.last_written_at != ''
		GROUP BY a.id`,
		libraryID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying newest write times by artist for library %s: %w", libraryID, err)
	}
	defer rows.Close() //nolint:errcheck // Close error not actionable on cleanup

	result := make(map[string]string)
	for rows.Next() {
		var artistID, maxWriteTime string
		if err := rows.Scan(&artistID, &maxWriteTime); err != nil {
			return nil, fmt.Errorf("scanning newest write time row: %w", err)
		}
		result[artistID] = maxWriteTime
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating newest write time rows: %w", err)
	}
	return result, nil
}

// AllFanartHashes loads artist_id/phash for every exists_flag=1 fanart row in
// the library, unfiltered by artist. See ImageRepository.AllFanartHashes.
func (r *sqliteImageRepo) AllFanartHashes(ctx context.Context) ([]FanartHashRow, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT artist_id, phash FROM artist_images
		 WHERE exists_flag = 1 AND image_type = 'fanart'
		 ORDER BY artist_id, slot_index`)
	if err != nil {
		return nil, fmt.Errorf("querying fanart hashes: %w", err)
	}
	defer rows.Close() //nolint:errcheck // Close error not actionable on cleanup

	var out []FanartHashRow
	for rows.Next() {
		var row FanartHashRow
		if err := rows.Scan(&row.ArtistID, &row.PHashHex); err != nil {
			return nil, fmt.Errorf("scanning fanart hash row: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating fanart hash rows: %w", err)
	}
	return out, nil
}

func scanImageRows(rows *sql.Rows) ([]ArtistImage, error) {
	var images []ArtistImage
	for rows.Next() {
		var img ArtistImage
		var existsFlag, lowRes, locked int
		if err := rows.Scan(
			&img.ID, &img.ArtistID, &img.ImageType, &img.SlotIndex,
			&existsFlag, &lowRes, &img.Placeholder,
			&img.Width, &img.Height, &img.PHash, &img.ContentHash, &img.FileFormat, &img.Source,
			&img.LastWrittenAt, &locked,
		); err != nil {
			return nil, fmt.Errorf("scanning image: %w", err)
		}
		img.Exists = existsFlag == 1
		img.LowRes = lowRes == 1
		img.Locked = locked == 1
		images = append(images, img)
	}
	return images, rows.Err()
}
