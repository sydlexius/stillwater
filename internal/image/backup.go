package image

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

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
		return nil
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
