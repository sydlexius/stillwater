package httpclient

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// newTestClient creates a BaseClient pointed at srv with a simple bearer auth func.
func newTestClient(srv *httptest.Server, apiKey string) *BaseClient {
	bc := NewBase(srv.URL, apiKey, srv.Client(), testLogger(), "test")
	bc.AuthFunc = func(req *http.Request) {
		req.Header.Set("X-Test-Key", bc.APIKey)
	}
	return &bc
}

func TestGet_Success(t *testing.T) {
	type payload struct {
		Name string `json:"name"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Test-Key") != "secret" {
			t.Errorf("auth header missing or wrong: %q", r.Header.Get("X-Test-Key"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"ok"}`))
	}))
	defer srv.Close()

	c := newTestClient(srv, "secret")
	var result payload
	if err := c.Get(context.Background(), "/test", &result); err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if result.Name != "ok" {
		t.Errorf("name = %q, want ok", result.Name)
	}
}

func TestGet_NilResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(srv, "key")
	if err := c.Get(context.Background(), "/ping", nil); err != nil {
		t.Fatalf("Get with nil result failed: %v", err)
	}
}

func TestGet_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("not found"))
	}))
	defer srv.Close()

	c := newTestClient(srv, "key")
	err := c.Get(context.Background(), "/missing", nil)
	if err == nil {
		t.Fatal("expected error for 404")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error %q does not mention 404", err)
	}
}

func TestGet_ErrorBodyLimited(t *testing.T) {
	largeBody := strings.Repeat("x", 4096)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(largeBody))
	}))
	defer srv.Close()

	c := newTestClient(srv, "key")
	err := c.Get(context.Background(), "/boom", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if len(err.Error()) > 1100 {
		t.Errorf("error length %d exceeds expected bounded size", len(err.Error()))
	}
}

func TestPost_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := newTestClient(srv, "key")
	if err := c.Post(context.Background(), "/action", nil); err != nil {
		t.Fatalf("Post failed: %v", err)
	}
}

func TestPost_ErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("server error"))
	}))
	defer srv.Close()

	c := newTestClient(srv, "key")
	if err := c.Post(context.Background(), "/fail", nil); err == nil {
		t.Fatal("expected error for 500")
	}
}

func TestPost_ErrorBodyLimited(t *testing.T) {
	largeBody := strings.Repeat("y", 4096)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(largeBody))
	}))
	defer srv.Close()

	c := newTestClient(srv, "key")
	err := c.Post(context.Background(), "/fail", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if len(err.Error()) > 1100 {
		t.Errorf("error length %d exceeds expected bounded size", len(err.Error()))
	}
}

func TestGetRaw_Success(t *testing.T) {
	imageData := []byte{0xFF, 0xD8, 0xFF, 0xE0} // JPEG magic bytes
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(imageData)
	}))
	defer srv.Close()

	c := newTestClient(srv, "key")
	data, ct, err := c.GetRaw(context.Background(), "/image")
	if err != nil {
		t.Fatalf("GetRaw failed: %v", err)
	}
	if ct != "image/jpeg" {
		t.Errorf("content-type = %q, want image/jpeg", ct)
	}
	if len(data) != len(imageData) {
		t.Errorf("got %d bytes, want %d", len(data), len(imageData))
	}
}

func TestGetRaw_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("not found"))
	}))
	defer srv.Close()

	c := newTestClient(srv, "key")
	_, _, err := c.GetRaw(context.Background(), "/image")
	if err == nil {
		t.Fatal("expected error for 404")
	}
}

func TestGetRaw_OversizedImage(t *testing.T) {
	const maxImageSize = 25 << 20
	oversized := make([]byte, maxImageSize+1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(oversized)
	}))
	defer srv.Close()

	c := newTestClient(srv, "key")
	_, _, err := c.GetRaw(context.Background(), "/big")
	if err == nil {
		t.Fatal("expected error for oversized image")
	}
	if !strings.Contains(err.Error(), "exceeds 25 MB") {
		t.Errorf("error %q does not mention 25 MB limit", err)
	}
}

func TestPostJSON_Success(t *testing.T) {
	type result struct {
		ID int `json:"id"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", r.Header.Get("Content-Type"))
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"name":"test"}` {
			t.Errorf("body = %q, want {\"name\":\"test\"}", string(body))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":42}`))
	}))
	defer srv.Close()

	c := newTestClient(srv, "key")
	var r result
	body, _ := json.Marshal(map[string]string{"name": "test"})
	if err := c.PostJSON(context.Background(), "/create", strings.NewReader(string(body)), &r); err != nil {
		t.Fatalf("PostJSON failed: %v", err)
	}
	if r.ID != 42 {
		t.Errorf("id = %d, want 42", r.ID)
	}
}

func TestPostJSON_ErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"bad"}`))
	}))
	defer srv.Close()

	c := newTestClient(srv, "key")
	if err := c.PostJSON(context.Background(), "/fail", nil, nil); err == nil {
		t.Fatal("expected error for 400")
	}
}

// TestDo_SetsMethodPathAuthAndContentType exercises the raw primitive the
// mediabrowser image write/delete free functions build on: unlike Get/
// GetRaw/PostJSON, Do does not interpret the response status or decode a
// body -- it just issues the request and hands back the unconsumed
// response, leaving status handling to the caller.
func TestDo_SetsMethodPathAuthAndContentType(t *testing.T) {
	var gotMethod, gotPath, gotAuth, gotContentType string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotAuth = r.Header.Get("X-Test-Key")
		gotContentType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(srv, "key")
	resp, err := c.Do(context.Background(), http.MethodPost, "/images/thumb", strings.NewReader("payload"), "text/plain")
	if err != nil {
		t.Fatalf("Do failed: %v", err)
	}
	defer resp.Body.Close()

	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if gotPath != "/images/thumb" {
		t.Errorf("path = %s, want /images/thumb", gotPath)
	}
	if gotAuth != "key" {
		t.Errorf("auth header = %q, want %q", gotAuth, "key")
	}
	if gotContentType != "text/plain" {
		t.Errorf("Content-Type = %q, want text/plain", gotContentType)
	}
	if string(gotBody) != "payload" {
		t.Errorf("body = %q, want %q", gotBody, "payload")
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

// TestDo_NilBodyBecomesNoBody covers the DELETE-with-no-body path: a nil
// io.Reader must not panic and must reach the server as an empty body,
// matching what DeleteImageRaw/DeleteImageAtIndexRaw pass.
func TestDo_NilBodyBecomesNoBody(t *testing.T) {
	var gotMethod string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := newTestClient(srv, "key")
	resp, err := c.Do(context.Background(), http.MethodDelete, "/images/thumb", nil, "")
	if err != nil {
		t.Fatalf("Do failed: %v", err)
	}
	defer resp.Body.Close()

	if gotMethod != http.MethodDelete {
		t.Errorf("method = %s, want DELETE", gotMethod)
	}
	if len(gotBody) != 0 {
		t.Errorf("body = %q, want empty", gotBody)
	}
}

// TestDo_DoesNotInterpretStatus proves Do's contract: it never turns a
// non-2xx response into an error. Callers (the mediabrowser free functions)
// own reading resp.StatusCode themselves, unlike Get/PostJSON/PutJSON.
func TestDo_DoesNotInterpretStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer srv.Close()

	c := newTestClient(srv, "key")
	resp, err := c.Do(context.Background(), http.MethodPost, "/x", nil, "")
	if err != nil {
		t.Fatalf("Do returned an error for a 500 response; it should only surface transport errors: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
}

func TestNewBase_InvalidURL(t *testing.T) {
	bc := NewBase("not-a-url", "key", http.DefaultClient, testLogger(), "test")
	bc.AuthFunc = func(*http.Request) {}
	// BaseURL should be empty after failed validation
	if bc.BaseURL != "" {
		t.Errorf("BaseURL = %q, want empty for invalid URL", bc.BaseURL)
	}
}

// TestStatusError_ErrorAndIsAuth pins the substring shape that the publish
// layer's classifyPushErr depends on, plus the 401/403 IsAuth contract used
// by per-package ErrAuthRequired wrappers.
func TestReadBoundedStatusError_BodyAndStatusCaptured(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusBadGateway,
		Body:       io.NopCloser(strings.NewReader("upstream blew up")),
	}
	got := ReadBoundedStatusError(resp)
	if got == nil {
		t.Fatal("ReadBoundedStatusError returned nil; want non-nil for non-2xx")
	}
	if got.StatusCode != http.StatusBadGateway {
		t.Errorf("StatusCode = %d, want %d", got.StatusCode, http.StatusBadGateway)
	}
	if got.Body != "upstream blew up" {
		t.Errorf("Body = %q, want %q", got.Body, "upstream blew up")
	}
}

func TestReadBoundedStatusError_OneMBCap(t *testing.T) {
	// Build a body just over the 1 MB cap so the limiter must trim and
	// the drain must consume the overflow. Use a single byte repeated so
	// the slice grows fast without burning the test on JSON parsing.
	const cap1MB = 1 << 20
	bodyBytes := strings.Repeat("x", cap1MB+512)
	resp := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Body:       io.NopCloser(strings.NewReader(bodyBytes)),
	}
	got := ReadBoundedStatusError(resp)
	if got == nil {
		t.Fatal("ReadBoundedStatusError returned nil; want non-nil for 5xx")
	}
	if got.StatusCode != http.StatusInternalServerError {
		t.Errorf("StatusCode = %d, want %d", got.StatusCode, http.StatusInternalServerError)
	}
	if len(got.Body) != cap1MB {
		t.Errorf("len(Body) = %d, want exactly 1 MB cap (%d) -- limiter not enforced", len(got.Body), cap1MB)
	}
	// Body must be drained so the underlying transport can reuse the
	// connection -- a subsequent ReadAll on the same body returns nothing.
	rest, _ := io.ReadAll(resp.Body)
	if len(rest) != 0 {
		t.Errorf("Body not drained: %d bytes remain after read", len(rest))
	}
}

func TestStatusError_ErrorAndIsAuth(t *testing.T) {
	tests := []struct {
		name    string
		err     *StatusError
		wantSub string
		isAuth  bool
	}{
		{"401", &StatusError{StatusCode: 401, Body: "denied"}, "unexpected status 401: denied", true},
		{"403", &StatusError{StatusCode: 403, Body: "forbidden"}, "unexpected status 403", true},
		{"500", &StatusError{StatusCode: 500, Body: "boom"}, "unexpected status 500", false},
		{"200", &StatusError{StatusCode: 200, Body: ""}, "unexpected status 200", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Error(); !strings.Contains(got, tc.wantSub) {
				t.Errorf("Error() = %q, want substring %q", got, tc.wantSub)
			}
			if got := tc.err.IsAuth(); got != tc.isAuth {
				t.Errorf("IsAuth() = %v, want %v", got, tc.isAuth)
			}
		})
	}
}
