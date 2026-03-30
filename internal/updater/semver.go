// Package updater provides a self-update mechanism that checks GitHub Releases
// for new versions, supports release channel selection (latest/beta/dev), and
// auto-detects container environments to disable binary replacement when running
// inside Docker or similar runtimes.
package updater

import (
	"fmt"
	"strconv"
	"strings"
)

// Version represents a parsed semantic version.
type Version struct {
	Major int
	Minor int
	Patch int
	Pre   string // pre-release identifier, e.g. "beta.1", "rc.2"
}

// Parse parses a semantic version string (without leading "v").
// Accepts forms: "1.2.3", "1.2.3-beta.1", "1.2.3-rc.2".
func Parse(s string) (Version, error) {
	s = strings.TrimPrefix(s, "v")
	if s == "" {
		return Version{}, fmt.Errorf("empty version string")
	}

	var pre string
	if idx := strings.IndexByte(s, '-'); idx >= 0 {
		pre = s[idx+1:]
		s = s[:idx]
	}

	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return Version{}, fmt.Errorf("invalid semver %q: expected MAJOR.MINOR.PATCH", s)
	}

	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return Version{}, fmt.Errorf("invalid major version %q: %w", parts[0], err)
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return Version{}, fmt.Errorf("invalid minor version %q: %w", parts[1], err)
	}
	patch, err := strconv.Atoi(parts[2])
	if err != nil {
		return Version{}, fmt.Errorf("invalid patch version %q: %w", parts[2], err)
	}

	return Version{Major: major, Minor: minor, Patch: patch, Pre: pre}, nil
}

// String returns the canonical string representation of the version.
func (v Version) String() string {
	base := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if v.Pre != "" {
		return base + "-" + v.Pre
	}
	return base
}

// GT reports whether v is strictly greater than other.
// Pre-release versions are ordered after releases with the same core version
// (e.g. 1.0.0-beta.1 < 1.0.0).  When both have a pre-release, they are
// compared lexicographically.
func (v Version) GT(other Version) bool {
	if v.Major != other.Major {
		return v.Major > other.Major
	}
	if v.Minor != other.Minor {
		return v.Minor > other.Minor
	}
	if v.Patch != other.Patch {
		return v.Patch > other.Patch
	}
	// Same core version: a release beats a pre-release.
	if v.Pre == "" && other.Pre != "" {
		return true
	}
	if v.Pre != "" && other.Pre == "" {
		return false
	}
	// Both pre-release or both release.
	return v.Pre > other.Pre
}

// IsPreRelease reports whether the version has a pre-release identifier.
func (v Version) IsPreRelease() bool {
	return v.Pre != ""
}
