package image

import (
	"bytes"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/sydlexius/stillwater/internal/filesystem"
)

// Save writes image data to all configured filenames for the given image type.
// It handles format detection, logo-to-PNG conversion, conflicting format cleanup,
// and atomic file writes.
//
// When useSymlinks is true and the image type is not "fanart", only the first
// filename is written as a real file. Subsequent filenames are created as
// relative symlinks pointing to the primary file. If symlink creation fails,
// it falls back to a regular atomic write.
func Save(dir string, imageType string, data []byte, fileNames []string, useSymlinks bool, logger *slog.Logger) ([]string, error) {
	if len(fileNames) == 0 {
		return nil, fmt.Errorf("no filenames configured for image type %q", imageType)
	}

	// Detect the format of the incoming data
	format, _, err := DetectFormat(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("detecting image format: %w", err)
	}

	// Logos must always be PNG to preserve alpha channel
	if imageType == "logo" && format != FormatPNG {
		converted, convErr := ConvertToFormat(bytes.NewReader(data), FormatPNG)
		if convErr != nil {
			return nil, fmt.Errorf("converting logo to PNG: %w", convErr)
		}
		data = converted
		format = FormatPNG
	}

	// Determine the output extension based on the actual format
	ext := formatToExt(format)

	// Symlinks are only used for non-fanart types (fanart images can be
	// genuinely different files, e.g. numbered fanart1.jpg, fanart2.jpg).
	symlinkEligible := useSymlinks && imageType != "fanart"

	var saved []string
	var primaryPath string
	for i, name := range fileNames {
		// Replace the extension with the actual format extension
		baseName := strings.TrimSuffix(name, filepath.Ext(name))
		targetName := baseName + ext

		// Clean up conflicting formats before writing
		if err := CleanupConflictingFormats(dir, targetName, logger); err != nil {
			logger.Warn("failed to clean up conflicting formats",
				slog.String("dir", dir),
				slog.String("file", targetName),
				slog.String("error", err.Error()))
		}

		targetPath := filepath.Join(dir, targetName)

		if i == 0 || !symlinkEligible {
			// First file (or symlinks disabled): write as real file
			if err := filesystem.WriteFileAtomic(targetPath, data, 0o644); err != nil {
				return saved, fmt.Errorf("writing %s: %w", targetPath, err)
			}
			if i == 0 {
				primaryPath = targetPath
			}
		} else {
			// Subsequent files: create symlink to primary
			if err := filesystem.CreateRelativeSymlink(primaryPath, targetPath); err != nil {
				logger.Warn("symlink creation failed, falling back to copy",
					slog.String("target", targetPath),
					slog.String("error", err.Error()))
				if err := filesystem.WriteFileAtomic(targetPath, data, 0o644); err != nil {
					return saved, fmt.Errorf("writing %s: %w", targetPath, err)
				}
			}
		}

		saved = append(saved, targetName)
		logger.Debug("saved image",
			slog.String("path", targetPath),
			slog.String("type", imageType),
			slog.String("format", format))
	}

	return saved, nil
}

// formatToExt converts a format string to a file extension.
func formatToExt(format string) string {
	switch format {
	case FormatJPEG:
		return ".jpg"
	case FormatPNG:
		return ".png"
	default:
		return ".jpg"
	}
}
