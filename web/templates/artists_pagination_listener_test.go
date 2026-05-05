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

	// Assertions key off JS-syntax fragments unique to the rawPath fallback
	// chain expression. Plain runtime tokens like "xhr.responseURL" also
	// appear in the priority-list comment ("evt.detail.xhr.responseURL"),
	// so we anchor on operator/grouping syntax that only the code carries:
	// "pi && (pi.finalRequestPath", "pi.requestPath)", "rc && rc.path", and
	// "xhr && xhr.responseURL". A regression to comments-only mentions or
	// to a different chain shape would not match these patterns.
	for _, want := range []string{
		"pi && (pi.finalRequestPath",
		"pi.requestPath)",
		"rc && rc.path",
		"xhr && xhr.responseURL",
	} {
		if !strings.Contains(scope, want) {
			t.Errorf("URL listener missing rawPath chain fragment %q; scope:\n%s", want, scope)
		}
	}

	// Strict priority order: the rawPath fallback chain must read
	// pi.finalRequestPath -> pi.requestPath -> rc.path -> xhr.responseURL.
	// First-occurrence indices of the four code-only fragments must appear
	// in that order; any inversion means the priority chain regressed.
	tokens := []string{
		"pi && (pi.finalRequestPath",
		"pi.requestPath)",
		"rc && rc.path",
		"xhr && xhr.responseURL",
	}
	indices := make([]int, len(tokens))
	for i, tok := range tokens {
		indices[i] = strings.Index(scope, tok)
	}
	for i := 1; i < len(indices); i++ {
		if indices[i] >= 0 && indices[i-1] >= 0 && indices[i] < indices[i-1] {
			t.Errorf("rawPath chain order regressed: %q (idx=%d) appears before %q (idx=%d)",
				tokens[i], indices[i], tokens[i-1], indices[i-1])
		}
	}

	// The silent-fail path must surface a console.debug so a future
	// regression on the event shape is visible during dev/UAT instead of
	// presenting as a stale URL bar with no other symptom.
	if !strings.Contains(scope, "console.debug") {
		t.Errorf("URL listener missing console.debug on silent-fail path; scope:\n%s", scope)
	}

	// History rewrite must be guarded: if rawPath is missing or URL parse
	// fails, the listener has to bail out before history.replaceState runs
	// (otherwise stale params get pushed to the URL bar). The prior
	// implementation threw on the no-rawPath path and let the catch block
	// fall through into replaceState -- a bare "throw new Error(" must no
	// longer appear in this scope; the listener now uses early return.
	if strings.Contains(scope, "throw new Error(") {
		t.Errorf("URL listener still uses throw on the no-rawPath path; this lets history.replaceState run on stale params. Use early return. scope:\n%s", scope)
	}
}
