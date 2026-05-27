package version

import (
	"errors"
	"fmt"

	"golang.org/x/mod/semver"
	"golang.org/x/net/http/httpguts"
)

// ErrEmpty is returned when Version is the empty string -- typically because
// no -ldflags injection ran. Callers can distinguish this from a malformed
// Version via errors.Is.
var ErrEmpty = errors.New("version is empty")

// ErrInvalidSemver is returned when Version is set but does not parse as semver.
// Callers can distinguish this from an empty Version via errors.Is.
var ErrInvalidSemver = errors.New("invalid semver")

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
// Returns a wrapped error on failure so callers can use errors.Is/As to
// distinguish ErrEmpty from ErrInvalidSemver.
func Validate() error {
	if Version == "" {
		return fmt.Errorf("version: ldflags injection may be missing: %w", ErrEmpty)
	}
	v := canonicalVersion(Version)
	if !semver.IsValid(v) {
		return fmt.Errorf("version: %q is not a valid semver string: %w", Version, ErrInvalidSemver)
	}
	return nil
}

// IsDevBuild reports whether the binary was built without full ldflags
// injection. It returns true when Commit or Date is "unknown", which is the
// default value set in version.go for local/IDE builds.
//
// Version is NOT checked here because the dev fallback intentionally mirrors
// the last release tag (e.g., "1.0.6") to keep semver comparisons sensible.
func IsDevBuild() bool {
	return Commit == "unknown" || Date == "unknown"
}

// IsReleaseBuild reports whether the binary was produced by the release
// pipeline (goreleaser, stable or nightly). It checks BuildType rather
// than negating IsDevBuild because both `make build` and goreleaser
// inject non-"unknown" Commit and Date values, so the two cannot be
// distinguished by those fields alone. Only the goreleaser configs set
// `-X .../version.BuildType=release`; everything else (make build,
// `go build`, IDE builds, CI test builds) leaves it as the zero value.
//
// Used by the startup check for SW_FORCE_PROVIDER_ERROR: the env var is
// allowed in dev/CI/smoke builds and blocked in release builds so the
// hook cannot survive an accidental config copy into production.
func IsReleaseBuild() bool {
	return BuildType == "release"
}

// UserAgent returns an HTTP User-Agent header value for outbound requests.
//
// prefix is the application/subsystem identifier (e.g., "Stillwater",
// "Stillwater-Webhook"). An empty prefix defaults to "stillwater" (lowercase)
// to preserve the historical updater convention.
//
// When repoURL is non-empty the format is "<prefix>/<version> (<repoURL>)";
// otherwise it is "<prefix>/<version>". The MusicBrainz API requires the URL
// form; webhook receivers identify Stillwater-Webhook by the subsystem prefix.
func UserAgent(prefix, repoURL string) string {
	if prefix == "" {
		prefix = "stillwater"
	}
	// Defensive: ldflags injection from a CI pipeline that runs e.g.
	// `git describe | tr ...` can leak newlines or control characters into
	// Version. net/http.Client.Do() refuses to send headers whose values
	// contain such characters, which would silently fail every outbound
	// request to MusicBrainz, Wikipedia, Discogs, the updater, etc. Validate()
	// at startup logs a warning, but does not abort -- so we still need a
	// safe fallback here. Empty Version also falls back so the header is
	// never just "stillwater/".
	uaVersion := Version
	if uaVersion == "" || !httpguts.ValidHeaderFieldValue(uaVersion) {
		uaVersion = "unknown"
	}
	if repoURL != "" {
		return fmt.Sprintf("%s/%s (%s)", prefix, uaVersion, repoURL)
	}
	return prefix + "/" + uaVersion
}

// canonicalVersion returns v with a leading "v" if it is not already present.
// golang.org/x/mod/semver requires the "v" prefix.
func canonicalVersion(v string) string {
	if v != "" && v[0] != 'v' {
		return "v" + v
	}
	return v
}
