package version

// These variables are set at build time via -ldflags.
var (
	Version = "1.0.6"
	Commit  = "unknown"
	Date    = "unknown"
)
