package templates

import (
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/provider"
)

// #2415. A needs_crop response is a 200 that saved nothing. Every surface that
// POSTs /images/fetch or /images/upload must react to it, and -- just as
// importantly -- the handler it delegates to must actually EXIST on the page
// that surface renders on.
//
// That second half is what the original wiring got wrong: openAutoCrop was
// defined inside the drag-drop IIFE, which only the CONTEXTUALIZED layout
// renders, while components.ImageCard and components.ImageUpload render on the
// GENERIC one. A handler hung off that IIFE would have been undefined on
// exactly the surfaces this issue is about.

const afterRequestAttr = `hx-on::after-request="swHandleFetchNeedsCrop(event)"`

// tagContaining returns the single HTML tag (from '<' to '>') holding marker,
// so an assertion cannot pass on an attribute that lives elsewhere in the page.
func tagContaining(t *testing.T, html, marker string) string {
	t.Helper()
	at := strings.Index(html, marker)
	if at < 0 {
		t.Fatalf("marker %q not found in rendered output", marker)
	}
	open := strings.LastIndex(html[:at], "<")
	if open < 0 {
		t.Fatalf("no opening '<' before marker %q", marker)
	}
	end := strings.Index(html[open:], ">")
	if end < 0 {
		t.Fatalf("no closing '>' after marker %q", marker)
	}
	return html[open : open+end+1]
}

// TestArtworkManageEditor_NeedsCropHandlerOnBothLayouts is the ancestry guard.
// Moving the handler back into a layout-specific script block breaks this.
func TestArtworkManageEditor_NeedsCropHandlerOnBothLayouts(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		selectedType string
	}{
		{"contextualized layout (single type)", "thumb"},
		// The generic layout is the one that renders components.ImageCard (into
		// #search-results) and components.ImageUpload -- the surfaces that were
		// silently dropping needs_crop.
		{"generic layout (all types)", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			html := renderEditor(t, editorData(tc.selectedType))

			if !strings.Contains(html, "function swHandleFetchNeedsCrop") {
				t.Errorf("swHandleFetchNeedsCrop must be DEFINED on this layout; the surfaces that call it render here, and an hx-on handler naming a missing global just throws")
			}
			if !strings.Contains(html, "function openAutoCrop") {
				t.Errorf("openAutoCrop must be DEFINED on this layout, not only on the contextualized one")
			}
			if !strings.Contains(html, `id="crop-modal"`) {
				t.Errorf("the crop modal must render on this layout; it is what a needs_crop response opens, and it carries the handler's translated copy")
			}
		})
	}
}

func TestImageSearch_CompareSaveButtonHandlesNeedsCrop(t *testing.T) {
	t.Parallel()
	html := renderEditor(t, editorData("thumb"))

	tag := tagContaining(t, html, `id="compare-save-btn"`)
	if !strings.Contains(tag, afterRequestAttr) {
		t.Errorf("the compare-panel Save button must react to needs_crop via the shared handler.\ntag: %s", tag)
	}
	// The inline try/catch it replaced swallowed the response two ways: an empty
	// catch(e){} and a typeof guard that no-opped when openAutoCrop was missing.
	if strings.Contains(tag, "catch(e){}") {
		t.Errorf("the silent catch(e){} guard is back on compare-save-btn.\ntag: %s", tag)
	}
}

func TestFanartSearchResults_SaveButtonHandlesNeedsCrop(t *testing.T) {
	t.Parallel()
	var sb strings.Builder
	images := []provider.ImageResult{{
		URL: "https://example.com/sq.jpg", Type: "fanart", Source: "fanarttv", Width: 800, Height: 800,
	}}
	if err := FanartSearchResults("art-1", images, "").Render(testCtx(t), &sb); err != nil {
		t.Fatalf("render FanartSearchResults: %v", err)
	}
	html := sb.String()

	tag := tagContaining(t, html, "/images/fetch")
	if !strings.Contains(tag, afterRequestAttr) {
		t.Errorf("the fanart search-result Save button picks a raw provider thumbnail, so it is a prime needs_crop candidate.\ntag: %s", tag)
	}
	if strings.Contains(tag, "catch(e){}") {
		t.Errorf("the silent catch(e){} guard is back on the fanart Save button.\ntag: %s", tag)
	}
}
