package components

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/provider"
)

// #2415. /images/fetch and /images/upload answer an aspect-ratio mismatch with
// a 200 that saved NOTHING; the body carries needs_crop plus the image data so
// the client can prompt for a crop. Three of the four surfaces that POST to
// those endpoints used to discard that response outright -- the user clicked
// Save, saw no error, and got no image.
//
// These tests pin the wiring on each surface. They are markup-level by design:
// the handler's own behavior is covered by tests/unit/image-needs-crop-handler.
// A surface that loses its handler goes RED here.

const (
	afterRequestAttr = `hx-on::after-request="swHandleFetchNeedsCrop(event)"`
	beforeSwapAttr   = `hx-on::before-swap="swSuppressNeedsCropSwap(event)"`
)

// tagContaining returns the single HTML tag (from its '<' to its '>') that
// contains marker. Scoping the assertion to one tag is what stops it passing
// because the attribute happens to appear somewhere else in the document.
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
	close := strings.Index(html[open:], ">")
	if close < 0 {
		t.Fatalf("no closing '>' after marker %q", marker)
	}
	return html[open : open+close+1]
}

func renderToString(t *testing.T, render func(context.Context, *bytes.Buffer) error) string {
	t.Helper()
	var buf bytes.Buffer
	if err := render(context.Background(), &buf); err != nil {
		t.Fatalf("rendering: %v", err)
	}
	return buf.String()
}

func TestImageCard_SaveButtonHandlesNeedsCrop(t *testing.T) {
	t.Parallel()
	html := renderToString(t, func(ctx context.Context, buf *bytes.Buffer) error {
		return ImageCard("artist-1", provider.ImageResult{
			URL: "https://example.com/wide.jpg", Type: "thumb", Source: "fanarttv", Width: 1000, Height: 100,
		}, false).Render(ctx, buf)
	})

	tag := tagContaining(t, html, "image-card-save")
	if !strings.Contains(tag, afterRequestAttr) {
		t.Errorf("the ImageCard Save button must react to needs_crop; it POSTs /images/fetch with hx-swap=\"none\", so without this the response -- and the user's image -- is silently dropped.\ntag: %s", tag)
	}
}

func TestImageUpload_BothFormsHandleNeedsCrop(t *testing.T) {
	t.Parallel()
	html := renderToString(t, func(ctx context.Context, buf *bytes.Buffer) error {
		return ImageUpload("artist-1", "thumb").Render(ctx, buf)
	})

	for _, tc := range []struct {
		name   string
		marker string
	}{
		{"multipart upload form", "/images/upload"},
		{"fetch-from-URL form", "/images/fetch"},
	} {
		tag := tagContaining(t, html, tc.marker)
		if !strings.Contains(tag, afterRequestAttr) {
			t.Errorf("%s: must open the crop modal on needs_crop.\ntag: %s", tc.name, tag)
		}
		// htmx dispatches htmx:beforeSwap on the SWAP TARGET, not on the
		// requesting element. #upload-result is a sibling of each form (not a
		// descendant), so a listener on the form never sees the event -- it
		// must live on #upload-result instead (asserted below). Putting it
		// back on the form regressed #2415: the needs_crop JSON/base64 body
		// dumps into #upload-result unsuppressed.
		if strings.Contains(tag, beforeSwapAttr) {
			t.Errorf("%s: before-swap suppression must NOT be on the form -- htmx:beforeSwap fires on the swap target (#upload-result), never on the form, so a listener here is dead code.\ntag: %s", tc.name, tag)
		}
	}

	// Both forms target #upload-result and swap the response body straight
	// into it. Without the before-swap guard ON THE TARGET, a needs_crop
	// response lands as a screenful of raw JSON and base64 image data, with
	// no hint that nothing was saved.
	resultTag := tagContaining(t, html, `id="upload-result"`)
	if !strings.Contains(resultTag, beforeSwapAttr) {
		t.Errorf("#upload-result must suppress the swap of a needs_crop body (htmx:beforeSwap fires on the swap target).\ntag: %s", resultTag)
	}
}

func TestComparePanel_SaveButtonHandlesNeedsCrop(t *testing.T) {
	t.Parallel()
	// ComparePanel has no call site today (the live compare panel is hand-rolled
	// in image_search.templ). It is wired anyway so it is not pre-broken the
	// moment someone renders it.
	html := renderToString(t, func(ctx context.Context, buf *bytes.Buffer) error {
		return ComparePanel("artist-1", "https://example.com/x.jpg", "fanarttv", "thumb", 1000, 100).Render(ctx, buf)
	})

	tag := tagContaining(t, html, "Use this one")
	if !strings.Contains(tag, afterRequestAttr) {
		t.Errorf("the ComparePanel save button must react to needs_crop.\ntag: %s", tag)
	}
}

// TestImageCropModal_CarriesNeedsCropCopy pins the data attributes the shared
// handler reads. The handler falls back to English if they go missing, so
// losing them would degrade silently to an untranslated string rather than
// failing anywhere visible.
func TestImageCropModal_CarriesNeedsCropCopy(t *testing.T) {
	t.Parallel()
	html := renderToString(t, func(ctx context.Context, buf *bytes.Buffer) error {
		return ImageCropModal("artist-1").Render(ctx, buf)
	})

	tag := tagContaining(t, html, `id="crop-modal"`)
	for _, attr := range []string{"data-msg-needs-crop", "data-msg-crop-unavailable", "data-msg-save-unreadable"} {
		if !strings.Contains(tag, attr) {
			t.Errorf("#crop-modal must carry %s -- it is where the shared needs_crop handler reads its translated copy from, on BOTH image layouts.\ntag: %s", attr, tag)
		}
	}
}
