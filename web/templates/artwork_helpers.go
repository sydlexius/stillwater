package templates

// ArtworkKindType maps a UI artwork kind (the plain-language switcher label:
// primary, logo, banner, backdrops) to the API image-type segment used by
// /api/v1/artists/{id}/images/{type}/... It is the single source of truth for
// both the artist-detail artwork tiles and the Manage-artwork modal handler
// (#2214). Anything unrecognized (including empty) maps to "thumb", the primary
// slot.
//
// NOTE: tests/unit/artwork-modal.test.js mirrors this table in GO_KIND_TO_TYPE;
// keep both in sync when adding or changing cases.
func ArtworkKindType(kind string) string {
	switch kind {
	case "logo":
		return "logo"
	case "banner":
		return "banner"
	case "backdrops":
		return "fanart"
	default:
		return "thumb" // primary / unknown
	}
}
