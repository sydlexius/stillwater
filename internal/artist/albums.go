package artist

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// AlbumMatch records a single matched pair between local and remote album names.
type AlbumMatch struct {
	LocalName  string `json:"local_name"`
	RemoteName string `json:"remote_name"`
	Matched    bool   `json:"matched"`
}

// AlbumComparison holds the result of comparing local album directories
// against a provider's release group titles.
type AlbumComparison struct {
	Matches      []AlbumMatch `json:"matches"`
	LocalOnly    []string     `json:"local_only"`
	RemoteOnly   []string     `json:"remote_only"`
	MatchCount   int          `json:"match_count"`
	LocalCount   int          `json:"local_count"`
	RemoteCount  int          `json:"remote_count"`
	MatchPercent int          `json:"match_percent"`
}

// ListLocalAlbums reads subdirectory names from an artist path, skipping hidden
// directories (those starting with "."). Returns sorted names.
func ListLocalAlbums(artistPath string) []string {
	entries, err := os.ReadDir(artistPath)
	if err != nil {
		return nil
	}

	var albums []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		albums = append(albums, name)
	}
	sort.Strings(albums)
	return albums
}

// parenSuffix matches trailing parenthetical text like " (Deluxe Edition)".
var parenSuffix = regexp.MustCompile(`\s*\([^)]*\)\s*$`)

// punctuation matches non-alphanumeric, non-space characters.
var punctuation = regexp.MustCompile(`[^\p{L}\p{N}\s]`)

// multiSpace collapses multiple whitespace chars into one.
var multiSpace = regexp.MustCompile(`\s+`)

// normalizeAlbumName normalizes an album name for comparison:
// lowercase, strip trailing parenthetical suffixes, remove punctuation, collapse whitespace.
func normalizeAlbumName(name string) string {
	s := strings.ToLower(name)
	s = parenSuffix.ReplaceAllString(s, "")
	s = punctuation.ReplaceAllString(s, "")
	s = multiSpace.ReplaceAllString(s, " ")
	s = strings.TrimFunc(s, unicode.IsSpace)
	return s
}

// CompareAlbums compares local directory names against remote release group
// titles using case-insensitive normalized matching. MatchPercent is based
// on the local count (what percentage of local albums have a remote match).
func CompareAlbums(localDirs, remoteTitles []string) AlbumComparison {
	comp := AlbumComparison{
		LocalCount:  len(localDirs),
		RemoteCount: len(remoteTitles),
	}

	if len(localDirs) == 0 && len(remoteTitles) == 0 {
		return comp
	}

	// Build normalized lookup of remote titles.
	type remoteEntry struct {
		original string
		matched  bool
	}
	remoteMap := make(map[string]*remoteEntry, len(remoteTitles))
	for _, title := range remoteTitles {
		norm := normalizeAlbumName(title)
		if _, exists := remoteMap[norm]; !exists {
			remoteMap[norm] = &remoteEntry{original: title}
		}
	}

	for _, local := range localDirs {
		norm := normalizeAlbumName(local)
		if entry, ok := remoteMap[norm]; ok {
			comp.Matches = append(comp.Matches, AlbumMatch{
				LocalName:  local,
				RemoteName: entry.original,
				Matched:    true,
			})
			entry.matched = true
			comp.MatchCount++
		} else {
			comp.LocalOnly = append(comp.LocalOnly, local)
		}
	}

	for _, entry := range remoteMap {
		if !entry.matched {
			comp.RemoteOnly = append(comp.RemoteOnly, entry.original)
		}
	}
	sort.Strings(comp.RemoteOnly)

	if comp.LocalCount > 0 {
		comp.MatchPercent = (comp.MatchCount * 100) / comp.LocalCount
	}

	return comp
}
