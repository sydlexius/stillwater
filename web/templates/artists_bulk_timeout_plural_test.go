package templates

import (
	"bytes"
	"strings"
	"testing"
)

// TestBulkProgressPill_TimedOutSummaryIsSingularSafe pins the singular/plural
// split on the timed-out toast (#2931 review).
//
// A single-artist bulk action runs through the same handler as a 500-artist
// one, so a plural-only template renders "1 of 1 artists". The strings live in
// data attributes because the toast is composed client-side, which means the
// contract this test guards is the MARKUP: if only one attribute is emitted,
// the JS silently falls back to its hard-coded English and the localized
// singular never appears.
func TestBulkProgressPill_TimedOutSummaryIsSingularSafe(t *testing.T) {
	var buf bytes.Buffer
	if err := BulkProgressPill().Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()

	for _, attr := range []string{
		"data-i18n-timed-out-summary-one",
		"data-i18n-timed-out-summary-other",
	} {
		if !strings.Contains(body, attr) {
			t.Errorf("%s absent; the JS falls back to hard-coded English without it:\n%s", attr, body)
		}
	}

	// The one-item case is the whole point: it must not say "artists".
	// Asserting the rendered TEXT rather than the key, because a key wired to
	// the wrong string would satisfy an attribute-only check.
	if !strings.Contains(body, "of 1 artist.") {
		t.Errorf("singular variant missing its \"1 artist\" text:\n%s", body)
	}
	if strings.Contains(body, "of 1 artists") {
		t.Errorf("singular variant still reads \"1 artists\":\n%s", body)
	}

	// And the plural must survive, so the fix does not simply invert the bug.
	if !strings.Contains(body, "{total} artists.") {
		t.Errorf("plural variant missing:\n%s", body)
	}
}
