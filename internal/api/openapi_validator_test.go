package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"
)

// kin-openapi runtime request + response validation for handler tests.
//
// `TestOpenAPIConsistency` only checks that handler-emitted field names appear
// somewhere in the spec; it cannot catch enum drift, wrong status codes,
// `oneOf` discriminator mismatches, request bodies that don't match their
// declared schema, or required-field omissions. The wrapper below loads
// `openapi.yaml` once per test binary, matches each incoming request to its
// spec operation via gorilla/mux-style routing, and asserts both directions
// conform to the spec. Adoption is opt-in per test: tests that want spec
// conformance call `serveValidated` instead of constructing a recorder and
// invoking `h.ServeHTTP` directly.
//
// Fuzz tests are NOT wrapped: their input is hostile by design, so request
// validation would fail on nearly every fuzzed seed, drowning the signal that
// the wrapper is meant to surface. Fuzz still proves the no-panic invariant;
// the wrapper proves spec conformance on happy paths.

var (
	specOnce   sync.Once
	specRouter routers.Router
	specErr    error
)

// loadSpec parses internal/api/openapi.yaml once per test binary and builds
// a gorillamux router for operation lookup. The spec path is resolved
// relative to this source file via runtime.Caller so the test binary's
// working directory does not matter.
func loadSpec(t *testing.T) routers.Router {
	t.Helper()
	specOnce.Do(func() {
		_, thisFile, _, ok := runtime.Caller(0)
		if !ok {
			specErr = errors.New("runtime.Caller(0) returned ok=false")
			return
		}
		path := filepath.Join(filepath.Dir(thisFile), "openapi.yaml")
		loader := openapi3.NewLoader()
		doc, err := loader.LoadFromFile(path)
		if err != nil {
			specErr = err
			return
		}
		ctx := context.Background()
		if err := doc.Validate(ctx); err != nil {
			specErr = err
			return
		}
		// Replace the spec's host-bearing server URL with a path-only base
		// so gorillamux matches on path alone. httptest.NewRequest defaults
		// req.Host to "example.com" which would never match localhost:1973
		// from the spec, returning "no matching operation" for every test
		// request. Path-only matching is what we want at the test layer;
		// host validation isn't part of the contract surface this wrapper
		// proves.
		doc.Servers = openapi3.Servers{&openapi3.Server{URL: "/api/v1"}}
		specRouter, specErr = gorillamux.NewRouter(doc)
	})
	if specErr != nil {
		t.Fatalf("openapi: loading spec: %v", specErr)
	}
	return specRouter
}

// serveValidated invokes h against req while asserting both directions
// conform to openapi.yaml. Any validation failure calls t.Fatalf so the
// adopting test stays a 1-line call.
//
// For tests that want to assert the wrapper REJECTS drift, call
// validateExchange directly and check the returned error -- avoids the
// goexit-from-Fatalf complication when probing negative invariants.
func serveValidated(t *testing.T, h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	w, err := validateExchange(loadSpec(t), h, req)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return w
}

// validateExchange is the testable core of the wrapper: returns an error
// instead of fataling so negative tests can assert drift rejection.
//
//  1. Find the matching spec operation (gorillamux strips the spec's server
//     base URL before matching).
//  2. Validate the request (headers, query, path params, body schema).
//  3. Restore req.Body (validation consumes it) and run the handler.
//  4. Validate response status + headers + body against the operation's
//     declared responses.
//
// Authentication validation is short-circuited via NoopAuthenticationFunc
// because handler tests construct sessions / tokens through internal services
// rather than the cookie/header shapes the spec declares; auth is exercised
// by separate session/middleware tests outside this wrapper.
func validateExchange(specR routers.Router, h http.Handler, req *http.Request) (*httptest.ResponseRecorder, error) {
	var bodyBytes []byte
	if req.Body != nil {
		var err error
		bodyBytes, err = io.ReadAll(req.Body)
		if err != nil {
			return nil, fmt.Errorf("openapi: reading request body: %w", err)
		}
		_ = req.Body.Close()
		req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	}

	route, pathParams, err := specR.FindRoute(req)
	if err != nil {
		return nil, fmt.Errorf("openapi: no spec route for %s %s: %w", req.Method, req.URL.Path, err)
	}

	ctx := req.Context()

	// IncludeResponseStatus flips kin-openapi from lenient (default) to
	// strict: without it ValidateResponse silently passes undeclared status
	// codes, defeating the wrapper's drift-catching purpose. The shared
	// options instance is attached to both inputs because ValidateRequest
	// reads from RequestValidationInput.Options and ValidateResponse reads
	// from ResponseValidationInput.Options (no inheritance between the
	// two).
	opts := &openapi3filter.Options{
		AuthenticationFunc:    openapi3filter.NoopAuthenticationFunc,
		IncludeResponseStatus: true,
	}

	reqInput := &openapi3filter.RequestValidationInput{
		Request:    req,
		PathParams: pathParams,
		Route:      route,
		Options:    opts,
	}
	if err := openapi3filter.ValidateRequest(ctx, reqInput); err != nil {
		return nil, fmt.Errorf("openapi request validation [%s %s]: %w",
			req.Method, req.URL.Path, err)
	}

	if bodyBytes != nil {
		req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	}

	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	respInput := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: reqInput,
		Status:                 w.Code,
		Header:                 w.Header(),
		Options:                opts,
	}
	respInput.SetBodyBytes(w.Body.Bytes())
	if err := openapi3filter.ValidateResponse(ctx, respInput); err != nil {
		return w, fmt.Errorf("openapi response validation [%s %s] status=%d: %w",
			req.Method, req.URL.Path, w.Code, err)
	}

	return w, nil
}

// --- Negative tests for the wrapper itself ---
//
// Lock in the invariants that make the wrapper useful: it must reject
// undeclared status codes, response bodies that violate declared schemas,
// request bodies missing required fields, and routes that aren't in the
// spec. Without these, a future refactor that drops IncludeResponseStatus
// or otherwise neuters the validator would leave every adopting test
// silently vacuous.

func TestValidateExchange_RejectsUndeclaredResponseStatus(t *testing.T) {
	t.Parallel()
	specR := loadSpec(t)

	// /auth/setup declares 201/400/401/403/409/500/502. 418 is not declared.
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte(`{"error":"intentional"}`))
	})
	body := `{"auth_method":"local","username":"admin","password":"correcthorse"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/setup", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")

	_, err := validateExchange(specR, h, req)
	if err == nil {
		t.Fatal("expected error for undeclared status 418; got nil")
	}
}

func TestValidateExchange_RejectsResponseBodyMissingRequiredField(t *testing.T) {
	t.Parallel()
	specR := loadSpec(t)

	// /auth/setup 400 response body $ref's Error which has required: [error].
	// Emit a 400 with the required field missing.
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"unknown":"value"}`))
	})
	body := `{"auth_method":"local","username":"admin","password":"correcthorse"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/setup", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")

	_, err := validateExchange(specR, h, req)
	if err == nil {
		t.Fatal("expected error for response body missing required 'error' field; got nil")
	}
}

func TestValidateExchange_RejectsRequestBodyMissingRequiredField(t *testing.T) {
	t.Parallel()
	specR := loadSpec(t)

	// /auth/setup request body declares required: [username, password]. Omit
	// password.
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})
	body := `{"auth_method":"local","username":"admin"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/setup", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")

	_, err := validateExchange(specR, h, req)
	if err == nil {
		t.Fatal("expected error for request body missing 'password'; got nil")
	}
}

func TestValidateExchange_RejectsUnknownRoute(t *testing.T) {
	t.Parallel()
	specR := loadSpec(t)

	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/does/not/exist", nil)

	_, err := validateExchange(specR, h, req)
	if err == nil {
		t.Fatal("expected error for unknown route; got nil")
	}
}
