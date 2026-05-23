package templates

import (
	"strings"
	"testing"
)

func TestWizardRetryURL(t *testing.T) {
	got := wizardRetryURL("sess-abc", 3)
	want := "/api/v1/artists/re-identify/wizard/sess-abc/step/3/retry"
	if got != want {
		t.Errorf("wizardRetryURL = %q, want %q", got, want)
	}
}

func TestWizardSkippedReasonLabel(t *testing.T) {
	ctx := testCtx(t)

	tests := []struct {
		name      string
		reason    string
		mustMatch string // substring assertion; the exact phrase lives in en.json
	}{
		{
			name:      "not_found reason returns localized phrase",
			reason:    "not_found",
			mustMatch: "not found",
		},
		{
			name:      "load_error reason returns localized phrase",
			reason:    "load_error",
			mustMatch: "load",
		},
		{
			name:      "unknown reason falls through to generic label",
			reason:    "some-future-reason-string",
			mustMatch: "skipped",
		},
		{
			name:      "empty reason falls through to generic label",
			reason:    "",
			mustMatch: "skipped",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := wizardSkippedReasonLabel(ctx, tt.reason)
			if got == "" {
				t.Fatalf("expected a non-empty label for reason %q", tt.reason)
			}
			if !strings.Contains(strings.ToLower(got), tt.mustMatch) {
				t.Errorf("label for reason %q = %q, expected to contain %q",
					tt.reason, got, tt.mustMatch)
			}
			// Guard against leaking the raw reason token to the UI.
			if tt.reason != "" && strings.Contains(got, tt.reason) {
				t.Errorf("label %q must not echo the raw reason token %q",
					got, tt.reason)
			}
		})
	}
}
