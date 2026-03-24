// Package tagclass provides shared tag classification for metadata providers.
//
// Last.fm, MusicBrainz, and other sources return free-form tags that mix
// genres, sub-genre styles, moods, and non-musical descriptors. This package
// classifies each tag into one of those buckets so that the scraper can
// populate the correct metadata fields.
package tagclass

import "strings"

// TagClass represents the classification of a tag.
type TagClass int

const (
	// TagClassGenre is the default bucket for unrecognized tags.
	TagClassGenre TagClass = iota
	// TagClassStyle indicates a sub-genre or approach descriptor.
	TagClassStyle
	// TagClassMood indicates an emotional or atmospheric descriptor.
	TagClassMood
	// TagClassIgnore indicates a non-musical descriptor (e.g., "seen live").
	TagClassIgnore
)

// Classify returns the classification for a single tag (case-insensitive).
// Unknown tags default to TagClassGenre.
func Classify(tag string) TagClass {
	normalized := strings.ToLower(strings.TrimSpace(tag))
	if normalized == "" {
		return TagClassIgnore
	}
	if class, ok := classificationMap[normalized]; ok {
		return class
	}
	return TagClassGenre
}

// ClassifyTags splits tags into genres, styles, and moods.
// Tags classified as TagClassIgnore are dropped. Unclassified tags default
// to genres.
func ClassifyTags(tags []string) (genres, styles, moods []string) {
	for _, tag := range tags {
		trimmed := strings.TrimSpace(tag)
		if trimmed == "" {
			continue
		}
		switch Classify(trimmed) {
		case TagClassGenre:
			genres = append(genres, trimmed)
		case TagClassStyle:
			styles = append(styles, trimmed)
		case TagClassMood:
			moods = append(moods, trimmed)
		case TagClassIgnore:
			// Drop non-musical descriptors.
		}
	}
	return genres, styles, moods
}

// classificationMap holds the known tag-to-class mappings. Keys are
// lowercase. The map is intentionally large to cover common Last.fm,
// MusicBrainz, and Discogs tags.
var classificationMap = map[string]TagClass{
	// -- Moods --
	"melancholic":   TagClassMood,
	"melancholy":    TagClassMood,
	"chill":         TagClassMood,
	"energetic":     TagClassMood,
	"atmospheric":   TagClassMood,
	"dreamy":        TagClassMood,
	"aggressive":    TagClassMood,
	"uplifting":     TagClassMood,
	"dark":          TagClassMood,
	"happy":         TagClassMood,
	"sad":           TagClassMood,
	"intense":       TagClassMood,
	"mellow":        TagClassMood,
	"romantic":      TagClassMood,
	"haunting":      TagClassMood,
	"euphoric":      TagClassMood,
	"brooding":      TagClassMood,
	"nostalgic":     TagClassMood,
	"anxious":       TagClassMood,
	"peaceful":      TagClassMood,
	"angry":         TagClassMood,
	"bittersweet":   TagClassMood,
	"sombre":        TagClassMood,
	"somber":        TagClassMood,
	"eerie":         TagClassMood,
	"serene":        TagClassMood,
	"playful":       TagClassMood,
	"gloomy":        TagClassMood,
	"triumphant":    TagClassMood,
	"tender":        TagClassMood,
	"ethereal":      TagClassMood,
	"introspective": TagClassMood,
	"moody":         TagClassMood,
	"sensual":       TagClassMood,
	"rebellious":    TagClassMood,
	"epic":          TagClassMood,
	"hypnotic":      TagClassMood,
	"relaxing":      TagClassMood,
	"soothing":      TagClassMood,
	"groovy":        TagClassMood,
	"sexy":          TagClassMood,
	"party":         TagClassMood,
	"feel good":     TagClassMood,
	"summer":        TagClassMood,

	// -- Styles --
	"progressive rock":        TagClassStyle,
	"prog rock":               TagClassStyle,
	"shoegaze":                TagClassStyle,
	"trip-hop":                TagClassStyle,
	"trip hop":                TagClassStyle,
	"lo-fi":                   TagClassStyle,
	"lo fi":                   TagClassStyle,
	"lofi":                    TagClassStyle,
	"dream pop":               TagClassStyle,
	"post-punk":               TagClassStyle,
	"post punk":               TagClassStyle,
	"art rock":                TagClassStyle,
	"noise rock":              TagClassStyle,
	"math rock":               TagClassStyle,
	"krautrock":               TagClassStyle,
	"slowcore":                TagClassStyle,
	"sadcore":                 TagClassStyle,
	"chamber pop":             TagClassStyle,
	"baroque pop":             TagClassStyle,
	"neo-psychedelia":         TagClassStyle,
	"space rock":              TagClassStyle,
	"stoner rock":             TagClassStyle,
	"stoner metal":            TagClassStyle,
	"doom metal":              TagClassStyle,
	"black metal":             TagClassStyle,
	"death metal":             TagClassStyle,
	"thrash metal":            TagClassStyle,
	"power metal":             TagClassStyle,
	"symphonic metal":         TagClassStyle,
	"nu metal":                TagClassStyle,
	"nu-metal":                TagClassStyle,
	"metalcore":               TagClassStyle,
	"post-metal":              TagClassStyle,
	"post metal":              TagClassStyle,
	"sludge metal":            TagClassStyle,
	"acid house":              TagClassStyle,
	"deep house":              TagClassStyle,
	"tech house":              TagClassStyle,
	"progressive house":       TagClassStyle,
	"minimal techno":          TagClassStyle,
	"acid techno":             TagClassStyle,
	"detroit techno":          TagClassStyle,
	"idm":                     TagClassStyle,
	"intelligent dance music": TagClassStyle,
	"glitch":                  TagClassStyle,
	"vaporwave":               TagClassStyle,
	"synthwave":               TagClassStyle,
	"chillwave":               TagClassStyle,
	"witch house":             TagClassStyle,
	"dark ambient":            TagClassStyle,
	"drone":                   TagClassStyle,
	"noise":                   TagClassStyle,
	"power pop":               TagClassStyle,
	"jangle pop":              TagClassStyle,
	"twee pop":                TagClassStyle,
	"sunshine pop":            TagClassStyle,
	"bubblegum pop":           TagClassStyle,
	"electropop":              TagClassStyle,
	"synth-pop":               TagClassStyle,
	"synth pop":               TagClassStyle,
	"synthpop":                TagClassStyle,
	"new wave":                TagClassStyle,
	"no wave":                 TagClassStyle,
	"post-rock":               TagClassStyle,
	"post rock":               TagClassStyle,
	"emo":                     TagClassStyle,
	"screamo":                 TagClassStyle,
	"hardcore":                TagClassStyle,
	"post-hardcore":           TagClassStyle,
	"post hardcore":           TagClassStyle,
	"midwest emo":             TagClassStyle,
	"skramz":                  TagClassStyle,
	"britpop":                 TagClassStyle,
	"madchester":              TagClassStyle,
	"acid jazz":               TagClassStyle,
	"smooth jazz":             TagClassStyle,
	"free jazz":               TagClassStyle,
	"bebop":                   TagClassStyle,
	"hard bop":                TagClassStyle,
	"cool jazz":               TagClassStyle,
	"fusion":                  TagClassStyle,
	"nu jazz":                 TagClassStyle,
	"delta blues":             TagClassStyle,
	"chicago blues":           TagClassStyle,
	"electric blues":          TagClassStyle,
	"country blues":           TagClassStyle,
	"outlaw country":          TagClassStyle,
	"alt-country":             TagClassStyle,
	"alt country":             TagClassStyle,
	"americana":               TagClassStyle,
	"roots rock":              TagClassStyle,
	"garage rock":             TagClassStyle,
	"surf rock":               TagClassStyle,
	"psychedelic rock":        TagClassStyle,
	"grunge":                  TagClassStyle,
	"neo-soul":                TagClassStyle,
	"neo soul":                TagClassStyle,
	"trap":                    TagClassStyle,
	"grime":                   TagClassStyle,
	"dubstep":                 TagClassStyle,
	"drum and bass":           TagClassStyle,
	"drum n bass":             TagClassStyle,
	"dnb":                     TagClassStyle,
	"jungle":                  TagClassStyle,
	"breakbeat":               TagClassStyle,
	"downtempo":               TagClassStyle,
	"ambient":                 TagClassStyle,
	"new age":                 TagClassStyle,
	"world music":             TagClassStyle,
	"afrobeat":                TagClassStyle,
	"bossa nova":              TagClassStyle,
	"flamenco":                TagClassStyle,
	"reggaeton":               TagClassStyle,
	"dancehall":               TagClassStyle,
	"industrial":              TagClassStyle,
	"gothic rock":             TagClassStyle,
	"goth":                    TagClassStyle,
	"darkwave":                TagClassStyle,
	"coldwave":                TagClassStyle,
	"minimal wave":            TagClassStyle,
	"deathcore":               TagClassStyle,
	"melodic death metal":     TagClassStyle,
	"technical death metal":   TagClassStyle,
	"progressive metal":       TagClassStyle,
	"djent":                   TagClassStyle,
	"groove metal":            TagClassStyle,
	"speed metal":             TagClassStyle,
	"folk metal":              TagClassStyle,
	"viking metal":            TagClassStyle,
	"black metal atmospheric": TagClassStyle,
	"depressive black metal":  TagClassStyle,
	"blackgaze":               TagClassStyle,
	"crossover thrash":        TagClassStyle,
	"crust punk":              TagClassStyle,
	"d-beat":                  TagClassStyle,
	"ska punk":                TagClassStyle,
	"folk punk":               TagClassStyle,
	"pop punk":                TagClassStyle,
	"skate punk":              TagClassStyle,
	"melodic hardcore":        TagClassStyle,
	"street punk":             TagClassStyle,
	"horror punk":             TagClassStyle,
	"psychobilly":             TagClassStyle,
	"rockabilly":              TagClassStyle,
	"swing":                   TagClassStyle,
	"big band":                TagClassStyle,
	"dixieland":               TagClassStyle,
	"ragtime":                 TagClassStyle,
	"boogie-woogie":           TagClassStyle,
	"doo-wop":                 TagClassStyle,
	"motown":                  TagClassStyle,
	"philly soul":             TagClassStyle,
	"northern soul":           TagClassStyle,
	"southern soul":           TagClassStyle,
	"deep soul":               TagClassStyle,
	"blue-eyed soul":          TagClassStyle,
	"quiet storm":             TagClassStyle,
	"contemporary r&b":        TagClassStyle,
	"new jack swing":          TagClassStyle,
	"g-funk":                  TagClassStyle,
	"gangsta rap":             TagClassStyle,
	"conscious rap":           TagClassStyle,
	"boom bap":                TagClassStyle,
	"east coast hip hop":      TagClassStyle,
	"west coast hip hop":      TagClassStyle,
	"southern hip hop":        TagClassStyle,
	"crunk":                   TagClassStyle,
	"mumble rap":              TagClassStyle,
	"cloud rap":               TagClassStyle,
	"lo-fi hip hop":           TagClassStyle,
	"abstract hip hop":        TagClassStyle,
	"turntablism":             TagClassStyle,
	"uk garage":               TagClassStyle,
	"2-step":                  TagClassStyle,
	"speed garage":            TagClassStyle,
	"future garage":           TagClassStyle,
	"footwork":                TagClassStyle,
	"juke":                    TagClassStyle,
	"hardstyle":               TagClassStyle,
	"gabber":                  TagClassStyle,
	"happy hardcore":          TagClassStyle,
	"eurodance":               TagClassStyle,
	"italo disco":             TagClassStyle,
	"hi-nrg":                  TagClassStyle,
	"freestyle":               TagClassStyle,
	"balearic":                TagClassStyle,
	"tropical house":          TagClassStyle,
	"future bass":             TagClassStyle,
	"future house":            TagClassStyle,
	"bass music":              TagClassStyle,
	"wonky":                   TagClassStyle,
	"uk funky":                TagClassStyle,
	"broken beat":             TagClassStyle,
	"nu-disco":                TagClassStyle,
	"nu disco":                TagClassStyle,
	"indie pop":               TagClassStyle,
	"indie rock":              TagClassStyle,
	"c86":                     TagClassStyle,
	"noise pop":               TagClassStyle,

	// -- Ignore (non-musical descriptors) --
	"seen live":            TagClassIgnore,
	"favorites":            TagClassIgnore,
	"favourite":            TagClassIgnore, //nolint:misspell // British English Last.fm tag
	"favourites":           TagClassIgnore, //nolint:misspell // British English Last.fm tag
	"love":                 TagClassIgnore,
	"awesome":              TagClassIgnore,
	"favourite songs":      TagClassIgnore, //nolint:misspell // British English Last.fm tag
	"favorite songs":       TagClassIgnore,
	"beautiful":            TagClassIgnore,
	"my collection":        TagClassIgnore,
	"check out":            TagClassIgnore,
	"spotify":              TagClassIgnore,
	"under 2000 listeners": TagClassIgnore,
	"cool":                 TagClassIgnore,
	"amazing":              TagClassIgnore,
	"best":                 TagClassIgnore,
	"good":                 TagClassIgnore,
	"great":                TagClassIgnore,
	"wish list":            TagClassIgnore,
	"to listen":            TagClassIgnore,
}
