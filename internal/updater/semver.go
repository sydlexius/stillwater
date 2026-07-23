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

	// Split off pre-release suffix. Reject a trailing "-" with nothing after
	// it: otherwise "v1.2.3-" would parse with PreRelease == "" and sort as a
	// stable release, which is wrong for a malformed tag. The upstream
	// semverRE gate already rejects this, but parseSemver is callable
	// in-package (e.g. from tests) and must not silently normalize bad input.
	core := v
	pre := ""
	if idx := strings.IndexByte(v, '-'); idx >= 0 {
		if idx == len(v)-1 {
			return semver{}, fmt.Errorf("empty prerelease in %q", v)
		}
		core = v[:idx]
		pre = v[idx+1:]
	}

	parts := strings.SplitN(core, ".", 3)
	if len(parts) != 3 {
		return semver{}, fmt.Errorf("invalid semver %q", v)
	}

	// SemVer 2.0.0 spec 2 restricts core components to [0-9]+ with no
	// leading zeros (except bare "0"). isSemverNumeric gates each part
	// before strconv.Atoi, which would otherwise happily accept signed
	// ("-1", "+1") and leading-zero ("01") forms and silently normalize
	// malformed tags. The semverRE gate in pickLatest already filters
	// these, but parseSemver is callable directly within the package
	// (e.g. from tests) and must not produce a misleading semver.
	// Atoi can still fail on overflow for very long digit strings, so
	// the error branch remains as a second line of defense.
	if !isSemverNumeric(parts[0]) {
		return semver{}, fmt.Errorf("invalid major in %q", v)
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return semver{}, fmt.Errorf("invalid major in %q: %w", v, err)
	}
	if !isSemverNumeric(parts[1]) {
		return semver{}, fmt.Errorf("invalid minor in %q", v)
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return semver{}, fmt.Errorf("invalid minor in %q: %w", v, err)
	}
	if !isSemverNumeric(parts[2]) {
		return semver{}, fmt.Errorf("invalid patch in %q", v)
	}
	patch, err := strconv.Atoi(parts[2])
	if err != nil {
		return semver{}, fmt.Errorf("invalid patch in %q: %w", v, err)
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
//
// SemVer 2.0.0 restricts numeric identifiers to bare digits [0-9]+, so
// "-1" and "+1" are alphanumeric (strconv.Atoi would accept both), and
// "01" is invalid as numeric. We detect digits-only ourselves and fall
// back to lexicographic comparison for anything else.
//
// DELIBERATE DEVIATION from strict spec 11.4 (issue #2732): this project's
// release tags use a single, undotted prerelease identifier of the form
// "rc11", "rc9" -- not the dotted "rc.11" / "rc.9" the spec assumes for
// numeric ordering. Spec 11.4.1 says a single alphanumeric identifier like
// "rc11" is compared purely lexicographically (ASCII), which ranks
// "rc11" < "rc9" (the digit '1' sorts before '9') even though the project
// means rc11 to be numerically after rc9. That misranking is the exact bug:
// the updater picked rc9 as "newer" than rc11 and offered it as an upgrade,
// silently downgrading an operator on rc11.
//
// Fix: when two identifiers at the same dot-separated position are both
// alphanumeric, share an identical alpha prefix, AND both have a trailing
// digit run (e.g. "rc11"/"rc9" -> prefix "rc", suffixes "11"/"9"), compare
// the alpha prefix (equal, so a no-op) and then the numeric suffix
// NUMERICALLY instead of lexicographically. Every other case -- dotted
// identifiers ("rc.11" vs "rc.9", handled by the aNum/bNum branches below
// since "11" and "9" are themselves numeric identifiers), identifiers with
// no trailing digits ("alpha" vs "beta"), identifiers with different alpha
// prefixes ("alpha2" vs "beta1"), and one identifier with a prefix but no
// digits ("rc" vs "rc9") -- keeps the spec-correct lexicographic path. Do
// NOT "correct" this back to spec: the project's tag convention is rcN
// (never zero-padded, never dotted) and this is intentional.
func comparePrerelease(a, b string) int {
	ap := strings.Split(a, ".")
	bp := strings.Split(b, ".")
	n := len(ap)
	if len(bp) < n {
		n = len(bp)
	}
	for i := 0; i < n; i++ {
		aNum := isSemverNumeric(ap[i])
		bNum := isSemverNumeric(bp[i])
		switch {
		case aNum && bNum:
			// Both numeric: compare without integer parsing to avoid silent
			// overflow misordering for identifiers beyond max int (Atoi
			// clamps on overflow, which would collapse "9223372036854775807"
			// and "9223372036854775808" to equal and misrank the release).
			// SemVer numeric identifiers are digits-only with no leading
			// zeros, so numeric precedence is equivalent to: shorter length
			// sorts lower, same length compares lexicographically.
			if len(ap[i]) != len(bp[i]) {
				return cmpInt(len(ap[i]), len(bp[i]))
			}
			if ap[i] < bp[i] {
				return -1
			}
			if ap[i] > bp[i] {
				return 1
			}
		case aNum && !bNum:
			// Numeric has lower precedence than alphanumeric (spec 11.4.1).
			return -1
		case !aNum && bNum:
			// Alphanumeric has higher precedence than numeric.
			return 1
		default:
			// Both alphanumeric. If they share an identical alpha prefix and
			// both carry a trailing digit run (e.g. "rc11"/"rc9"), compare
			// the numeric suffix numerically -- see the deviation note on
			// comparePrerelease. Otherwise fall back to spec-correct
			// lexicographic comparison.
			aPrefix, aDigits, aHasDigits := splitTrailingDigits(ap[i])
			bPrefix, bDigits, bHasDigits := splitTrailingDigits(bp[i])
			if aHasDigits && bHasDigits && aPrefix == bPrefix {
				if c := cmpDigitRun(aDigits, bDigits); c != 0 {
					return c
				}
				continue
			}
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

// isSemverNumeric reports whether s is a valid SemVer numeric identifier:
// one or more digits, no sign, no leading zeros unless the identifier is
// exactly "0". Used to decide whether to compare two prerelease identifiers
// numerically or lexicographically.
func isSemverNumeric(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	// Reject leading zeros per spec 9 (e.g. "01" is not a valid numeric
	// identifier). Bare "0" is valid.
	if len(s) > 1 && s[0] == '0' {
		return false
	}
	return true
}

// splitTrailingDigits splits an identifier into its alpha prefix and
// trailing run of ASCII digits, e.g. "rc11" -> ("rc", "11", true). ok is
// false when the identifier has no trailing digit run at all (e.g. "rc",
// "alpha"), in which case prefix is the whole identifier and digits is
// empty. Used only to detect the "rc11 vs rc9" undotted-tag shape; see the
// deviation note on comparePrerelease.
func splitTrailingDigits(s string) (prefix, digits string, ok bool) {
	i := len(s)
	for i > 0 && s[i-1] >= '0' && s[i-1] <= '9' {
		i--
	}
	if i == len(s) {
		return s, "", false
	}
	return s[:i], s[i:], true
}

// cmpDigitRun numerically compares two digit runs (as produced by
// splitTrailingDigits) without parsing them into an int, to avoid overflow
// on pathologically long digit strings. Unlike SemVer numeric identifiers,
// these runs are not guaranteed free of leading zeros (they are an
// alphanumeric identifier's suffix, not a standalone numeric identifier),
// so leading zeros are stripped before comparing.
func cmpDigitRun(a, b string) int {
	a = strings.TrimLeft(a, "0")
	b = strings.TrimLeft(b, "0")
	if len(a) != len(b) {
		return cmpInt(len(a), len(b))
	}
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
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
