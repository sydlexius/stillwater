package middleware

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestScrubQuery_RedactsSensitiveParams(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"apikey=secret123&page=1", "apikey=REDACTED&page=1"},
		{"api_key=abc&name=test", "api_key=REDACTED&name=test"},
		{"password=hunter2&user=admin", "password=REDACTED&user=admin"},
		{"token=tok123&format=json", "token=REDACTED&format=json"},
		{"secret=s3cr3t", "secret=REDACTED"},
		{"authorization=bearer123", "authorization=REDACTED"},
	}

	for _, tt := range tests {
		got := scrubQuery(tt.input)
		if got != tt.want {
			t.Errorf("scrubQuery(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestScrubQuery_PreservesSafeParams(t *testing.T) {
	input := "page=1&page_size=50&sort=name"
	got := scrubQuery(input)
	if got != input {
		t.Errorf("scrubQuery(%q) = %q, want unchanged", input, got)
	}
}

func TestScrubQuery_Empty(t *testing.T) {
	got := scrubQuery("")
	if got != "" {
		t.Errorf("scrubQuery(\"\") = %q, want empty", got)
	}
}

func TestScrubQuery_BareKeyNoEquals(t *testing.T) {
	// A bare key with no = sign should pass through unchanged (len(kv) == 1).
	// This verifies the function does not panic or misredact.
	got := scrubQuery("apikey&page=1")
	want := "apikey&page=1"
	if got != want {
		t.Errorf("scrubQuery(bare key) = %q, want %q", got, want)
	}
}

func TestScrubQuery_CaseInsensitive(t *testing.T) {
	got := scrubQuery("API_KEY=secret&APIKEY=val")
	want := "API_KEY=REDACTED&APIKEY=REDACTED"
	if got != want {
		t.Errorf("scrubQuery(uppercase) = %q, want %q", got, want)
	}
}

func TestLogging_LogLevels(t *testing.T) {
	// Successful requests must be logged at DEBUG, 4xx at WARN, 5xx at ERROR.
	tests := []struct {
		status    int
		wantLevel slog.Level
	}{
		{http.StatusOK, slog.LevelDebug},
		{http.StatusCreated, slog.LevelDebug},
		{http.StatusNoContent, slog.LevelDebug},
		{http.StatusBadRequest, slog.LevelWarn},
		{http.StatusUnauthorized, slog.LevelWarn},
		{http.StatusNotFound, slog.LevelWarn},
		{http.StatusInternalServerError, slog.LevelError},
		{http.StatusBadGateway, slog.LevelError},
	}

	for _, tt := range tests {
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
		handler := Logging(logger, "")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tt.status)
		}))

		req := httptest.NewRequest("GET", "/api/v1/artists", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		output := buf.String()
		if output == "" {
			t.Errorf("status %d: expected log output but got none", tt.status)
			continue
		}
		wantKey := "level=" + tt.wantLevel.String()
		if !strings.Contains(output, wantKey) {
			t.Errorf("status %d: expected log level %s, got output: %s", tt.status, tt.wantLevel, output)
		}
	}
}

func TestLogging_QuietPaths(t *testing.T) {
	// Requests to quiet paths (/api/v1/logs, /static/) should not produce
	// log output. This prevents self-referential noise from the log viewer
	// polling endpoint and reduces static-asset log spam.
	tests := []struct {
		basePath string
		path     string
		status   int
		quiet    bool
	}{
		// No base path.
		{"", "/api/v1/logs", http.StatusOK, true},
		{"", "/api/v1/logs?limit=200&level=info", http.StatusOK, true},
		{"", "/static/css/styles.css", http.StatusOK, true},
		{"", "/static/js/htmx.min.js", http.StatusOK, true},
		{"", "/api/v1/artists", http.StatusOK, false},
		{"", "/api/v1/settings", http.StatusOK, false},
		{"", "/api/v1/logs-archive", http.StatusOK, false},
		// Error responses on quiet paths must still be logged.
		{"", "/api/v1/logs", http.StatusInternalServerError, false},
		{"", "/static/css/missing.css", http.StatusNotFound, false},
		// With base path (sub-path deployment).
		{"/stillwater", "/stillwater/api/v1/logs", http.StatusOK, true},
		{"/stillwater", "/stillwater/static/css/styles.css", http.StatusOK, true},
		{"/stillwater", "/stillwater/api/v1/artists", http.StatusOK, false},
		// Base path: root paths should NOT be quiet (wrong prefix).
		{"/stillwater", "/api/v1/logs", http.StatusOK, false},
		{"/stillwater", "/static/css/styles.css", http.StatusOK, false},
	}

	for _, tt := range tests {
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
		handler := Logging(logger, tt.basePath)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tt.status)
		}))

		req := httptest.NewRequest("GET", tt.path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		logged := buf.Len() > 0
		if tt.quiet && logged {
			t.Errorf("basePath=%q path=%q (status %d) should be quiet but produced log output", tt.basePath, tt.path, tt.status)
		}
		if !tt.quiet && !logged {
			t.Errorf("basePath=%q path=%q (status %d) should produce log output but was quiet", tt.basePath, tt.path, tt.status)
		}
	}
}
