package api

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"math"
	"testing"

	"github.com/sydlexius/stillwater/internal/webhook"
)

// FuzzInboundWebhookLidarr feeds arbitrary byte slices to the Lidarr inbound
// webhook JSON decoder. The decoder must never panic regardless of input --
// returning an error for invalid JSON is expected and correct.
//
// Target: internal/api/handlers_inbound_webhook.go line 23
// (json.NewDecoder(req.Body).Decode into webhook.LidarrPayload)
func FuzzInboundWebhookLidarr(f *testing.F) {
	// Happy-path: representative ArtistAdded payload.
	f.Add([]byte(`{"eventType":"ArtistAdded","artist":{"id":1,"name":"Radiohead","path":"/music/Radiohead","mbId":"a74b1b7f-71a5-4011-9441-d0b5e4122711","foreignArtistId":""}}`))

	// Happy-path: Test ping.
	f.Add([]byte(`{"eventType":"Test"}`))

	// Happy-path: Download event with album list.
	f.Add([]byte(`{"eventType":"Download","artist":{"id":2,"name":"Portishead","path":"/music/Portishead","mbId":"8f6bd1e4-fbe1-4f50-aa9b-94c450ec0a11","foreignArtistId":""},"albums":[{"id":10,"title":"Dummy","foreignAlbumId":"abc"}]}`))

	// Extra fields that the decoder should ignore.
	f.Add([]byte(`{"eventType":"ArtistAdded","artist":{"id":1,"name":"Test","mbId":"00000000-0000-0000-0000-000000000001"},"unexpectedField":"value","anotherExtra":42}`))

	// Missing required fields -- eventType absent.
	f.Add([]byte(`{"artist":{"id":1,"name":"No EventType"}}`))

	// Arrays where objects are expected (artist as array).
	f.Add([]byte(`{"eventType":"ArtistAdded","artist":[{"id":1}]}`))

	// Albums field as an object instead of array.
	f.Add([]byte(`{"eventType":"Download","albums":{"id":10,"title":"Not an array"}}`))

	// Deeply nested structure.
	f.Add([]byte(`{"eventType":"ArtistAdded","artist":{"id":999,"name":"Deep","mbId":"deep","nested":{"a":{"b":{"c":{"d":{"e":"deep"}}}}}}}`))

	// MinInt64 in an integer field.
	f.Add([]byte(`{"eventType":"Grab","artist":{"id":-9223372036854775808,"name":"Timestamp","mbId":"ts"}}`))

	// NUL bytes embedded in string values (JSON decoder must handle these gracefully).
	f.Add(append(
		[]byte(`{"eventType":"ArtistAdded","artist":{"id":1,"name":"Null`),
		append([]byte{0x00}, []byte(`Byte","mbId":"nul-id"}}`)...)...))

	// Empty body.
	f.Add([]byte(``))

	// Bare null.
	f.Add([]byte(`null`))

	// Gzip-compressed body (decoder receives raw bytes; should fail gracefully).
	f.Add(gzipWebhookBytes(f, []byte(`{"eventType":"Test"}`)))

	// Large string to exercise allocator paths.
	bigName := make([]byte, 0, 64+1024*1024)
	bigName = append(bigName, []byte(`{"eventType":"ArtistAdded","artist":{"id":1,"name":"`)...)
	for i := 0; i < 1024*1024; i++ {
		bigName = append(bigName, 'A')
	}
	bigName = append(bigName, []byte(`","mbId":"big"}}`)...)
	f.Add(bigName)

	f.Fuzz(func(t *testing.T, data []byte) {
		// The decoder must not panic. Errors for invalid input are expected.
		var payload webhook.LidarrPayload
		_ = json.Unmarshal(data, &payload)
	})
}

// FuzzInboundWebhookGeneric feeds arbitrary byte slices to the Emby inbound
// webhook JSON decoder. The decoder must never panic regardless of input --
// returning an error for invalid JSON is expected and correct.
//
// Target: internal/api/handlers_inbound_webhook.go line 196
// (json.NewDecoder(req.Body).Decode into webhook.EmbyPayload)
func FuzzInboundWebhookGeneric(f *testing.F) {
	// Happy-path: system test notification.
	f.Add([]byte(`{"Event":"system.notificationtest"}`))

	// Happy-path: ItemAdded with full MusicAlbum item.
	f.Add([]byte(`{"Event":"library.new","Item":{"Id":"abc123","Name":"Dummy","Type":"MusicAlbum","ProviderIds":{"MusicBrainzAlbumArtist":"8f6bd1e4-fbe1-4f50-aa9b-94c450ec0a11"},"Path":"/music/Portishead/Dummy","ArtistItems":[{"Id":"x1","Name":"Portishead"}]}}`))

	// Happy-path: library changed.
	f.Add([]byte(`{"Event":"library.changed"}`))

	// Extra fields.
	f.Add([]byte(`{"Event":"item.updated","Item":{"Id":"z1","Name":"Album","Type":"MusicAlbum","ProviderIds":{}},"ExtraField":"ignored"}`))

	// Missing Event field.
	f.Add([]byte(`{"Item":{"Id":"1","Name":"No Event"}}`))

	// Item as array instead of object.
	f.Add([]byte(`{"Event":"library.new","Item":[{"Id":"x"}]}`))

	// ProviderIds with nested map value of unexpected type.
	f.Add([]byte(`{"Event":"library.new","Item":{"Id":"1","ProviderIds":{"MusicBrainzAlbumArtist":42}}}`))

	// ArtistItems as object instead of array.
	f.Add([]byte(`{"Event":"library.new","Item":{"Id":"1","ArtistItems":{"Id":"x","Name":"Artist"}}}`))

	// Deeply nested structure.
	f.Add([]byte(`{"Event":"library.new","Item":{"Id":"1","Name":"Deep","ProviderIds":{"MusicBrainzAlbumArtist":"mbid"},"ArtistItems":[{"Id":"a","Name":"A","Extra":{"B":{"C":{"D":"deep"}}}}]}}`))

	// MinInt64 in a numeric-looking string field (ProviderIds value).
	f.Add([]byte(`{"Event":"library.new","Item":{"Id":"-9223372036854775808","Name":"Ts","Type":"MusicAlbum","ProviderIds":{"MusicBrainzAlbumArtist":"-9223372036854775808"}}}`))

	// Multiple slash-separated MBIDs (multi-artist album).
	f.Add([]byte(`{"Event":"library.new","Item":{"Id":"1","Name":"Collab","Type":"MusicAlbum","ProviderIds":{"MusicBrainzAlbumArtist":"aaaa/bbbb/cccc"}}}`))

	// NUL byte in string values.
	f.Add(append(
		[]byte(`{"Event":"library.new","Item":{"Id":"1","Name":"Nul`),
		append([]byte{0x00}, []byte(`","Type":"MusicAlbum","ProviderIds":{}}}`)...)...))

	// Empty body.
	f.Add([]byte(``))

	// Bare null.
	f.Add([]byte(`null`))

	// Gzip-compressed body.
	f.Add(gzipWebhookBytes(f, []byte(`{"Event":"system.notificationtest"}`)))

	f.Fuzz(func(t *testing.T, data []byte) {
		var payload webhook.EmbyPayload
		_ = json.Unmarshal(data, &payload)
		// Exercise the ArtistMBIDs helper too -- it must not panic on any decoded state.
		if payload.Item != nil {
			_ = payload.Item.ArtistMBIDs()
		}
	})
}

// FuzzInboundWebhookJellyfin feeds arbitrary byte slices to the Jellyfin inbound
// webhook JSON decoder. The decoder must never panic regardless of input --
// returning an error for invalid JSON is expected and correct.
//
// Target: internal/api/handlers_inbound_webhook.go line 327
// (json.NewDecoder(req.Body).Decode into webhook.JellyfinPayload)
func FuzzInboundWebhookJellyfin(f *testing.F) {
	// Happy-path: Test notification.
	f.Add([]byte(`{"NotificationType":"Test"}`))

	// Happy-path: ItemAdded with MusicAlbum and MBID.
	f.Add([]byte(`{"NotificationType":"ItemAdded","ItemId":"item001","ItemType":"MusicAlbum","Name":"Dummy","Artist":"Portishead","Provider_musicbrainzalbumartist":"8f6bd1e4-fbe1-4f50-aa9b-94c450ec0a11"}`))

	// Happy-path: LibraryChanged.
	f.Add([]byte(`{"NotificationType":"LibraryChanged"}`))

	// Extra unexpected fields.
	f.Add([]byte(`{"NotificationType":"ItemAdded","ItemId":"x","ExtraField":"ignored","AnotherExtra":{"nested":true}}`))

	// Missing NotificationType.
	f.Add([]byte(`{"ItemId":"x","ItemType":"MusicAlbum","Name":"No Type"}`))

	// NotificationType as integer instead of string.
	f.Add([]byte(`{"NotificationType":42,"ItemId":"x"}`))

	// NotificationType as array.
	f.Add([]byte(`{"NotificationType":["ItemAdded"],"ItemId":"x"}`))

	// All string fields set to minimum int64 value as a string.
	f.Add([]byte(`{"NotificationType":"ItemAdded","ItemId":"-9223372036854775808","ItemType":"MusicAlbum","Name":"MinInt","Artist":"","Provider_musicbrainzalbumartist":"-9223372036854775808"}`))

	// MinInt64 formatted as a JSON number in a field that expects a string.
	minInt64Bytes, _ := json.Marshal(math.MinInt64)
	minSeed := append(append([]byte(`{"NotificationType":"ItemAdded","ItemId":`), minInt64Bytes...), '}')
	f.Add(minSeed)

	// NUL byte in string values.
	f.Add(append(
		[]byte(`{"NotificationType":"ItemAdded","Name":"Nul`),
		append([]byte{0x00}, []byte(`Byte","Provider_musicbrainzalbumartist":"mb-id"}`)...)...))

	// Deeply nested extra fields.
	f.Add([]byte(`{"NotificationType":"Test","Extra":{"A":{"B":{"C":{"D":{"E":"deep"}}}}}}`))

	// Empty body.
	f.Add([]byte(``))

	// Bare null.
	f.Add([]byte(`null`))

	// Gzip-compressed body.
	f.Add(gzipWebhookBytes(f, []byte(`{"NotificationType":"Test"}`)))

	f.Fuzz(func(t *testing.T, data []byte) {
		var payload webhook.JellyfinPayload
		_ = json.Unmarshal(data, &payload)
		// Exercise the MBID helper -- it must not panic on any decoded state.
		_ = payload.MBID()
	})
}

// gzipWebhookBytes compresses data and returns the gzip-encoded result. Used
// to seed fuzz corpora with compressed bodies, which the JSON decoder rejects
// gracefully rather than panicking.
func gzipWebhookBytes(f interface {
	Helper()
	Fatalf(string, ...any)
}, data []byte) []byte {
	f.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(data); err != nil {
		f.Fatalf("gzip write: %v", err)
	}
	if err := w.Close(); err != nil {
		f.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}
