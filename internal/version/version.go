package version

// These variables are set at build time via -ldflags.
//
// BuildType is the release-pipeline marker: goreleaser (both stable and
// nightly configs) injects "release"; nothing else does. IsReleaseBuild
// reads this explicitly because Commit/Date alone cannot distinguish a
// real release artifact from a developer's `make build` output (both get
// non-"unknown" values via the Makefile's git/date probes).
var (
	Version   = "1.6.0-rc0"
	Commit    = "unknown"
	Date      = "unknown"
	BuildType = ""
)
