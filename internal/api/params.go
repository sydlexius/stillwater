package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
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

// formBoolPtr reads a form field as an optional boolean, mirroring the
// pointer-as-tristate semantics the JSON request bodies use: a key the form
// did not carry at all yields nil, meaning "leave the stored value unchanged"
// rather than "set false". This matters because HTML forms render only the
// fields they display -- collapsing an unrendered checkbox into false would
// clear settings the operator never touched.
//
// req.PostForm is read directly rather than via req.FormValue because
// FormValue returns "" for both an absent key and a present-but-empty one,
// which cannot express the tristate.
//
// "on" is accepted alongside the strconv.ParseBool set because a checked
// checkbox with no value attribute posts the literal string "on".
//
// The second return value reports whether the field was WELL-FORMED, which is
// not the same question as whether it was present: a field the form omitted
// entirely is (nil, true), while a field carrying a value that is not a
// boolean is (nil, false). Both leave the stored value unchanged -- the safe
// direction, since a garbled value must never disable a connection -- but the
// false distinguishes a client bug from a deliberate omission so the caller
// can log it rather than let it vanish.
func formBoolPtr(req *http.Request, key string) (*bool, bool) {
	if err := req.ParseForm(); err != nil {
		return nil, false
	}
	vs, ok := req.PostForm[key]
	if !ok || len(vs) == 0 {
		return nil, true
	}
	raw := strings.ToLower(strings.TrimSpace(vs[0]))
	if raw == "on" {
		v := true
		return &v, true
	}
	parsed, err := strconv.ParseBool(raw)
	if err != nil {
		return nil, false
	}
	return &parsed, true
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
