package updater

import (
	"fmt"
	"strconv"
	"strings"
)

// semver holds a parsed semantic version.
type semver struct {
	Major      int
	Minor      int
	Patch      int
	PreRelease string // empty for stable, e.g. "rc.1", "beta.2"
}

// parseSemver parses a version string like "v1.2.3", "1.2.3", "v1.2.3-rc.1".
func parseSemver(v string) (semver, error) {
	v = strings.TrimPrefix(v, "v")

	// Split off pre-release suffix.
	core := v
	pre := ""
	if idx := strings.IndexByte(v, '-'); idx >= 0 {
		core = v[:idx]
		pre = v[idx+1:]
	}

	parts := strings.SplitN(core, ".", 3)
	if len(parts) != 3 {
		return semver{}, fmt.Errorf("invalid semver %q", v)
	}

	// strconv.Atoi accepts negative integers, so without a range check
	// "v-1.2.3" would parse to {Major: -1, ...}. The upstream semverRE gate
	// in pickLatest already rejects such tags, but parseSemver is callable
	// directly within the package (e.g. from tests) and must not produce a
	// negative-valued semver that would then sort before legitimate versions.
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return semver{}, fmt.Errorf("invalid major in %q: %w", v, err)
	}
	if major < 0 {
		return semver{}, fmt.Errorf("negative major in %q", v)
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return semver{}, fmt.Errorf("invalid minor in %q: %w", v, err)
	}
	if minor < 0 {
		return semver{}, fmt.Errorf("negative minor in %q", v)
	}
	patch, err := strconv.Atoi(parts[2])
	if err != nil {
		return semver{}, fmt.Errorf("invalid patch in %q: %w", v, err)
	}
	if patch < 0 {
		return semver{}, fmt.Errorf("negative patch in %q", v)
	}

	return semver{
		Major:      major,
		Minor:      minor,
		Patch:      patch,
		PreRelease: pre,
	}, nil
}

// semverCompare returns:
//
//	-1 if a < b
//	 0 if a == b
//	+1 if a > b
//
// Stable releases (no pre-release) are considered greater than pre-releases
// at the same version number per the semver spec.
func semverCompare(a, b semver) int {
	if a.Major != b.Major {
		return cmpInt(a.Major, b.Major)
	}
	if a.Minor != b.Minor {
		return cmpInt(a.Minor, b.Minor)
	}
	if a.Patch != b.Patch {
		return cmpInt(a.Patch, b.Patch)
	}

	// Same numeric version: compare pre-release.
	// "" (stable) > any pre-release.
	switch {
	case a.PreRelease == "" && b.PreRelease == "":
		return 0
	case a.PreRelease == "" && b.PreRelease != "":
		return 1
	case a.PreRelease != "" && b.PreRelease == "":
		return -1
	default:
		// Both have pre-release: use semver 11.4 identifier-level comparison.
		return comparePrerelease(a.PreRelease, b.PreRelease)
	}
}

// comparePrerelease implements semver spec section 11.4: split on ".",
// compare pairwise. Numeric identifiers are compared as integers. A numeric
// identifier has lower precedence than an alphanumeric one. If all shared
// identifiers are equal, the version with more identifiers has higher
// precedence (rc.1.1 > rc.1).
func comparePrerelease(a, b string) int {
	ap := strings.Split(a, ".")
	bp := strings.Split(b, ".")
	n := len(ap)
	if len(bp) < n {
		n = len(bp)
	}
	for i := 0; i < n; i++ {
		ai, aErr := strconv.Atoi(ap[i])
		bi, bErr := strconv.Atoi(bp[i])
		switch {
		case aErr == nil && bErr == nil:
			// Both numeric: compare as integers.
			if ai != bi {
				return cmpInt(ai, bi)
			}
		case aErr == nil && bErr != nil:
			// Numeric has lower precedence than alphanumeric (spec 11.4.1).
			return -1
		case aErr != nil && bErr == nil:
			// Alphanumeric has higher precedence than numeric.
			return 1
		default:
			// Both alphanumeric: lexicographic comparison.
			if ap[i] < bp[i] {
				return -1
			}
			if ap[i] > bp[i] {
				return 1
			}
		}
	}
	// All shared identifiers equal: more identifiers means higher precedence.
	return cmpInt(len(ap), len(bp))
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}
