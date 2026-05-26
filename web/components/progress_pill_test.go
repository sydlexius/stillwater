package components

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/i18n"
)

// progressPillTestCtx builds a context carrying just the translation keys
// the ProgressPill templ reads. Mirrors filter_flyout_test.go's pattern --
// the helpers.t() lookup returns the bare key when no translator is on the
// context, which would mask attribute-value assertions for the new stale
// indicator (the rendered string would be e.g. "progress_pill.stale" not
// the localized copy).
func progressPillTestCtx() context.Context {
	tr := i18n.NewTranslator("en", map[string]string{
		"progress_pill.aria_label":                 "Long-running operations",
		"progress_pill.cancel":                     "Cancel",
		"progress_pill.dismiss":                    "Dismiss",
		"progress_pill.template":                   "{verb}: {processed} of {total}",
		"progress_pill.completed":                  "completed",
		"progress_pill.failed":                     "failed",
		"progress_pill.canceled":                   "canceled",
		"progress_pill.stale":                      "waiting for update...",
		"progress_pill.aria_stale":                 "Waiting for update",
		"progress_pill.verb.bulk_action":           "Working",
		"artists.bulk.progress.run_rules":          "Running rules",
		"artists.bulk.progress.re_identify":        "Re-identifying",
		"artists.bulk.progress.scan":               "Scanning",
		"artists.bulk.progress.fetch_images":       "Fetching images",
		"artists.bulk.progress.lock":               "Locking",
		"artists.bulk.progress.unlock":             "Unlocking",
		"artists.bulk.progress.re_identify_auto":   "Re-identifying (auto)",
		"artists.bulk.progress.re_identify_review": "Re-identifying (review)",
	})
	return i18n.WithTranslator(context.Background(), tr)
}

// TestProgressPillRender_StaleAttrsPresent verifies that the rendered
// container carries the data-stale-after-ms and the stale i18n attributes
// the inline JS reads at boot. A missing attribute would silently land
// the JS on its fallback (10000ms threshold, "Waiting for update" label)
// rather than the templ-localized strings; this test pins the contract.
func TestProgressPillRender_StaleAttrsPresent(t *testing.T) {
	var buf bytes.Buffer
	if err := ProgressPill().Render(progressPillTestCtx(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	wantAttrs := []string{
		`data-stale-after-ms="10000"`,
		`data-i18n-stale=`,
		`data-i18n-aria-stale=`,
	}
	for _, attr := range wantAttrs {
		if !strings.Contains(out, attr) {
			t.Errorf("rendered ProgressPill is missing required attribute %q\nfull output:\n%s", attr, out)
		}
	}
}

// TestProgressPillRender_LocalizedStaleStrings verifies the data-i18n-stale
// and data-i18n-aria-stale attributes carry the localized English copy from
// progress_pill.stale / progress_pill.aria_stale. The JS reads these via
// the data-i18n-<key> selector and falls back to "Waiting for update" only
// when the attribute is empty; pinning the value here catches accidental
// removal of the locale entries.
func TestProgressPillRender_LocalizedStaleStrings(t *testing.T) {
	var buf bytes.Buffer
	if err := ProgressPill().Render(progressPillTestCtx(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `data-i18n-stale="waiting for update..."`) {
		t.Errorf("data-i18n-stale missing or wrong English copy")
	}
	if !strings.Contains(out, `data-i18n-aria-stale="Waiting for update"`) {
		t.Errorf("data-i18n-aria-stale missing or wrong English copy")
	}
}

// TestProgressPillRender_PreservedPriorContract is a smoke check that the
// pre-existing data-i18n-cancel and data-i18n-template attributes still
// render after the stale-indicator additions; an accidental templ regression
// here would silently break the Cancel button label and the pill text
// formatter.
func TestProgressPillRender_PreservedPriorContract(t *testing.T) {
	var buf bytes.Buffer
	if err := ProgressPill().Render(progressPillTestCtx(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	for _, attr := range []string{`data-i18n-cancel="Cancel"`, `data-i18n-template=`} {
		if !strings.Contains(out, attr) {
			t.Errorf("preserved attribute missing: %q", attr)
		}
	}
}
