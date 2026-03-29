package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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

// WithAttrs returns a DerivedHandler that delegates through this
// SwappableHandler, so derived loggers observe Reconfigure changes.
func (s *SwappableHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &DerivedHandler{parent: s, attrs: attrs}
}

// WithGroup returns a DerivedHandler that delegates through this
// SwappableHandler, so derived loggers observe Reconfigure changes.
func (s *SwappableHandler) WithGroup(name string) slog.Handler {
	return &DerivedHandler{parent: s, group: name}
}

// DerivedHandler delegates to a parent SwappableHandler, applying accumulated
// attributes and group prefix. When the parent's inner handler is swapped via
// Reconfigure, derived handlers automatically observe the change.
type DerivedHandler struct {
	parent *SwappableHandler
	attrs  []slog.Attr
	group  string
}

// Enabled delegates to the parent's current inner handler.
func (d *DerivedHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return (*d.parent.inner.Load()).Enabled(ctx, level)
}

// Handle loads the current inner handler from the parent, applies any
// accumulated group and attrs, then delegates.
func (d *DerivedHandler) Handle(ctx context.Context, r slog.Record) error {
	inner := *d.parent.inner.Load()
	if d.group != "" {
		inner = inner.WithGroup(d.group)
	}
	if len(d.attrs) > 0 {
		inner = inner.WithAttrs(d.attrs)
	}
	return inner.Handle(ctx, r)
}

// WithAttrs returns a new DerivedHandler with the additional attributes appended.
func (d *DerivedHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	newAttrs := make([]slog.Attr, len(d.attrs)+len(attrs))
	copy(newAttrs, d.attrs)
	copy(newAttrs[len(d.attrs):], attrs)
	return &DerivedHandler{parent: d.parent, attrs: newAttrs, group: d.group}
}

// WithGroup returns a new DerivedHandler with the group appended.
func (d *DerivedHandler) WithGroup(name string) slog.Handler {
	g := name
	if d.group != "" {
		g = d.group + "." + name
	}
	return &DerivedHandler{parent: d.parent, attrs: d.attrs, group: g}
}

// LevelTrace is a custom slog level below Debug, intended for very verbose
// output (per-request headers, SQL parameters, etc.).
const LevelTrace = slog.LevelDebug - 4 // -8

// DefaultRingBufferSize is the default number of log entries retained in memory
// for the log viewer.
const DefaultRingBufferSize = 2000

// Manager owns the logger lifecycle and supports runtime reconfiguration.
type Manager struct {
	levelVar   *slog.LevelVar
	handler    *SwappableHandler
	config     Config
	mu         sync.Mutex
	closer     io.Closer // lumberjack writer, if any
	ringBuffer *RingBuffer
}

// NewManager creates a Manager and returns it along with a ready-to-use logger.
// Log entries are captured in an in-memory ring buffer for the log viewer.
func NewManager(cfg Config) (*Manager, *slog.Logger) {
	lvl := &slog.LevelVar{}
	lvl.Set(parseLevel(cfg.Level))

	rb := NewRingBuffer(DefaultRingBufferSize)

	writer, closer := buildWriter(cfg)
	inner := buildMultiHandler(writer, lvl, cfg.Format, rb)
	handler := NewSwappableHandler(inner)

	m := &Manager{
		levelVar:   lvl,
		handler:    handler,
		config:     cfg,
		closer:     closer,
		ringBuffer: rb,
	}

	logger := slog.New(handler)
	return m, logger
}

// RingBuffer returns the in-memory ring buffer used by the log viewer.
func (m *Manager) RingBuffer() *RingBuffer {
	return m.ringBuffer
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
		writer, closer := buildWriter(cfg)
		inner := buildMultiHandler(writer, m.levelVar, cfg.Format, m.ringBuffer)
		m.handler.Swap(inner)

		// Close old file writer after swapping to eliminate the race
		// window where a goroutine could write to a closed writer.
		oldCloser := m.closer
		m.closer = closer
		if oldCloser != nil {
			oldCloser.Close() //nolint:errcheck
		}
	}

	m.config = cfg
}

// Config returns the current configuration snapshot.
func (m *Manager) Config() Config {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.config
}

// ListLogFiles returns available log files (current + rotated backups).
// Returns nil if file logging is not configured.
func (m *Manager) ListLogFiles() ([]LogFileInfo, error) {
	m.mu.Lock()
	fp := m.config.FilePath
	m.mu.Unlock()
	return ListLogFiles(fp)
}

// DeleteRotatedFiles deletes rotated log files (all files except the current
// one). Returns the count of files deleted and total bytes freed.
func (m *Manager) DeleteRotatedFiles() (int, int64, error) {
	m.mu.Lock()
	fp := m.config.FilePath
	m.mu.Unlock()
	if fp == "" {
		return 0, 0, nil
	}
	files, err := ListLogFiles(fp)
	if err != nil {
		return 0, 0, fmt.Errorf("listing log files: %w", err)
	}
	dir := filepath.Dir(fp)
	var count int
	var bytesFreed int64
	for _, f := range files {
		if f.IsCurrent {
			continue
		}
		if err := os.Remove(filepath.Join(dir, f.Name)); err != nil {
			continue // best-effort
		}
		count++
		bytesFreed += f.Size
	}
	return count, bytesFreed, nil
}

// ReadLogFile reads entries from a named log file in the configured log
// directory and returns them newest-first. The filename must be a plain name
// with no path separators. Returns an error if file logging is not configured.
func (m *Manager) ReadLogFile(filename string, filter LogFilter) ([]LogEntry, error) {
	if filename != filepath.Base(filename) || strings.ContainsAny(filename, "/\\") {
		return nil, fmt.Errorf("invalid log filename")
	}
	m.mu.Lock()
	fp := m.config.FilePath
	m.mu.Unlock()
	if fp == "" {
		return nil, fmt.Errorf("file logging not configured")
	}
	fullPath := filepath.Join(filepath.Dir(fp), filename)
	return ReadLogFile(fullPath, filter)
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
	case "trace":
		return LevelTrace
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
	case LevelTrace:
		return "trace"
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
		maxSize = 10
	}
	maxFiles := cfg.FileMaxFiles
	if maxFiles <= 0 {
		maxFiles = 5
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
// AddSource is enabled so that log entries include the source file and line number.
func buildHandler(w io.Writer, leveler slog.Leveler, format string) slog.Handler {
	opts := &slog.HandlerOptions{Level: leveler, AddSource: true}
	if format == "text" {
		return slog.NewTextHandler(w, opts)
	}
	return slog.NewJSONHandler(w, opts)
}

// buildMultiHandler creates a MultiHandler that fans out to the primary
// text/JSON handler and a RingHandler for in-memory log capture.
func buildMultiHandler(w io.Writer, leveler slog.Leveler, format string, rb *RingBuffer) slog.Handler {
	primary := buildHandler(w, leveler, format)
	// The ring handler captures at the same level as the primary handler so
	// that logger.Enabled() reflects the configured level accurately.
	// addSource=true captures caller file:line for the log viewer.
	ring := NewRingHandler(rb, leveler, true)
	return NewMultiHandler(primary, ring)
}

// ValidLevel returns true if s is a recognized log level.
func ValidLevel(s string) bool {
	switch s {
	case "trace", "debug", "info", "warn", "error":
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
		FileMaxSizeMB:  10,
		FileMaxFiles:   5,
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
