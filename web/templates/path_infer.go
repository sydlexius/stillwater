package templates

// PathInferResult carries the outcome of a Lidarr path-mapping inference run to
// the connection path-mapping card so it can render a read-only info line. It is
// zero-valued (Show=false) on the initial page render, where no inference has
// run yet; the POST /connections/{id}/path-mappings/infer handler passes a
// populated value with Show=true to report what the manual re-infer found
// (#2329).
type PathInferResult struct {
	// Show gates the info line. False (the zero value) renders no line, used on
	// the first page render before any inference has been requested.
	Show bool
	// Inferred is the number of host->platform mappings the run derived.
	Inferred int
	// Matched is the number of artists that matched a Lidarr artist by MBID,
	// i.e. the pair count fed to inference. Reported even when Inferred is 0 so
	// the operator can tell "no artists matched" apart from "matched but no
	// consistent prefix difference".
	Matched int
	// Applied reports whether the inferred mappings were actually written. It is
	// false when Inferred > 0 but the connection already had mappings (the
	// empty-only precedence rule withheld the derived set): the info line then
	// tells the operator their existing mappings were kept, rather than implying
	// the inferred rows replaced them.
	Applied bool
}
