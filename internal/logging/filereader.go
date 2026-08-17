package logging

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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
	f, err := os.Open(cleaned)
	if err != nil {
		return nil, fmt.Errorf("opening log file: %w", err)
	}
	defer f.Close() //nolint:errcheck // Close error not actionable on read path

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

// parseLogLine parses a single log line into a LogEntry. The line is routed to
// the JSON path or the logfmt path by sniffing its first non-whitespace byte
// (a leading '{' means JSON), not by reading config, so a directory holding
// files from both eras (a config change mid-deployment, or a rotated file
// from before one) still reads correctly. A line that the selected path
// cannot parse falls through to an explicit, loud fallback: a distinct Level
// and a parse_error attr, so it can never be mistaken for a genuine INFO
// record.
func parseLogLine(line string) LogEntry {
	trimmed := strings.TrimLeft(line, " \t")
	if strings.HasPrefix(trimmed, "{") {
		if entry, ok := parseSlogJSONLine(line); ok {
			return entry
		}
	} else if entry, ok := parseLogfmtLine(line); ok {
		return entry
	}

	return LogEntry{
		Level:   "unknown",
		Message: line,
		Attrs:   map[string]any{"parse_error": true},
	}
}

// parseSlogJSONLine parses a line written by slog's JSON handler. ok is false
// if the line is not valid JSON, signaling the caller to fall through.
func parseSlogJSONLine(line string) (LogEntry, bool) {
	var known slogJSONLine
	if err := json.Unmarshal([]byte(line), &known); err != nil {
		return LogEntry{}, false
	}

	// Second unmarshal to extract arbitrary attrs. Known fields are skipped.
	var raw map[string]json.RawMessage
	_ = json.Unmarshal([]byte(line), &raw)

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

	return entry, true
}

// parseLogfmtLine parses a line written by slog's TextHandler (key=value
// pairs, values quoted per strconv.Quote rules when they contain whitespace,
// a quote, an '=', or a non-printing character). ok is false, signaling the
// caller to fall through to the explicit fallback rather than silently
// returning an empty record, in any of three cases: a quoted value is
// malformed (an unterminated quote or an invalid escape, which fails
// parseLogfmtPairs/scanLogfmtValue outright), the parsed pairs are missing
// the required "level" key, or they are missing the required "msg" key. A
// line with no "=" pairs at all is well-formed with an empty pair set (each
// token with no '=' is skipped, not rejected), so it is caught by the
// missing-"level" check rather than by parseLogfmtPairs itself.
func parseLogfmtLine(line string) (LogEntry, bool) {
	pairs, wellFormed := parseLogfmtPairs(line)
	if !wellFormed {
		return LogEntry{}, false
	}

	// REQUIRE the two fields every TextHandler record carries. Accepting any
	// line with a single key=value pair was the defect this parser was written
	// to fix, reintroduced one level down: `foo=bar` produced an entry with an
	// EMPTY Level, and levelSeverity("") sorts as info, so a corrupt line
	// passed a level filter and read as a valid INFO record. Presence is
	// checked rather than non-emptiness so a genuine `msg=""` still parses.
	if _, ok := pairs["level"]; !ok {
		return LogEntry{}, false
	}
	if _, ok := pairs["msg"]; !ok {
		return LogEntry{}, false
	}

	entry := LogEntry{}

	if tsStr, ok := pairs["time"]; ok {
		if ts, err := time.Parse(time.RFC3339Nano, tsStr); err == nil {
			entry.Time = ts
		}
		// If the timestamp does not parse, Time is left zero and parsing
		// continues; a bad timestamp should not hide an otherwise-good record.
	}

	// slog serializes custom levels as offsets (e.g. LevelTrace = DEBUG-4 -> "DEBUG-4").
	// Normalize to the canonical name so filtering and badge styling work
	// correctly, matching the JSON path.
	level := strings.ToLower(pairs["level"])
	if level == "debug-4" {
		level = "trace"
	}
	entry.Level = level
	entry.Message = pairs["msg"]

	// TextHandler emits source as a single "<file>:<line>" value, unlike the
	// nested JSON source object. Split on the last ':' to recover the file
	// and line separately, matching the JSON path's "<basename>:<line>" output.
	var sourceFile string
	if src := pairs["source"]; src != "" {
		if idx := strings.LastIndex(src, ":"); idx >= 0 {
			sourceFile = src[:idx]
			entry.Source = fmt.Sprintf("%s:%s", filepath.Base(sourceFile), src[idx+1:])
		} else {
			sourceFile = src
			entry.Source = filepath.Base(src)
		}
	}

	if c := pairs["component"]; c != "" {
		entry.Component = c
	} else if sourceFile != "" {
		// Auto-derive component from the Go package directory when not
		// explicitly set, matching the JSON path.
		if base := filepath.Base(filepath.Dir(sourceFile)); base != "." && base != "" {
			entry.Component = base
		}
	}

	reserved := map[string]bool{"time": true, "level": true, "msg": true, "source": true, "component": true}
	attrs := make(map[string]any)
	for k, v := range pairs {
		if reserved[k] {
			continue
		}
		attrs[k] = v
	}
	if len(attrs) > 0 {
		entry.Attrs = attrs
	}

	return entry, true
}

// parseLogfmtPairs splits a logfmt line into key=value pairs. A value is
// quoted (strconv.Quote rules) when it contains whitespace, a non-printing
// character, '"', or '='; quoted values are decoded with strconv.Unquote. A
// token with no '=' is skipped rather than treated as a bare key, since slog's
// TextHandler never emits one.
func parseLogfmtPairs(line string) (map[string]string, bool) {
	pairs := make(map[string]string)
	i, n := 0, len(line)
	for i < n {
		for i < n && line[i] == ' ' {
			i++
		}
		if i >= n {
			break
		}

		keyStart := i
		for i < n && line[i] != '=' && line[i] != ' ' {
			i++
		}
		if i >= n || line[i] != '=' {
			// No '=' before the next space (or EOL): not a key=value token.
			// Skip past it and keep scanning the rest of the line.
			for i < n && line[i] != ' ' {
				i++
			}
			continue
		}
		key := line[keyStart:i]
		i++ // skip '='

		var value string
		var wellFormed bool
		value, i, wellFormed = scanLogfmtValue(line, i)
		if !wellFormed {
			return nil, false
		}
		if key != "" {
			pairs[key] = value
		}
	}
	return pairs, true
}

// scanLogfmtValue reads a single logfmt value starting at index i (just past
// the '='). It returns the decoded value, the index of the next unread byte,
// and whether the value was WELL-FORMED. A leading '"' selects the quoted form
// (scanned to its closing, unescaped quote and decoded with strconv.Unquote);
// otherwise the value runs to the next space or end of line.
//
// The ok return exists because an unterminated quote or an invalid escape must
// invalidate the whole record rather than yield raw text that looks parsed.
func scanLogfmtValue(line string, i int) (string, int, bool) {
	n := len(line)
	if i >= n || line[i] != '"' {
		valStart := i
		for i < n && line[i] != ' ' {
			i++
		}
		return line[valStart:i], i, true
	}

	valStart := i
	i++
	for i < n {
		if line[i] == '\\' && i+1 < n {
			i += 2
			continue
		}
		if line[i] == '"' {
			i++
			break
		}
		i++
	}
	quoted := line[valStart:i]
	unquoted, err := strconv.Unquote(quoted)
	if err != nil {
		// An unterminated quote or an invalid escape is a MALFORMED record, not
		// a value that happens to contain quote characters. Returning the raw
		// text here let a corrupt line through as if it had parsed, which is
		// the same class of defect this file exists to fix.
		return "", i, false
	}
	return unquoted, i, true
}
