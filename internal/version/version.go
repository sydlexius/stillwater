package version

// These variables are set at build time via -ldflags.
var (
	Version = "1.3.0"
	Commit  = "unknown"
	Date    = "unknown"
)
