package rule

// PlatformPruneThreshold answers, for the Settings screen, the question the
// fixer and the background sweep ask before touching a media server: at what
// similarity would the platform phase of the duplicate-images rule run, and
// does it accept the rule's stored tolerance at all?
//
// configured is the rule's stored Config.Tolerance: zero means "never set" and
// reads as the rule default; any other value comes back unchanged, with
// accepted false when the platform phase would refuse it.
//
// A thin wrapper on purpose: it delegates to platformPruneTolerance, the one
// function the fixer, the sweep policy and the checker call. A second copy of
// the range could drift, and the screen would then promise a cleanup the fixer
// refuses (#3138 S4a).
func PlatformPruneThreshold(configured float64) (threshold float64, accepted bool) {
	return platformPruneTolerance(configured)
}
