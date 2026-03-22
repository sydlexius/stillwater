package image

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ProvenanceData holds the fields extracted from a saved image file that are
// needed for recording provenance in the database. All fields are strings to
// match the UpdateImageProvenance signature.
type ProvenanceData struct {
	PHash         string
	Source        string
	FileFormat    string
	LastWrittenAt string
}

// IsEmpty returns true when no provenance data was collected.
func (p ProvenanceData) IsEmpty() bool {
	return p.PHash == "" && p.Source == "" && p.FileFormat == "" && p.LastWrittenAt == ""
}

// CollectProvenance reads EXIF provenance metadata and file metadata from a
// saved image at filePath. Errors are logged as warnings and do not prevent
// partial data collection. Returns a zero ProvenanceData if the file does not
// exist or nothing could be collected.
func CollectProvenance(filePath string, logger *slog.Logger) ProvenanceData {
	var d ProvenanceData

	// Read Stillwater provenance metadata (dhash and source) from the image.
	// If the file does not exist (interrupted atomic write, deleted, network
	// share unavailable), return immediately rather than producing duplicate
	// warnings from subsequent stat calls.
	meta, err := ReadProvenance(filePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Return silently; callers log a single contextual warning
			// when they see empty provenance data.
			return d
		}
		logger.Warn("reading image provenance for evidence",
			slog.String("path", filePath),
			slog.String("error", err.Error()))
	}
	if meta != nil {
		d.PHash = meta.DHash
		d.Source = meta.Source
	}

	// Determine file format from extension.
	ext := strings.ToLower(filepath.Ext(filePath))
	switch ext {
	case ".jpg", ".jpeg":
		d.FileFormat = "jpeg"
	case ".png":
		d.FileFormat = "png"
	default:
		logger.Warn("unrecognized image file extension",
			slog.String("extension", ext),
			slog.String("path", filePath))
	}

	// Read the file's mtime as the write timestamp.
	stat, statErr := os.Stat(filePath)
	if statErr != nil {
		logger.Warn("stat image file for write timestamp",
			slog.String("path", filePath),
			slog.String("error", statErr.Error()))
	} else {
		d.LastWrittenAt = stat.ModTime().UTC().Format(time.RFC3339)
	}

	return d
}
