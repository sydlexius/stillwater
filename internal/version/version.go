package version

// These variables are set at build time via -ldflags.
var (
	Version = "1.2.0"
	Commit  = "unknown"
	Date    = "unknown"
)
