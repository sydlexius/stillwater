package image

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// FanartFilename returns the correct filename for a fanart image at the given
// 0-based index. Index 0 returns the primary name unchanged. Index 1+ returns
// numbered variants following platform conventions.
//
// kodiNumbering controls the numbering offset for additional fanart:
//   - false (Emby/Jellyfin/Plex): index 1 -> base2.ext, index 2 -> base3.ext
//   - true  (Kodi):               index 1 -> base1.ext, index 2 -> base2.ext
func FanartFilename(primaryName string, index int, kodiNumbering bool) string {
	if index == 0 {
		return primaryName
	}
	ext := filepath.Ext(primaryName)
	base := strings.TrimSuffix(primaryName, ext)
	n := index + 1
	if kodiNumbering {
		n = index
	}
	return fmt.Sprintf("%s%d%s", base, n, ext)
}

// indexedFile pairs a discovery index with an absolute file path.
type indexedFile struct {
	index int
	path  string
}

// DiscoverFanart scans an artist directory and returns sorted absolute paths
// for all fanart files that match the primary name or its numbered variants.
// The primary name comes from the active platform profile (e.g., "backdrop.jpg"
// for Emby, "fanart.jpg" for Kodi). Files are returned in index order: primary
// first, then numbered variants sorted ascending.
func DiscoverFanart(dir string, primaryName string) []string {
	if primaryName == "" {
		return nil
	}

	base := strings.TrimSuffix(primaryName, filepath.Ext(primaryName))
	baseLower := strings.ToLower(base)

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	var files []indexedFile

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		ext := strings.ToLower(filepath.Ext(name))
		if ext != ".jpg" && ext != ".jpeg" && ext != ".png" {
			continue
		}

		nameBase := strings.TrimSuffix(name, filepath.Ext(name))
		nameBaseLower := strings.ToLower(nameBase)

		// Primary (index 0): exact base match
		if nameBaseLower == baseLower {
			files = append(files, indexedFile{0, filepath.Join(dir, name)})
			continue
		}

		// Numbered variant: {base}{N} where N is a positive integer
		if strings.HasPrefix(nameBaseLower, baseLower) {
			suffix := nameBaseLower[len(baseLower):]
			if n, parseErr := strconv.Atoi(suffix); parseErr == nil && n > 0 {
				files = append(files, indexedFile{n, filepath.Join(dir, name)})
			}
		}
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].index < files[j].index
	})

	paths := make([]string, 0, len(files))
	for _, f := range files {
		paths = append(paths, f.path)
	}
	return paths
}
