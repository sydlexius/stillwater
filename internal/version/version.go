package version

// DevVersion is the fallback Version for builds produced without ldflags
// injection (a raw `go build`/`go run` or an IDE run). Every build that
// matters overrides it: `make build` and CI inject `git describe --tags`, and
// goreleaser injects the release tag via `{{ .Version }}`. It is deliberately
// a non-semver sentinel so an un-injected build is obvious and never
// masquerades as a stale release version -- the release version comes solely
// from the git tag, so version.go never needs a hand-edit per release (#2254).
const DevVersion = "dev"

// These variables are set at build time via -ldflags.
//
// BuildType is the release-pipeline marker: goreleaser (both stable and
// nightly configs) injects "release"; nothing else does. IsReleaseBuild
// reads this explicitly because Commit/Date alone cannot distinguish a
// real release artifact from a developer's `make build` output (both get
// non-"unknown" values via the Makefile's git/date probes).
var (
	Version   = DevVersion
	Commit    = "unknown"
	Date      = "unknown"
	BuildType = ""
)
