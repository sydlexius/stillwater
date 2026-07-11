package artist

import "strings"

// ignored_names.go -- built-in, case-insensitive classification of directory
// and file base names that must never be treated as artist content. Two
// distinct sets live here so both the scanner (artist discovery) and the merge
// orchestrator (collision gating) share exactly one source of truth:
//
//   - ignoredSystemNames: OS / NAS junk (recycle bins, thumbnail caches,
//     volume metadata). Never an artist, never scanned, never a collision.
//   - nonArtistDirNames: compilation placeholder buckets ("Various Artists").
//     Never created as an artist row.
//
// Both are separate from the operator-editable scanner exclusion list
// (Service.SetExclusions): that mechanism CREATES the artist row and flags it
// IsExcluded so it still carries rules; the sets here drop the directory
// entirely so no row (and no merge collision) is ever produced.

// ignoredSystemNames holds the lowercased base names of OS- and NAS-generated
// junk directories/files. Filesystems and network-attached-storage appliances
// drop these into shares (recycle bins, thumbnail caches, volume metadata);
// none is ever a real artist directory or a mergeable child.
var ignoredSystemNames = map[string]bool{
	"$recycle.bin":              true, // Windows recycle bin
	"system volume information": true, // Windows volume metadata
	"@eadir":                    true, // Synology thumbnail / index cache
	"@__thumb":                  true, // Synology thumbnail cache
	".trash":                    true, // Linux / desktop trash
	".trashes":                  true, // macOS trash on removable volumes
	".ds_store":                 true, // macOS directory metadata
	"lost+found":                true, // ext filesystem recovery dir
	"thumbs.db":                 true, // Windows thumbnail cache
	"desktop.ini":               true, // Windows folder configuration
}

// IsIgnoredSystemName reports whether name (a path base name) is an OS- or
// NAS-generated junk entry that must never be treated as an artist directory,
// scanned as artist content, or gated as a merge collision. The match is
// case-insensitive on the trimmed base name. Some of these names are dot-
// prefixed (.DS_Store, .Trash) and are also caught by hidden-file skips
// elsewhere; the explicit set additionally covers the non-dot-prefixed junk
// ($RECYCLE.BIN, System Volume Information, @eaDir, lost+found, Thumbs.db,
// desktop.ini) that hidden-file skips miss.
func IsIgnoredSystemName(name string) bool {
	return ignoredSystemNames[strings.ToLower(strings.TrimSpace(name))]
}

// nonArtistDirNames holds the lowercased base names of compilation /
// placeholder buckets that media servers use for various-artist albums.
// Treating these as artists pollutes the catalogue and the rule engine, so
// they are skipped entirely at discovery time.
var nonArtistDirNames = map[string]bool{
	"various artists": true,
	"various artist":  true,
	"various":         true,
	"va":              true,
}

// IsNonArtistDirName reports whether name (a top-level library directory base
// name) is a compilation / placeholder bucket that must not be created as an
// artist. Case-insensitive on the trimmed base name. Matching is exact on the
// whole name, so a genuine artist like "Various Voices" is NOT excluded.
func IsNonArtistDirName(name string) bool {
	return nonArtistDirNames[strings.ToLower(strings.TrimSpace(name))]
}

// backupDirName is the hidden per-artist rollback directory the image editor
// writes (crop/trim/replace originals). During a merge it is neither album
// content nor a mergeable child, so enumerateChildren routes it into the
// `ignored` bucket (BEFORE the generic dot-prefix skip) so removeIgnoredJunk
// sweeps it and the emptied loser directory can still be unlinked (#2363).
//
// Source of truth for the literal is image.BackupDirName
// (internal/image/backup.go). We deliberately avoid a cross-package import for
// a single string constant; a sync test (backupDirName == image.BackupDirName)
// guards against drift.
const backupDirName = ".sw-backup"

// isAdditiveMergeDir reports whether name is a subdirectory whose contents are
// additive across a merge -- both artists' copies can coexist, so a collision
// on the directory itself must NOT halt the merge. Kodi/Emby store extra
// artwork under extrafanart/ and extrathumbs/; merging two duplicate artists
// should combine their extra images rather than refuse. Case-insensitive on
// the trimmed base name.
func isAdditiveMergeDir(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "extrafanart", "extrathumbs":
		return true
	default:
		return false
	}
}
