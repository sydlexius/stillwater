package filesystem

import (
	"log/slog"
	"os"
	"runtime"
	"strings"
)

// traceFSEnabled is resolved once at init time so disabled call sites cost
// only a branch + atomic-free bool load. Enable with STILLWATER_TRACE_FS=1.
var traceFSEnabled = os.Getenv("STILLWATER_TRACE_FS") == "1"

// TraceFSWrite emits a single log line capturing a write-side filesystem
// operation (write, rename, symlink) with a compact caller stack. Intended for
// ephemeral investigation of who writes a given path. Grep for "trace.fs" to
// find every entry. No-op unless STILLWATER_TRACE_FS=1 is set in the env.
//
// skip is the number of frames above TraceFSWrite itself to hide from the
// stack. Pass 0 to start from the direct caller.
func TraceFSWrite(op, path string, skip int) {
	if !traceFSEnabled {
		return
	}
	pcs := make([]uintptr, 12)
	// +2 skips runtime.Callers and TraceFSWrite itself.
	n := runtime.Callers(2+skip, pcs)
	frames := runtime.CallersFrames(pcs[:n])

	var parts []string
	for range 8 {
		f, more := frames.Next()
		if f.Function == "" {
			break
		}
		// Strip the long module prefix to keep the line readable.
		fn := f.Function
		if idx := strings.LastIndex(fn, "/stillwater/"); idx >= 0 {
			fn = fn[idx+len("/stillwater/"):]
		}
		file := f.File
		if idx := strings.LastIndex(file, "/stillwater/"); idx >= 0 {
			file = file[idx+len("/stillwater/"):]
		}
		parts = append(parts, fn+" ("+file+":"+itoa(f.Line)+")")
		if !more {
			break
		}
	}

	slog.Info("trace.fs",
		slog.String("op", op),
		slog.String("path", path),
		slog.String("stack", strings.Join(parts, " > ")),
	)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
