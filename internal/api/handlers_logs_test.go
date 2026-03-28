package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/logging"
)

// newTestRouterWithLogs creates a minimal Router with a log manager for testing.
func newTestRouterWithLogs(t *testing.T) (*Router, *logging.RingBuffer) {
	t.Helper()
	mgr, _ := logging.NewManager(logging.Config{Level: "debug", Format: "json"})
	t.Cleanup(func() { mgr.Close() }) //nolint:errcheck
	rb := mgr.RingBuffer()
	r := &Router{
		logManager: mgr,
		logger:     slog.Default(),
	}
	return r, rb
}

func TestHandleGetLogs_JSON(t *testing.T) {
	r, rb := newTestRouterWithLogs(t)

	now := time.Now()
	rb.Write(logging.LogEntry{Time: now, Level: "info", Message: "test message", Component: "api"})
	rb.Write(logging.LogEntry{Time: now.Add(time.Second), Level: "warn", Message: "warning here"})

	req := httptest.NewRequest("GET", "/api/v1/logs?limit=10", nil)
	rec := httptest.NewRecorder()
	r.handleGetLogs(rec, req)

	res := rec.Result()
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("expected JSON content type, got %q", ct)
	}

	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}

	var entries []logging.LogEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		t.Fatalf("unmarshaling: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
}

func TestHandleGetLogs_HTMX(t *testing.T) {
	r, rb := newTestRouterWithLogs(t)

	now := time.Now()
	rb.Write(logging.LogEntry{Time: now, Level: "info", Message: "hello world"})

	req := httptest.NewRequest("GET", "/api/v1/logs?limit=10", nil)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	r.handleGetLogs(rec, req)

	res := rec.Result()
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("expected HTML content type, got %q", ct)
	}

	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	html := string(body)
	if !strings.Contains(html, "hello world") {
		t.Error("expected HTML to contain 'hello world'")
	}
	if !strings.Contains(html, "INFO") {
		t.Error("expected HTML to contain level badge 'INFO'")
	}
}

func TestHandleGetLogs_LevelFilter(t *testing.T) {
	r, rb := newTestRouterWithLogs(t)

	now := time.Now()
	rb.Write(logging.LogEntry{Time: now, Level: "debug", Message: "debug msg"})
	rb.Write(logging.LogEntry{Time: now.Add(time.Second), Level: "info", Message: "info msg"})
	rb.Write(logging.LogEntry{Time: now.Add(2 * time.Second), Level: "error", Message: "error msg"})

	req := httptest.NewRequest("GET", "/api/v1/logs?level=error&limit=10", nil)
	rec := httptest.NewRecorder()
	r.handleGetLogs(rec, req)

	res := rec.Result()
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}

	var entries []logging.LogEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		t.Fatalf("unmarshaling: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 error entry, got %d", len(entries))
	}
	if entries[0].Level != "error" {
		t.Errorf("expected level 'error', got %q", entries[0].Level)
	}
}

func TestHandleGetLogs_SearchFilter(t *testing.T) {
	r, rb := newTestRouterWithLogs(t)

	now := time.Now()
	rb.Write(logging.LogEntry{Time: now, Level: "info", Message: "connecting to database"})
	rb.Write(logging.LogEntry{Time: now.Add(time.Second), Level: "info", Message: "starting server"})

	req := httptest.NewRequest("GET", "/api/v1/logs?search=database&limit=10", nil)
	rec := httptest.NewRecorder()
	r.handleGetLogs(rec, req)

	res := rec.Result()
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}

	var entries []logging.LogEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		t.Fatalf("unmarshaling: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry matching 'database', got %d", len(entries))
	}
}

func TestHandleGetLogs_ComponentFilter(t *testing.T) {
	r, rb := newTestRouterWithLogs(t)

	now := time.Now()
	rb.Write(logging.LogEntry{Time: now, Level: "info", Message: "scanning dirs", Component: "scanner"})
	rb.Write(logging.LogEntry{Time: now.Add(time.Second), Level: "info", Message: "fetching art", Component: "provider"})

	req := httptest.NewRequest("GET", "/api/v1/logs?component=scanner&limit=10", nil)
	rec := httptest.NewRecorder()
	r.handleGetLogs(rec, req)

	res := rec.Result()
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}

	var entries []logging.LogEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		t.Fatalf("unmarshaling: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 scanner entry, got %d", len(entries))
	}
	if entries[0].Component != "scanner" {
		t.Errorf("expected component 'scanner', got %q", entries[0].Component)
	}
	if entries[0].Message != "scanning dirs" {
		t.Errorf("expected message 'scanning dirs', got %q", entries[0].Message)
	}
}

func TestHandleGetLogs_Empty(t *testing.T) {
	r, _ := newTestRouterWithLogs(t)

	req := httptest.NewRequest("GET", "/api/v1/logs?limit=10", nil)
	rec := httptest.NewRecorder()
	r.handleGetLogs(rec, req)

	res := rec.Result()
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}

	var entries []logging.LogEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		t.Fatalf("unmarshaling: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected 0 entries, got %d", len(entries))
	}
}

func TestHandleGetLogs_EmptyHTMX(t *testing.T) {
	r, _ := newTestRouterWithLogs(t)

	req := httptest.NewRequest("GET", "/api/v1/logs?limit=10", nil)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	r.handleGetLogs(rec, req)

	res := rec.Result()
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	if !strings.Contains(string(body), "No log entries") {
		t.Error("expected empty state message")
	}
}

func TestHandleClearLogs_JSON(t *testing.T) {
	r, rb := newTestRouterWithLogs(t)

	rb.Write(logging.LogEntry{Time: time.Now(), Level: "info", Message: "test"})
	if rb.Len() == 0 {
		t.Fatal("expected entries before clear")
	}

	req := httptest.NewRequest("DELETE", "/api/v1/logs", nil)
	rec := httptest.NewRecorder()
	r.handleClearLogs(rec, req)

	res := rec.Result()
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", res.StatusCode)
	}
	if rb.Len() != 0 {
		t.Errorf("expected 0 entries after clear, got %d", rb.Len())
	}
}

func TestHandleClearLogs_HTMX(t *testing.T) {
	r, rb := newTestRouterWithLogs(t)

	rb.Write(logging.LogEntry{Time: time.Now(), Level: "info", Message: "test"})

	req := httptest.NewRequest("DELETE", "/api/v1/logs", nil)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	r.handleClearLogs(rec, req)

	res := rec.Result()
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", res.StatusCode)
	}

	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	if !strings.Contains(string(body), "Log buffer cleared") {
		t.Error("expected cleared message in HTML response")
	}
	if rb.Len() != 0 {
		t.Errorf("expected 0 entries after clear, got %d", rb.Len())
	}
}

func TestHandleGetLogs_NilManager(t *testing.T) {
	r := &Router{
		logManager: nil,
		logger:     slog.Default(),
	}

	req := httptest.NewRequest("GET", "/api/v1/logs", nil)
	rec := httptest.NewRecorder()
	r.handleGetLogs(rec, req)

	res := rec.Result()
	defer res.Body.Close()

	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", res.StatusCode)
	}
}

func TestHandleClearLogs_NilManager(t *testing.T) {
	r := &Router{
		logManager: nil,
		logger:     slog.Default(),
	}

	req := httptest.NewRequest("DELETE", "/api/v1/logs", nil)
	rec := httptest.NewRecorder()
	r.handleClearLogs(rec, req)

	res := rec.Result()
	defer res.Body.Close()

	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", res.StatusCode)
	}
}

func TestHandleGetLogs_AfterFilter(t *testing.T) {
	r, rb := newTestRouterWithLogs(t)

	base := time.Date(2025, 6, 15, 10, 0, 0, 0, time.UTC)
	rb.Write(logging.LogEntry{Time: base, Level: "info", Message: "old entry"})
	rb.Write(logging.LogEntry{Time: base.Add(5 * time.Minute), Level: "info", Message: "new entry"})

	afterStr := base.Add(time.Minute).Format(time.RFC3339Nano)
	req := httptest.NewRequest("GET", "/api/v1/logs?after="+afterStr+"&limit=10", nil)
	rec := httptest.NewRecorder()
	r.handleGetLogs(rec, req)

	res := rec.Result()
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}

	var entries []logging.LogEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		t.Fatalf("unmarshaling: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry after filter, got %d", len(entries))
	}
	if entries[0].Message != "new entry" {
		t.Errorf("expected 'new entry', got %q", entries[0].Message)
	}
}

func TestHandleGetLogs_InvalidLevel(t *testing.T) {
	r, _ := newTestRouterWithLogs(t)

	req := httptest.NewRequest("GET", "/api/v1/logs?level=trace", nil)
	rec := httptest.NewRecorder()
	r.handleGetLogs(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid level, got %d", rec.Code)
	}
}

func TestHandleGetLogs_InvalidAfter(t *testing.T) {
	r, _ := newTestRouterWithLogs(t)

	req := httptest.NewRequest("GET", "/api/v1/logs?after=not-a-date", nil)
	rec := httptest.NewRecorder()
	r.handleGetLogs(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid after, got %d", rec.Code)
	}
}

func TestHandleGetLogs_InvalidLimit(t *testing.T) {
	r, _ := newTestRouterWithLogs(t)

	req := httptest.NewRequest("GET", "/api/v1/logs?limit=abc", nil)
	rec := httptest.NewRecorder()
	r.handleGetLogs(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid limit, got %d", rec.Code)
	}
}

func TestHandleGetLogs_NegativeLimit(t *testing.T) {
	r, _ := newTestRouterWithLogs(t)

	req := httptest.NewRequest("GET", "/api/v1/logs?limit=-5", nil)
	rec := httptest.NewRecorder()
	r.handleGetLogs(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for negative limit, got %d", rec.Code)
	}
}

func TestLevelBadgeClass(t *testing.T) {
	tests := []struct {
		level string
		want  string
	}{
		{"debug", "bg-gray-700 text-gray-300"},
		{"info", "bg-blue-900/50 text-blue-300"},
		{"warn", "bg-amber-900/50 text-amber-300"},
		{"error", "bg-red-900/50 text-red-300"},
		{"unknown", "bg-gray-700 text-gray-300"},
	}
	for _, tt := range tests {
		got := levelBadgeClass(tt.level)
		if got != tt.want {
			t.Errorf("levelBadgeClass(%q) = %q, want %q", tt.level, got, tt.want)
		}
	}
}
