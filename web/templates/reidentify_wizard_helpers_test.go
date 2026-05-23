package templates

import (
	"testing"
)

func TestWizardRetryURL(t *testing.T) {
	tests := []struct {
		name      string
		sessionID string
		index     int
		want      string
	}{
		{
			name:      "typical session and step",
			sessionID: "sess-abc",
			index:     3,
			want:      "/api/v1/artists/re-identify/wizard/sess-abc/step/3/retry",
		},
		{
			name:      "first step",
			sessionID: "sess-xyz",
			index:     0,
			want:      "/api/v1/artists/re-identify/wizard/sess-xyz/step/0/retry",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := wizardRetryURL(tt.sessionID, tt.index)
			if got != tt.want {
				t.Errorf("wizardRetryURL = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWizardSkippedReasonLabel(tt *testing.T) {
	ctx := testCtx(tt)

	// Assert against the exact translated value for each reason key so a
	// future rename of a skipped_reason.* key without updating
	// wizardSkippedReasonLabel is caught here (substring matching against
	// a coincidentally-shared word in en.json would mask the regression).
	// The outer receiver is named `tt` rather than the conventional `t`
	// because the templates package's translation helper is also named t.
	cases := []struct {
		name   string
		reason string
		want   string
	}{
		{
			name:   "not_found returns the not_found translation",
			reason: "not_found",
			want:   t(ctx, "artists.bulk.reidentify.wizard.skipped_reason.not_found"),
		},
		{
			name:   "load_error returns the load_error translation",
			reason: "load_error",
			want:   t(ctx, "artists.bulk.reidentify.wizard.skipped_reason.load_error"),
		},
		{
			name:   "unknown reason falls through to the unknown translation",
			reason: "some-future-reason-string",
			want:   t(ctx, "artists.bulk.reidentify.wizard.skipped_reason.unknown"),
		},
		{
			name:   "empty reason falls through to the unknown translation",
			reason: "",
			want:   t(ctx, "artists.bulk.reidentify.wizard.skipped_reason.unknown"),
		},
	}
	for _, tc := range cases {
		tt.Run(tc.name, func(sub *testing.T) {
			got := wizardSkippedReasonLabel(ctx, tc.reason)
			if got != tc.want {
				sub.Errorf("label for reason %q = %q, want %q", tc.reason, got, tc.want)
			}
		})
	}
}
