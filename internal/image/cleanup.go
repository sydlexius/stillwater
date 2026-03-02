package image

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// conflictingExtensions maps each image extension to the alternatives that conflict.
var conflictingExtensions = map[string][]string{
	".jpg":  {".jpeg", ".png", ".webp"},
	".jpeg": {".jpg", ".png", ".webp"},
	".png":  {".jpg", ".jpeg", ".webp"},
	".webp": {".jpg", ".jpeg", ".png"},
}

// CleanupConflictingFormats removes existing files that share the same base name
// (case-insensitive) but have a different image format extension, or the same
// format with different casing. This prevents duplicate files on case-sensitive
// filesystems when the canonical filename differs in case from what is on disk.
//
// For example, when saving "folder.jpg", this function:
//   - deletes "folder.png" (conflicting format, same case)
//   - renames "Folder.JPG" to "folder.jpg" (same format, different case)
//   - deletes "Folder.PNG" (conflicting format, different case)
//
// Same-format case mismatches are renamed rather than deleted so that if the
// subsequent WriteFileAtomic fails, the original image content is preserved
// under the canonical name. Conflicting formats are deleted outright since
// the content is being replaced.
//
// The file being written (exact match on fileName) is skipped, since
// WriteFileAtomic handles overwriting it.
func CleanupConflictingFormats(dir string, fileName string, logger *slog.Logger) error {
	ext := strings.ToLower(filepath.Ext(fileName))
	base := strings.TrimSuffix(fileName, filepath.Ext(fileName))
	lowerBase := strings.ToLower(base)

	conflicts, ok := conflictingExtensions[ext]
	if !ok {
		return nil
	}

	conflictSet := make(map[string]struct{}, len(conflicts))
	for _, c := range conflicts {
		conflictSet[c] = struct{}{}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("reading directory %s: %w", dir, err)
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if name == fileName {
			continue // exact match: WriteFileAtomic will handle overwrite
		}

		entryExt := strings.ToLower(filepath.Ext(name))
		entryBase := strings.TrimSuffix(name, filepath.Ext(name))
		if strings.ToLower(entryBase) != lowerBase {
			continue // different base name entirely
		}

		_, isConflict := conflictSet[entryExt]
		isSameFormat := entryExt == ext

		oldPath := filepath.Join(dir, name)
		newPath := filepath.Join(dir, fileName)

		if isSameFormat {
			// Same format, different casing (e.g., "Folder.JPG" -> "folder.jpg").
			// If the canonical file already exists as a separate file (case-
			// sensitive FS with both present), delete the duplicate to avoid
			// overwriting the canonical content. Otherwise rename so the
			// content survives if the subsequent WriteFileAtomic fails.
			//
			// We use os.SameFile to distinguish: on case-insensitive
			// filesystems, Stat("folder.jpg") resolves to "Folder.JPG"
			// (same file), so we must rename rather than delete.
			canonicalIsSeparateFile := false
			if oldInfo, err := os.Stat(oldPath); err == nil {
				if newInfo, err := os.Stat(newPath); err == nil {
					canonicalIsSeparateFile = !os.SameFile(oldInfo, newInfo)
				}
			}
			if canonicalIsSeparateFile {
				// Both files exist as separate inodes: delete the duplicate.
				if err := os.Remove(oldPath); err != nil {
					return err
				}
				logger.Info("deleted duplicate case-mismatched image file",
					slog.String("deleted", oldPath),
					slog.String("canonical", newPath))
			} else {
				if err := os.Rename(oldPath, newPath); err != nil {
					return err
				}
				logger.Info("renamed case-mismatched image file",
					slog.String("from", oldPath),
					slog.String("to", newPath))
			}
		} else if isConflict {
			// Different format (e.g., .png when writing .jpg): delete.
			if err := os.Remove(oldPath); err != nil {
				return err
			}
			logger.Info("deleted conflicting image format",
				slog.String("deleted", oldPath),
				slog.String("replaced_by", newPath))
		}
	}

	return nil
}
