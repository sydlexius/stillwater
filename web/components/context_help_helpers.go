package components

import "strings"

const docsBaseURL = "https://sydlexius.github.io/stillwater"

// contextHelpDocURL returns the absolute "Read more" URL for a ContextHelp
// docAnchor.
//
// Routing rules:
//   - If docAnchor contains a slash, it is treated as a path-and-fragment
//     relative to the docs root (e.g. "core-concepts/field-locks#layer-1").
//     The returned URL is docsBaseURL + "/" + docAnchor.
//   - Otherwise the legacy settings-reference prefix is applied, keeping all
//     existing Settings ContextHelp call sites unchanged.
func contextHelpDocURL(docAnchor string) string {
	if strings.Contains(docAnchor, "/") {
		return docsBaseURL + "/" + docAnchor
	}
	return docsBaseURL + "/reference/settings-by-tab/#" + docAnchor
}
