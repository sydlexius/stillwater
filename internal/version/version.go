package version

// These variables are set at build time via -ldflags.
var (
	Version = "1.0.5"
	Commit  = "unknown"
	Date    = "unknown"
)
