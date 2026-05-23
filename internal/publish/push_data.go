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
func BuildArtistPushData(a *artist.Artist, members []artist.BandMember) connection.ArtistPushData {
	data := connection.ArtistPushData{
		Name:           a.Name,
		SortName:       a.SortName,
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
		return emby.New(conn.URL, conn.APIKey, conn.PlatformUserID, logger), true
	case connection.TypeJellyfin:
		return jellyfin.New(conn.URL, conn.APIKey, conn.PlatformUserID, logger), true
	default:
		return nil, false
	}
}
