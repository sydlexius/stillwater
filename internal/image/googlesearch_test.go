package image

import (
	"net/url"
	"testing"
)

// queryOf decodes the query string of a GoogleImagesSearchURL result.
// Comparing decoded values (not raw escaping) is the right level: Go's
// url.Values.Encode() and Google's server both accept %2C/%3A-escaped "tbs"
// interchangeably (verified live during #3223 implementation).
func queryOf(t *testing.T, rawURL string) url.Values {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("unparsable URL %q: %v", rawURL, err)
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		t.Fatalf("unparsable query %q: %v", u.RawQuery, err)
	}
	return q
}

// TestGoogleImagesSearchURL covers the four documented slot shapes plus the
// AC's named encoding cases (&, spaces, non-ASCII) folded into the same
// table via the artist name. A wrong filter here silently misdirects the
// operator's search, so udm/q/imgar/tbs are each asserted.
//
// Aspect ratio (imgar) is a TOP-LEVEL param, not part of "tbs" -- corrected
// via maintainer UAT after the first cut asserted our own constructed
// string instead of Google's real behavior and shipped a dead filter. See
// googlesearch.go's doc comment for how this was re-verified against
// Google's own Advanced Search UI. "logo" carries no size filter at all
// (also a maintainer correction).
func TestGoogleImagesSearchURL(t *testing.T) {
	tests := []struct {
		name, slot, wantQuery, wantImgar, wantTbs string
	}{
		{"Radiohead", "thumb", "Radiohead", "s", "imgo:1,isz:l"},
		{"Radiohead", "fanart", "Radiohead", "w", "imgo:1,isz:l"},
		{"Radiohead", "logo", "Radiohead logo", "", "imgo:1,ic:trans"},
		{"Radiohead", "banner", "Radiohead logo", "xw", "imgo:1,isz:l,ic:trans"},
		// AC: "&" must survive encoding, not truncate the query at the next
		// "&"-delimited pair. Fails without url.Values.Encode() escaping it
		// (confirmed: naive "q="+name concatenation split this into two keys).
		{"All Sons & Daughters", "thumb", "All Sons & Daughters", "s", "imgo:1,isz:l"},
		{"for KING & COUNTRY", "logo", "for KING & COUNTRY logo", "", "imgo:1,ic:trans"},
		// AC: spaces and non-ASCII must round-trip.
		{"Mötley Crüe", "thumb", "Mötley Crüe", "s", "imgo:1,isz:l"},
		{"坂本龍一", "banner", "坂本龍一 logo", "xw", "imgo:1,isz:l,ic:trans"},
	}

	for _, tt := range tests {
		t.Run(tt.slot+"/"+tt.name, func(t *testing.T) {
			got := GoogleImagesSearchURL(tt.name, tt.slot)
			q := queryOf(t, got)
			if q.Get("udm") != "2" {
				t.Errorf("udm = %q, want 2", q.Get("udm"))
			}
			if q.Get("q") != tt.wantQuery {
				t.Errorf("q = %q, want %q", q.Get("q"), tt.wantQuery)
			}
			if got, want := q.Get("imgar"), tt.wantImgar; got != want {
				t.Errorf("imgar = %q, want %q", got, want)
			}
			if q.Has("imgar") && tt.wantImgar == "" {
				t.Errorf("imgar param present but slot %q wants no aspect filter", tt.slot)
			}
			if q.Get("tbs") != tt.wantTbs {
				t.Errorf("tbs = %q, want %q", q.Get("tbs"), tt.wantTbs)
			}
		})
	}
}

// TestGoogleImagesSearchURL_LogoOmitsImgarEntirely guards against a
// regression that sets imgar="" rather than omitting the param outright --
// url.Values.Get returns "" for both cases, so the table test above cannot
// distinguish "param absent" from "param present but empty". Checked via
// the raw query string instead.
func TestGoogleImagesSearchURL_LogoOmitsImgarEntirely(t *testing.T) {
	got := GoogleImagesSearchURL("Radiohead", "logo")
	if u, err := url.Parse(got); err != nil {
		t.Fatalf("unparsable URL %q: %v", got, err)
	} else if u.Query().Has("imgar") {
		t.Errorf("logo URL %q carries an imgar param; logo must omit it entirely", got)
	}
}

// TestGoogleImagesSearchURL_FailsClosed covers the two cases that must
// return "" rather than a useless or wrongly-filtered link: a blank artist
// name, and a slot outside the fixed filter table -- notably "backdrops",
// the indexed multi-slot kind GoogleImagesSearchURL deliberately does not
// know about (see image_search.templ's caller for how that's handled).
func TestGoogleImagesSearchURL_FailsClosed(t *testing.T) {
	for _, name := range []string{"", "   ", "\t\n"} {
		if got := GoogleImagesSearchURL(name, "thumb"); got != "" {
			t.Errorf("empty name %q: got %q, want \"\"", name, got)
		}
	}
	for _, slot := range []string{"", "backdrops", "poster"} {
		if got := GoogleImagesSearchURL("Radiohead", slot); got != "" {
			t.Errorf("slot %q: got %q, want \"\"", slot, got)
		}
	}
}
