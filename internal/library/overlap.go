package library

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
)

// OverlapResult describes a shared-filesystem overlap between two libraries.
type OverlapResult struct {
	LibraryID   string // The library that has the overlap
	LibraryName string
	OverlapWith string // Description of what it overlaps with (e.g. "Emby library 'Music'")
	Path        string // The overlapping path
}

// DetectOverlaps checks all libraries for path overlaps that indicate a
// shared-filesystem condition. A library has a shared filesystem when its
// path overlaps with the path of another library that is sourced from a
// different connection or source type. This indicates that both Stillwater
// and a media platform are managing files at the same location.
//
// Two paths "overlap" when one is a prefix of the other (after cleaning
// and resolving symlinks) or they are identical.
func DetectOverlaps(libs []Library) []OverlapResult {
	// Build a list of libraries with non-empty, resolved paths.
	type resolved struct {
		lib  Library
		path string // cleaned, resolved path
	}
	var entries []resolved
	for _, lib := range libs {
		if lib.Path == "" {
			continue
		}
		p := cleanPath(lib.Path)
		entries = append(entries, resolved{lib: lib, path: p})
	}

	var results []OverlapResult

	// Compare all pairs. When paths overlap and at least one library is
	// platform-sourced (emby, jellyfin), flag both libraries -- the
	// platform one AND the non-platform one. This ensures the warning bar
	// surfaces every library involved in the conflict, regardless of source.
	for i := 0; i < len(entries); i++ {
		for j := i + 1; j < len(entries); j++ {
			a, b := entries[i], entries[j]
			if !pathsOverlap(a.path, b.path) {
				continue
			}

			if isPlatformSource(a.lib.Source) || isPlatformSource(b.lib.Source) {
				// Always flag both sides of the overlap.
				results = append(results, OverlapResult{
					LibraryID:   a.lib.ID,
					LibraryName: a.lib.Name,
					OverlapWith: describeOverlap(b.lib),
					Path:        a.lib.Path,
				})
				results = append(results, OverlapResult{
					LibraryID:   b.lib.ID,
					LibraryName: b.lib.Name,
					OverlapWith: describeOverlap(a.lib),
					Path:        b.lib.Path,
				})
			}
		}
	}

	return deduplicateResults(results)
}

// pathsOverlap returns true if one path is a prefix of the other or they
// are identical. Both paths should be cleaned before calling.
func pathsOverlap(a, b string) bool {
	if a == b {
		return true
	}
	// Ensure the shorter path is the prefix candidate.
	if len(a) > len(b) {
		a, b = b, a
	}
	// a is the prefix if b starts with a followed by a separator.
	return strings.HasPrefix(b, a+string(filepath.Separator))
}

// cleanPath normalizes a filesystem path by cleaning it, converting to
// lowercase for comparison, and resolving symlinks (best-effort). When
// resolving symlinks, we walk up the path to find the deepest existing
// ancestor and resolve that, so paths like /data/music where /data is a
// symlink get properly resolved even when the full path does not exist yet.
func cleanPath(p string) string {
	p = filepath.Clean(p)
	p = resolvePathBestEffort(p)
	// Always lowercase for comparison. False-positive overlap detection
	// (treating two paths as overlapping when they are different due to
	// case) is far less harmful than missing a real overlap. In particular,
	// os.PathSeparator == '/' is an unreliable indicator because
	// Docker-on-Windows mounts NTFS volumes (case-insensitive) with a
	// Linux path separator, so the heuristic produces incorrect results
	// there. Case-insensitive comparison is the safe universal default.
	return strings.ToLower(p)
}

// resolvePathBestEffort resolves symlinks as far as possible by walking
// up the path tree. If the full path resolves, great. Otherwise, it finds
// the deepest existing ancestor, resolves that, and appends the remaining
// path components. This handles cases like /data/music where /data is a
// symlink but /data/music does not yet exist.
func resolvePathBestEffort(p string) string {
	resolved, err := filepath.EvalSymlinks(p)
	if err == nil {
		return resolved
	}

	// Walk upward to find the deepest resolvable ancestor.
	dir := filepath.Dir(p)
	base := filepath.Base(p)
	if dir == p {
		// Reached the root without resolving anything.
		return p
	}
	resolvedDir := resolvePathBestEffort(dir)
	return filepath.Join(resolvedDir, base)
}

// isPlatformSource returns true if the source indicates a platform connection
// that may write metadata to the filesystem.
func isPlatformSource(source string) bool {
	return source == SourceEmby || source == SourceJellyfin
}

// describeOverlap returns a human-readable description of a library for
// overlap reporting.
func describeOverlap(lib Library) string {
	source := lib.SourceDisplayName()
	if source != "" {
		return source + " library '" + lib.Name + "'"
	}
	return "library '" + lib.Name + "'"
}

// RecheckOverlaps lists all libraries, detects path overlaps, and updates each
// library's shared_filesystem flag accordingly. Returns the number of libraries
// with overlaps detected. This is the shared implementation used at startup and
// by the API recheck endpoint.
func RecheckOverlaps(ctx context.Context, svc *Service, logger *slog.Logger) (int, error) {
	libs, err := svc.List(ctx)
	if err != nil {
		return 0, err
	}

	overlaps := DetectOverlaps(libs)
	overlapIDs := make(map[string]bool, len(overlaps))
	for _, o := range overlaps {
		overlapIDs[o.LibraryID] = true
	}

	for _, lib := range libs {
		shouldBeShared := overlapIDs[lib.ID]
		if lib.SharedFilesystem != shouldBeShared {
			if setErr := svc.SetSharedFilesystem(ctx, lib.ID, shouldBeShared); setErr != nil {
				logger.Warn("failed to update shared_filesystem flag",
					slog.String("library_id", lib.ID),
					slog.String("error", setErr.Error()))
			}
		}
	}

	return len(overlaps), nil
}

// deduplicateResults removes duplicate OverlapResult entries based on LibraryID.
func deduplicateResults(results []OverlapResult) []OverlapResult {
	seen := make(map[string]bool, len(results))
	var deduped []OverlapResult
	for _, r := range results {
		if !seen[r.LibraryID] {
			seen[r.LibraryID] = true
			deduped = append(deduped, r)
		}
	}
	return deduped
}
