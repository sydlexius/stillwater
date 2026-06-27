package api

import (
	"bufio"
	"bytes"
	"context"
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
	t.Cleanup(func() { mgr.Close() })
	rb := mgr.RingBuffer()
	r := &Router{
		logManager: mgr,
		logger:     slog.Default(),
	}
	return r, rb
}

func TestHandleLogsComponents(t *testing.T) {
	t.Parallel()
	r, rb := newTestRouterWithLogs(t)

	now := time.Now()
	rb.Write(logging.LogEntry{Time: now, Level: "info", Message: "a", Component: "scanner"})
	rb.Write(logging.LogEntry{Time: now.Add(time.Second), Level: "warn", Message: "b", Component: "api"})
	rb.Write(logging.LogEntry{Time: now.Add(2 * time.Second), Level: "info", Message: "c", Component: "scanner"})
	rb.Write(logging.LogEntry{Time: now.Add(3 * time.Second), Level: "info", Message: "d"}) // no component

	req := httptest.NewRequest(http.MethodGet, "/api/v1/logs/components", nil)
	rec := httptest.NewRecorder()
	r.handleLogsComponents(rec, req)

	res := rec.Result()
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("expected JSON content type, got %q", ct)
	}

	var payload struct {
		Components []string `json:"components"`
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshaling: %v", err)
	}
	// Distinct, sorted, empty component omitted.
	want := []string{"api", "scanner"}
	if len(payload.Components) != len(want) {
		t.Fatalf("components = %v, want %v", payload.Components, want)
	}
	for i := range want {
		if payload.Components[i] != want[i] {
			t.Errorf("components[%d] = %q, want %q (full: %v)", i, payload.Components[i], want[i], payload.Components)
		}
	}
}

func TestHandleLogsComponents_EmptyBufferReturnsArray(t *testing.T) {
	t.Parallel()
	r, _ := newTestRouterWithLogs(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/logs/components", nil)
	rec := httptest.NewRecorder()
	r.handleLogsComponents(rec, req)

	res := rec.Result()
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", res.StatusCode)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	// Must serialize as an empty array, never null, so clients can iterate.
	if !strings.Contains(string(body), `"components":[]`) {
		t.Errorf("empty buffer should yield an empty array, got %s", string(body))
	}
}

func TestHandleLogsComponents_NoManager(t *testing.T) {
	t.Parallel()
	r := &Router{logManager: nil, logger: slog.Default()}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/logs/components", nil)
	rec := httptest.NewRecorder()
	r.handleLogsComponents(rec, req)

	if rec.Result().StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("nil logManager should yield 503, got %d", rec.Result().StatusCode)
	}
}

func TestHandleGetLogs_JSON(t *testing.T) {
	t.Parallel()
	r, rb := newTestRouterWithLogs(t)

	now := time.Now()
	rb.Write(logging.LogEntry{Time: now, Level: "info", Message: "test message", Component: "api"})
	rb.Write(logging.LogEntry{Time: now.Add(time.Second), Level: "warn", Message: "warning here"})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/logs?limit=10", nil)
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
	t.Parallel()
	r, rb := newTestRouterWithLogs(t)

	now := time.Now()
	rb.Write(logging.LogEntry{Time: now, Level: "info", Message: "hello world"})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/logs?limit=10", nil)
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

func TestHandleGetLogs_HTMX_AttrsAndSource(t *testing.T) {
	t.Parallel()
	r, rb := newTestRouterWithLogs(t)

	now := time.Now()
	rb.Write(logging.LogEntry{
		Time:    now,
		Level:   "info",
		Message: "http request",
		Source:  "router.go:42",
		Attrs: map[string]any{
			"method":   "GET",
			"path":     "/api/v1/artists",
			"status":   200,
			"duration": "12ms",
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/logs?limit=10", nil)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	r.handleGetLogs(rec, req)

	res := rec.Result()
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	out := string(body)

	// Bug 3: timestamp should include date portion.
	dateStr := now.Format(time.DateOnly)
	if !strings.Contains(out, dateStr) {
		t.Errorf("expected HTML to contain date %q", dateStr)
	}

	// Bug 4: source field should appear instead of [-].
	if !strings.Contains(out, "router.go:42") {
		t.Error("expected HTML to contain source 'router.go:42'")
	}

	// Bug 5/7: attrs should be rendered inline.
	if !strings.Contains(out, "method=GET") {
		t.Error("expected HTML to contain 'method=GET'")
	}
	if !strings.Contains(out, "status=200") {
		t.Error("expected HTML to contain 'status=200'")
	}
}

func TestHandleGetLogs_LevelFilter(t *testing.T) {
	t.Parallel()
	r, rb := newTestRouterWithLogs(t)

	now := time.Now()
	rb.Write(logging.LogEntry{Time: now, Level: "debug", Message: "debug msg"})
	rb.Write(logging.LogEntry{Time: now.Add(time.Second), Level: "info", Message: "info msg"})
	rb.Write(logging.LogEntry{Time: now.Add(2 * time.Second), Level: "error", Message: "error msg"})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/logs?level=error&limit=10", nil)
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
	t.Parallel()
	r, rb := newTestRouterWithLogs(t)

	now := time.Now()
	rb.Write(logging.LogEntry{Time: now, Level: "info", Message: "connecting to database"})
	rb.Write(logging.LogEntry{Time: now.Add(time.Second), Level: "info", Message: "starting server"})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/logs?search=database&limit=10", nil)
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
	t.Parallel()
	r, rb := newTestRouterWithLogs(t)

	now := time.Now()
	rb.Write(logging.LogEntry{Time: now, Level: "info", Message: "scanning dirs", Component: "scanner"})
	rb.Write(logging.LogEntry{Time: now.Add(time.Second), Level: "info", Message: "fetching art", Component: "provider"})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/logs?component=scanner&limit=10", nil)
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
	t.Parallel()
	r, _ := newTestRouterWithLogs(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/logs?limit=10", nil)
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
	t.Parallel()
	r, _ := newTestRouterWithLogs(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/logs?limit=10", nil)
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
	t.Parallel()
	r, rb := newTestRouterWithLogs(t)

	rb.Write(logging.LogEntry{Time: time.Now(), Level: "info", Message: "test"})
	if rb.Len() == 0 {
		t.Fatal("expected entries before clear")
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/logs", nil)
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
	t.Parallel()
	r, rb := newTestRouterWithLogs(t)

	rb.Write(logging.LogEntry{Time: time.Now(), Level: "info", Message: "test"})

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/logs", nil)
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
	t.Parallel()
	r := &Router{
		logManager: nil,
		logger:     slog.Default(),
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/logs", nil)
	rec := httptest.NewRecorder()
	r.handleGetLogs(rec, req)

	res := rec.Result()
	defer res.Body.Close()

	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", res.StatusCode)
	}
}

func TestHandleClearLogs_NilManager(t *testing.T) {
	t.Parallel()
	r := &Router{
		logManager: nil,
		logger:     slog.Default(),
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/logs", nil)
	rec := httptest.NewRecorder()
	r.handleClearLogs(rec, req)

	res := rec.Result()
	defer res.Body.Close()

	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", res.StatusCode)
	}
}

func TestHandleGetLogs_AfterFilter(t *testing.T) {
	t.Parallel()
	r, rb := newTestRouterWithLogs(t)

	base := time.Date(2025, 6, 15, 10, 0, 0, 0, time.UTC)
	rb.Write(logging.LogEntry{Time: base, Level: "info", Message: "old entry"})
	rb.Write(logging.LogEntry{Time: base.Add(5 * time.Minute), Level: "info", Message: "new entry"})

	afterStr := base.Add(time.Minute).Format(time.RFC3339Nano)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/logs?after="+afterStr+"&limit=10", nil)
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
	t.Parallel()
	r, _ := newTestRouterWithLogs(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/logs?level=verbose", nil)
	rec := httptest.NewRecorder()
	r.handleGetLogs(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid level, got %d", rec.Code)
	}
}

func TestHandleGetLogs_InvalidAfter(t *testing.T) {
	t.Parallel()
	r, _ := newTestRouterWithLogs(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/logs?after=not-a-date", nil)
	rec := httptest.NewRecorder()
	r.handleGetLogs(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid after, got %d", rec.Code)
	}
}

func TestHandleGetLogs_InvalidLimit(t *testing.T) {
	t.Parallel()
	r, _ := newTestRouterWithLogs(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/logs?limit=abc", nil)
	rec := httptest.NewRecorder()
	r.handleGetLogs(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid limit, got %d", rec.Code)
	}
}

func TestHandleGetLogs_NegativeLimit(t *testing.T) {
	t.Parallel()
	r, _ := newTestRouterWithLogs(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/logs?limit=-5", nil)
	rec := httptest.NewRecorder()
	r.handleGetLogs(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for negative limit, got %d", rec.Code)
	}
}

func TestHandleLogsStream_NilManager(t *testing.T) {
	t.Parallel()
	r := &Router{
		logManager: nil,
		logger:     slog.Default(),
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/logs/stream", nil)
	r.handleLogsStream(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
}

func TestHandleLogsStream_InvalidLevel(t *testing.T) {
	t.Parallel()
	r, _ := newTestRouterWithLogs(t)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/logs/stream?level=verbose", nil)
	r.handleLogsStream(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestLevelBadgeClass(t *testing.T) {
	t.Parallel()
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

// --- SSE stream handler tests ---

// newTestRouterWithLogsAndLogger extends newTestRouterWithLogs to also return
// the slog.Logger wired to the broadcaster, so tests can publish live records.
func newTestRouterWithLogsAndLogger(t *testing.T) (*Router, *logging.RingBuffer, *slog.Logger) {
	t.Helper()
	mgr, logger := logging.NewManager(logging.Config{Level: "debug", Format: "json"})
	t.Cleanup(func() { mgr.Close() })
	rb := mgr.RingBuffer()
	r := &Router{logManager: mgr, logger: slog.Default()}
	return r, rb, logger
}

// collectSSEFrames reads up to n blank-line-terminated SSE frames from scanner.
// Each returned string contains all non-blank lines of one frame joined by "\n".
// It stops after n frames or when the scanner closes (e.g., context canceled).
func collectSSEFrames(scanner *bufio.Scanner, n int) []string {
	var frames []string
	var cur []string
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if len(cur) > 0 {
				frames = append(frames, strings.Join(cur, "\n"))
				cur = nil
				if len(frames) >= n {
					break
				}
			}
		} else {
			cur = append(cur, line)
		}
	}
	if len(cur) > 0 {
		frames = append(frames, strings.Join(cur, "\n"))
	}
	return frames
}

// TestHandleLogsStream_ConnectedAndBackfill verifies the stream sends a
// "connected" event reporting the replayed count, followed by ring-buffer
// entries replayed oldest-first as logs.line events.
func TestHandleLogsStream_ConnectedAndBackfill(t *testing.T) {
	t.Parallel()
	r, rb, _ := newTestRouterWithLogsAndLogger(t)

	base := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	rb.Write(logging.LogEntry{Time: base, Level: "info", Message: "alpha"})
	rb.Write(logging.LogEntry{Time: base.Add(time.Second), Level: "warn", Message: "beta"})
	rb.Write(logging.LogEntry{Time: base.Add(2 * time.Second), Level: "error", Message: "gamma"})

	ts := httptest.NewServer(http.HandlerFunc(r.handleLogsStream))
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("want text/event-stream, got %q", ct)
	}

	// connected frame + 3 backfill frames
	scanner := bufio.NewScanner(resp.Body)
	frames := collectSSEFrames(scanner, 4)
	cancel()

	if len(frames) < 4 {
		t.Fatalf("want 4 SSE frames (connected + 3 backfill), got %d: %v", len(frames), frames)
	}

	if !strings.Contains(frames[0], "event: connected") {
		t.Errorf("frame[0] should be connected event, got: %q", frames[0])
	}
	if !strings.Contains(frames[0], `"replayed":3`) {
		t.Errorf("connected frame should report replayed:3, got: %q", frames[0])
	}

	// emitLogBackfill reverses entries so oldest lands first in the stream.
	for i, want := range []string{"alpha", "beta", "gamma"} {
		if !strings.Contains(frames[i+1], "event: logs.line") {
			t.Errorf("frame[%d] should be logs.line event, got: %q", i+1, frames[i+1])
		}
		if !strings.Contains(frames[i+1], want) {
			t.Errorf("frame[%d] should contain %q, got: %q", i+1, want, frames[i+1])
		}
	}

	// id lines should be present (RFC3339Nano timestamps).
	if !strings.Contains(frames[1], "id: ") {
		t.Errorf("backfill frame should have id line, got: %q", frames[1])
	}
}

// TestHandleLogsStream_LiveRecord verifies a record published to the log
// broadcaster after connect is delivered as a logs.line SSE event.
func TestHandleLogsStream_LiveRecord(t *testing.T) {
	t.Parallel()
	r, _, logger := newTestRouterWithLogsAndLogger(t)

	ts := httptest.NewServer(http.HandlerFunc(r.handleLogsStream))
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)

	// Read and discard the connected frame (replayed: 0 since ring is empty).
	connFrames := collectSSEFrames(scanner, 1)
	if len(connFrames) == 0 || !strings.Contains(connFrames[0], "event: connected") {
		t.Fatalf("expected connected frame first, got: %v", connFrames)
	}

	// Publish a live record through the broadcaster.
	logger.Info("live-record-marker", slog.String("src", "test"))

	// Read the live frame.
	liveFrames := collectSSEFrames(scanner, 1)
	cancel()

	if len(liveFrames) == 0 {
		t.Fatal("expected a live logs.line frame but got none")
	}
	if !strings.Contains(liveFrames[0], "event: logs.line") {
		t.Errorf("live frame should be logs.line, got: %q", liveFrames[0])
	}
	if !strings.Contains(liveFrames[0], "live-record-marker") {
		t.Errorf("live frame should contain the log message, got: %q", liveFrames[0])
	}
}

// TestHandleLogsStream_LastEventIDCursor verifies that a reconnect with a
// valid RFC3339Nano Last-Event-ID cursor replays only entries after the cursor.
func TestHandleLogsStream_LastEventIDCursor(t *testing.T) {
	t.Parallel()
	r, rb, _ := newTestRouterWithLogsAndLogger(t)

	base := time.Date(2025, 3, 1, 10, 0, 0, 0, time.UTC)
	rb.Write(logging.LogEntry{Time: base, Level: "info", Message: "old-entry"})
	rb.Write(logging.LogEntry{Time: base.Add(10 * time.Second), Level: "info", Message: "new-entry"})

	ts := httptest.NewServer(http.HandlerFunc(r.handleLogsStream))
	defer ts.Close()

	// Cursor is set to 5s after base, so only "new-entry" should be replayed.
	cursor := base.Add(5 * time.Second).Format(time.RFC3339Nano)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Last-Event-ID", cursor)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	// connected + 1 backfill (only "new-entry")
	frames := collectSSEFrames(scanner, 2)
	cancel()

	if len(frames) < 2 {
		t.Fatalf("want 2 frames (connected + 1 backfill), got %d: %v", len(frames), frames)
	}
	if !strings.Contains(frames[0], `"replayed":1`) {
		t.Errorf("connected frame should report replayed:1, got: %q", frames[0])
	}
	if strings.Contains(frames[1], "old-entry") {
		t.Errorf("frame[1] should not contain old-entry (before cursor), got: %q", frames[1])
	}
	if !strings.Contains(frames[1], "new-entry") {
		t.Errorf("frame[1] should contain new-entry (after cursor), got: %q", frames[1])
	}
}

// TestHandleLogsStream_MalformedCursor verifies that an unparsable
// Last-Event-ID causes the handler to log a warning and fall back to a full
// backfill rather than silently dropping entries.
func TestHandleLogsStream_MalformedCursor(t *testing.T) {
	t.Parallel()
	r, rb, _ := newTestRouterWithLogsAndLogger(t)
	var logBuf bytes.Buffer
	r.logger = slog.New(slog.NewTextHandler(&logBuf, nil))

	base := time.Date(2025, 3, 1, 10, 0, 0, 0, time.UTC)
	rb.Write(logging.LogEntry{Time: base, Level: "info", Message: "entry-one"})
	rb.Write(logging.LogEntry{Time: base.Add(time.Second), Level: "info", Message: "entry-two"})

	ts := httptest.NewServer(http.HandlerFunc(r.handleLogsStream))
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Last-Event-ID", "not-a-valid-timestamp")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	// Expect full backfill (both entries) despite malformed cursor.
	frames := collectSSEFrames(scanner, 3) // connected + 2 entries
	cancel()

	if len(frames) < 3 {
		t.Fatalf("want 3 frames (connected + 2 full backfill), got %d: %v", len(frames), frames)
	}
	if !strings.Contains(frames[0], `"replayed":2`) {
		t.Errorf("connected frame should report replayed:2 (full replay), got: %q", frames[0])
	}
	if got := logBuf.String(); !strings.Contains(got, "unparsable Last-Event-ID cursor") {
		t.Errorf("expected malformed cursor Warn to be logged, got: %q", got)
	}
}

// TestHandleLogsStream_LevelFilter verifies the level query param filters both
// backfill and live log entries so only entries at or above the requested level
// appear in the stream.
func TestHandleLogsStream_LevelFilter(t *testing.T) {
	t.Parallel()
	r, rb, _ := newTestRouterWithLogsAndLogger(t)

	base := time.Date(2025, 3, 1, 10, 0, 0, 0, time.UTC)
	rb.Write(logging.LogEntry{Time: base, Level: "debug", Message: "debug-msg"})
	rb.Write(logging.LogEntry{Time: base.Add(time.Second), Level: "info", Message: "info-msg"})
	rb.Write(logging.LogEntry{Time: base.Add(2 * time.Second), Level: "error", Message: "error-msg"})

	ts := httptest.NewServer(http.HandlerFunc(r.handleLogsStream))
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"?level=error", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	frames := collectSSEFrames(scanner, 2) // connected + 1 error entry
	cancel()

	if len(frames) < 2 {
		t.Fatalf("want 2 frames (connected + 1 error), got %d", len(frames))
	}
	if !strings.Contains(frames[0], `"replayed":1`) {
		t.Errorf("connected frame should report replayed:1, got: %q", frames[0])
	}
	if strings.Contains(frames[1], "debug-msg") || strings.Contains(frames[1], "info-msg") {
		t.Errorf("filtered frame should not contain debug or info messages, got: %q", frames[1])
	}
	if !strings.Contains(frames[1], "error-msg") {
		t.Errorf("filtered frame should contain error-msg, got: %q", frames[1])
	}
}

// TestHandleLogsStream_ScopeFilter verifies the scope query param filters
// backfill entries to only those with a matching component.
func TestHandleLogsStream_ScopeFilter(t *testing.T) {
	t.Parallel()
	r, rb, _ := newTestRouterWithLogsAndLogger(t)

	base := time.Date(2025, 3, 1, 10, 0, 0, 0, time.UTC)
	rb.Write(logging.LogEntry{Time: base, Level: "info", Message: "scanner-work", Component: "scanner"})
	rb.Write(logging.LogEntry{Time: base.Add(time.Second), Level: "info", Message: "backup-work", Component: "backup"})

	ts := httptest.NewServer(http.HandlerFunc(r.handleLogsStream))
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"?scope=scanner", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	frames := collectSSEFrames(scanner, 2) // connected + 1 scanner entry
	cancel()

	if len(frames) < 2 {
		t.Fatalf("want 2 frames (connected + 1 scope match), got %d", len(frames))
	}
	if !strings.Contains(frames[0], `"replayed":1`) {
		t.Errorf("connected frame should report replayed:1 (scope filtered), got: %q", frames[0])
	}
	if strings.Contains(frames[1], "backup-work") {
		t.Errorf("frame[1] should not contain backup-work, got: %q", frames[1])
	}
	if !strings.Contains(frames[1], "scanner-work") {
		t.Errorf("frame[1] should contain scanner-work, got: %q", frames[1])
	}
}

// TestHandleLogsStream_LiveFilter verifies that the level filter applies on the
// live-tail path (records arriving via the LogBroadcaster after connect), not
// only during backfill. The ring buffer starts empty so all frames come from live.
func TestHandleLogsStream_LiveFilter(t *testing.T) {
	t.Parallel()
	r, _, logger := newTestRouterWithLogsAndLogger(t)

	ts := httptest.NewServer(http.HandlerFunc(r.handleLogsStream))
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"?level=error", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)

	// Wait for "connected" frame -- confirms the subscription with level=error is active.
	connected := collectSSEFrames(scanner, 1)
	if len(connected) < 1 || !strings.Contains(connected[0], "event: connected") {
		t.Fatalf("expected connected frame, got: %v", connected)
	}

	// Publish one non-matching (info) and one matching (error) record.
	logger.Info("should-be-filtered")
	logger.Error("should-arrive")

	// Collect one logs.line frame; the filter must suppress the info record.
	frames := collectSSEFrames(scanner, 1)
	cancel()

	if len(frames) < 1 {
		t.Fatal("expected a logs.line frame but got none")
	}
	if strings.Contains(frames[0], "should-be-filtered") {
		t.Errorf("info record leaked through level=error filter: %q", frames[0])
	}
	if !strings.Contains(frames[0], "should-arrive") {
		t.Errorf("error record not delivered through live filter: %q", frames[0])
	}
}

// TestHandleLogsStream_Throttle verifies that when the subscriber buffer
// overflows, streamLogLines emits a logs.throttled SSE frame with a non-zero
// dropped count. Pre-overflows the channel so the throttle signal is present
// before streamLogLines starts; uses an io.Pipe so the scanner blocks
// deterministically instead of sleeping.
func TestHandleLogsStream_Throttle(t *testing.T) {
	t.Parallel()
	r, _, logger := newTestRouterWithLogsAndLogger(t)

	lb := r.logManager.LogBroadcaster()
	sub := lb.Subscribe(logging.LogFilter{})
	defer sub.Close()

	// Publish enough records to overflow the subscriber's default 256-entry
	// buffer before streamLogLines starts. The broadcaster fans out
	// non-blocking: once s.lines is full, dropped++ and the throttle signal
	// fires. Publishing 300 guarantees overflow.
	for i := 0; i < 300; i++ {
		logger.Info("flood", slog.Int("i", i))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pr, pw := io.Pipe()
	rw := &pipeResponseWriter{pw: pw, header: make(http.Header)}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/logs/stream", nil).WithContext(ctx)

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer pw.Close()
		r.streamLogLines(rw, rw, req, sub, time.Time{})
	}()

	// Scanner blocks on the pipe until streamLogLines writes a frame -- no
	// sleep needed. Scan until the throttled frame is found.
	scanner := bufio.NewScanner(pr)
	var foundThrottle string
	for foundThrottle == "" {
		frames := collectSSEFrames(scanner, 1)
		if len(frames) == 0 {
			break // pipe closed (context done)
		}
		if strings.Contains(frames[0], "event: logs.throttled") {
			foundThrottle = frames[0]
		}
	}
	cancel()
	io.Copy(io.Discard, pr) // drain so the goroutine can finish writing
	<-done

	if foundThrottle == "" {
		t.Error("expected logs.throttled event but none found within timeout")
	}
	if !strings.Contains(foundThrottle, `"dropped"`) {
		t.Errorf("throttled event should include dropped count, got: %q", foundThrottle)
	}
}

// pipeResponseWriter is a minimal http.ResponseWriter + http.Flusher backed by
// an io.PipeWriter. It lets tests scan SSE frames written by streamLogLines
// without the fixed sleeps that make httptest.NewRecorder-based tests racy.
type pipeResponseWriter struct {
	pw     *io.PipeWriter
	header http.Header
}

func (p *pipeResponseWriter) Header() http.Header         { return p.header }
func (p *pipeResponseWriter) Write(b []byte) (int, error) { return p.pw.Write(b) }
func (p *pipeResponseWriter) WriteHeader(int)             {}
func (p *pipeResponseWriter) Flush()                      {}

// TestHandleLogsStream_LivePath verifies that a record published via the
// broadcaster is forwarded as a logs.line SSE event. Uses httptest.Server +
// scanner so the connected frame provides a deterministic subscription barrier.
func TestHandleLogsStream_LivePath(t *testing.T) {
	t.Parallel()
	r, _, logger := newTestRouterWithLogsAndLogger(t)

	ts := httptest.NewServer(http.HandlerFunc(r.handleLogsStream))
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)

	// Wait for "connected" frame -- this is the deterministic signal that
	// handleLogsStream has subscribed and streamLogLines is in its select loop.
	connected := collectSSEFrames(scanner, 1)
	if len(connected) < 1 || !strings.Contains(connected[0], "event: connected") {
		t.Fatalf("expected connected frame, got: %v", connected)
	}

	// Publish after receiving the connected frame -- goroutine is definitely
	// in the select loop at this point (it already wrote and flushed a frame).
	logger.Info("stream-live-test-marker")

	// Scanner blocks until the logs.line frame arrives -- no sleep needed.
	frames := collectSSEFrames(scanner, 1)
	cancel()

	if len(frames) < 1 {
		t.Fatal("expected a logs.line frame but got none")
	}
	if !strings.Contains(frames[0], "event: logs.line") {
		t.Errorf("expected logs.line event, got: %q", frames[0])
	}
	if !strings.Contains(frames[0], "stream-live-test-marker") {
		t.Errorf("expected log message in SSE output, got: %q", frames[0])
	}
}

// TestEmitLogBackfill_EmptyRing verifies the handler sends a connected frame
// with replayed:0 when the ring buffer contains no matching entries.
func TestEmitLogBackfill_EmptyRing(t *testing.T) {
	t.Parallel()
	r, rb, _ := newTestRouterWithLogsAndLogger(t)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/logs/stream", nil)
	filter := logging.LogFilter{}

	ts, ok := r.emitLogBackfill(w, w, req, rb, filter)
	if !ok {
		t.Fatal("emitLogBackfill returned false (client disconnect) unexpectedly")
	}
	if !ts.IsZero() {
		t.Errorf("newestTS should be zero for empty ring, got %v", ts)
	}

	body := w.Body.String()
	if !strings.Contains(body, "event: connected") {
		t.Errorf("expected connected event, got: %q", body)
	}
	if !strings.Contains(body, `"replayed":0`) {
		t.Errorf("expected replayed:0, got: %q", body)
	}
}

// TestEmitLogBackfill_WithEntries verifies backfill replay and newestTS return.
func TestEmitLogBackfill_WithEntries(t *testing.T) {
	t.Parallel()
	r, rb, _ := newTestRouterWithLogsAndLogger(t)

	base := time.Date(2025, 5, 1, 0, 0, 0, 0, time.UTC)
	rb.Write(logging.LogEntry{Time: base, Level: "info", Message: "first"})
	rb.Write(logging.LogEntry{Time: base.Add(time.Second), Level: "info", Message: "second"})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/logs/stream", nil)
	filter := logging.LogFilter{}

	ts, ok := r.emitLogBackfill(w, w, req, rb, filter)
	if !ok {
		t.Fatal("emitLogBackfill returned false unexpectedly")
	}
	if ts.IsZero() {
		t.Error("newestTS should be non-zero after backfill")
	}
	// newestTS should be the second entry (later timestamp).
	wantTS := base.Add(time.Second)
	if !ts.Equal(wantTS) {
		t.Errorf("newestTS = %v, want %v", ts, wantTS)
	}

	body := w.Body.String()
	if !strings.Contains(body, `"replayed":2`) {
		t.Errorf("expected replayed:2, got: %q", body)
	}
	if !strings.Contains(body, "first") || !strings.Contains(body, "second") {
		t.Errorf("expected both entries in backfill output, got: %q", body)
	}
}

// TestWriteLogSSE_Basic verifies the SSE frame format: id line, event line,
// data line (valid JSON), and terminating blank line.
func TestWriteLogSSE_Basic(t *testing.T) {
	t.Parallel()
	w := httptest.NewRecorder()
	payload := map[string]any{"msg": "hello", "level": "info"}

	err := writeLogSSE(w, "2025-01-01T12:00:00Z", "logs.line", payload, slog.Default())
	if err != nil {
		t.Fatalf("writeLogSSE returned error: %v", err)
	}

	body := w.Body.String()
	if !strings.Contains(body, "id: 2025-01-01T12:00:00Z\n") {
		t.Errorf("missing id line, got: %q", body)
	}
	if !strings.Contains(body, "event: logs.line\n") {
		t.Errorf("missing event line, got: %q", body)
	}
	if !strings.Contains(body, "data: ") {
		t.Errorf("missing data line, got: %q", body)
	}
	if !strings.HasSuffix(body, "\n\n") {
		t.Errorf("SSE frame should end with double newline, got: %q", body)
	}

	// Extract and validate data JSON.
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "data: ") {
			raw := strings.TrimPrefix(line, "data: ")
			var got map[string]any
			if err := json.Unmarshal([]byte(raw), &got); err != nil {
				t.Errorf("data line is not valid JSON: %v", err)
			}
			if got["msg"] != "hello" {
				t.Errorf("data.msg = %v, want hello", got["msg"])
			}
		}
	}
}

// TestWriteLogSSE_NoID verifies that when id is empty the id line is omitted
// (so transport-only frames do not advance the client's Last-Event-ID).
func TestWriteLogSSE_NoID(t *testing.T) {
	t.Parallel()
	w := httptest.NewRecorder()

	err := writeLogSSE(w, "", "connected", map[string]any{"replayed": 0}, slog.Default())
	if err != nil {
		t.Fatalf("writeLogSSE returned error: %v", err)
	}

	body := w.Body.String()
	if strings.Contains(body, "id: ") {
		t.Errorf("frame with empty id should not contain id line, got: %q", body)
	}
	if !strings.Contains(body, "event: connected\n") {
		t.Errorf("expected event line, got: %q", body)
	}
}

// TestHandleLogsStream_RecorderFlusher verifies that the stream handler starts
// correctly when given an httptest.ResponseRecorder (which implements
// http.Flusher), setting SSE headers and emitting the connected frame.
func TestHandleLogsStream_RecorderFlusher(t *testing.T) {
	t.Parallel()
	r, _, _ := newTestRouterWithLogsAndLogger(t)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/logs/stream", nil).WithContext(ctx)

	done := make(chan struct{})
	go func() {
		defer close(done)
		r.handleLogsStream(w, req)
	}()
	<-done

	if w.Code != http.StatusOK {
		t.Errorf("want 200, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("want text/event-stream, got %q", ct)
	}
	if !strings.Contains(w.Body.String(), "event: connected") {
		t.Errorf("expected connected frame in body, got: %q", w.Body.String())
	}
}
