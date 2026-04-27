package templates

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/rule"
)

// TestArtistViolationsTab_DoubleSubmitGuard pins the per-row double-submit
// guard. The artist-detail violations tab uses raw fetch() rather than
// htmx, so the guard lives in the templ script function: both onclick
// handlers must disable the row's action buttons before issuing the POST
// and re-enable them in finally(). We assert the rendered tab contains
// the disable code so a future refactor cannot quietly drop it.
//
// We also assert finally() and window.showToast appear in the rendered
// script: finally guarantees re-enable on every settled outcome (success,
// HTTP error, network error) and showToast is the user-facing failure
// surface that replaces the prior native alert() dialog.
func TestArtistViolationsTab_DoubleSubmitGuard(t *testing.T) {
	data := ArtistViolationsTabData{
		ArtistID: "a-1110",
		Violations: []rule.RuleViolation{
			{
				ID:         "v-1110",
				RuleID:     rule.RuleNFOExists,
				ArtistID:   "a-1110",
				ArtistName: "Tab Test Artist",
				Severity:   "error",
				Message:    "missing nfo",
				Fixable:    true,
				Status:     rule.ViolationStatusOpen,
				CreatedAt:  time.Now().UTC(),
			},
		},
	}

	var buf bytes.Buffer
	if err := ArtistViolationsTab(data).Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()

	// The Fix and Dismiss buttons render their JS via templ script blocks,
	// which the templ runtime may emit either inline or in a hoisted
	// <script> tag. Either way the function bodies appear in the rendered
	// HTML for the tab, so substring assertions are stable across both
	// emission modes.

	// Disable-on-click: every settled outcome must re-enable, so the
	// finally() block is required. Without it a network error would leave
	// the row's buttons stuck in the disabled state.
	if !strings.Contains(body, "actionBtns[i].disabled = true") {
		t.Errorf("artist-tab fix/dismiss script missing the in-flight disable assignment; got:\n%s", body)
	}
	if !strings.Contains(body, "actionBtns[j].disabled = false") {
		t.Errorf("artist-tab fix/dismiss script missing the finally() re-enable assignment; got:\n%s", body)
	}
	if !strings.Contains(body, ".finally(function()") {
		t.Errorf("artist-tab fix/dismiss script missing finally() block; got:\n%s", body)
	}

	// User-facing failure surface: the script must route failures through
	// window.showToast so the artist tab matches the dashboard toast
	// styling instead of using a native browser alert dialog.
	if !strings.Contains(body, "window.showToast") {
		t.Errorf("artist-tab fix/dismiss script missing window.showToast routing; got:\n%s", body)
	}

	// Disabled-state styling so the visual feedback matches user expectation
	// while the request is in flight. Without this the buttons stay clickable-
	// looking even though the disabled property suppresses the click.
	if !strings.Contains(body, "disabled:opacity-60") {
		t.Errorf("artist-tab action buttons missing disabled:opacity-60 styling; got:\n%s", body)
	}
}
