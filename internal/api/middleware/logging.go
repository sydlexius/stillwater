package middleware

import (
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// scrubPatterns are substrings that indicate sensitive values in log output.
var scrubPatterns = []string{"apikey", "api_key", "password", "secret", "token", "authorization"}

// Logging returns middleware that logs each HTTP request with structured fields.
// It scrubs sensitive query parameters and headers from log output.
func Logging(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}

			next.ServeHTTP(sw, r)

			logger.Info("http request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.String("query", scrubQuery(r.URL.RawQuery)),
				slog.Int("status", sw.status),
				slog.Duration("duration", time.Since(start)),
				slog.String("remote", r.RemoteAddr),
			)
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
