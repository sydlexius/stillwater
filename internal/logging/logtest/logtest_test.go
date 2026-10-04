package logtest

import (
	"strings"
	"testing"
)

func TestDuplicateKeys(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int
	}{
		{"clean record", `{"level":"INFO","msg":"x","component":"a"}`, 0},
		{"stacked key", `{"msg":"x","component":"a","component":"b"}`, 1},
		{"stacked key on one of two lines", "{\"a\":1}\n{\"component\":1,\"component\":2}\n", 1},
		// Only top-level keys count; a group may legitimately reuse a name.
		{"same name nested in a group", `{"component":"a","g":{"component":"b"}}`, 0},
		{"empty output", "", 0},
		{"malformed line is reported, not skipped", `not json`, 1},
		{"truncated object is reported", `{"component":"a"`, 1},
		{"truncated mid-value is reported", `{"component":"a","msg":`, 1},
		{"trailing content is reported", `{"component":"a"} {"x":1}`, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := DuplicateKeys(tc.in); len(got) != tc.want {
				t.Fatalf("DuplicateKeys(%q) = %v, want %d finding(s)", tc.in, got, tc.want)
			}
		})
	}
}

func TestNewJSONLoggerCapturesAndResets(t *testing.T) {
	l, buf := NewJSONLogger()
	l.Debug("probe", "k", "v") // debug level: the logger must not filter it
	if out := buf.String(); !strings.Contains(out, `"msg":"probe"`) || !strings.Contains(out, `"k":"v"`) {
		t.Fatalf("record not captured: %q", out)
	}
	buf.Reset()
	if out := buf.String(); out != "" {
		t.Fatalf("Reset left %q", out)
	}
}
