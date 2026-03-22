package library

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// mtimeTolerance is the minimum difference between a file's mtime and the
// reference timestamp before it counts as an external modification. FAT32
// filesystems have 2-second granularity, so anything within that window is
// ambiguous.
const mtimeTolerance = 2 * time.Second

// imageExtensions lists the file extensions treated as image files when
// scanning artist directories for mtime evidence.
var imageExtensions = map[string]bool{
	".jpg":  true,
	".jpeg": true,
	".png":  true,
	".webp": true,
}

// MtimeEvidence describes a file whose filesystem mtime is newer than the
// reference timestamp, indicating an external (non-Stillwater) write.
type MtimeEvidence struct {
	Path      string    `json:"path"`
	FileMtime time.Time `json:"file_mtime"`
}

// CheckArtistDirMtimes scans an artist directory for image files whose mtime
// is newer than lastWrittenAt (plus a 2-second tolerance for FAT32). Returns
// true if any externally-modified image is found.
//
// If the directory does not exist, returns false with no error. Other
// filesystem errors (permission denied, I/O errors) are returned to the caller.
func CheckArtistDirMtimes(artistDir string, lastWrittenAt time.Time) (bool, error) {
	if artistDir == "" || lastWrittenAt.IsZero() {
		return false, nil
	}

	entries, err := os.ReadDir(artistDir)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("reading artist dir %s: %w", artistDir, err)
	}

	threshold := lastWrittenAt.Add(mtimeTolerance)

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(entry.Name()))
		if !imageExtensions[ext] {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		if info.ModTime().After(threshold) {
			return true, nil
		}
	}

	return false, nil
}

// CollectMtimeEvidence scans artist directories for image files with mtimes
// newer than each artist's newest last_written_at timestamp. It returns a
// list of evidence items suitable for storing in the library's shared-FS
// evidence field.
//
// artistDirs maps artist IDs (or any unique key) to filesystem paths.
// lastWrittenAts maps artist paths to the newest last_written_at timestamp.
//
// This function is designed to be called once after a library sync, not
// inside the per-artist loop.
func CollectMtimeEvidence(artistDirs map[string]string, lastWrittenAts map[string]time.Time, logger *slog.Logger) []MtimeEvidence {
	var evidence []MtimeEvidence

	// Deduplicate directories: multiple artist IDs can point to the same
	// path, and scanning the same directory twice would produce duplicate
	// evidence entries.
	seenDirs := make(map[string]bool, len(artistDirs))

	for _, dir := range artistDirs {
		if seenDirs[dir] {
			continue
		}
		seenDirs[dir] = true
		lwt, ok := lastWrittenAts[dir]
		if !ok || lwt.IsZero() {
			continue
		}

		entries, err := os.ReadDir(dir)
		if err != nil {
			if !os.IsNotExist(err) && logger != nil {
				logger.Warn("mtime evidence: could not read artist directory",
					slog.String("dir", dir), slog.String("error", err.Error()))
			}
			continue
		}

		threshold := lwt.Add(mtimeTolerance)

		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			ext := strings.ToLower(filepath.Ext(entry.Name()))
			if !imageExtensions[ext] {
				continue
			}

			info, infoErr := entry.Info()
			if infoErr != nil {
				continue
			}

			if info.ModTime().After(threshold) {
				evidence = append(evidence, MtimeEvidence{
					Path:      filepath.Join(dir, entry.Name()),
					FileMtime: info.ModTime(),
				})
				// One hit per directory is enough evidence; skip remaining
				// files in this directory to avoid noise.
				break
			}
		}
	}

	return evidence
}
