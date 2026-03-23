package publish

import (
	"log/slog"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/connection/emby"
	"github.com/sydlexius/stillwater/internal/connection/jellyfin"
)

// BuildArtistPushData maps an Artist into the platform-agnostic push payload.
// Groups and orchestras get Formed and Disbanded; solo artists get Born and
// Died; unknown types get all four fields so the push code's Born > Formed
// fallback chain still works.
func BuildArtistPushData(a *artist.Artist) connection.ArtistPushData {
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
