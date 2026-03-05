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

func TestNewBase_InvalidURL(t *testing.T) {
	bc := NewBase("not-a-url", "key", http.DefaultClient, testLogger(), "test")
	bc.AuthFunc = func(*http.Request) {}
	// BaseURL should be empty after failed validation
	if bc.BaseURL != "" {
		t.Errorf("BaseURL = %q, want empty for invalid URL", bc.BaseURL)
	}
}
