package templates

import (
	"net/url"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
)

// TestArtworkManageEditor_GoogleImagesLink covers the template-level wiring
// that internal/image/googlesearch_test.go cannot: that the link actually
// renders in the editor, with the right target/rel/role attributes, that
// it's omitted for a blank artist name, and that the indexed backdrop-slot
// branch (fanartIdx >= 0) uses the same unscoped artist-name search as the
// generic fanart view. Per-slot query/filter correctness is covered at the
// GoogleImagesSearchURL unit level and is not re-asserted here.
func TestArtworkManageEditor_GoogleImagesLink(t *testing.T) {
	t.Parallel()

	// Also covers the AC's "&"-in-name encoding case (templ's HTML attribute
	// escaping of "&" as "&amp;" must round-trip via extractGoogleHref).
	t.Run("renders with target/rel/role, ampersand name survives escaping", func(t *testing.T) {
		t.Parallel()
		a := artist.Artist{ID: "art-amp", Name: "All Sons & Daughters", ThumbExists: true}
		out := renderEditor(t, ImageSearchData{Artist: a, SelectedType: "thumb", SelectedIndex: -1})

		tag := tagContaining(t, out, `href="https://www.google.com/search?`)
		for _, want := range []string{`target="_blank"`, `rel="noopener noreferrer"`, `role="menuitem"`} {
			if !strings.Contains(tag, want) {
				t.Errorf("Google Images link missing %s:\n%s", want, tag)
			}
		}
		if q := parseHrefQuery(t, extractGoogleHref(t, out)); q.Get("q") != "All Sons & Daughters" {
			t.Errorf("q = %q, want %q", q.Get("q"), "All Sons & Daughters")
		}
	})

	t.Run("omitted for a blank artist name", func(t *testing.T) {
		t.Parallel()
		a := artist.Artist{ID: "art-blank", ThumbExists: true}
		out := renderEditor(t, ImageSearchData{Artist: a, SelectedType: "thumb", SelectedIndex: -1})
		if strings.Contains(out, "Search with Google Images") {
			t.Error("rendered the affordance for a blank artist name; GoogleImagesSearchURL should return \"\" and suppress it")
		}
	})

	t.Run("indexed backdrop slot uses the unscoped fanart search", func(t *testing.T) {
		t.Parallel()
		a := artist.Artist{ID: "art-bd", Name: "Parity", FanartExists: true, FanartCount: 2}
		data := ImageSearchData{Artist: a, SelectedType: "fanart", SelectedIndex: 1} // fanartIdx >= 0
		out := renderEditor(t, data)

		href := extractGoogleHref(t, out)
		q := parseHrefQuery(t, href)
		if q.Get("q") != "Parity" {
			t.Errorf("q = %q, want %q", q.Get("q"), "Parity")
		}
		// imgar is a top-level param, not part of tbs (maintainer UAT
		// correction -- see internal/image/googlesearch.go).
		if q.Get("imgar") != "w" {
			t.Errorf("imgar = %q, want %q", q.Get("imgar"), "w")
		}
		if q.Get("tbs") != "imgo:1,isz:l" {
			t.Errorf("tbs = %q, want the fanart size filter", q.Get("tbs"))
		}

		// New behavior (maintainer round 2): clicking the link ALSO
		// pre-opens the fetch-from-URL dialog targeted at THIS slot index.
		tag := tagContaining(t, out, `href="https://www.google.com/search?`)
		if !strings.Contains(tag, "window.swOpenFetchUrlForSlot(1)") {
			t.Errorf("Google Images link for indexed slot 1 does not pre-target swOpenFetchUrlForSlot(1):\n%s", tag)
		}
		if !strings.Contains(tag, "typeof window.swOpenFetchUrlForSlot === 'function'") || !strings.Contains(tag, "console.error") {
			t.Errorf("Google Images link's auto-open guard must follow the no-silent-failure pattern (typeof check + console.error):\n%s", tag)
		}
	})

	t.Run("unscoped slot pre-opens the fetch-from-URL modal on click", func(t *testing.T) {
		t.Parallel()
		out := renderEditor(t, editorData("logo"))
		tag := tagContaining(t, out, `href="https://www.google.com/search?`)
		if !strings.Contains(tag, "window.swOpenFetchUrlModal()") {
			t.Errorf("unscoped Google Images link does not pre-open swOpenFetchUrlModal():\n%s", tag)
		}
		if !strings.Contains(tag, "typeof window.swOpenFetchUrlModal === 'function'") || !strings.Contains(tag, "console.error") {
			t.Errorf("unscoped Google Images link's auto-open guard must follow the no-silent-failure pattern:\n%s", tag)
		}
	})
}

// extractGoogleHref finds the Google Images link's href in rendered HTML,
// undoing templ's "&" -> "&amp;" attribute escaping (values here never
// contain "<", ">", or quotes, so this narrow unescape is sufficient).
func extractGoogleHref(t *testing.T, html string) string {
	t.Helper()
	marker := `href="https://www.google.com/search?`
	at := strings.Index(html, marker)
	if at < 0 {
		t.Fatalf("no Google Images href found in rendered output")
	}
	rest := html[at+len(`href="`):]
	end := strings.Index(rest, `"`)
	if end < 0 {
		t.Fatalf("unterminated href attribute in rendered output")
	}
	return strings.ReplaceAll(rest[:end], "&amp;", "&")
}

func parseHrefQuery(t *testing.T, href string) url.Values {
	t.Helper()
	u, err := url.Parse(href)
	if err != nil {
		t.Fatalf("parsing href %q: %v", href, err)
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		t.Fatalf("parsing query %q: %v", u.RawQuery, err)
	}
	return q
}
