package templates

import (
	"encoding/json"
	"strconv"
)

// WizardCandidateView is the flat projection of a scored provider candidate
// used by the re-identify wizard. The handler package converts its domain
// ScoredCandidate into this type so the template does not need to import the
// api package (which would create a dependency cycle).
type WizardCandidateView struct {
	Name           string
	MBID           string
	Country        string
	Disambiguation string
	ConfidencePct  int
}

// wizardCandidatesNil reports whether the Candidates field on the step data
// was never populated (pre-fetch still in flight). We accept the nil-slice
// sentinel from the handler in two forms: a raw nil any, or an explicit
// typed nil slice.
func wizardCandidatesNil(c any) bool {
	if c == nil {
		return true
	}
	views, ok := c.([]WizardCandidateView)
	if !ok {
		// Any other non-nil value means "the handler passed candidates but
		// failed to project them", which we surface as empty rather than
		// hiding behind the spinner.
		return false
	}
	return views == nil
}

// wizardCandidatesLen returns the number of candidates the step has to show.
func wizardCandidatesLen(c any) int {
	views, ok := c.([]WizardCandidateView)
	if !ok {
		return 0
	}
	return len(views)
}

// wizardCandidatesIter returns a slice safe to range over from the template.
func wizardCandidatesIter(c any) []WizardCandidateView {
	views, ok := c.([]WizardCandidateView)
	if !ok {
		return nil
	}
	return views
}

// wizardStepURL is the GET URL for a specific wizard step fragment.
func wizardStepURL(sessionID string, index int) string {
	return "/artists/re-identify/wizard/" + sessionID + "/step/" + itoa(index)
}

// wizardActionURL is the POST URL for a per-step decision action.
func wizardActionURL(sessionID string, index int, action string) string {
	return "/api/v1/artists/re-identify/wizard/" + sessionID + "/step/" + itoa(index) + "/" + action
}

// wizardAcceptVals builds the hx-vals JSON literal for the accept button.
// Uses encoding/json rather than string concatenation so that any mbid
// containing quotes, backslashes, or other JSON-special characters is
// properly escaped. The empty-object fallback keeps the accept handler from
// panicking on malformed input -- the server-side mbid presence check will
// then reject the request with a 400.
func wizardAcceptVals(mbid string) string {
	b, err := json.Marshal(map[string]string{"mbid": mbid})
	if err != nil {
		return "{}"
	}
	return string(b)
}

// wizardSaveExitURL is the POST URL that ends the session early.
func wizardSaveExitURL(sessionID string) string {
	return "/api/v1/artists/re-identify/wizard/" + sessionID + "/save-exit"
}

// itoa delegates to strconv.Itoa. Kept as a package-private alias so the
// wizard URL-assembly helpers stay compact and can move to a bytes/builder
// approach later without touching call sites.
func itoa(n int) string {
	return strconv.Itoa(n)
}
