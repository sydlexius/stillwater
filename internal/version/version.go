package version

// These variables are set at build time via -ldflags.
var (
	Version = "0.1.0"
	Commit  = "unknown"
	Date    = "unknown"
)
