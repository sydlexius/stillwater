package image

import (
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

// CleanupConflictingFormats deletes existing files that share the same base name
// but have a different image format extension. For example, when saving "folder.png",
// this function deletes "folder.jpg" and "folder.jpeg" if they exist.
func CleanupConflictingFormats(dir string, fileName string, logger *slog.Logger) error {
	ext := strings.ToLower(filepath.Ext(fileName))
	base := strings.TrimSuffix(fileName, filepath.Ext(fileName))

	conflicts, ok := conflictingExtensions[ext]
	if !ok {
		return nil
	}

	for _, altExt := range conflicts {
		altPath := filepath.Join(dir, base+altExt)
		if _, err := os.Stat(altPath); err == nil {
			if err := os.Remove(altPath); err != nil {
				return err
			}
			logger.Info("deleted conflicting image format",
				slog.String("deleted", altPath),
				slog.String("replaced_by", filepath.Join(dir, fileName)))
		}
	}

	return nil
}
