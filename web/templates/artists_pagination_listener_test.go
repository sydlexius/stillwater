package templates

import (
	"bytes"
	"strings"
	"testing"
)

// TestArtistsPage_AfterSwapListener_UsesPathInfo pins the htmx 2.x event
// contract for the URL-update listener that runs after the artist content
// swaps. Issue #1228: the prior listener read evt.detail.requestConfig.path
// (legacy) without falling through to evt.detail.pathInfo.{finalRequestPath,
// requestPath}, which is the canonical 2.x event payload. The try/catch
// around the parse made the failure silent, so pagination/sort URL state
// quietly drifted on swap. This test guards the migration so a future
// regression to the legacy-only shape, or a removal of the silent-fail
// debug log, fails here instead of presenting as a stale URL bar in the UI.
func TestArtistsPage_AfterSwapListener_UsesPathInfo(t *testing.T) {
	var buf bytes.Buffer
	if err := ArtistsPage(AssetPaths{}, ArtistListData{}).Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()

	// Anchor the assertions inside the afterSwap handler that owns the
	// artist-content target. The same template registers other htmx
	// listeners (notably the bulk-selection block at line ~1434), and
	// pinning by handler scope keeps the test from accidentally passing
	// because some other unrelated listener happens to mention pathInfo.
	listenerStart := strings.Index(body, "htmx:afterSwap")
	if listenerStart < 0 {
		t.Fatalf("expected htmx:afterSwap listener in rendered ArtistsPage; body did not contain it")
	}
	// The artist-content guard immediately follows the listener registration;
	// take a generous window from there so the assertions only see code that
	// runs when the URL listener actually fires.
	scopeIdx := strings.Index(body[listenerStart:], "artist-content")
	if scopeIdx < 0 {
		t.Fatalf("could not locate artist-content guard near htmx:afterSwap listener")
	}
	// 4096 bytes is comfortably larger than the listener body and small
	// enough that we don't capture the unrelated bulk-selection listener
	// further down.
	end := listenerStart + scopeIdx + 4096
	if end > len(body) {
		end = len(body)
	}
	scope := body[listenerStart+scopeIdx : end]

	// Canonical htmx 2.x property reads must both be present.
	for _, want := range []string{
		"pathInfo.finalRequestPath",
		"pathInfo.requestPath",
	} {
		if !strings.Contains(scope, want) {
			t.Errorf("URL listener missing canonical htmx 2.x read %q; scope:\n%s", want, scope)
		}
	}

	// The silent-fail path must surface a console.debug so a future
	// regression on the event shape is visible during dev/UAT instead of
	// presenting as a stale URL bar with no other symptom.
	if !strings.Contains(scope, "console.debug") {
		t.Errorf("URL listener missing console.debug on silent-fail path; scope:\n%s", scope)
	}

	// Defensive: the legacy mono-source pattern (requestConfig.path with no
	// pathInfo fallback chain) must not reappear. We allow the legacy
	// property as one element of the priority chain, but the pathInfo read
	// must come first lexically -- otherwise the fallback chain is wrong.
	piIdx := strings.Index(scope, "pathInfo.finalRequestPath")
	rcIdx := strings.Index(scope, "requestConfig")
	if rcIdx >= 0 && piIdx >= 0 && rcIdx < piIdx {
		t.Errorf("requestConfig fallback must come AFTER pathInfo read in the priority chain; pathInfo idx=%d requestConfig idx=%d", piIdx, rcIdx)
	}
}
