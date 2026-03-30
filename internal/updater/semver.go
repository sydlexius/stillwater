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
	if major < 0 {
		return Version{}, fmt.Errorf("invalid major version %q: must be non-negative", parts[0])
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return Version{}, fmt.Errorf("invalid minor version %q: %w", parts[1], err)
	}
	if minor < 0 {
		return Version{}, fmt.Errorf("invalid minor version %q: must be non-negative", parts[1])
	}
	patch, err := strconv.Atoi(parts[2])
	if err != nil {
		return Version{}, fmt.Errorf("invalid patch version %q: %w", parts[2], err)
	}
	if patch < 0 {
		return Version{}, fmt.Errorf("invalid patch version %q: must be non-negative", parts[2])
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
//
// Core version (Major.Minor.Patch) is compared numerically first.
// For equal core versions, a release (empty Pre) beats any pre-release.
// When both have a pre-release, identifiers are compared per the semver spec:
// dot-separated segments are compared left to right; numeric segments are
// compared as integers; alphanumeric segments are compared lexicographically;
// a numeric segment has lower precedence than an alphanumeric one; if all
// compared segments are equal, the longer identifier wins.
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
	// Both pre-release: compare per semver spec.
	return comparePre(v.Pre, other.Pre) > 0
}

// comparePre compares two pre-release identifier strings per the semver spec.
// Returns positive when a > b, negative when a < b, and 0 when equal.
func comparePre(a, b string) int {
	if a == b {
		return 0
	}
	partsA := strings.Split(a, ".")
	partsB := strings.Split(b, ".")
	limit := len(partsA)
	if len(partsB) < limit {
		limit = len(partsB)
	}
	for i := 0; i < limit; i++ {
		aNum, aErr := strconv.Atoi(partsA[i])
		bNum, bErr := strconv.Atoi(partsB[i])
		switch {
		case aErr == nil && bErr == nil:
			// Both numeric: compare as integers.
			if aNum != bNum {
				if aNum > bNum {
					return 1
				}
				return -1
			}
		case aErr != nil && bErr != nil:
			// Both alphanumeric: lexicographic comparison.
			if partsA[i] != partsB[i] {
				if partsA[i] > partsB[i] {
					return 1
				}
				return -1
			}
		case aErr == nil:
			// a is numeric, b is alphanumeric: numeric has lower precedence.
			return -1
		default:
			// a is alphanumeric, b is numeric: alphanumeric has higher precedence.
			return 1
		}
	}
	// All compared segments equal; longer identifier wins.
	if len(partsA) > len(partsB) {
		return 1
	}
	if len(partsA) < len(partsB) {
		return -1
	}
	return 0
}

// IsPreRelease reports whether the version has a pre-release identifier.
func (v Version) IsPreRelease() bool {
	return v.Pre != ""
}
