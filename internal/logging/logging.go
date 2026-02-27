package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"

	"gopkg.in/natefinch/lumberjack.v2"
)

// Config describes the desired logging configuration.
type Config struct {
	Level          string `json:"level"`
	Format         string `json:"format"`
	FilePath       string `json:"file_path,omitempty"`
	FileMaxSizeMB  int    `json:"file_max_size_mb,omitempty"`
	FileMaxFiles   int    `json:"file_max_files,omitempty"`
	FileMaxAgeDays int    `json:"file_max_age_days,omitempty"`
}

// SwappableHandler is a thread-safe slog.Handler that delegates to an inner
// handler which can be atomically swapped at runtime.
type SwappableHandler struct {
	inner atomic.Pointer[slog.Handler]
}

// NewSwappableHandler creates a SwappableHandler wrapping h.
func NewSwappableHandler(h slog.Handler) *SwappableHandler {
	s := &SwappableHandler{}
	s.inner.Store(&h)
	return s
}

// Swap replaces the inner handler.
func (s *SwappableHandler) Swap(h slog.Handler) {
	s.inner.Store(&h)
}

// Enabled delegates to the inner handler.
func (s *SwappableHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return (*s.inner.Load()).Enabled(ctx, level)
}

// Handle delegates to the inner handler.
func (s *SwappableHandler) Handle(ctx context.Context, r slog.Record) error {
	return (*s.inner.Load()).Handle(ctx, r)
}

// WithAttrs returns a new SwappableHandler whose inner handler has the attrs.
func (s *SwappableHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	inner := (*s.inner.Load()).WithAttrs(attrs)
	return NewSwappableHandler(inner)
}

// WithGroup returns a new SwappableHandler whose inner handler has the group.
func (s *SwappableHandler) WithGroup(name string) slog.Handler {
	inner := (*s.inner.Load()).WithGroup(name)
	return NewSwappableHandler(inner)
}

// Manager owns the logger lifecycle and supports runtime reconfiguration.
type Manager struct {
	levelVar *slog.LevelVar
	handler  *SwappableHandler
	config   Config
	mu       sync.Mutex
	closer   io.Closer // lumberjack writer, if any
}

// NewManager creates a Manager and returns it along with a ready-to-use logger.
func NewManager(cfg Config) (*Manager, *slog.Logger) {
	lvl := &slog.LevelVar{}
	lvl.Set(parseLevel(cfg.Level))

	writer, closer := buildWriter(cfg)
	inner := buildHandler(writer, lvl, cfg.Format)
	handler := NewSwappableHandler(inner)

	m := &Manager{
		levelVar: lvl,
		handler:  handler,
		config:   cfg,
		closer:   closer,
	}

	logger := slog.New(handler)
	return m, logger
}

// Reconfigure applies a new configuration at runtime. Level-only changes
// are instant via LevelVar; format or output changes rebuild the handler.
func (m *Manager) Reconfigure(cfg Config) {
	m.mu.Lock()
	defer m.mu.Unlock()

	newLevel := parseLevel(cfg.Level)
	m.levelVar.Set(newLevel)

	needSwap := cfg.Format != m.config.Format ||
		cfg.FilePath != m.config.FilePath ||
		cfg.FileMaxSizeMB != m.config.FileMaxSizeMB ||
		cfg.FileMaxFiles != m.config.FileMaxFiles ||
		cfg.FileMaxAgeDays != m.config.FileMaxAgeDays

	if needSwap {
		// Close old file writer if any
		if m.closer != nil {
			m.closer.Close() //nolint:errcheck
			m.closer = nil
		}

		writer, closer := buildWriter(cfg)
		inner := buildHandler(writer, m.levelVar, cfg.Format)
		m.handler.Swap(inner)
		m.closer = closer
	}

	m.config = cfg
}

// Config returns the current configuration snapshot.
func (m *Manager) Config() Config {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.config
}

// Close releases resources (e.g. the log file writer).
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closer != nil {
		err := m.closer.Close()
		m.closer = nil
		return err
	}
	return nil
}

// parseLevel converts a string to slog.Level, defaulting to Info.
func parseLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// FormatLevel converts a slog.Level to its string name.
func FormatLevel(l slog.Level) string {
	switch l {
	case slog.LevelDebug:
		return "debug"
	case slog.LevelWarn:
		return "warn"
	case slog.LevelError:
		return "error"
	default:
		return "info"
	}
}

// buildWriter creates the io.Writer for log output. If a file path is
// configured, it returns a MultiWriter (stdout + lumberjack) and the
// lumberjack logger as the closer.
func buildWriter(cfg Config) (io.Writer, io.Closer) {
	if cfg.FilePath == "" {
		return os.Stdout, nil
	}

	maxSize := cfg.FileMaxSizeMB
	if maxSize <= 0 {
		maxSize = 100
	}
	maxFiles := cfg.FileMaxFiles
	if maxFiles <= 0 {
		maxFiles = 3
	}
	maxAge := cfg.FileMaxAgeDays
	if maxAge <= 0 {
		maxAge = 30
	}

	lj := &lumberjack.Logger{
		Filename:   cfg.FilePath,
		MaxSize:    maxSize,
		MaxBackups: maxFiles,
		MaxAge:     maxAge,
		Compress:   false,
	}

	return io.MultiWriter(os.Stdout, lj), lj
}

// buildHandler creates a slog.Handler with the given writer, leveler, and format.
func buildHandler(w io.Writer, leveler slog.Leveler, format string) slog.Handler {
	opts := &slog.HandlerOptions{Level: leveler}
	if format == "text" {
		return slog.NewTextHandler(w, opts)
	}
	return slog.NewJSONHandler(w, opts)
}

// ValidLevel returns true if s is a recognized log level.
func ValidLevel(s string) bool {
	switch s {
	case "debug", "info", "warn", "error":
		return true
	}
	return false
}

// ValidFormat returns true if s is a recognized log format.
func ValidFormat(s string) bool {
	switch s {
	case "text", "json":
		return true
	}
	return false
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() Config {
	return Config{
		Level:          "info",
		Format:         "json",
		FileMaxSizeMB:  100,
		FileMaxFiles:   3,
		FileMaxAgeDays: 30,
	}
}

// String returns a human-readable summary of the config.
func (c Config) String() string {
	s := fmt.Sprintf("level=%s format=%s", c.Level, c.Format)
	if c.FilePath != "" {
		s += fmt.Sprintf(" file=%s max_size=%dMB max_files=%d max_age=%dd",
			c.FilePath, c.FileMaxSizeMB, c.FileMaxFiles, c.FileMaxAgeDays)
	}
	return s
}
