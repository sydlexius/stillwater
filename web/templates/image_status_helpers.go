package templates

import (
	"strings"

	"github.com/sydlexius/stillwater/internal/provider"
)

// Helpers backing the per-provider status banner in the image-search results
// templates (issue #2457). They exist so a thin or empty result set is never
// mistaken for "we searched everywhere and there is nothing out there".

// skippedProviderNames returns a comma-separated list of the display names of
// providers that were never queried, in orchestrator priority order. It returns
// the empty string when nothing was skipped, which is what the templates branch
// on.
func skippedProviderNames(statuses []provider.ProviderImageStatus) string {
	names := make([]string, 0, len(statuses))
	for _, st := range statuses {
		if st.Outcome == provider.ImageOutcomeSkipped {
			names = append(names, st.Provider.DisplayName())
		}
	}
	return strings.Join(names, ", ")
}

// erroredProviderStatuses returns the statuses of providers that were queried
// and failed. Each carries an already-scrubbed message in Reason; templates must
// not render any other error text, because raw provider errors can embed the
// API key from the request URL.
func erroredProviderStatuses(statuses []provider.ProviderImageStatus) []provider.ProviderImageStatus {
	errored := make([]provider.ProviderImageStatus, 0, len(statuses))
	for _, st := range statuses {
		if st.Outcome == provider.ImageOutcomeErrored {
			errored = append(errored, st)
		}
	}
	return errored
}

// allProvidersSkipped reports whether every provider the orchestrator iterated
// was skipped, meaning nothing was searched at all. It is false for an empty
// status list: no statuses means no information, not "everything was skipped".
func allProvidersSkipped(statuses []provider.ProviderImageStatus) bool {
	if len(statuses) == 0 {
		return false
	}
	for _, st := range statuses {
		if st.Outcome != provider.ImageOutcomeSkipped {
			return false
		}
	}
	return true
}
