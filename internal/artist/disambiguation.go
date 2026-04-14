package artist

import (
	"strings"

	"github.com/sydlexius/stillwater/internal/provider"
)

// SplitNameDisambiguation detects the on-disk ingest pattern
// "Canonical Name (disambiguation)" and promotes the parenthesised suffix
// into the dedicated Disambiguation field. MusicBrainz stores the two parts
// separately, but many users name directories by concatenating them with a
// parenthetical suffix. Without this split the application ends up with
// Name="Nirvana (Seattle grunge)", which duplicates the canonical
// disambiguation text and breaks name-based matching against providers.
//
// The promotion is a safe-default: it only fires when the provider metadata
// confirms the split. Specifically, if the provider's Name + " (" +
// Disambiguation + ")" reconstructs the current artist Name exactly, we
// know the concatenation is authoritative and can split without data loss.
// Any other shape leaves the artist untouched so ambiguous cases do not get
// silently rewritten.
//
// Returns true when the artist was modified.
func SplitNameDisambiguation(a *Artist, meta *provider.ArtistMetadata) bool {
	if a == nil || meta == nil {
		return false
	}
	if a.Disambiguation != "" {
		// Respect any disambiguation the artist already carries; do not
		// overwrite via concatenation-guessing.
		return false
	}
	if meta.Name == "" || meta.Disambiguation == "" {
		return false
	}
	combined := meta.Name + " (" + meta.Disambiguation + ")"
	if !strings.EqualFold(a.Name, combined) {
		return false
	}
	a.Name = meta.Name
	a.Disambiguation = meta.Disambiguation
	return true
}
