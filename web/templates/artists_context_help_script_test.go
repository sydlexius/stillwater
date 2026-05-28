package templates

import (
	"bytes"
	"strings"
	"testing"
)

// TestArtistsPage_IncludesContextHelpScript pins that the ArtistsPage template
// renders the global ContextHelpScript block. Without it, the "?" help icons
// next to Filters, Sort, and Choose Action call window.swContextHelpToggle as
// an undefined function and silently no-op (issue #1727). Asserting on the
// function definition string is a robust marker because ContextHelpScript is
// the only template that defines window.swContextHelpToggle.
func TestArtistsPage_IncludesContextHelpScript(t *testing.T) {
	var buf bytes.Buffer
	if err := ArtistsPage(AssetPaths{}, ArtistListData{}).Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()

	if !strings.Contains(body, "window.swContextHelpToggle") {
		t.Errorf("ArtistsPage missing ContextHelpScript: rendered body does not define window.swContextHelpToggle")
	}
	if !strings.Contains(body, "window.swContextHelpClose") {
		t.Errorf("ArtistsPage missing ContextHelpScript: rendered body does not define window.swContextHelpClose")
	}
}
