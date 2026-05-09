package version

import (
	"fmt"

	"golang.org/x/mod/semver"
)

// String returns a canonical human-readable version string.
//
// Format:
//   - Release build: "v1.0.6 (commit abc1234, built 2026-05-09)"
//   - Dev build (Commit or Date is "unknown"): "v1.0.6 (dev build)"
//
// The "v" prefix is added if Version does not already start with one,
// because golang.org/x/mod/semver requires the "v" prefix, but the
// ldflags injected value typically omits it (e.g., "1.0.6").
func String() string {
	v := canonicalVersion(Version)
	if IsDevBuild() {
		return v + " (dev build)"
	}
	return fmt.Sprintf("%s (commit %s, built %s)", v, Commit, Date)
}

// Validate asserts that Version is non-empty and parses as a valid semver
// string according to golang.org/x/mod/semver rules (which require a "v"
// prefix). It does not distinguish between dev and release builds -- a dev
// fallback like "1.0.6" is a valid semver and Validate() succeeds.
//
// Returns a wrapped error on failure so callers can use errors.Is/As if needed.
func Validate() error {
	if Version == "" {
		return fmt.Errorf("version: Version is empty: ldflags injection may be missing")
	}
	v := canonicalVersion(Version)
	if !semver.IsValid(v) {
		return fmt.Errorf("version: %q is not a valid semver string: %w", Version, errInvalidSemver)
	}
	return nil
}

// errInvalidSemver is a sentinel used as the wrapped cause in Validate errors.
var errInvalidSemver = fmt.Errorf("invalid semver")

// IsDevBuild reports whether the binary was built without full ldflags
// injection. It returns true when Commit or Date is "unknown", which is the
// default value set in version.go for local/IDE builds.
//
// Version is NOT checked here because the dev fallback intentionally mirrors
// the last release tag (e.g., "1.0.6") to keep semver comparisons sensible.
func IsDevBuild() bool {
	return Commit == "unknown" || Date == "unknown"
}

// UserAgent returns an HTTP User-Agent header value for outbound requests.
//
// When repoURL is non-empty the format is:
//
//	Stillwater/1.0.6 (https://github.com/sydlexius/stillwater)
//
// When repoURL is empty the format is:
//
//	stillwater/1.0.6
//
// The MusicBrainz API requires the URL form; all other callers use the bare form.
// Capitalisation follows each consumer's prior convention: MusicBrainz wants
// "Stillwater/..." (capital S) while the updater uses "stillwater/..." (lower s).
// Both forms are produced by passing or omitting repoURL.
func UserAgent(repoURL string) string {
	if repoURL != "" {
		return fmt.Sprintf("Stillwater/%s (%s)", Version, repoURL)
	}
	return "stillwater/" + Version
}

// canonicalVersion returns v with a leading "v" if it is not already present.
// golang.org/x/mod/semver requires the "v" prefix.
func canonicalVersion(v string) string {
	if len(v) > 0 && v[0] != 'v' {
		return "v" + v
	}
	return v
}
