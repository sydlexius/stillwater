package connection

import (
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
)

// ErrPathOutsideRoots is the sentinel returned by the pre-flight root guard when
// a path Stillwater is about to push to a peer does not resolve inside any of
// that peer's own root folders / library locations.
//
// This is the #2380 showstopper's guard rail. Before it existed, Stillwater
// pushed the HOST path (e.g. "/host/music/X") into the peers'
// CONTAINER namespace (e.g. "/music/X"): Lidarr stored the nonsense path
// verbatim, Jellyfin rejected it and silently kept the old one (whose NFO saver
// then re-created the merged-away directory for the scanner to re-import as a
// duplicate artist), and ALL THREE peers reported a green "ok". A wrong path must
// never be able to report ok, so the guard fails CLOSED: when the peer's roots
// cannot be determined, the push is refused rather than attempted blind.
var ErrPathOutsideRoots = errors.New("path is outside every platform root folder")

// PathWithinRoots reports whether candidate resolves inside at least one of roots,
// using the same separator-boundary semantics as MapArtistPath: a root of
// "/music" contains "/music" and "/music/Artist" but NOT "/musicvideos". An
// empty roots list returns false - "no roots known" is never "path is fine";
// the caller must treat that as an unverifiable push and refuse it.
//
// Comparison is forward-slash based because platform paths cross the wire in
// POSIX form regardless of host OS, and case-sensitive because the peers whose
// roots these are (Linux containers, overwhelmingly) are case-sensitive.
func PathWithinRoots(candidate string, roots []string) bool {
	if candidate == "" || len(roots) == 0 {
		return false
	}
	p := normalizeRootPath(candidate)
	if p == "" {
		return false
	}
	for _, r := range roots {
		root := normalizeRootPath(r)
		if root == "" {
			continue
		}
		if _, ok := pathRemainder(p, root); ok {
			return true
		}
	}
	return false
}

// normalizeRootPath puts a path into the comparison form PathWithinRoots uses:
// backslashes folded to forward slashes (toPosixPath), lexically cleaned, and
// trailing slashes trimmed, matching MapArtistPath's own TrimRight("/") so a
// root entered as "/music/" behaves identically to "/music".
//
// The path.Clean step is what makes the guard a real security boundary rather
// than a string-prefix test: without it "/music/../../etc/evil" is a literal
// prefix match against the root "/music" and reads as IN-root, so a traversal
// component would let a path escape every root the peer reported. Cleaning
// resolves ".." before the comparison, so the escape is measured against the
// path the peer would actually resolve. (No traversal component can reach here
// today - RenameDirectory gates the name with filepath.IsLocal + Clean - so this
// is defense in depth on a boundary that must not depend on its callers.)
func normalizeRootPath(p string) string {
	trimmed := strings.TrimSpace(toPosixPath(p))
	if trimmed == "" {
		return ""
	}
	// path.Clean("") is "." and Clean never returns "", so the empty check
	// above is what keeps a blank root from normalizing to a bogus ".".
	return strings.TrimRight(path.Clean(trimmed), "/")
}

// SamePeerPath reports whether two paths denote the same location in a peer's
// namespace, using the same normalization as PathWithinRoots (backslashes
// folded, lexically cleaned, trailing slashes trimmed). Two empty paths are NOT
// "the same": an absent path is unknown, not equal, and the post-update
// read-back verifier depends on that - a peer that returns no path at all
// (Emby, whose artist items carry Path: null) must read as "did not honor the
// write", never as "matches what we sent".
//
// Case-SENSITIVE, matching PathWithinRoots: the peers whose namespaces these are
// run in Linux containers overwhelmingly, so folding case here would let a
// genuinely different path pass the verifier.
func SamePeerPath(a, b string) bool {
	na, nb := normalizeRootPath(a), normalizeRootPath(b)
	if na == "" || nb == "" {
		return false
	}
	return na == nb
}

// PeerArtist is one artist item as a peer reports it, reduced to the three
// fields the post-move relink needs. Path is the peer's OWN path for the item
// in the peer's namespace and is EMPTY when the peer does not expose one (Emby
// reports a null path for every MusicArtist, since Emby artists are virtual
// name-keyed items rather than folder-backed ones).
//
// Deliberately NOT a place to source artists.path from: a peer path is a
// container path in the peer's namespace, and writing one back into Stillwater's
// artists.path would break every filesystem operation.
type PeerArtist struct {
	ID   string
	Name string
	Path string
}

// RemedyForOutsideRoots renders the operator-facing explanation attached to an
// ErrPathOutsideRoots failure. It names the path that was refused, the roots the
// peer actually reported, and the concrete fix - because the failure is almost
// always a missing path mapping on a split-mount deployment, and an error that
// only said "rejected" would leave the operator guessing at a translation they
// cannot see.
//
// hostPath is the pre-mapping path (what Stillwater sees on disk) and
// platformPath is what MapArtistPath produced; showing BOTH is what makes a
// no-op mapping legible - if they are identical, no mapping matched at all.
func RemedyForOutsideRoots(connName, connType, hostPath, platformPath string, roots []string) string {
	sorted := make([]string, 0, len(roots))
	for _, r := range roots {
		if n := normalizeRootPath(r); n != "" {
			sorted = append(sorted, n)
		}
	}
	sort.Strings(sorted)

	rootList := "none reported"
	if len(sorted) > 0 {
		rootList = strings.Join(sorted, ", ")
	}

	translation := fmt.Sprintf("%q", platformPath)
	if hostPath != platformPath {
		translation = fmt.Sprintf("%q (mapped from %q)", platformPath, hostPath)
	} else {
		translation += " (no path mapping matched, so the host path was sent unchanged)"
	}

	return fmt.Sprintf(
		"refusing to push %s to %s connection %q: it is outside that server's root folders [%s]. "+
			"Add a path mapping on this connection (Settings > Connections > Path Mappings) translating "+
			"Stillwater's host prefix to the prefix this server uses for the same library.",
		translation, connType, connName, rootList)
}
