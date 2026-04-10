package middleware

import (
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// scrubPatterns are substrings that indicate sensitive values in log output.
var scrubPatterns = []string{"apikey", "api_key", "password", "secret", "token", "authorization"}

// quietPrefixSuffixes are path suffixes checked with base-path prepended.
// This reduces static-asset log spam.
var quietPrefixSuffixes = []string{
	"/static/",
}

// quietExactSuffixes are path suffixes checked via exact match after
// base-path prepending. Exact match prevents accidentally suppressing
// unrelated paths (e.g. /api/v1/logs-archive).
var quietExactSuffixes = []string{
	"/api/v1/logs",
}

// Logging returns middleware that logs each HTTP request with structured fields.
// It scrubs sensitive query parameters and headers from log output.
// basePath is prepended to quiet path patterns so sub-path deployments are
// handled correctly. Successful requests on quiet paths are not logged;
// error responses (>= 400) are still logged.
func Logging(logger *slog.Logger, basePath string) func(http.Handler) http.Handler {
	// Build the resolved quiet-path lists once at init.
	quietPrefixes := make([]string, len(quietPrefixSuffixes))
	for i, s := range quietPrefixSuffixes {
		quietPrefixes[i] = basePath + s
	}
	quietExact := make([]string, len(quietExactSuffixes))
	for i, s := range quietExactSuffixes {
		quietExact[i] = basePath + s
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Check whether this path should be silently served.
			quiet := false
			for _, prefix := range quietPrefixes {
				if strings.HasPrefix(r.URL.Path, prefix) {
					quiet = true
					break
				}
			}
			if !quiet {
				for _, exact := range quietExact {
					if r.URL.Path == exact {
						quiet = true
						break
					}
				}
			}

			start := time.Now()
			sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}

			if quiet {
				next.ServeHTTP(sw, r)
				// Still log errors on quiet paths so failures are never invisible.
				if sw.status >= 400 {
					level := slog.LevelWarn
					if sw.status >= 500 {
						level = slog.LevelError
					}
					logger.LogAttrs(r.Context(), level, "http request",
						slog.String("method", r.Method),
						slog.String("path", r.URL.Path),
						slog.Int("status", sw.status),
						slog.Duration("duration", time.Since(start)),
					)
				}
				return
			}

			next.ServeHTTP(sw, r)

			attrs := []slog.Attr{
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.String("query", scrubQuery(r.URL.RawQuery)),
				slog.Int("status", sw.status),
				slog.Duration("duration", time.Since(start)),
				slog.String("remote", r.RemoteAddr),
			}

			level := slog.LevelDebug
			switch {
			case sw.status >= 500:
				level = slog.LevelError
			case sw.status >= 400:
				level = slog.LevelWarn
			}
			logger.LogAttrs(r.Context(), level, "http request", attrs...)
		})
	}
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Flush implements http.Flusher so that SSE and streaming responses work
// through this wrapper. Without this, w.(http.Flusher) in the SSE handler
// fails and the endpoint returns 500.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap returns the underlying ResponseWriter. This is required by
// http.NewResponseController so it can reach the concrete writer for
// operations like SetWriteDeadline, which the SSE handler uses to keep
// the connection open indefinitely.
func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// scrubQuery redacts sensitive query parameter values.
func scrubQuery(raw string) string {
	if raw == "" {
		return ""
	}

	parts := strings.Split(raw, "&")
	for i, part := range parts {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) == 2 {
			lower := strings.ToLower(kv[0])
			for _, pattern := range scrubPatterns {
				if strings.Contains(lower, pattern) {
					parts[i] = kv[0] + "=REDACTED"
					break
				}
			}
		}
	}
	return strings.Join(parts, "&")
}
