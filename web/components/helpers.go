package components

import "encoding/json"

// escapeJSONValue escapes special characters in a string for safe embedding
// in a JSON value within an HTML attribute.
func escapeJSONValue(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return s
	}
	// json.Marshal wraps the string in quotes; strip them for embedding in hx-vals.
	return string(b[1 : len(b)-1])
}
