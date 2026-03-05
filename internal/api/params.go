package api

import (
	"encoding/json"
	"net/http"
)

// RequirePathParam extracts a named path parameter from the request.
// If the value is empty, it writes a 400 error and returns ("", false).
// Callers must return immediately when the second return value is false.
func RequirePathParam(w http.ResponseWriter, req *http.Request, name string) (string, bool) {
	val := req.PathValue(name)
	if val == "" {
		writeError(w, req, http.StatusBadRequest, "missing "+name)
		return "", false
	}
	return val, true
}

// DecodeJSON decodes the JSON request body into target.
// If decoding fails, it writes a 400 error and returns false.
// Callers must return immediately when the return value is false.
func DecodeJSON(w http.ResponseWriter, req *http.Request, target any) bool {
	if err := json.NewDecoder(req.Body).Decode(target); err != nil {
		writeError(w, req, http.StatusBadRequest, "invalid request body")
		return false
	}
	return true
}
