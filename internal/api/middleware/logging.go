package middleware

import (
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// scrubPatterns are substrings that indicate sensitive values in log output.
var scrubPatterns = []string{"apikey", "api_key", "password", "secret", "token", "authorization"}

// quietPaths are URL path prefixes that should not generate HTTP request log
// entries. This prevents self-referential noise (e.g. the log viewer polling
// endpoint logging its own requests) and reduces static-asset log spam.
var quietPaths = []string{
	"/api/v1/logs",
	"/static/",
}

// Logging returns middleware that logs each HTTP request with structured fields.
// It scrubs sensitive query parameters and headers from log output.
// Requests to paths in quietPaths are served but not logged.
func Logging(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Check whether this path should be silently served.
			quiet := false
			for _, prefix := range quietPaths {
				if strings.HasPrefix(r.URL.Path, prefix) {
					quiet = true
					break
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

			level := slog.LevelInfo
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
