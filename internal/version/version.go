package version

// These variables are set at build time via -ldflags.
var (
	Version = "0.9.5"
	Commit  = "unknown"
	Date    = "unknown"
)
