package emby

import (
	"testing"

	"github.com/sydlexius/stillwater/internal/connection"
)

// TestBuildProviderIDs covers the #1084 mapping from ArtistPushData external
// IDs into Emby's canonical ProviderIds dictionary keys. Empty IDs must be
// omitted so a missing-in-Stillwater value never clears a Jellyfin/Emby-side
// value via "".
func TestBuildProviderIDs(t *testing.T) {
	t.Run("all four IDs map to canonical Emby keys", func(t *testing.T) {
		got := buildProviderIDs(connection.ArtistPushData{
			MusicBrainzID: "mb-1",
			AudioDBID:     "adb-2",
			DiscogsID:     "dsc-3",
			SpotifyID:     "spo-4",
		})
		want := map[string]string{
			"MusicBrainzArtist": "mb-1",
			"TheAudioDb":        "adb-2",
			"Discogs":           "dsc-3",
			"Spotify":           "spo-4",
		}
		if len(got) != len(want) {
			t.Fatalf("expected %d keys, got %d: %+v", len(want), len(got), got)
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("key %q: want %q, got %q", k, v, got[k])
			}
		}
	})

	t.Run("partial input only emits non-empty IDs", func(t *testing.T) {
		got := buildProviderIDs(connection.ArtistPushData{
			MusicBrainzID: "mb-1",
			SpotifyID:     "spo-4",
		})
		if len(got) != 2 {
			t.Fatalf("expected 2 keys, got %d: %+v", len(got), got)
		}
		if got["MusicBrainzArtist"] != "mb-1" {
			t.Errorf("MusicBrainzArtist missing or wrong: %+v", got)
		}
		if got["Spotify"] != "spo-4" {
			t.Errorf("Spotify missing or wrong: %+v", got)
		}
		if _, present := got["TheAudioDb"]; present {
			t.Errorf("TheAudioDb must be omitted when AudioDBID is empty: %+v", got)
		}
		if _, present := got["Discogs"]; present {
			t.Errorf("Discogs must be omitted when DiscogsID is empty: %+v", got)
		}
	})

	t.Run("empty input yields empty map", func(t *testing.T) {
		got := buildProviderIDs(connection.ArtistPushData{})
		if len(got) != 0 {
			t.Errorf("expected empty map, got %+v", got)
		}
	})
}
