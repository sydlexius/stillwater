package version

// These variables are set at build time via -ldflags.
var (
	Version = "0.9.6"
	Commit  = "unknown"
	Date    = "unknown"
)
