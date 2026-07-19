package templates

// duplicate_images_nav_test.go -- render-level coverage for the sidebar's
// "Images" section (#2608).
//
// Scope reminder, because it drives every assertion here: this fragment
// renders the WHOLE section -- header, Unmatched row and duplicate rows. A
// section without violations HIDES, so "renders nothing" is both the healthy
// clean state AND the hide behavior, and the header must never survive on its
// own.

import (
	"bytes"
	"strings"
	"testing"
)

func renderImagesNav(t *testing.T, v ImagesNavView) string {
	t.Helper()
	var buf bytes.Buffer
	if err := ImagesNav(v).Render(testCtx(t), &buf); err != nil {
		t.Fatalf("rendering images nav: %v", err)
	}
	return buf.String()
}

func populatedImagesNavView() ImagesNavView {
	return ImagesNavView{
		BasePath:       "",
		SectionLabel:   "Images",
		UnmatchedCount: 7,
		UnmatchedLabel: "Unmatched",
		UnmatchedAria:  "7 unrecognized images in your library",
		LibraryCount:   12,
		LibraryLabel:   "Library Duplicates",
		LibraryAria:    "12 duplicate backdrops in your libraries",
		Platforms: []ImagesNavPlatformRow{
			{Type: "emby", Label: "Emby Duplicates", Aria: "4 duplicate images on Emby", Count: 4},
			{Type: "jellyfin", Label: "Jellyfin Duplicates", Aria: "2 duplicate images on Jellyfin", Count: 2},
		},
	}
}

// THE HIDE BEHAVIOR (#2608, maintainer's spec). Every count zero -> the whole
// section is gone: no header, no rows, no wrapper. This is the case a
// "hide only the rows" regression would break, so assert the HEADER's absence
// explicitly rather than settling for "no rows".
func TestImagesNav_HidesEntireSectionWhenAllCountsZero(t *testing.T) {
	// Labels are populated: a regression must fail because of the COUNTS, not
	// because there was no text to render in the first place.
	v := ImagesNavView{
		SectionLabel:   "Images",
		UnmatchedLabel: "Unmatched",
		LibraryLabel:   "Library Duplicates",
	}

	if !v.Empty() {
		t.Error("Empty() false with every count zero")
	}

	got := renderImagesNav(t, v)
	if got != "" {
		t.Fatalf("all-zero state rendered %q, want empty (section must hide entirely)", got)
	}
	// Belt and braces: name the pieces that must not survive, so a future
	// change that renders a bare header or an empty wrapper fails HERE with a
	// legible message instead of only tripping the equality check above.
	for _, forbidden := range []string{
		"sw-sidebar-section-label",
		">Images<",
		"sw-sidebar-nav-list",
		"sidebar-foreign-next",
	} {
		if strings.Contains(got, forbidden) {
			t.Errorf("all-zero render contains %q; the section header/wrapper must not render", forbidden)
		}
	}
}

// Exactly one non-zero count -> the header comes back, with ONLY that row.
// Table-driven so each of the three rows is proven to independently resurrect
// the section and to independently stay hidden when it is the zero one.
func TestImagesNav_HeaderPlusOnlyTheNonZeroRow(t *testing.T) {
	tests := []struct {
		name string
		view ImagesNavView
		// want is the row id that must render; absent are the ones that must not.
		want   string
		absent []string
	}{
		{
			name: "only unmatched",
			view: ImagesNavView{
				SectionLabel: "Images", UnmatchedCount: 3,
				UnmatchedLabel: "Unmatched", UnmatchedAria: "3 unrecognized images in your library",
				LibraryLabel: "Library Duplicates",
			},
			want:   `id="sidebar-foreign-next"`,
			absent: []string{`id="sidebar-image-duplicates-library"`, `id="sidebar-image-duplicates-emby"`},
		},
		{
			name: "only library duplicates",
			view: ImagesNavView{
				SectionLabel: "Images", LibraryCount: 5,
				UnmatchedLabel: "Unmatched",
				LibraryLabel:   "Library Duplicates", LibraryAria: "5 duplicate backdrops in your libraries",
			},
			want:   `id="sidebar-image-duplicates-library"`,
			absent: []string{`id="sidebar-foreign-next"`, `id="sidebar-image-duplicates-emby"`},
		},
		{
			name: "only platform duplicates",
			view: ImagesNavView{
				SectionLabel: "Images", UnmatchedLabel: "Unmatched", LibraryLabel: "Library Duplicates",
				Platforms: []ImagesNavPlatformRow{
					{Type: "emby", Label: "Emby Duplicates", Aria: "4 duplicate images on Emby", Count: 4},
				},
			},
			want:   `id="sidebar-image-duplicates-emby"`,
			absent: []string{`id="sidebar-foreign-next"`, `id="sidebar-image-duplicates-library"`},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.view.Empty() {
				t.Fatal("Empty() true with a non-zero count")
			}
			html := renderImagesNav(t, tc.view)

			// The section header must be back.
			if !strings.Contains(html, `class="sw-sidebar-section-label"`) {
				t.Error("section header missing; a non-zero count must render the header")
			}
			if !strings.Contains(html, ">Images<") {
				t.Error(`section label "Images" missing`)
			}
			if !strings.Contains(html, tc.want) {
				t.Errorf("missing the non-zero row %s", tc.want)
			}
			for _, a := range tc.absent {
				if strings.Contains(html, a) {
					t.Errorf("zero-count row %s rendered; each row renders only when its own count > 0", a)
				}
			}
		})
	}
}

func TestImagesNav_AllRowsWhenPopulated(t *testing.T) {
	html := renderImagesNav(t, populatedImagesNavView())

	for _, want := range []string{
		// Section chrome.
		`class="sw-sidebar-section"`,
		`class="sw-sidebar-section-label"`,
		`>Images<`,
		// One <li> per offender, each id'd for targeting.
		`id="sidebar-foreign-next"`,
		`id="sidebar-image-duplicates-library"`,
		`id="sidebar-image-duplicates-emby"`,
		`id="sidebar-image-duplicates-jellyfin"`,
		// Sub-nav link class, matching the other sidebar children.
		`class="sw-sidebar-link sw-sidebar-subnav-link"`,
		// Destinations, unchanged from the links these rows replace.
		`href="/reports/foreign-files"`,
		`href="/reports/backdrop-duplicates"`,
		`href="/reports/platform-backdrop-duplicates"`,
		// Count pills. The unmatched pill also carries data-count, which is
		// what swImagesNavSwap reads to detect a rise.
		`data-count="7"`,
		`class="sw-sidebar-count-pill">12<`,
		`class="sw-sidebar-count-pill">4<`,
		`class="sw-sidebar-count-pill">2<`,
		// Terse visible labels, explicitly platform-named.
		`>Unmatched<`,
		`>Library Duplicates<`,
		`>Emby Duplicates<`,
		`>Jellyfin Duplicates<`,
		// Descriptive accessible names -- the count-bearing text lives HERE,
		// not in the visible label, so the label cannot truncate.
		`aria-label="7 unrecognized images in your library"`,
		`aria-label="12 duplicate backdrops in your libraries"`,
		`aria-label="4 duplicate images on Emby"`,
		`aria-label="2 duplicate images on Jellyfin"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("populated render missing %q", want)
		}
	}
}

// BasePath must prefix every href so a reverse-proxy subpath deployment does
// not emit links that 404.
func TestImagesNav_BasePathPrefixesEveryHref(t *testing.T) {
	v := populatedImagesNavView()
	v.BasePath = "/sw"

	html := renderImagesNav(t, v)
	for _, want := range []string{
		`href="/sw/reports/foreign-files"`,
		`href="/sw/reports/backdrop-duplicates"`,
		`href="/sw/reports/platform-backdrop-duplicates"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("missing base-path-prefixed href %q", want)
		}
	}
}

// A platform row renders from its own Count, so a platform type the code has
// never heard of still gets a correctly shaped row.
//
// "Correctly shaped" is the WHOLE contract on this path, so it is asserted in
// full rather than by id and label alone: an unknown platform must reach the
// same destination, carry its count in the pill, and expose the same
// descriptive accessible name as a known one. Checking only the id and the
// visible text would let the unknown-platform row lose its href or drop its
// count entirely and still pass -- and this is precisely the path with no
// hard-coded knowledge of the platform to fall back on.
//
// The known platforms (emby, jellyfin) get the identical treatment in
// TestImagesNav_AllRowsWhenPopulated, which already asserts their href, pill
// and aria-label.
func TestImagesNav_UnknownPlatformStillRenders(t *testing.T) {
	v := ImagesNavView{
		SectionLabel: "Images",
		Platforms: []ImagesNavPlatformRow{
			{Type: "plex", Label: "Plex Duplicates", Aria: "9 duplicate images on Plex", Count: 9},
		},
	}

	// The view renders this ONE row and nothing else, so a whole-document
	// match is unambiguously a match against the plex row.
	html := renderImagesNav(t, v)
	for _, tc := range []struct {
		want, why string
	}{
		{`id="sidebar-image-duplicates-plex"`, "unknown platform row did not render"},
		{`>Plex Duplicates<`, "unknown platform visible label did not render"},
		{`href="/reports/platform-backdrop-duplicates"`, "unknown platform row lost its destination; the row is a link to the platform report, not decoration"},
		{`class="sw-sidebar-count-pill">9<`, "unknown platform row did not render its count in the pill"},
		{`aria-label="9 duplicate images on Plex"`, "unknown platform row lost its accessible name; the count-bearing description lives here, not in the terse visible label"},
	} {
		if !strings.Contains(html, tc.want) {
			t.Errorf("%s (missing %q)\n%s", tc.why, tc.want, html)
		}
	}
}
