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

// TestGoogleImagesSearchURL covers the four documented slot shapes (#3223's
// table) plus the AC's named encoding cases (&, spaces, non-ASCII) folded
// into the same table via the artist name. A wrong filter here silently
// misdirects the operator's search, so udm/q/tbs are each asserted.
func TestGoogleImagesSearchURL(t *testing.T) {
	tests := []struct {
		name, slot, wantQuery, wantFilter string
	}{
		{"Radiohead", "thumb", "Radiohead", "imgo:1,isz:l,iar:s"},
		{"Radiohead", "fanart", "Radiohead", "imgo:1,isz:l,iar:w"},
		{"Radiohead", "logo", "Radiohead logo", "imgo:1,isz:l,ic:trans"},
		{"Radiohead", "banner", "Radiohead logo", "imgo:1,isz:l,iar:xw,ic:trans"},
		// AC: "&" must survive encoding, not truncate the query at the next
		// "&"-delimited pair. Fails without url.Values.Encode() escaping it
		// (confirmed: naive "q="+name concatenation split this into two keys).
		{"All Sons & Daughters", "thumb", "All Sons & Daughters", "imgo:1,isz:l,iar:s"},
		{"for KING & COUNTRY", "logo", "for KING & COUNTRY logo", "imgo:1,isz:l,ic:trans"},
		// AC: spaces and non-ASCII must round-trip.
		{"Mötley Crüe", "thumb", "Mötley Crüe", "imgo:1,isz:l,iar:s"},
		{"坂本龍一", "banner", "坂本龍一 logo", "imgo:1,isz:l,iar:xw,ic:trans"},
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
			if q.Get("tbs") != tt.wantFilter {
				t.Errorf("tbs = %q, want %q", q.Get("tbs"), tt.wantFilter)
			}
		})
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
