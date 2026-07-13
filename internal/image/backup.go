package image

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/sydlexius/stillwater/internal/filesystem"
)

// backupImageTypes is the closed set of artwork kinds that may key a backup
// path. It mirrors the request-input allowlist enforced at the API boundary
// (handlers_image.go) so the constraint is re-asserted at this lower layer:
// imageType is the only segment of a backup path that originates from caller
// input, and backupTypeDir is the single chokepoint where it is joined into a
// filesystem path.
var backupImageTypes = map[string]bool{
	"thumb":  true,
	"fanart": true,
	"logo":   true,
	"banner": true,
}

// BackupDirName is the hidden subdirectory inside an artist folder where the
// pre-edit original of a single-slot image (thumb/logo/banner) is kept so a
// destructive edit (crop, auto-trim, replace) is revertible exactly one step.
//
// It is a dotfile-prefixed SUBDIRECTORY, not a sibling file, which makes it
// peer-inert: Emby, Jellyfin, and Kodi scan the artist folder's top level for a
// fixed allowlist of artwork basenames and never recurse into a hidden subdir,
// so the backup is never picked up as artwork. A subdir also cannot collide with
// the atomic writer's transient "<target>.tmp"/".bak"/".removing" temporaries,
// which are always written next to the target file, never inside this dir.
const BackupDirName = ".sw-backup"

// backupTypeDir returns the per-type backup subdirectory for an image type. Each
// single-slot kind (thumb/logo/banner) gets its own subdir so the backup
// identity is keyed by image TYPE, not by the file's basename-with-extension.
// This makes the backup format-INDEPENDENT: a png->jpg crop still finds its
// backup after Save deletes the old png and writes a jpg (#1837).
//
// PATH-SANITIZATION GUARD: imageType is validated against the closed artwork-kind
// allowlist AND rejected if it is empty, contains a path separator, or contains
// "..", BEFORE it is joined into the path. This dominates every os.* sink reached
// through the returned dir (ReadDir, Remove, MkdirAll, WriteFileAtomic), so a
// tainted type can never traverse out of the .sw-backup subtree. It is
// defense-in-depth: the API boundary already allowlists imageType, but
// re-asserting it here fails closed at the filesystem layer and gives static
// analysis a recognizable sanitizer for the go/path-injection class.
func backupTypeDir(dir, imageType string) (string, error) {
	if !backupImageTypes[imageType] ||
		imageType == "" ||
		strings.ContainsRune(imageType, os.PathSeparator) ||
		strings.ContainsRune(imageType, '/') ||
		strings.Contains(imageType, "..") {
		return "", fmt.Errorf("invalid image type %q for backup path", imageType)
	}
	return filepath.Join(dir, BackupDirName, imageType), nil
}

// findBackupFile returns the single backup file path for an image type, or "" if
// none exists. Exactly one file lives in the per-type dir (one-deep), so the
// first regular entry found is the backup. The returned path preserves the
// ORIGINAL basename (and thus the original format).
func findBackupFile(dir, imageType string) (string, error) {
	typeDir, err := backupTypeDir(dir, imageType)
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(typeDir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("reading backup dir: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		return filepath.Join(typeDir, e.Name()), nil
	}
	return "", nil
}

// pruneBackupFiles removes backup file(s) for an image type, except the file
// whose basename equals keep, so exactly one backup remains (one-deep,
// format-independent). Pass keep == "" to remove every backup file (the
// post-restore consume path). A missing per-type dir is not an error.
func pruneBackupFiles(dir, imageType, keep string) error {
	typeDir, err := backupTypeDir(dir, imageType)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(typeDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading backup dir for cleanup: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || e.Name() == keep {
			continue
		}
		if rmErr := os.Remove(filepath.Join(typeDir, e.Name())); rmErr != nil && !os.IsNotExist(rmErr) {
			return fmt.Errorf("removing prior backup: %w", rmErr)
		}
	}
	return nil
}

// BackupSingleSlot copies the current PRIMARY original of a single-slot image
// type into the hidden per-type backup subdirectory, preserving the original
// basename (and thus the original format). It is keyed by image TYPE so the
// backup survives a format-changing edit (e.g. png->jpg crop). It is one-deep
// PER TYPE: any prior backup file for this type (in any format) is removed first
// so exactly one remains.
//
// It is a no-op (returns nil) when no original file exists yet for the type, so
// a first write of a kind that had no prior image leaves no backup to revert to.
//
// naming is the configured filename list for imageType (the type's canonical
// names); the PRIMARY existing original is located by probing those names plus
// alternate extensions, matching FindExistingImageStrict semantics.
func BackupSingleSlot(dir, imageType string, naming []string) error {
	// Strict probe: a transient stat error must not be mistaken for "absent",
	// which would silently skip the backup and let the destructive caller
	// overwrite a still-present original (#1161).
	existing, found, statErr := FindExistingImageStrict(dir, naming)
	if statErr != nil {
		return fmt.Errorf("probing original for backup: %w", statErr)
	}
	if !found {
		// Nothing to back up -- but DROP any stale backup for this type. Otherwise a
		// backup left by an earlier overwrite outlives the image itself: the user
		// deletes the artwork, a later save fails, and the rollback happily restores
		// the deleted image from that stale backup. Artwork the user threw away comes
		// back on its own (#2413).
		return pruneBackupFiles(dir, imageType, "")
	}

	data, err := os.ReadFile(existing) //nolint:gosec // existing built from trusted naming patterns
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading original for backup: %w", err)
	}

	typeDir, err := backupTypeDir(dir, imageType)
	if err != nil {
		return err
	}
	backup := filepath.Join(typeDir, filepath.Base(existing))
	if mkErr := os.MkdirAll(filepath.Dir(backup), 0o750); mkErr != nil {
		return fmt.Errorf("creating backup dir: %w", mkErr)
	}
	// Write the fresh backup BEFORE pruning any prior backup. If the write fails
	// (disk full, permission, transient IO) the caller aborts the destructive
	// save, and because the previous backup is still present the edit keeps a
	// valid revert path. Pruning first would delete the only recoverable original
	// on a failed refresh, leaving nothing to revert to (Qodo #1839 reliability).
	if writeErr := filesystem.WriteFileAtomic(backup, data, 0o644); writeErr != nil {
		return fmt.Errorf("writing backup: %w", writeErr)
	}
	// One-deep: now that the fresh backup is durable, drop any other (older,
	// possibly different-format) backup files for this type so exactly one remains.
	if rmErr := pruneBackupFiles(dir, imageType, filepath.Base(backup)); rmErr != nil {
		return rmErr
	}
	return nil
}

// HasBackup reports whether a one-deep backup exists for the image type.
func HasBackup(dir, imageType string) bool {
	path, err := findBackupFile(dir, imageType)
	return err == nil && path != ""
}

// RestoreSingleSlot restores the most recent pre-edit original for a single-slot
// image type by routing the backup bytes back through Save, so ALL configured
// names and symlinks are rebuilt and Save's CleanupConflictingFormats drops the
// now-stale post-edit format (e.g. the jpg written over a png-original crop).
// After a successful restore it removes the backup so revert is one-shot (a
// second revert finds no backup).
//
// Returns the bare os.ErrNotExist sentinel when no backup exists, so callers can
// map it to 404 via errors.Is / os.IsNotExist.
func RestoreSingleSlot(dir, imageType string, naming []string, useSymlinks bool, meta *ExifMeta, logger *slog.Logger) error {
	backup, err := findBackupFile(dir, imageType)
	if err != nil {
		return fmt.Errorf("locating backup: %w", err)
	}
	if backup == "" {
		return os.ErrNotExist
	}

	data, err := os.ReadFile(backup) //nolint:gosec // backup path derived from trusted naming patterns
	if err != nil {
		if os.IsNotExist(err) {
			return os.ErrNotExist
		}
		return fmt.Errorf("reading backup: %w", err)
	}

	// Route through Save so every canonical name + symlink is rebuilt from the
	// original bytes and CleanupConflictingFormats removes the post-edit format.
	if _, saveErr := Save(dir, imageType, data, naming, useSymlinks, meta, logger); saveErr != nil {
		return fmt.Errorf("restoring via save: %w", saveErr)
	}

	// Consume the backup (and drop any siblings, defensively) so a repeat revert
	// is a clean no-backup case. This is BEST-EFFORT: Save above has already
	// rewritten the canonical image, so the revert has succeeded on disk. Failing
	// here would make the handler return 500 and skip sync/event side effects even
	// though the file already changed. A leftover backup is harmless (a repeat
	// revert just re-restores the same original), so we log and continue (CR #1839).
	if rmErr := pruneBackupFiles(dir, imageType, ""); rmErr != nil && logger != nil {
		logger.Warn("consuming backup after successful restore failed (revert already applied on disk)",
			slog.String("dir", dir),
			slog.String("image_type", imageType),
			slog.String("error", rmErr.Error()))
	}
	return nil
}

// slotBase is the extension-less name that identifies one image slot. fanart.jpg
// and fanart.png are the same slot in two formats; fanart1.jpg is a different slot.
func slotBase(fileName string) string {
	return strings.TrimSuffix(fileName, filepath.Ext(fileName))
}

// pruneSlotBackups drops backups belonging to ONE slot (same base name, any
// extension), leaving every other slot's backup alone. keep is a basename to
// preserve, or "" to drop them all for that slot.
//
// BackupSingleSlot's pruneBackupFiles is one-deep PER TYPE: it deletes everything in
// the type's backup dir except the file it just wrote. That is correct for a
// genuinely single-slot type (thumb/logo/banner) and CATASTROPHIC for a multi-slot
// one -- backing up fanart1.jpg would delete the primary fanart.jpg's backup. Hence
// this slot-scoped variant.
func pruneSlotBackups(dir, imageType, fileName, keep string) error {
	typeDir, err := backupTypeDir(dir, imageType)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(typeDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading backup dir for slot cleanup: %w", err)
	}
	base := slotBase(fileName)
	for _, e := range entries {
		if e.IsDir() || e.Name() == keep || slotBase(e.Name()) != base {
			continue
		}
		if rmErr := os.Remove(filepath.Join(typeDir, e.Name())); rmErr != nil && !os.IsNotExist(rmErr) {
			return fmt.Errorf("removing prior slot backup: %w", rmErr)
		}
	}
	return nil
}

// BackupSlot backs up ONE named image file before a destructive write to it, so the
// edit is revertible and a failed save can be rolled back.
//
// This is the multi-slot counterpart to BackupSingleSlot. Fanart is multi-slot and
// its slot 0 IS the primary backdrop (see FanartFilename), so every per-slot
// overwrite -- crop, fetch/replace, assign, import -- destroys a real image and needs
// this (#2413).
//
// Strict, like BackupSingleSlot (#1161): a stat error is not mistaken for "absent",
// so the caller can ABORT rather than destroy an original it could not protect.
//
// When no original exists, any STALE backup for this slot is pruned. Without that, a
// backup left behind by an earlier overwrite could be restored over a slot the user
// has since DELETED -- resurrecting artwork they threw away.
func BackupSlot(dir, imageType, fileName string) error {
	existing, found, statErr := FindExistingImageStrict(dir, []string{fileName})
	if statErr != nil {
		return fmt.Errorf("probing slot original for backup: %w", statErr)
	}
	if !found {
		// Nothing to back up. Drop any stale backup for this slot so a later failed
		// save cannot roll back to an image the user has deleted.
		return pruneSlotBackups(dir, imageType, fileName, "")
	}

	data, err := os.ReadFile(existing) //nolint:gosec // existing built from trusted naming patterns
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading slot original for backup: %w", err)
	}

	typeDir, err := backupTypeDir(dir, imageType)
	if err != nil {
		return err
	}
	backup := filepath.Join(typeDir, filepath.Base(existing))
	if mkErr := os.MkdirAll(filepath.Dir(backup), 0o750); mkErr != nil {
		return fmt.Errorf("creating backup dir: %w", mkErr)
	}
	// Write the fresh backup BEFORE pruning the prior one, so a failed write leaves
	// the previous recoverable original in place (same ordering as BackupSingleSlot).
	if writeErr := filesystem.WriteFileAtomic(backup, data, 0o644); writeErr != nil {
		return fmt.Errorf("writing slot backup: %w", writeErr)
	}
	return pruneSlotBackups(dir, imageType, fileName, filepath.Base(backup))
}

// RestoreSlot puts ONE named slot back from its own backup, used to roll back a
// failed destructive write. Returns os.ErrNotExist when this slot has no backup (a
// first-ever write, which lost nothing).
//
// Routes the bytes through Save so CleanupConflictingFormats drops the half-written
// post-edit format -- a png-original overwritten with jpeg data restores the png AND
// removes the jpg, rather than leaving both.
func RestoreSlot(dir, imageType, fileName string, meta *ExifMeta, logger *slog.Logger) error {
	typeDir, err := backupTypeDir(dir, imageType)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(typeDir)
	if err != nil {
		if os.IsNotExist(err) {
			return os.ErrNotExist
		}
		return fmt.Errorf("reading slot backup dir: %w", err)
	}
	base := slotBase(fileName)
	var backupName string
	for _, e := range entries {
		if !e.IsDir() && slotBase(e.Name()) == base {
			backupName = e.Name()
			break
		}
	}
	if backupName == "" {
		return os.ErrNotExist
	}

	data, err := os.ReadFile(filepath.Join(typeDir, backupName)) //nolint:gosec // path derived from trusted naming
	if err != nil {
		if os.IsNotExist(err) {
			return os.ErrNotExist
		}
		return fmt.Errorf("reading slot backup: %w", err)
	}
	if _, saveErr := Save(dir, imageType, data, []string{backupName}, false, meta, logger); saveErr != nil {
		return fmt.Errorf("restoring slot via save: %w", saveErr)
	}
	// Consume the backup. Best-effort: the restore has already landed on disk.
	if rmErr := pruneSlotBackups(dir, imageType, fileName, ""); rmErr != nil && logger != nil {
		logger.Warn("consuming slot backup after successful restore failed (restore already applied on disk)",
			slog.String("dir", dir), slog.String("slot", fileName), slog.String("error", rmErr.Error()))
	}
	return nil
}

// slotMu holds one mutex per image SLOT, serializing SaveSlotProtected's
// backup -> save -> rollback sequence against a concurrent write to the SAME slot.
//
// The key is (dir, imageType, slot base name) -- the three things that together
// name one file on disk and one backup entry under .sw-backup/<type>/. It is
// deliberately NOT the artist: two writes to fanart1 and fanart2 of the same artist
// touch different files and different backups, and must run in parallel. And it is
// deliberately the extension-LESS base (slotBase), because fanart.png and fanart.jpg
// are the same slot in two formats -- Save's CleanupConflictingFormats has each one
// deleting the other, so keying on the full basename would let them race.
//
// The key is built with a NUL separator, which cannot occur in a path segment or an
// image type, so no (dir, type, slot) triple can be confused for another by
// concatenation.
//
// Entries are never evicted: one mutex per slot ever written, for the process
// lifetime. That is the same unbounded-sync.Map shape as this repo's existing
// per-ID lock idiom (Router.stillwaterManagedMu in internal/api), and it is bounded
// in practice by the number of image slots in the library -- a few bytes each, only
// for slots actually written. An eviction scheme would need its own lock to close
// the evict-while-acquiring race, which costs more than it saves.
var slotMu sync.Map // map[string]*sync.Mutex

// slotMutex returns the one mutex guarding a slot, creating it on first use.
// LoadOrStore guarantees every caller for a given slot gets the SAME mutex even
// when several arrive at once.
func slotMutex(dir, imageType, fileName string) *sync.Mutex {
	key := filepath.Clean(dir) + "\x00" + imageType + "\x00" + slotBase(fileName)
	mu, _ := slotMu.LoadOrStore(key, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

// SaveSlotProtected performs a DESTRUCTIVE write to ONE image slot: it backs the
// existing image up first (BackupSlot), and puts it back (RestoreSlot) if the save
// fails. It is THE chokepoint every destructive slot write must go through.
//
// Fanart slot 0 IS the primary backdrop (FanartFilename returns the primary name
// verbatim for index 0), and every numbered slot is a real image the user chose. So
// crop, fetch/replace, assign and import all destroy artwork when they overwrite a
// slot -- and every one of them used to call Save directly, with no backup and no
// rollback (#2413).
//
// The backup is SLOT-SCOPED: backing up fanart1.jpg must not delete the primary's
// backup, which the one-deep per-type prune in BackupSingleSlot would.
//
// A backup that cannot be taken ABORTS the save (#1161). We never destroy an original
// we could not protect.
//
// It lives HERE, in internal/image, rather than on the API router, because its own
// primitives do and because callers outside internal/api (the rule engine's fixers)
// must be able to reach it. There is exactly one implementation of this policy.
//
// naming[0] is the slot's canonical name and is the backup key. THAT NAME IS ALL
// THE BACKUP COVERS. Save writes every name in naming, but only naming[0] is
// backed up, so a rollback restores only naming[0]; any other configured name is
// left holding the failed write's bytes (#2434). Nothing today configures a
// second fanart name, which is why that gap is tracked rather than fixed here --
// but do not read this function as protecting names it does not.
//
// CONCURRENCY: the whole backup -> save -> rollback sequence is ONE critical
// section, serialized per slot (see slotMu). Without that, two concurrent writes
// to the same slot interleave their steps and the ROLLBACK becomes the very thing
// that destroys data: the loser's RestoreSlot puts stale bytes back OVER the
// winner's successful write, and either racer's BackupSlot can clobber the other's
// backup. Different slots (fanart vs fanart1) never contend; the same slot always
// does.
//
// A nil logger is accepted: it falls back to slog.Default(), since this is a
// cross-package entry point (internal/rule and others call it without
// necessarily holding a logger) and a nil logger must never panic on the
// failed-rollback path.
func SaveSlotProtected(dir, imageType string, naming []string, data []byte, meta *ExifMeta, logger *slog.Logger) ([]string, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if len(naming) == 0 {
		return nil, fmt.Errorf("no filenames configured for image type %q", imageType)
	}
	targetName := naming[0]

	// Take the slot lock BEFORE the backup and hold it through the rollback. The
	// lock lives here, at the chokepoint, and not in the caller: a lock a caller
	// must remember to take is a lock the next caller forgets, and a second caller
	// (the rule engine) is already queued behind this (#2433).
	mu := slotMutex(dir, imageType, targetName)
	mu.Lock()
	defer mu.Unlock()

	if bErr := BackupSlot(dir, imageType, targetName); bErr != nil {
		return nil, fmt.Errorf("backing up the %s slot before overwrite (aborting destructive save): %w", imageType, bErr)
	}
	saved, saveErr := Save(dir, imageType, data, naming, false, meta, logger)
	if saveErr == nil {
		return saved, nil
	}
	// The save failed after a successful backup. Put the original back rather than
	// leaving the slot empty or half-written -- Save's CleanupConflictingFormats has
	// already DELETED the original's other-format file by the time a write can fail,
	// so without this the artwork is simply gone. A first-ever write has no backup,
	// and that is not a failed rollback -- nothing was lost.
	//
	// Pass a nil meta so RestoreSlot re-derives fresh provenance from the restored
	// bytes rather than stamping them with the failed edit's metadata.
	if restoreErr := RestoreSlot(dir, imageType, targetName, nil, logger); restoreErr != nil &&
		!errors.Is(restoreErr, os.ErrNotExist) {
		logger.Error("rolling back a failed image slot save FAILED; the original may need manual recovery",
			slog.String("dir", dir), slog.String("image_type", imageType), slog.String("slot", targetName),
			slog.String("save_error", saveErr.Error()), slog.String("restore_error", restoreErr.Error()))
		return nil, fmt.Errorf("saving: %w (the original could not be restored: %w)", saveErr, restoreErr)
	}
	return nil, fmt.Errorf("saving: %w", saveErr)
}
