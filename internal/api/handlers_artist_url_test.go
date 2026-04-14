package api

import (
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/connection"
)

// TestBuildPlatformArtistURL_ServerIDPlumbing verifies that the Emby and
// Jellyfin deep-link builders append ?serverId= when the connection has a
// resolved server ID and omit it cleanly when the value is empty.
// Regression coverage for #1064: without serverId the Emby/Jellyfin web
// clients cannot navigate to the correct item and frequently show a
// generic home page or fail to load in multi-server setups.
func TestBuildPlatformArtistURL_ServerIDPlumbing(t *testing.T) {
	cases := []struct {
		name    string
		conn    *connection.Connection
		id      string
		wantSub []string
		notWant []string
	}{
		{
			name: "emby with server id",
			conn: &connection.Connection{
				Type:             connection.TypeEmby,
				URL:              "https://emby.example.com/",
				PlatformServerID: "abc123",
			},
			id:      "artist-42",
			wantSub: []string{"https://emby.example.com/web/index.html#!/item?id=artist-42", "&serverId=abc123"},
		},
		{
			name: "emby without server id",
			conn: &connection.Connection{
				Type: connection.TypeEmby,
				URL:  "https://emby.example.com",
			},
			id:      "artist-42",
			wantSub: []string{"https://emby.example.com/web/index.html#!/item?id=artist-42"},
			notWant: []string{"serverId"},
		},
		{
			name: "jellyfin with server id",
			conn: &connection.Connection{
				Type:             connection.TypeJellyfin,
				URL:              "https://jf.example.com/",
				PlatformServerID: "srv-xyz",
			},
			id:      "a-1",
			wantSub: []string{"https://jf.example.com/web/index.html#!/details?id=a-1", "&serverId=srv-xyz"},
		},
		{
			name: "jellyfin without server id",
			conn: &connection.Connection{
				Type: connection.TypeJellyfin,
				URL:  "https://jf.example.com",
			},
			id:      "a-1",
			wantSub: []string{"https://jf.example.com/web/index.html#!/details?id=a-1"},
			notWant: []string{"serverId"},
		},
		{
			name: "emby server id with special chars is query-escaped",
			conn: &connection.Connection{
				Type:             connection.TypeEmby,
				URL:              "https://emby.example.com",
				PlatformServerID: "id with spaces&+",
			},
			id:      "x",
			wantSub: []string{"serverId=id+with+spaces%26%2B"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildPlatformArtistURL(tc.conn, tc.id)
			for _, want := range tc.wantSub {
				if !strings.Contains(got, want) {
					t.Errorf("URL = %q, expected to contain %q", got, want)
				}
			}
			for _, nope := range tc.notWant {
				if strings.Contains(got, nope) {
					t.Errorf("URL = %q, expected NOT to contain %q", got, nope)
				}
			}
		})
	}
}
