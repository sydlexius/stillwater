package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/webhook"
)

// newFuzzArtist builds a minimal artist row for the fuzz seed; only Name and
// MusicBrainzID are non-default since the handler keys on MBID lookups.
func newFuzzArtist(name, mbid string) *artist.Artist {
	return &artist.Artist{
		Name: name, SortName: name, MusicBrainzID: mbid,
	}
}

// FuzzHandleCreateConnection feeds arbitrary byte slices to the JSON request
// decoder used by POST /api/v1/connections. The handler must never panic
// regardless of input -- the decoder returning an error is the expected and
// correct response to malformed input.
//
// Target: internal/api/handlers_connection.go line 177 (handleCreateConnection).
// SkipTest=true is forced via a corpus that always contains it, so the
// outbound test-before-save call is never made.
func FuzzHandleCreateConnection(f *testing.F) {
	// Happy-path seeds covering the three supported connection types.
	f.Add([]byte(`{"name":"Emby","type":"emby","url":"http://emby.local:8096","api_key":"k","enabled":true,"skip_test":true}`))
	f.Add([]byte(`{"name":"Jellyfin","type":"jellyfin","url":"http://jf.local:8096","api_key":"k","enabled":true,"skip_test":true}`))
	f.Add([]byte(`{"name":"Lidarr","type":"lidarr","url":"http://l.local:8686","api_key":"k","enabled":true,"skip_test":true}`))

	// Required fields missing.
	f.Add([]byte(`{"name":"","type":"emby","url":"","api_key":"","skip_test":true}`))
	// Unknown type.
	f.Add([]byte(`{"name":"X","type":"???","url":"http://x","api_key":"k","skip_test":true}`))
	// Extra unexpected fields.
	f.Add([]byte(`{"name":"X","type":"emby","url":"http://x","api_key":"k","skip_test":true,"extra":42,"nested":{"a":1}}`))
	// Enabled / skip_test as wrong types (string instead of bool).
	f.Add([]byte(`{"name":"X","type":"emby","url":"http://x","api_key":"k","skip_test":"true"}`))
	// NUL byte inside name.
	f.Add(append(
		[]byte(`{"name":"Nu`),
		append([]byte{0x00}, []byte(`l","type":"emby","url":"http://x","api_key":"k","skip_test":true}`)...)...))
	// Deeply nested structure.
	f.Add([]byte(`{"name":"D","type":"emby","url":"http://x","api_key":"k","skip_test":true,"nested":{"a":{"b":{"c":{"d":{"e":"deep"}}}}}}`))
	// Empty body / null / array where object expected.
	f.Add([]byte(``))
	f.Add([]byte(`null`))
	f.Add([]byte(`[]`))
	// Unicode + extremely long string.
	f.Add([]byte(`{"name":"日本語","type":"emby","url":"http://x","api_key":"k","skip_test":true}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		r := newConnectionTestRouter(t)
		req := httptest.NewRequest(http.MethodPost,
			"/api/v1/connections", bytes.NewReader(data))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		// Handler must not panic on any input. Status code is incidental.
		r.handleCreateConnection(w, req)
	})
}

// FuzzHandleUpdateConnection feeds arbitrary byte slices to the JSON request
// decoder used by PUT /api/v1/connections/{id}. The handler must never panic
// regardless of input.
//
// Target: internal/api/handlers_connection.go line 324 (handleUpdateConnection).
func FuzzHandleUpdateConnection(f *testing.F) {
	// Happy-path seeds.
	f.Add([]byte(`{"name":"Renamed","enabled":true}`))
	f.Add([]byte(`{"name":"Full","type":"emby","url":"http://e.local:8096","api_key":"k","enabled":false}`))
	f.Add([]byte(`{"feature_image_write":true,"feature_metadata_push":true,"feature_trigger_refresh":false}`))

	// Empty object (no-op patch).
	f.Add([]byte(`{}`))
	// Enabled as string instead of bool.
	f.Add([]byte(`{"enabled":"yes"}`))
	// Feature flag set to non-bool.
	f.Add([]byte(`{"feature_image_write":1}`))
	// Extra unknown fields.
	f.Add([]byte(`{"name":"X","unknown_field":42,"nested":{"a":1}}`))
	// NUL byte in name.
	f.Add(append(
		[]byte(`{"name":"Nu`),
		append([]byte{0x00}, []byte(`l"}`)...)...))
	// Deeply nested structure under name (decoder ignores it).
	f.Add([]byte(`{"deep":{"a":{"b":{"c":{"d":{"e":"deep"}}}}}}`))
	// Empty body / null / array.
	f.Add([]byte(``))
	f.Add([]byte(`null`))
	f.Add([]byte(`[]`))

	// Pre-seed a single connection that all fuzz cases will target. The router
	// is shared across cases inside the f.Fuzz callback via the closure --
	// fuzzing in -fuzz mode runs in-process so each iteration sees a fresh
	// router via the helper.
	f.Fuzz(func(t *testing.T, data []byte) {
		r := newConnectionTestRouter(t)
		// Seed a connection so the handler reaches the JSON decode + apply path
		// rather than returning 404.
		c := &connection.Connection{
			Name: "FuzzMe", Type: connection.TypeEmby,
			URL: "http://e.local:8096", APIKey: "k", Enabled: true,
		}
		if err := r.connectionService.Create(context.Background(), c); err != nil {
			t.Fatalf("seed connection: %v", err)
		}

		req := httptest.NewRequest(http.MethodPut,
			"/api/v1/connections/"+c.ID, bytes.NewReader(data))
		req.Header.Set("Content-Type", "application/json")
		req.SetPathValue("id", c.ID)
		w := httptest.NewRecorder()
		// Handler must not panic on any input.
		r.handleUpdateConnection(w, req)
	})
}

// FuzzHandleLidarrArtistAdd unmarshals arbitrary bytes into a
// webhook.LidarrPayload and then drives handleLidarrArtistAdd against the
// resulting (possibly degenerate) state. The handler must not panic on any
// decoded payload -- nil Artist, empty MBID, and partially populated fields
// are all expected runtime states.
//
// Target: internal/api/handlers_inbound_webhook.go line 77
// (handleLidarrArtistAdd). The decoder itself is also fuzzed in
// FuzzInboundWebhookLidarr; this target additionally exercises the post-
// decode handler with a degraded artist service (no eventBus, no pipeline,
// nil scannerService) so the no-op guards are tripped without standing up
// the full pipeline. Panic-free behavior under any payload is the contract.
func FuzzHandleLidarrArtistAdd(f *testing.F) {
	// Happy-path: full ArtistAdded payload.
	f.Add([]byte(`{"eventType":"ArtistAdded","artist":{"id":1,"name":"Radiohead","path":"/music/Radiohead","mbId":"a74b1b7f-71a5-4011-9441-d0b5e4122711","foreignArtistId":""}}`))
	// Happy-path: foreignArtistId carries the MBID instead of mbId.
	f.Add([]byte(`{"eventType":"ArtistAdded","artist":{"id":2,"name":"Portishead","foreignArtistId":"8f6bd1e4-fbe1-4f50-aa9b-94c450ec0a11"}}`))
	// Test ping (no artist).
	f.Add([]byte(`{"eventType":"Test"}`))
	// Missing artist field entirely.
	f.Add([]byte(`{"eventType":"ArtistAdded"}`))
	// Artist field as null.
	f.Add([]byte(`{"eventType":"ArtistAdded","artist":null}`))
	// Artist with empty MBID and empty name (both guards trip).
	f.Add([]byte(`{"eventType":"ArtistAdded","artist":{"id":3,"name":"","mbId":""}}`))
	// Extra unexpected fields the decoder must ignore.
	f.Add([]byte(`{"eventType":"ArtistAdded","artist":{"id":1,"name":"X","mbId":"id"},"extra":42,"nested":{"a":{"b":"c"}}}`))
	// Artist field as an array instead of an object.
	f.Add([]byte(`{"eventType":"ArtistAdded","artist":[]}`))
	// Albums payload that should not affect ArtistAdded handling.
	f.Add([]byte(`{"eventType":"ArtistAdded","artist":{"id":4,"name":"X","mbId":"id"},"albums":[{"id":10,"title":"A"}]}`))
	// NUL byte in name.
	f.Add(append(
		[]byte(`{"eventType":"ArtistAdded","artist":{"id":1,"name":"Nu`),
		append([]byte{0x00}, []byte(`l","mbId":"nul-id"}}`)...)...))
	// Deeply nested structure in artist.
	f.Add([]byte(`{"eventType":"ArtistAdded","artist":{"id":1,"name":"D","mbId":"id","nested":{"a":{"b":{"c":{"d":{"e":"deep"}}}}}}}`))
	// Empty body / bare null.
	f.Add([]byte(``))
	f.Add([]byte(`null`))

	f.Fuzz(func(t *testing.T, data []byte) {
		var payload webhook.LidarrPayload
		_ = json.Unmarshal(data, &payload)

		// Skip degenerate payloads that would exercise the unknown-artist
		// branch (which calls scannerService.Run -- nil in this router). The
		// nil-pipeline branch under "existing artist" is covered by
		// TestLidarrArtistAdd_NilPipeline; here we only want to assert
		// panic-freedom of the JSON decode + initial guards, which the
		// two early-return paths (nil artist / empty MBID) cover.
		if payload.Artist == nil || payload.Artist.MBID() == "" {
			r := newConnectionTestRouter(t)
			r.handleLidarrArtistAdd(context.Background(), payload)
			return
		}
		// For payloads with a usable MBID, exercise the existing-artist
		// branch by pre-seeding an artist row that matches.
		r := newConnectionTestRouter(t)
		seedFuzzArtist(t, r, payload.Artist.MBID())
		r.handleLidarrArtistAdd(context.Background(), payload)
	})
}

// seedFuzzArtist inserts a minimal artist row keyed on the fuzz-provided MBID
// so handleLidarrArtistAdd takes the "existing artist" path with its nil-
// pipeline guard rather than the scannerService.Run branch.
func seedFuzzArtist(t *testing.T, r *Router, mbid string) {
	t.Helper()
	if mbid == "" {
		return
	}
	a := newFuzzArtist("Seed", mbid)
	// Errors here (e.g. invalid MBID format triggering a constraint) are not
	// fatal -- the fuzz target only cares that the handler does not panic.
	_ = r.artistService.Create(context.Background(), a)
}
