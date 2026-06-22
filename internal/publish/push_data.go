package publish

import (
	"log/slog"
	"strings"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/connection/emby"
	"github.com/sydlexius/stillwater/internal/connection/jellyfin"
)

// BuildArtistPushData maps an Artist into the platform-agnostic push payload.
// Groups and orchestras get Formed and Disbanded; solo artists get Born and
// Died; unknown types get all four fields so the push code's Born > Formed
// fallback chain still works. The members slice is mapped into the
// platform-agnostic BandMembers field; pass nil when the caller has no
// member data to send (a nil/empty slice yields a nil BandMembers field on
// the payload, so the platform push code can branch cleanly).
//
// SortName fallback (#1083): when the artist has no SortName from
// MusicBrainz and the Name begins with an ASCII digit run, the push code
// derives a zero-padded sort key ("12 Stones" -> "0000000012 Stones") so
// numeric-prefix artists sort numerically rather than lexically in Emby
// and Jellyfin library views. The derivation also sets LockSortName, which
// the Emby push consumes to add "SortName" to the platform's LockedFields
// (Emby clears ForcedSortName on the next refresh otherwise -- see
// docs/architecture/emby-artist-metadata.md section 5). The Jellyfin push
// intentionally ignores LockSortName because Jellyfin's MetadataField enum
// has no "SortName" member; ForcedSortName persists on Jellyfin without a
// lock anyway. The flag is NOT set when SortName came from upstream so a
// user's manual unlock on the Emby side is preserved.
func BuildArtistPushData(a *artist.Artist, members []artist.BandMember) connection.ArtistPushData {
	derivedSort, locked := deriveSortNameFallback(a.Name, a.SortName)
	data := connection.ArtistPushData{
		Name:           a.Name,
		SortName:       derivedSort,
		LockSortName:   locked,
		Biography:      a.Biography,
		Genres:         a.Genres,
		Styles:         a.Styles,
		Moods:          a.Moods,
		Disambiguation: a.Disambiguation,
		YearsActive:    a.YearsActive,
		MusicBrainzID:  a.MusicBrainzID,
		AudioDBID:      a.AudioDBID,
		DiscogsID:      a.DiscogsID,
		SpotifyID:      a.SpotifyID,
		BandMembers:    buildMemberRefs(members),
	}
	switch a.Type {
	case "group", "orchestra", "choir":
		data.Formed = a.Formed
		data.Disbanded = a.Disbanded
	case "solo":
		data.Born = a.Born
		data.Died = a.Died
	default:
		data.Born = a.Born
		data.Formed = a.Formed
		data.Died = a.Died
		data.Disbanded = a.Disbanded
	}
	return data
}

// sortNamePadWidth is the zero-pad target width for the leading numeric
// run when Stillwater derives a SortName. 10 digits handles every plausible
// real-world numeric prefix (the largest is 4 digits, e.g. "1349"); a
// wider pad would still sort correctly but bloats the on-disk SortName
// field. Matches the convention used by Beets and MusicBrainz Picard for
// the same purpose.
const sortNamePadWidth = 10

// deriveSortNameFallback returns the SortName value to push to the
// platform and whether Stillwater derived it.
//
// Behavior matrix (see #1083 design checkpoint):
//   - mbSort is non-empty: pass through verbatim; derived=false. Honors
//     whatever upstream (MusicBrainz, manual edit) gave us.
//   - mbSort is empty AND name starts with an ASCII digit run: split the
//     leading digit run from the remainder, zero-pad the digit run to
//     sortNamePadWidth, concatenate. derived=true so the push code locks
//     SortName on the platform side.
//   - mbSort is empty AND name does NOT start with an ASCII digit run
//     (alphabetic prefix, leading symbol like "!!!", empty string, etc.):
//     pass through the empty string verbatim; derived=false. Behavior on
//     the platform side then diverges: Emby preserves any existing
//     ForcedSortName because emby/push.go's itemUpdateBody.ForcedSortName
//     has `omitempty` and an empty value is dropped from the body.
//     Jellyfin, by contrast, performs a fetch-and-merge full-replacement
//     POST that writes existing["ForcedSortName"] = "" unconditionally
//     (jellyfin/push.go), which clears any user-set ForcedSortName on the
//     platform side. This asymmetry is pre-existing Jellyfin push behavior
//     (not introduced by #1083) and is documented here so callers know not
//     to expect a no-op on Jellyfin in this branch.
//
// Only ASCII digits trigger the fallback. Unicode digits (Arabic-Indic,
// Devanagari, fullwidth) are intentionally out of scope: their
// platform-side sort behavior is undefined and Beets/Picard limit the
// same transform to ASCII digits. A follow-up issue can extend coverage
// once a real-world test artist surfaces.
func deriveSortNameFallback(name, mbSort string) (sortName string, derived bool) {
	if mbSort != "" {
		return mbSort, false
	}
	if name == "" {
		return "", false
	}
	// Walk the leading digit run by byte position; ASCII digits are
	// single-byte in UTF-8 so a byte-index split is safe and avoids
	// allocating a rune slice for the common non-numeric case.
	digitEnd := 0
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c < '0' || c > '9' {
			break
		}
		digitEnd++
	}
	if digitEnd == 0 {
		return "", false
	}
	digits := name[:digitEnd]
	remainder := name[digitEnd:]
	// strings.Repeat for the pad keeps the allocation visible and
	// avoids fmt.Sprintf("%010s", ...) which right-aligns with spaces
	// (not zeros) when the input is non-numeric -- a footgun if a future
	// caller passes a non-digit string by accident.
	if len(digits) < sortNamePadWidth {
		padded := strings.Repeat("0", sortNamePadWidth-len(digits)) + digits
		return padded + remainder, true
	}
	// Already at or above target width: no padding needed but still
	// flag as derived because we are still authoring SortName.
	return digits + remainder, true
}

// buildMemberRefs translates artist.BandMember entries into the
// platform-agnostic connection.ArtistPersonRef shape. The role string
// combines instruments and a vocal-type tag so platform-side mapping
// (Jellyfin's People[].Role) gets the most useful single-line summary.
// Returns nil for a nil or empty input so the caller can distinguish
// "members not provided" from "members provided but empty"; both are
// rendered as no-op by the platform push code.
func buildMemberRefs(members []artist.BandMember) []connection.ArtistPersonRef {
	if len(members) == 0 {
		return nil
	}
	out := make([]connection.ArtistPersonRef, 0, len(members))
	// Index-based loop with &members[i] avoids per-iteration struct copies.
	for i := range members {
		m := &members[i]
		if m.MemberName == "" {
			continue
		}
		out = append(out, connection.ArtistPersonRef{
			Name: m.MemberName,
			Role: memberRoleString(m),
		})
	}
	// Return nil rather than a non-nil empty slice when every input was
	// filtered out (e.g. all members had empty names). The Jellyfin push
	// path uses a non-nil BandMembers slice as the signal to overwrite
	// People, so collapsing all-filtered to nil preserves the no-clobber
	// invariant for downstream consumers.
	if len(out) == 0 {
		return nil
	}
	return out
}

// memberRoleString composes a short "Vocals (lead); Guitar, Bass"-style
// summary from a BandMember's instruments + vocal_type. Empty when neither
// field is set, in which case the Role JSON field is omitted via the
// `omitempty` tag on ArtistPersonRef.
func memberRoleString(m *artist.BandMember) string {
	parts := make([]string, 0, 2)
	if m.VocalType != "" {
		parts = append(parts, "Vocals ("+m.VocalType+")")
	}
	if len(m.Instruments) > 0 {
		parts = append(parts, strings.Join(m.Instruments, ", "))
	}
	return strings.Join(parts, "; ")
}

// NewMetadataPusher constructs a MetadataPusher for the given connection type.
// Returns (nil, false) for connection types that do not support metadata push
// (e.g. Lidarr).
func NewMetadataPusher(conn *connection.Connection, logger *slog.Logger) (connection.MetadataPusher, bool) {
	switch conn.Type {
	case connection.TypeEmby:
		return emby.New(conn.URL, conn.APIKey, conn.GetPlatformUserID(), logger), true
	case connection.TypeJellyfin:
		return jellyfin.New(conn.URL, conn.APIKey, conn.GetPlatformUserID(), logger), true
	default:
		return nil, false
	}
}
