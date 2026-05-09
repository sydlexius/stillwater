package version_test

import (
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/version"
)

// helpers that temporarily override the package-level vars for a test.
// We restore original values in t.Cleanup so parallel tests are unaffected.
// These are unit-level overrides -- not possible in black-box packages, but
// because this is an _test package within the same import we can address them
// via the exported vars directly.

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
	if err := version.Validate(); err == nil {
		t.Error("Validate() expected error for empty Version, got nil")
	}
}

func TestValidate_MalformedSemver(t *testing.T) {
	withVersion(t, "not-a-version", "abc1234", "2026-05-08")
	err := version.Validate()
	if err == nil {
		t.Error("Validate() expected error for malformed semver, got nil")
	}
	if !strings.Contains(err.Error(), "version:") {
		t.Errorf("Validate() error should contain 'version:' prefix, got: %v", err)
	}
}

func TestValidate_MajorVersionOnly(t *testing.T) {
	withVersion(t, "1", "abc1234", "2026-05-08")
	// "v1" is valid semver per golang.org/x/mod/semver rules
	if err := version.Validate(); err != nil {
		t.Errorf("Validate() unexpected error for major-only version: %v", err)
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

func TestUserAgent_WithURL(t *testing.T) {
	withVersion(t, "1.0.6", "unknown", "unknown")
	ua := version.UserAgent("https://github.com/sydlexius/stillwater")
	if !strings.HasPrefix(ua, "Stillwater/") {
		t.Errorf("UserAgent with URL should start with 'Stillwater/', got: %q", ua)
	}
	if !strings.Contains(ua, "https://github.com/sydlexius/stillwater") {
		t.Errorf("UserAgent with URL should contain the repoURL, got: %q", ua)
	}
	if !strings.Contains(ua, "1.0.6") {
		t.Errorf("UserAgent with URL should contain version, got: %q", ua)
	}
}

func TestUserAgent_WithoutURL(t *testing.T) {
	withVersion(t, "1.0.6", "unknown", "unknown")
	ua := version.UserAgent("")
	if !strings.HasPrefix(ua, "stillwater/") {
		t.Errorf("UserAgent without URL should start with 'stillwater/', got: %q", ua)
	}
	if strings.Contains(ua, "(") {
		t.Errorf("UserAgent without URL should not contain parentheses, got: %q", ua)
	}
	if !strings.Contains(ua, "1.0.6") {
		t.Errorf("UserAgent without URL should contain version, got: %q", ua)
	}
}
