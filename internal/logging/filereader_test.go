package logging

import (
	"encoding/json"
	"os"
	"path/filepath"
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
		{"time": "2024-03-29T10:00:00Z", "level": "DEBUG", "msg": "debug entry", "source": nil},
		{"time": "2024-03-29T10:00:01Z", "level": "INFO", "msg": "info entry", "source": nil},
		{"time": "2024-03-29T10:00:02Z", "level": "WARN", "msg": "warn entry", "source": nil},
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
	})

	t.Run("search filter", func(t *testing.T) {
		entries, err := ReadLogFile(path, LogFilter{Search: "info", Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 2 {
			t.Fatalf("got %d entries, want 2", len(entries))
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
}
