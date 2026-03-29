package middleware

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
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

func TestLogging_QuietPaths(t *testing.T) {
	// Requests to quiet paths (/api/v1/logs, /static/) should not produce
	// log output. This prevents self-referential noise from the log viewer
	// polling endpoint and reduces static-asset log spam.
	tests := []struct {
		path  string
		quiet bool
	}{
		{"/api/v1/logs", true},
		{"/api/v1/logs?limit=200&level=info", true},
		{"/static/css/styles.css", true},
		{"/static/js/htmx.min.js", true},
		{"/api/v1/artists", false},
		{"/api/v1/settings", false},
	}

	for _, tt := range tests {
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
		handler := Logging(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))

		req := httptest.NewRequest("GET", tt.path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		logged := buf.Len() > 0
		if tt.quiet && logged {
			t.Errorf("path %q should be quiet but produced log output: %s", tt.path, buf.String())
		}
		if !tt.quiet && !logged {
			t.Errorf("path %q should produce log output but was quiet", tt.path)
		}
	}
}
