package version

// These variables are set at build time via -ldflags.
var (
	Version = "1.4.0"
	Commit  = "unknown"
	Date    = "unknown"
)
