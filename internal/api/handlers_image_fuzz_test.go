package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

// FuzzHandleImageFetch feeds arbitrary byte slices to the JSON request body
// decoder used by handleImageFetch (extractImageFetchParams). The decoder
// must never panic regardless of input; returning a parse error for invalid
// JSON is expected and correct.
//
// Target: internal/api/handlers_image.go line 1497 (extractImageFetchParams,
// json.NewDecoder(req.Body).Decode into the URL+Type struct, exercised by
// POST /api/v1/artists/{id}/images/fetch).
//
// Seeds cover the happy-path JSON shape plus pathological variants the
// regular fixtures in handlers_image_test.go and handlers_image_coverage_test.go
// already drive through the handler. The fuzz target then calls
// extractImageFetchParams directly so each iteration is fast and panic-free
// regardless of network/database availability.
func FuzzHandleImageFetch(f *testing.F) {
	// Happy-path: thumb fetch.
	f.Add([]byte(`{"url":"https://example.com/x.jpg","type":"thumb"}`))
	// Happy-path: fanart fetch.
	f.Add([]byte(`{"url":"https://example.com/fanart.jpg","type":"fanart"}`))
	// Happy-path: logo fetch.
	f.Add([]byte(`{"url":"https://example.com/logo.png","type":"logo"}`))
	// Happy-path: banner fetch.
	f.Add([]byte(`{"url":"https://example.com/banner.jpg","type":"banner"}`))
	// Provider-aliased type normalization (hdlogo -> logo).
	f.Add([]byte(`{"url":"https://example.com/hdlogo.png","type":"hdlogo"}`))
	// Empty URL (handler rejects later, decoder must not panic).
	f.Add([]byte(`{"url":"","type":"thumb"}`))
	// Unknown type field (handler rejects later).
	f.Add([]byte(`{"url":"https://example.com/x.jpg","type":"poster"}`))
	// Extra unknown fields that the decoder should ignore.
	f.Add([]byte(`{"url":"https://example.com/x.jpg","type":"thumb","extra":42,"more":[1,2,3]}`))
	// Missing both fields.
	f.Add([]byte(`{}`))
	// Bare null.
	f.Add([]byte(`null`))
	// Empty body.
	f.Add([]byte(``))
	// Array where object expected.
	f.Add([]byte(`["https://example.com/x.jpg","thumb"]`))
	// URL field is an array instead of string.
	f.Add([]byte(`{"url":["a","b"],"type":"thumb"}`))
	// Numeric type field.
	f.Add([]byte(`{"url":"https://example.com/x.jpg","type":42}`))
	// Deeply nested structure.
	f.Add([]byte(`{"url":"https://example.com/x.jpg","type":"thumb","nested":{"a":{"b":{"c":"d"}}}}`))
	// NUL bytes embedded in the URL value.
	f.Add(append(
		[]byte(`{"url":"https://example.com/`),
		append([]byte{0x00}, []byte(`x.jpg","type":"thumb"}`)...)...))
	// Trailing garbage after a valid object (json.Decoder stops at the first
	// object so this should still decode cleanly).
	f.Add([]byte(`{"url":"https://example.com/x.jpg","type":"thumb"}garbage`))
	// Invalid JSON.
	f.Add([]byte(`{not valid json`))
	// Large URL string to exercise allocator paths.
	bigURL := make([]byte, 0, 64+512*1024)
	bigURL = append(bigURL, []byte(`{"url":"https://example.com/`)...)
	for i := 0; i < 512*1024; i++ {
		bigURL = append(bigURL, 'A')
	}
	bigURL = append(bigURL, []byte(`","type":"thumb"}`)...)
	f.Add(bigURL)

	f.Fuzz(func(t *testing.T, data []byte) {
		// Build a request that mimics what the router hands to the handler.
		// We exercise extractImageFetchParams directly: it is the JSON
		// decode boundary and the function that must stay panic-free. The
		// full handler path requires DB + provider services; the decoder
		// itself has no such dependencies, so this fuzz iteration is fast
		// and isolates the JSON layer.
		req := httptest.NewRequest(http.MethodPost,
			"/api/v1/artists/x/images/fetch", bytes.NewReader(data))
		req.Header.Set("Content-Type", "application/json")
		// The decoder must not panic. Errors for invalid input are expected.
		_, _, _ = extractImageFetchParams(req)
	})
}
