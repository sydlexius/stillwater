// Package logtest holds test helpers for asserting on emitted log records.
package logtest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
)

// Buffer is a goroutine-safe io.Writer for capturing JSON log output.
type Buffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// Write implements io.Writer.
func (b *Buffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// String returns everything captured so far.
func (b *Buffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// NewJSONLogger returns a debug-level JSON logger writing to a fresh Buffer.
func NewJSONLogger() (*slog.Logger, *Buffer) {
	b := &Buffer{}
	return slog.New(slog.NewJSONHandler(b, &slog.HandlerOptions{Level: slog.LevelDebug})), b
}

// DuplicateKeys returns one description per top-level key that appears more
// than once in a single JSON log line. encoding/json would silently keep the
// last value, so this walks the token stream instead.
func DuplicateKeys(output string) []string {
	var dups []string
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if line == "" {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(line))
		if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
			dups = append(dups, fmt.Sprintf("unparsable line %q: not a JSON object", line))
			continue
		}
		seen := map[string]bool{}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				break
			}
			key, _ := kt.(string)
			if seen[key] {
				dups = append(dups, fmt.Sprintf("key %q repeated in %s", key, line))
			}
			seen[key] = true
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				break
			}
		}
		// The line must be one complete object: a closing brace, then nothing.
		if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
			dups = append(dups, fmt.Sprintf("unparsable line %q: truncated or malformed object", line))
		} else if _, err := dec.Token(); err != io.EOF {
			dups = append(dups, fmt.Sprintf("unparsable line %q: trailing content after the object", line))
		}
	}
	return dups
}

// Reset discards everything captured so far.
func (b *Buffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}
