// Package tagdict provides canonical spelling normalization for genre/style/mood tags.
// Tags from different providers use inconsistent spellings; this package maps common
// variants to their canonical forms and deduplicates across providers.
package tagdict

import "strings"

// canonical maps normalized tag variants to their preferred spelling.
// Keys must be lowercase with whitespace collapsed.
// Seeded from AllMusic's taxonomy (most curated source), supplemented
// by MusicBrainz genre list and Last.fm top tags.
var canonical = map[string]string{
	// Synth/Electronic variants
	"synthpop":                "Synth-Pop",
	"synth pop":               "Synth-Pop",
	"synth-pop":               "Synth-Pop",
	"electropop":              "Electropop",
	"electronica":             "Electronica",
	"electronic":              "Electronic",
	"idm":                     "IDM",
	"intelligent dance music": "IDM",

	// Lo-Fi variants
	"lofi":          "Lo-Fi",
	"lo fi":         "Lo-Fi",
	"lo-fi":         "Lo-Fi",
	"lo-fi hip hop": "Lo-Fi Hip-Hop",
	"lofi hip hop":  "Lo-Fi Hip-Hop",

	// Hip-Hop/Rap
	"hip hop":          "Hip-Hop",
	"hip-hop":          "Hip-Hop",
	"hiphop":           "Hip-Hop",
	"rap":              "Rap",
	"trap":             "Trap",
	"r&b":              "R&B",
	"r & b":            "R&B",
	"rnb":              "R&B",
	"rhythm and blues": "R&B",

	// Rock subgenres
	"alt rock":         "Alternative Rock",
	"alt-rock":         "Alternative Rock",
	"alternative rock": "Alternative Rock",
	"indie rock":       "Indie Rock",
	"indie pop":        "Indie Pop",
	"post-rock":        "Post-Rock",
	"post rock":        "Post-Rock",
	"postrock":         "Post-Rock",
	"math rock":        "Math Rock",
	"math-rock":        "Math Rock",
	"prog rock":        "Progressive Rock",
	"progressive rock": "Progressive Rock",
	"psychedelic rock": "Psychedelic Rock",
	"psych rock":       "Psychedelic Rock",
	"shoegaze":         "Shoegaze",
	"dream pop":        "Dream Pop",
	"dream-pop":        "Dream Pop",
	"noise rock":       "Noise Rock",
	"noise-rock":       "Noise Rock",
	"new wave":         "New Wave",
	"post-punk":        "Post-Punk",
	"post punk":        "Post-Punk",
	"punk rock":        "Punk Rock",
	"garage rock":      "Garage Rock",
	"garage-rock":      "Garage Rock",

	// Metal
	"heavy metal":     "Heavy Metal",
	"death metal":     "Death Metal",
	"black metal":     "Black Metal",
	"doom metal":      "Doom Metal",
	"thrash metal":    "Thrash Metal",
	"power metal":     "Power Metal",
	"folk metal":      "Folk Metal",
	"symphonic metal": "Symphonic Metal",

	// Country/Folk/Americana
	"country":           "Country",
	"alt country":       "Alt-Country",
	"alt-country":       "Alt-Country",
	"americana":         "Americana",
	"folk":              "Folk",
	"bluegrass":         "Bluegrass",
	"roots rock":        "Roots Rock",
	"singer-songwriter": "Singer-Songwriter",
	"singer songwriter": "Singer-Songwriter",

	// Jazz/Blues/Soul
	"jazz":     "Jazz",
	"blues":    "Blues",
	"soul":     "Soul",
	"funk":     "Funk",
	"gospel":   "Gospel",
	"neo soul": "Neo-Soul",
	"neo-soul": "Neo-Soul",

	// Classical/Orchestral
	"classical":              "Classical",
	"orchestral":             "Orchestral",
	"chamber music":          "Chamber Music",
	"contemporary classical": "Contemporary Classical",

	// World
	"world music": "World Music",
	"latin":       "Latin",
	"bossa nova":  "Bossa Nova",
	"reggae":      "Reggae",
	"reggaeton":   "Reggaeton",
	"afrobeat":    "Afrobeat",

	// Mood-related canonical forms
	"melancholic":   "Melancholic",
	"melancholy":    "Melancholic",
	"bittersweet":   "Bittersweet",
	"chill":         "Chill",
	"chillout":      "Chill",
	"mellow":        "Mellow",
	"dark":          "Dark",
	"atmospheric":   "Atmospheric",
	"dreamy":        "Dreamy",
	"energetic":     "Energetic",
	"uplifting":     "Uplifting",
	"aggressive":    "Aggressive",
	"intense":       "Intense",
	"romantic":      "Romantic",
	"nostalgic":     "Nostalgic",
	"introspective": "Introspective",
	"ethereal":      "Ethereal",

	// K-pop / J-pop (preserve stylization)
	"k-pop":    "K-Pop",
	"kpop":     "K-Pop",
	"j-pop":    "J-Pop",
	"jpop":     "J-Pop",
	"j-rock":   "J-Rock",
	"jrock":    "J-Rock",
	"city pop": "City Pop",
}

// normalizeKey returns a lowercase, whitespace-collapsed form of tag for map lookup.
// Underscores are treated as word separators so "synth_pop" matches "synth pop".
func normalizeKey(tag string) string {
	tag = strings.ReplaceAll(strings.ToLower(tag), "_", " ")
	return strings.Join(strings.Fields(tag), " ")
}

// Canonical returns the preferred spelling for a tag. If no canonical form is
// known, the original tag is returned unchanged.
func Canonical(tag string) string {
	if c, ok := canonical[normalizeKey(tag)]; ok {
		return c
	}
	return tag
}

// MergeAndDeduplicate appends incoming tags to existing, normalizing to canonical
// spelling and deduplicating. First-seen ordering is preserved. Lookup is
// case-insensitive via normalizeKey.
func MergeAndDeduplicate(existing, incoming []string) []string {
	seen := make(map[string]struct{}, len(existing)+len(incoming))
	result := make([]string, 0, len(existing)+len(incoming))

	add := func(tag string) {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			return
		}
		c := Canonical(tag)
		key := normalizeKey(c)
		if _, dup := seen[key]; !dup {
			seen[key] = struct{}{}
			result = append(result, c)
		}
	}

	for _, t := range existing {
		add(t)
	}
	for _, t := range incoming {
		add(t)
	}
	return result
}
