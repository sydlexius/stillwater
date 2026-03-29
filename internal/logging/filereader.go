package logging

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// LogFileInfo describes a log file available for browsing in the log viewer.
type LogFileInfo struct {
	Name      string    `json:"name"` // plain filename, no directory components
	Size      int64     `json:"size"`
	ModTime   time.Time `json:"mod_time"`
	IsCurrent bool      `json:"is_current"`
}

// maxFileLines caps the number of lines read from a single log file to bound
// memory usage on large files.
const maxFileLines = 10000

// ListLogFiles returns the available log files for the configured path.
// If filePath is empty, it returns nil. The current file is listed first;
// rotated backups follow in newest-first order.
func ListLogFiles(filePath string) ([]LogFileInfo, error) {
	if filePath == "" {
		return nil, nil
	}

	dir := filepath.Dir(filePath)
	base := filepath.Base(filePath)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)

	var files []LogFileInfo

	// Current log file.
	if info, err := os.Stat(filePath); err == nil {
		files = append(files, LogFileInfo{
			Name:      base,
			Size:      info.Size(),
			ModTime:   info.ModTime(),
			IsCurrent: true,
		})
	}

	// Rotated backups: lumberjack names them "<stem>-<timestamp><ext>".
	pattern := filepath.Join(dir, stem+"-*"+ext)
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("listing log files: %w", err)
	}
	for _, m := range matches {
		info, err := os.Stat(m)
		if err != nil {
			continue
		}
		files = append(files, LogFileInfo{
			Name:    filepath.Base(m),
			Size:    info.Size(),
			ModTime: info.ModTime(),
		})
	}

	// Sort: current file first, then backups newest-first.
	sort.Slice(files, func(i, j int) bool {
		if files[i].IsCurrent != files[j].IsCurrent {
			return files[i].IsCurrent
		}
		return files[i].ModTime.After(files[j].ModTime)
	})

	return files, nil
}

// slogJSONLine is the shape of a single line written by slog's JSON handler.
type slogJSONLine struct {
	Time   time.Time       `json:"time"`
	Level  string          `json:"level"`
	Source *slogJSONSource `json:"source,omitempty"`
	Msg    string          `json:"msg"`
}

type slogJSONSource struct {
	File string `json:"file"`
	Line int    `json:"line"`
}

// ReadLogFile reads a log file and returns entries matching the filter, newest
// first. The After filter is ignored (it only applies to the live ring buffer).
// At most maxFileLines lines are read from the file to bound memory usage.
func ReadLogFile(path string, filter LogFilter) ([]LogEntry, error) {
	// Clean the path and reject traversal patterns so the sanitization is
	// visible to static analysis tools (CodeQL, gosec) at the call site.
	cleaned := filepath.Clean(path)
	if strings.Contains(cleaned, "..") {
		return nil, fmt.Errorf("invalid log file path")
	}
	f, err := os.Open(cleaned) //nolint:gosec // G304: path validated above and by Manager.ReadLogFile
	if err != nil {
		return nil, fmt.Errorf("opening log file: %w", err)
	}
	defer f.Close() //nolint:errcheck

	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	minSeverity := 0
	if filter.Level != "" {
		minSeverity = levelSeverity(filter.Level)
	}
	searchLower := strings.ToLower(filter.Search)

	// Maintain a fixed-size ring of the last maxFileLines lines so memory
	// stays bounded even for very large log files.
	scanner := bufio.NewScanner(f)
	const maxTokenSize = 512 * 1024 // 512 KB per line
	scanner.Buffer(make([]byte, maxTokenSize), maxTokenSize)

	rawLines := make([]string, 0, maxFileLines)
	ringStart := 0
	ringFull := false
	for scanner.Scan() {
		line := scanner.Text()
		if !ringFull {
			rawLines = append(rawLines, line)
			if len(rawLines) == maxFileLines {
				ringFull = true
			}
		} else {
			rawLines[ringStart] = line
			ringStart = (ringStart + 1) % maxFileLines
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading log file: %w", err)
	}
	// Reconstruct chronological order if the ring wrapped.
	if ringFull {
		ordered := make([]string, 0, maxFileLines)
		for i := 0; i < maxFileLines; i++ {
			ordered = append(ordered, rawLines[(ringStart+i)%maxFileLines])
		}
		rawLines = ordered
	}

	// Parse lines in reverse (newest first) and apply filters.
	const initialCap = 64
	result := make([]LogEntry, 0, initialCap)
	for i := len(rawLines) - 1; i >= 0 && len(result) < limit; i-- {
		line := strings.TrimSpace(rawLines[i])
		if line == "" {
			continue
		}
		entry := parseLogLine(line)

		if levelSeverity(entry.Level) < minSeverity {
			continue
		}
		if filter.Component != "" && entry.Component != filter.Component {
			continue
		}
		if searchLower != "" && !strings.Contains(strings.ToLower(entry.Message), searchLower) {
			continue
		}

		result = append(result, entry)
	}

	return result, nil
}

// parseLogLine parses a single log line into a LogEntry. Lines that cannot be
// parsed as slog JSON are returned as plain-text message entries.
func parseLogLine(line string) LogEntry {
	var known slogJSONLine
	if err := json.Unmarshal([]byte(line), &known); err != nil {
		return LogEntry{
			Level:   "info",
			Message: line,
		}
	}

	// Second unmarshal to extract arbitrary attrs. Known fields are skipped.
	var raw map[string]json.RawMessage
	_ = json.Unmarshal([]byte(line), &raw) //nolint:errcheck

	reserved := map[string]bool{"time": true, "level": true, "source": true, "msg": true}
	attrs := make(map[string]any)
	for k, v := range raw {
		if reserved[k] {
			continue
		}
		var val any
		if err := json.Unmarshal(v, &val); err == nil {
			attrs[k] = val
		}
	}

	// slog serializes custom levels as offsets (e.g. LevelTrace = DEBUG-4 -> "DEBUG-4").
	// Normalize to the canonical name so filtering and badge styling work correctly.
	level := strings.ToLower(known.Level)
	if level == "debug-4" {
		level = "trace"
	}

	entry := LogEntry{
		Time:    known.Time,
		Level:   level,
		Message: known.Msg,
	}

	if known.Source != nil && known.Source.File != "" {
		entry.Source = fmt.Sprintf("%s:%d", filepath.Base(known.Source.File), known.Source.Line)
	}

	if c, ok := attrs["component"]; ok {
		if cs, ok := c.(string); ok {
			entry.Component = cs
			delete(attrs, "component")
		}
	}

	// Auto-derive component from the Go package directory when not explicitly set.
	if entry.Component == "" && known.Source != nil && known.Source.File != "" {
		if base := filepath.Base(filepath.Dir(known.Source.File)); base != "." && base != "" {
			entry.Component = base
		}
	}

	if len(attrs) > 0 {
		entry.Attrs = attrs
	}

	return entry
}
