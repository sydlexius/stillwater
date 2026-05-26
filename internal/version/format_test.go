package version_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/version"
)

// Tests in this file MUST run sequentially (no t.Parallel). withVersion mutates
// the package-level vars version.Version/Commit/Date and restores them via
// t.Cleanup; calling t.Parallel would race two tests writing the same globals.
// Do not add t.Parallel here without first refactoring withVersion to a
// per-call snapshot type.

func withVersion(t *testing.T, v, commit, date string) {
	t.Helper()
	origV, origC, origD := version.Version, version.Commit, version.Date
	version.Version = v
	version.Commit = commit
	version.Date = date
	t.Cleanup(func() {
		version.Version = origV
		version.Commit = origC
		version.Date = origD
	})
}

// ---- IsDevBuild ----

func TestIsDevBuild_CommitUnknown(t *testing.T) {
	withVersion(t, "1.0.6", "unknown", "2026-05-08")
	if !version.IsDevBuild() {
		t.Error("expected IsDevBuild() == true when Commit is 'unknown'")
	}
}

func TestIsDevBuild_DateUnknown(t *testing.T) {
	withVersion(t, "1.0.6", "abc1234", "unknown")
	if !version.IsDevBuild() {
		t.Error("expected IsDevBuild() == true when Date is 'unknown'")
	}
}

func TestIsDevBuild_BothUnknown(t *testing.T) {
	withVersion(t, "1.0.6", "unknown", "unknown")
	if !version.IsDevBuild() {
		t.Error("expected IsDevBuild() == true when both Commit and Date are 'unknown'")
	}
}

func TestIsDevBuild_ReleaseBuild(t *testing.T) {
	withVersion(t, "1.0.6", "abc1234", "2026-05-08")
	if version.IsDevBuild() {
		t.Error("expected IsDevBuild() == false for a release build")
	}
}

// ---- IsReleaseBuild ----

func TestIsReleaseBuild_DevBuild(t *testing.T) {
	withVersion(t, "1.0.6", "unknown", "unknown")
	if version.IsReleaseBuild() {
		t.Error("expected IsReleaseBuild() == false for a dev build")
	}
}

func TestIsReleaseBuild_ReleaseBuild(t *testing.T) {
	withVersion(t, "1.0.6", "abc1234", "2026-05-08")
	if !version.IsReleaseBuild() {
		t.Error("expected IsReleaseBuild() == true for a release build")
	}
}

// ---- Validate ----

func TestValidate_Success(t *testing.T) {
	withVersion(t, "1.0.6", "unknown", "unknown")
	if err := version.Validate(); err != nil {
		t.Errorf("Validate() unexpected error: %v", err)
	}
}

func TestValidate_SuccessWithVPrefix(t *testing.T) {
	withVersion(t, "v1.0.6", "abc1234", "2026-05-08")
	if err := version.Validate(); err != nil {
		t.Errorf("Validate() unexpected error with v-prefix: %v", err)
	}
}

func TestValidate_EmptyVersion(t *testing.T) {
	withVersion(t, "", "unknown", "unknown")
	err := version.Validate()
	if err == nil {
		t.Fatal("Validate() expected error for empty Version, got nil")
	}
	if !errors.Is(err, version.ErrEmpty) {
		t.Errorf("Validate() empty-Version error should wrap ErrEmpty, got: %v", err)
	}
	if errors.Is(err, version.ErrInvalidSemver) {
		t.Errorf("Validate() empty-Version error should NOT wrap ErrInvalidSemver, got: %v", err)
	}
}

func TestValidate_MalformedSemver(t *testing.T) {
	withVersion(t, "not-a-version", "abc1234", "2026-05-08")
	err := version.Validate()
	if err == nil {
		t.Fatal("Validate() expected error for malformed semver, got nil")
	}
	if !strings.Contains(err.Error(), "version:") {
		t.Errorf("Validate() error should contain 'version:' prefix, got: %v", err)
	}
	if !errors.Is(err, version.ErrInvalidSemver) {
		t.Errorf("Validate() malformed-Version error should wrap ErrInvalidSemver, got: %v", err)
	}
	if errors.Is(err, version.ErrEmpty) {
		t.Errorf("Validate() malformed-Version error should NOT wrap ErrEmpty, got: %v", err)
	}
}

func TestValidate_MajorVersionOnly(t *testing.T) {
	withVersion(t, "1", "abc1234", "2026-05-08")
	// "v1" is valid semver per golang.org/x/mod/semver rules
	if err := version.Validate(); err != nil {
		t.Errorf("Validate() unexpected error for major-only version: %v", err)
	}
}

// Whitespace contamination is a realistic ldflags-injection failure mode --
// build pipelines that capture `git describe` via `$(...)` or `tr` can leave
// trailing newlines or leading spaces in Version. semver.IsValid rejects all
// such forms; pin that contract here so a future "TrimSpace before assigning"
// refactor cannot silently mask the very class of bug Validate exists to catch.
func TestValidate_RejectsWhitespaceContamination(t *testing.T) {
	cases := []string{" 1.0.6", "1.0.6 ", "1.0.6\n", "\t1.0.6"}
	for _, raw := range cases {
		withVersion(t, raw, "abc1234", "2026-05-08")
		err := version.Validate()
		if err == nil {
			t.Errorf("Validate() expected error for whitespace-contaminated Version %q, got nil", raw)
			continue
		}
		if !errors.Is(err, version.ErrInvalidSemver) {
			t.Errorf("Validate() whitespace error for %q should wrap ErrInvalidSemver, got: %v", raw, err)
		}
	}
}

// Pre-release and build-metadata semver are valid per golang.org/x/mod/semver
// and ARE emitted by goreleaser for RC builds (e.g. v1.0.6-rc.1) and for
// per-platform release artifacts (e.g. 1.0.6+darwin-arm64). Lock the contract
// so a future stricter regex refactor cannot silently break RC releases.
func TestValidate_AcceptsPrereleaseAndBuildMetadata(t *testing.T) {
	cases := []string{"1.0.6-rc.1", "1.0.6-beta.2", "1.0.6+darwin-arm64", "1.0.6-rc.1+build.20260509"}
	for _, raw := range cases {
		withVersion(t, raw, "abc1234", "2026-05-08")
		if err := version.Validate(); err != nil {
			t.Errorf("Validate() unexpected error for valid pre-release/build-meta %q: %v", raw, err)
		}
	}
}

// ---- String ----

func TestString_DevBuild(t *testing.T) {
	withVersion(t, "1.0.6", "unknown", "unknown")
	s := version.String()
	if !strings.Contains(s, "dev build") {
		t.Errorf("String() for dev build should contain 'dev build', got: %q", s)
	}
	if !strings.Contains(s, "v1.0.6") {
		t.Errorf("String() for dev build should contain version, got: %q", s)
	}
}

func TestString_ReleaseBuild(t *testing.T) {
	withVersion(t, "1.0.6", "abc1234", "2026-05-08")
	s := version.String()
	if strings.Contains(s, "dev build") {
		t.Errorf("String() for release build should not contain 'dev build', got: %q", s)
	}
	if !strings.Contains(s, "abc1234") {
		t.Errorf("String() for release build should contain commit hash, got: %q", s)
	}
	if !strings.Contains(s, "2026-05-08") {
		t.Errorf("String() for release build should contain build date, got: %q", s)
	}
	if !strings.Contains(s, "v1.0.6") {
		t.Errorf("String() for release build should contain version, got: %q", s)
	}
}

func TestString_VPrefixNotDoubled(t *testing.T) {
	withVersion(t, "v1.0.6", "abc1234", "2026-05-08")
	s := version.String()
	if strings.Contains(s, "vv") {
		t.Errorf("String() should not double the 'v' prefix, got: %q", s)
	}
}

// ---- UserAgent ----

func TestUserAgent_WithCapitalPrefixAndURL(t *testing.T) {
	withVersion(t, "1.0.6", "unknown", "unknown")
	ua := version.UserAgent("Stillwater", "https://github.com/sydlexius/stillwater")
	if !strings.HasPrefix(ua, "Stillwater/") {
		t.Errorf("UserAgent with capital prefix should start with 'Stillwater/', got: %q", ua)
	}
	if !strings.Contains(ua, "(https://github.com/sydlexius/stillwater)") {
		t.Errorf("UserAgent with URL should contain '(repoURL)', got: %q", ua)
	}
	if !strings.Contains(ua, "1.0.6") {
		t.Errorf("UserAgent should contain version, got: %q", ua)
	}
}

func TestUserAgent_EmptyPrefixDefaultsToLowercase(t *testing.T) {
	withVersion(t, "1.0.6", "unknown", "unknown")
	ua := version.UserAgent("", "")
	if !strings.HasPrefix(ua, "stillwater/") {
		t.Errorf("UserAgent with empty prefix should default to 'stillwater/', got: %q", ua)
	}
	if strings.Contains(ua, "(") {
		t.Errorf("UserAgent without URL should not contain parentheses, got: %q", ua)
	}
}

func TestUserAgent_CapitalPrefixWithoutURL(t *testing.T) {
	withVersion(t, "1.0.6", "unknown", "unknown")
	ua := version.UserAgent("Stillwater", "")
	if ua != "Stillwater/1.0.6" {
		t.Errorf("UserAgent capital-no-URL should be 'Stillwater/1.0.6', got: %q", ua)
	}
}

func TestUserAgent_SubsystemPrefix(t *testing.T) {
	withVersion(t, "1.0.6", "unknown", "unknown")
	ua := version.UserAgent("Stillwater-Webhook", "")
	if ua != "Stillwater-Webhook/1.0.6" {
		t.Errorf("UserAgent subsystem prefix should be 'Stillwater-Webhook/1.0.6', got: %q", ua)
	}
}

// Header-unsafe Version values must be replaced with "unknown" so that
// net/http.Client.Do() does not reject every outbound request. Whitespace
// contamination is the realistic ldflags failure mode (newline leak from
// `git describe | tr` in CI), but we also pin null-byte and control-char
// rejection because those are the canonical "unsafe header value" cases.
func TestUserAgent_FallsBackForHeaderUnsafeVersion(t *testing.T) {
	cases := []string{"1.0.6\n", "1.0.6\r", "1.0.6\x00", ""}
	for _, raw := range cases {
		withVersion(t, raw, "abc1234", "2026-05-09")
		ua := version.UserAgent("Stillwater", "")
		if raw != "" && strings.Contains(ua, raw) {
			t.Errorf("UserAgent leaked unsafe Version %q into header: %q", raw, ua)
		}
		if !strings.Contains(ua, "unknown") {
			t.Errorf("UserAgent should fall back to 'unknown' for unsafe Version %q, got: %q", raw, ua)
		}
	}
}
