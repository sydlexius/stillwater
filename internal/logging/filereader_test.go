package logging

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseLogLine_JSON(t *testing.T) {
	ts := time.Date(2024, 3, 29, 14, 0, 0, 0, time.UTC)
	line := map[string]any{
		"time":  ts.Format(time.RFC3339Nano),
		"level": "INFO",
		"source": map[string]any{
			"function": "github.com/sydlexius/stillwater/internal/api.handler",
			"file":     "/app/internal/api/handler.go",
			"line":     42,
		},
		"msg":       "test message",
		"component": "scanner",
		"key":       "value",
	}
	b, err := json.Marshal(line)
	if err != nil {
		t.Fatal(err)
	}

	entry := parseLogLine(string(b))

	if entry.Message != "test message" {
		t.Errorf("Message: got %q, want %q", entry.Message, "test message")
	}
	if entry.Level != "info" {
		t.Errorf("Level: got %q, want %q", entry.Level, "info")
	}
	if entry.Component != "scanner" {
		t.Errorf("Component: got %q, want %q", entry.Component, "scanner")
	}
	if entry.Source != "handler.go:42" {
		t.Errorf("Source: got %q, want %q", entry.Source, "handler.go:42")
	}
	if entry.Attrs["key"] != "value" {
		t.Errorf("Attrs[key]: got %v, want %q", entry.Attrs["key"], "value")
	}
	// component should not appear in Attrs
	if _, ok := entry.Attrs["component"]; ok {
		t.Error("component should not be in Attrs")
	}
}

func TestParseLogLine_InvalidJSON(t *testing.T) {
	entry := parseLogLine("not json at all")
	if entry.Level != "info" {
		t.Errorf("Level: got %q, want %q", entry.Level, "info")
	}
	if entry.Message != "not json at all" {
		t.Errorf("Message: got %q, want %q", entry.Message, "not json at all")
	}
	if !entry.Time.IsZero() {
		t.Error("Time should be zero for non-JSON lines")
	}
}

func TestReadLogFile_BasicFiltering(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")

	lines := []map[string]any{
		{"time": "2024-03-29T10:00:00Z", "level": "DEBUG", "msg": "debug entry", "source": nil, "component": "scanner"},
		{"time": "2024-03-29T10:00:01Z", "level": "INFO", "msg": "info entry", "source": nil},
		{"time": "2024-03-29T10:00:02Z", "level": "WARN", "msg": "warn entry", "source": nil, "component": "scanner"},
		{"time": "2024-03-29T10:00:03Z", "level": "ERROR", "msg": "error entry", "source": nil},
		{"time": "2024-03-29T10:00:04Z", "level": "INFO", "msg": "another info", "source": nil},
	}

	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	enc := json.NewEncoder(f)
	for _, l := range lines {
		if err := enc.Encode(l); err != nil {
			t.Fatal(err)
		}
	}
	f.Close()

	t.Run("no filter returns all newest-first", func(t *testing.T) {
		entries, err := ReadLogFile(path, LogFilter{Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 5 {
			t.Fatalf("got %d entries, want 5", len(entries))
		}
		// Newest first: "another info" should be first
		if entries[0].Message != "another info" {
			t.Errorf("first entry: got %q, want %q", entries[0].Message, "another info")
		}
	})

	t.Run("level filter", func(t *testing.T) {
		entries, err := ReadLogFile(path, LogFilter{Level: "warn", Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 2 {
			t.Fatalf("got %d entries, want 2 (warn+error)", len(entries))
		}
		// Newest first: error before warn.
		if entries[0].Level != "error" || entries[1].Level != "warn" {
			t.Fatalf("levels: got %q, %q; want error, warn", entries[0].Level, entries[1].Level)
		}
	})

	t.Run("search filter", func(t *testing.T) {
		entries, err := ReadLogFile(path, LogFilter{Search: "info", Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 2 {
			t.Fatalf("got %d entries, want 2", len(entries))
		}
		// Newest first: "another info" before "info entry".
		if entries[0].Message != "another info" || entries[1].Message != "info entry" {
			t.Fatalf("messages: got %q, %q; want 'another info', 'info entry'", entries[0].Message, entries[1].Message)
		}
	})

	t.Run("limit", func(t *testing.T) {
		entries, err := ReadLogFile(path, LogFilter{Limit: 2})
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 2 {
			t.Fatalf("got %d entries, want 2", len(entries))
		}
	})

	t.Run("component filter", func(t *testing.T) {
		entries, err := ReadLogFile(path, LogFilter{Component: "scanner", Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 2 {
			t.Fatalf("got %d entries, want 2 (scanner entries)", len(entries))
		}
		for _, e := range entries {
			if e.Component != "scanner" {
				t.Errorf("expected component=scanner, got %q", e.Component)
			}
		}
	})
}

func TestListLogFiles_Empty(t *testing.T) {
	files, err := ListLogFiles("")
	if err != nil {
		t.Fatal(err)
	}
	if files != nil {
		t.Errorf("expected nil for empty path, got %v", files)
	}
}

func TestListLogFiles_CurrentOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stillwater.log")
	if err := os.WriteFile(path, []byte("test\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	files, err := ListLogFiles(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("got %d files, want 1", len(files))
	}
	if !files[0].IsCurrent {
		t.Error("expected IsCurrent=true for the configured file")
	}
	if files[0].Name != "stillwater.log" {
		t.Errorf("Name: got %q, want %q", files[0].Name, "stillwater.log")
	}
}

func TestParseLogLine_TraceLevel(t *testing.T) {
	// slog serializes LevelTrace as "DEBUG-4". The parser should normalize this to "trace".
	line := `{"time":"2024-03-29T10:00:00Z","level":"DEBUG-4","msg":"trace entry"}`
	entry := parseLogLine(line)
	if entry.Level != "trace" {
		t.Errorf("Level: got %q, want %q", entry.Level, "trace")
	}
}

func TestManagerReadLogFile_PathTraversal(t *testing.T) {
	cfg := Config{Level: "info", Format: "json", FilePath: "/tmp/test.log"}
	mgr, _ := NewManager(cfg)
	defer mgr.Close() //nolint:errcheck

	tests := []struct {
		name     string
		filename string
		wantErr  bool
	}{
		{"valid filename", "test.log", true},        // file doesn't exist, but passes validation
		{"path traversal", "../etc/passwd", true},   // rejected by validation
		{"slash in name", "sub/test.log", true},     // rejected by validation
		{"backslash in name", `sub\test.log`, true}, // rejected by validation
		{"empty config", "", true},                  // tested separately below
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := mgr.ReadLogFile(tt.filename, LogFilter{})
			if err == nil && tt.wantErr {
				t.Error("expected error, got nil")
			}
		})
	}

	// Empty FilePath config should return "file logging not configured".
	emptyCfg := Config{Level: "info", Format: "json"}
	emptyMgr, _ := NewManager(emptyCfg)
	defer emptyMgr.Close() //nolint:errcheck
	_, err := emptyMgr.ReadLogFile("test.log", LogFilter{})
	if err == nil || err.Error() != "file logging not configured" {
		t.Errorf("expected 'file logging not configured' error, got %v", err)
	}
}

func TestReadLogFile_PathTraversal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")
	if err := os.WriteFile(path, []byte("{\"time\":\"2024-03-29T10:00:00Z\",\"level\":\"INFO\",\"msg\":\"ok\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A relative path containing ".." should be rejected by ReadLogFile's own
	// guard, independent of the Manager-level validation. filepath.Clean
	// preserves leading ".." in relative paths.
	traversalPath := "../" + filepath.Base(dir) + "/test.log"
	_, err := ReadLogFile(traversalPath, LogFilter{Limit: 1})
	if err == nil || !strings.Contains(err.Error(), "invalid log file path") {
		t.Fatalf("got err %v, want 'invalid log file path'", err)
	}

	// A clean path should succeed.
	entries, err := ReadLogFile(path, LogFilter{Limit: 1})
	if err != nil {
		t.Fatalf("clean path failed: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
}

func TestListLogFiles_WithRotated(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "stillwater.log")
	rotated := filepath.Join(dir, "stillwater-2024-03-28T10-00-00.000.log")

	for _, p := range []string{current, rotated} {
		if err := os.WriteFile(p, []byte("test\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	files, err := ListLogFiles(current)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("got %d files, want 2", len(files))
	}
	// Current file must be first.
	if !files[0].IsCurrent {
		t.Error("first file should be current")
	}
	// Rotated file must not be marked current and must have the correct name.
	if files[1].IsCurrent {
		t.Error("rotated file should have IsCurrent=false")
	}
	if files[1].Name != "stillwater-2024-03-28T10-00-00.000.log" {
		t.Errorf("rotated file name: got %q, want %q", files[1].Name, "stillwater-2024-03-28T10-00-00.000.log")
	}
}
