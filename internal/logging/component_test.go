package logging

import (
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/logging/logtest"
)

func TestWithComponent_EmitsOneComponent(t *testing.T) {
	base, buf := logtest.NewJSONLogger()
	WithComponent(base, "alpha").With("k", "v").Info("hello")

	out := buf.String()
	if d := logtest.DuplicateKeys(out); len(d) != 0 {
		t.Fatalf("duplicate keys: %v", d)
	}
	if !strings.Contains(out, `"component":"alpha"`) || !strings.Contains(out, `"k":"v"`) {
		t.Fatalf("missing component or attr: %s", out)
	}
}

// Re-tagging must never stack the key: the new name wins and an Error record
// names both so the wiring bug is visible.
func TestWithComponent_RetagReplacesAndFailsLoudly(t *testing.T) {
	base, buf := logtest.NewJSONLogger()
	outer := WithComponent(base, "outer")
	inner := WithComponent(outer, "inner")
	inner.Info("work")

	out := buf.String()
	if d := logtest.DuplicateKeys(out); len(d) != 0 {
		t.Fatalf("duplicate keys after re-tag: %v\n%s", d, out)
	}
	if !strings.Contains(out, `"msg":"work"`) || !strings.Contains(out, `"component":"inner"`) {
		t.Fatalf("the new component must win: %s", out)
	}
	for _, want := range []string{`"level":"ERROR"`, `"previous_component":"outer"`, `"new_component":"inner"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %s in loud re-tag record: %s", want, out)
		}
	}
	// The original logger is untouched by the re-tag.
	buf.Reset()
	outer.Info("still outer")
	if !strings.Contains(buf.String(), `"component":"outer"`) {
		t.Fatalf("re-tag mutated the original logger: %s", buf.String())
	}
}

// The detector must itself see the defect it guards against: a raw double
// With stacks the key and slog emits it twice.
func TestDuplicateKeysDetectsRawStacking(t *testing.T) {
	base, buf := logtest.NewJSONLogger()
	base.With("component", "a").With("component", "b").Info("x")
	if d := logtest.DuplicateKeys(buf.String()); len(d) != 1 {
		t.Fatalf("detector missed a raw stacked key: %v / %s", d, buf.String())
	}
}
