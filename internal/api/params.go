package api

import (
	"encoding/json"
	"errors"
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

// maxJSONBodyBytes caps the size of a JSON request body read by DecodeJSON.
// Authenticated JSON endpoints exchange small config/settings payloads; this
// is generous headroom against a client exhausting memory via an oversized body.
const maxJSONBodyBytes = 10 << 20 // 10 MB

// DecodeJSON decodes the JSON request body into target.
// If decoding fails, it writes a 400 error and returns false. A body
// exceeding maxJSONBodyBytes yields a 413 error instead.
// Callers must return immediately when the return value is false.
func DecodeJSON(w http.ResponseWriter, req *http.Request, target any) bool {
	req.Body = http.MaxBytesReader(w, req.Body, maxJSONBodyBytes)
	if err := json.NewDecoder(req.Body).Decode(target); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, req, http.StatusRequestEntityTooLarge, "request body too large")
			return false
		}
		writeError(w, req, http.StatusBadRequest, "invalid request body")
		return false
	}
	return true
}
