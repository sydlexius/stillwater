package image

import (
	"net/url"
	"strings"
)

// googleImagesFilterBySlot maps each slot to its "tbs" filter suffix,
// appended after the "imgo:1" base filter every slot shares (restricts
// results to genuine image hits, lifted from Bliss -- see the doc comment
// below). "logo" carries no aspect filter: logos are rarely square, and
// stacking one on top of ic:trans would filter out most real results, so
// transparency alone is the binding constraint there (#3223).
var googleImagesFilterBySlot = map[string]string{
	"thumb":  "isz:l,iar:s",
	"fanart": "isz:l,iar:w",
	"logo":   "isz:l,ic:trans",
	"banner": "isz:l,iar:xw,ic:trans",
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
// selector; udm=2 is Google's current one. Both were rendered live during
// #3223 implementation and reached the same filtered Images grid -- udm=2
// was chosen as current, with tbm=isch as the documented fallback.
//
// Returns "" when artistName is blank or slot is unrecognized (no filter
// mapping): a wrong filter is worse than no link, so callers should skip
// rendering the affordance when this returns "".
func GoogleImagesSearchURL(artistName, slot string) string {
	name := strings.TrimSpace(artistName)
	filter, ok := googleImagesFilterBySlot[slot]
	if name == "" || !ok {
		return ""
	}

	query := name + googleImagesQuerySuffixBySlot[slot]

	q := url.Values{}
	q.Set("udm", "2")
	q.Set("q", query)
	q.Set("tbs", "imgo:1,"+filter)

	return "https://www.google.com/search?" + q.Encode()
}
