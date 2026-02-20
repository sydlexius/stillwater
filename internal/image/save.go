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
func Save(dir string, imageType string, data []byte, fileNames []string, logger *slog.Logger) ([]string, error) {
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

	var saved []string
	for _, name := range fileNames {
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

		// Write atomically
		targetPath := filepath.Join(dir, targetName)
		if err := filesystem.WriteFileAtomic(targetPath, data, 0o644); err != nil {
			return saved, fmt.Errorf("writing %s: %w", targetPath, err)
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
