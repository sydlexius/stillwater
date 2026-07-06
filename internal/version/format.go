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
//   - make build (git-describe version, no release marker): "v1.6.0-... (dev build)"
//   - Raw go build (no ldflags, Version is the DevVersion sentinel): "dev build"
//
// The "v" prefix is added if Version does not already start with one,
// because golang.org/x/mod/semver requires the "v" prefix, but the
// ldflags injected value typically omits it (e.g., "1.0.6").
func String() string {
	if Version == DevVersion {
		return "dev build"
	}
	v := canonicalVersion(Version)
	if IsDevBuild() {
		return v + " (dev build)"
	}
	return fmt.Sprintf("%s (commit %s, built %s)", v, Commit, Date)
}

// Validate asserts that Version is non-empty and either the DevVersion
// sentinel (non-release builds only) or a valid semver string according to
// golang.org/x/mod/semver rules (which require a "v" prefix).
//
// The "dev" sentinel is accepted for non-release builds (raw go build, IDE,
// CI), where full ldflags injection did not run. A release build must never
// carry it -- goreleaser always injects the tag -- so it is rejected when
// IsReleaseBuild() is true, keeping semver enforcement intact for real
// releases.
//
// Returns a wrapped error on failure so callers can use errors.Is/As to
// distinguish ErrEmpty from ErrInvalidSemver.
func Validate() error {
	if Version == "" {
		return fmt.Errorf("version: ldflags injection may be missing: %w", ErrEmpty)
	}
	if Version == DevVersion {
		if IsReleaseBuild() {
			return fmt.Errorf("version: release build carries the %q sentinel (ldflags injection missing): %w", DevVersion, ErrInvalidSemver)
		}
		return nil
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
// Version is NOT checked here: `make build` injects a real git-describe
// Version alongside non-"unknown" Commit/Date, so Commit/Date are the reliable
// dev signal. A raw `go build` leaves Version as the DevVersion sentinel, which
// String()/Validate() handle explicitly.
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
