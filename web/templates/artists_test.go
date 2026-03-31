package templates

import (
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
)

func TestComputeArtistBadges(t *testing.T) {
	const artistID = "artist-1"

	tests := []struct {
		name      string
		a         artist.Artist
		sources   map[string]LibrarySourceInfo
		platforms map[string]artist.PlatformPresence
		want      []artistBadge
	}{
		{
			name:    "no badges when artist has no platform presence",
			a:       artist.Artist{ID: artistID},
			sources: nil,
			want:    nil,
		},
		{
			name: "filesystem badge when artist has a path",
			a:    artist.Artist{ID: artistID, Path: "/music/artist"},
			want: []artistBadge{
				{"filesystem", "Found in /music/artist"},
			},
		},
		{
			name: "lidarr badge from platform presence",
			a:    artist.Artist{ID: artistID},
			platforms: map[string]artist.PlatformPresence{
				artistID: {HasLidarr: true},
			},
			want: []artistBadge{
				{"lidarr", "Managed by Lidarr"},
			},
		},
		{
			name:    "lidarr badge from source with connection name",
			a:       artist.Artist{ID: artistID, LibraryID: "lib-1"},
			sources: map[string]LibrarySourceInfo{"lib-1": {Source: "lidarr", ConnectionName: "My Lidarr"}},
			want: []artistBadge{
				{"lidarr", "Managed by My Lidarr"},
			},
		},
		{
			name: "emby badge from platform presence",
			a:    artist.Artist{ID: artistID},
			platforms: map[string]artist.PlatformPresence{
				artistID: {HasEmby: true},
			},
			want: []artistBadge{
				{"emby", "Present in Emby"},
			},
		},
		{
			name:    "emby badge from source with connection name",
			a:       artist.Artist{ID: artistID, LibraryID: "lib-1"},
			sources: map[string]LibrarySourceInfo{"lib-1": {Source: "emby", ConnectionName: "My Emby"}},
			want: []artistBadge{
				{"emby", "Present in My Emby"},
			},
		},
		{
			name: "jellyfin badge from platform presence",
			a:    artist.Artist{ID: artistID},
			platforms: map[string]artist.PlatformPresence{
				artistID: {HasJellyfin: true},
			},
			want: []artistBadge{
				{"jellyfin", "Present in Jellyfin"},
			},
		},
		{
			name:    "jellyfin badge from source with connection name",
			a:       artist.Artist{ID: artistID, LibraryID: "lib-1"},
			sources: map[string]LibrarySourceInfo{"lib-1": {Source: "jellyfin", ConnectionName: "My Jellyfin"}},
			want: []artistBadge{
				{"jellyfin", "Present in My Jellyfin"},
			},
		},
		{
			name: "filesystem badge is first when combined with lidarr",
			a:    artist.Artist{ID: artistID, Path: "/music/artist"},
			platforms: map[string]artist.PlatformPresence{
				artistID: {HasLidarr: true},
			},
			want: []artistBadge{
				{"filesystem", "Found in /music/artist"},
				{"lidarr", "Managed by Lidarr"},
			},
		},
		{
			name: "badge order is filesystem lidarr emby jellyfin",
			a:    artist.Artist{ID: artistID, Path: "/music/artist"},
			platforms: map[string]artist.PlatformPresence{
				artistID: {HasLidarr: true, HasEmby: true, HasJellyfin: true},
			},
			want: []artistBadge{
				{"filesystem", "Found in /music/artist"},
				{"lidarr", "Managed by Lidarr"},
				{"emby", "Present in Emby"},
				{"jellyfin", "Present in Jellyfin"},
			},
		},
		{
			name: "filesystem path suppresses source-based lidarr tooltip",
			a:    artist.Artist{ID: artistID, Path: "/music/artist", LibraryID: "lib-1"},
			sources: map[string]LibrarySourceInfo{
				"lib-1": {Source: "lidarr", ConnectionName: "My Lidarr"},
			},
			want: []artistBadge{
				{"filesystem", "Found in /music/artist"},
			},
		},
		{
			name: "filesystem and lidarr presence both show badges",
			a:    artist.Artist{ID: artistID, Path: "/music/artist"},
			platforms: map[string]artist.PlatformPresence{
				artistID: {HasLidarr: true},
			},
			want: []artistBadge{
				{"filesystem", "Found in /music/artist"},
				{"lidarr", "Managed by Lidarr"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeArtistBadges(tt.a, tt.sources, tt.platforms)
			if len(got) != len(tt.want) {
				t.Fatalf("computeArtistBadges() returned %d badges, want %d: got %+v", len(got), len(tt.want), got)
			}
			for i, b := range got {
				if b != tt.want[i] {
					t.Errorf("badge[%d] = %+v, want %+v", i, b, tt.want[i])
				}
			}
		})
	}
}
