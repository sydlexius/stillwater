package image

import (
	"net/url"
	"strings"
)

// googleImagesSlotFilter holds one slot's Google Images filter shape.
//
// imgar (aspect ratio: "s" square, "w" wide, "xw" panoramic) is a TOP-LEVEL
// query param, not a "tbs" token. This was gotten wrong in the first cut of
// #3223 (which put isz/iar together under "tbs"), verified only by asserting
// our own constructed string rather than Google's actual behavior -- so
// the aspect filter silently did nothing while the tests stayed green.
// Corrected via maintainer UAT + re-verification: loaded a bare udm=2
// search, used Google's own Advanced Search UI to pick each aspect ratio,
// and read back the param Google itself wrote into the address bar
// (imgar=s|w|xw, tbs+isz untouched). "isz:l" (size) and "ic:trans"
// (transparency) DO belong in "tbs" -- confirmed the same way.
//
// "logo" carries no size filter at all (maintainer correction): stacking
// isz:l on top of ic:trans over-constrained results.
var googleImagesSlotFilter = map[string]struct {
	imgar string // top-level "imgar" param; "" omits it
	tbs   string // "tbs" value, always includes "imgo:1"
}{
	"thumb":  {imgar: "s", tbs: "imgo:1,isz:l"},
	"fanart": {imgar: "w", tbs: "imgo:1,isz:l"},
	"logo":   {imgar: "", tbs: "imgo:1,ic:trans"},
	"banner": {imgar: "xw", tbs: "imgo:1,isz:l,ic:trans"},
}

// googleImagesQuerySuffixBySlot appends a slot-specific term to the bare
// artist name. The two logo-shaped slots (logo, banner) search for
// "<artist> logo"; the others search on the artist name alone.
var googleImagesQuerySuffixBySlot = map[string]string{
	"logo":   " logo",
	"banner": " logo",
}

// GoogleImagesSearchURL builds a per-slot deep link into Google's Images
// vertical, pre-filtered for the target slot's size/aspect-ratio/
// transparency needs (#3223). The operator's own browser performs the
// search and copies a result URL back into the existing fetch-from-URL flow
// (handleImageFetch, internal/api/handlers_image.go) -- no new fetch or
// save code is needed here.
//
// Ground truth: Bliss (blisshq.com), the product this apes, ships
// https://www.google.com/search?tbm=isch&q=<artist>&tbs=imgo:1 (extracted
// from a running instance's UI jar). tbm=isch is the older vertical
// selector; udm=2 is Google's current one -- both were rendered live and
// reached the same page, so udm=2 was kept as current.
//
// Returns "" when artistName is blank or slot is unrecognized (no filter
// mapping): a wrong filter is worse than no link, so callers should skip
// rendering the affordance when this returns "".
func GoogleImagesSearchURL(artistName, slot string) string {
	name := strings.TrimSpace(artistName)
	f, ok := googleImagesSlotFilter[slot]
	if name == "" || !ok {
		return ""
	}

	query := name + googleImagesQuerySuffixBySlot[slot]

	q := url.Values{}
	q.Set("udm", "2")
	q.Set("q", query)
	if f.imgar != "" {
		q.Set("imgar", f.imgar)
	}
	q.Set("tbs", f.tbs)

	return "https://www.google.com/search?" + q.Encode()
}
