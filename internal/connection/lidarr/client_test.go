package lidarr

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestTestConnection_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/system/status" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("X-Api-Key") != "test-key" {
			t.Errorf("missing or wrong auth header: %s", r.Header.Get("X-Api-Key"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"2.0.0","appName":"Lidarr"}`))
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "test-key", srv.Client(), testLogger())
	if err := c.TestConnection(context.Background()); err != nil {
		t.Fatalf("TestConnection failed: %v", err)
	}
}

func TestTestConnection_Unauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "bad-key", srv.Client(), testLogger())
	if err := c.TestConnection(context.Background()); err == nil {
		t.Fatal("expected error for unauthorized")
	}
}

func TestGetArtists(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"id":1,"artistName":"Radiohead","foreignArtistId":"mbid-001","path":"/music/Radiohead","monitored":true,"metadataProfileId":1},
			{"id":2,"artistName":"Bjork","foreignArtistId":"mbid-002","path":"/music/Bjork","monitored":true,"metadataProfileId":1}
		]`))
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", srv.Client(), testLogger())
	artists, err := c.GetArtists(context.Background())
	if err != nil {
		t.Fatalf("GetArtists failed: %v", err)
	}
	if len(artists) != 2 {
		t.Fatalf("got %d artists, want 2", len(artists))
	}
	if artists[0].ForeignArtistID != "mbid-001" {
		t.Errorf("MBID = %q, want mbid-001", artists[0].ForeignArtistID)
	}
}

func TestGetMetadataProfiles(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":1,"name":"Standard"},{"id":2,"name":"Minimal"}]`))
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", srv.Client(), testLogger())
	profiles, err := c.GetMetadataProfiles(context.Background())
	if err != nil {
		t.Fatalf("GetMetadataProfiles failed: %v", err)
	}
	if len(profiles) != 2 {
		t.Fatalf("got %d profiles, want 2", len(profiles))
	}
}

// CheckNFOWriterEnabled now queries /api/v1/metadata (the NFO/image
// consumer endpoint) rather than /api/v1/config/metadataprovider (the
// audio-tag writer). Tests assert the new contract: Fields[artistMetadata]
// = true on an enabled consumer triggers the conflict.

func TestCheckNFOWriterEnabled_True(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/metadata" {
			t.Errorf("path = %s, want /api/v1/metadata", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"id":1,"name":"Kodi (XBMC) / Emby","enable":true,"fields":[
				{"name":"artistMetadata","value":true},
				{"name":"artistImages","value":false}
			]}
		]`))
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", srv.Client(), testLogger())
	enabled, name, err := c.CheckNFOWriterEnabled(context.Background())
	if err != nil {
		t.Fatalf("CheckNFOWriterEnabled failed: %v", err)
	}
	if !enabled {
		t.Error("expected NFO writer to be enabled")
	}
	if name != "Kodi (XBMC) / Emby" {
		t.Errorf("name = %q, want Kodi (XBMC) / Emby", name)
	}
}

func TestCheckNFOWriterEnabled_FieldFalseMeansNoConflict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"id":1,"name":"Kodi","enable":true,"fields":[
				{"name":"artistMetadata","value":false},
				{"name":"artistImages","value":false}
			]}
		]`))
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", srv.Client(), testLogger())
	enabled, _, err := c.CheckNFOWriterEnabled(context.Background())
	if err != nil {
		t.Fatalf("CheckNFOWriterEnabled failed: %v", err)
	}
	if enabled {
		t.Error("expected NFO writer to be disabled when artistMetadata=false")
	}
}

func TestCheckNFOWriterEnabled_False(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Master enable=false overrides whatever the fields say.
		_, _ = w.Write([]byte(`[
			{"id":1,"name":"Kodi","enable":false,"fields":[
				{"name":"artistMetadata","value":true}
			]}
		]`))
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", srv.Client(), testLogger())
	enabled, _, err := c.CheckNFOWriterEnabled(context.Background())
	if err != nil {
		t.Fatalf("CheckNFOWriterEnabled failed: %v", err)
	}
	if enabled {
		t.Error("expected NFO writer to be disabled when consumer is disabled")
	}
}

func TestTriggerArtistRefresh(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/command" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		var cmd CommandBody
		if err := json.NewDecoder(r.Body).Decode(&cmd); err != nil {
			t.Fatalf("decoding body: %v", err)
		}
		if cmd.Name != "RefreshArtist" {
			t.Errorf("command name = %q, want RefreshArtist", cmd.Name)
		}
		if cmd.ArtistID != 42 {
			t.Errorf("artistId = %d, want 42", cmd.ArtistID)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":1,"name":"RefreshArtist","status":"queued"}`))
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", srv.Client(), testLogger())
	resp, err := c.TriggerArtistRefresh(context.Background(), 42)
	if err != nil {
		t.Fatalf("TriggerArtistRefresh failed: %v", err)
	}
	if resp.Status != "queued" {
		t.Errorf("status = %q, want queued", resp.Status)
	}
}

func TestGetMetadataConsumers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/config/metadataprovider" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"id":1,"metadataType":"MediaBrowser","consumerId":1,"consumerName":"MediaBrowser","enable":true},
			{"id":2,"metadataType":"Kodi (XBMC) / Emby","consumerId":2,"consumerName":"Kodi (XBMC) / Emby","enable":false}
		]`))
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", srv.Client(), testLogger())
	consumers, err := c.GetMetadataConsumers(context.Background())
	if err != nil {
		t.Fatalf("GetMetadataConsumers: %v", err)
	}
	if len(consumers) != 2 {
		t.Fatalf("got %d consumers, want 2", len(consumers))
	}
	if consumers[0].ConsumerName != "MediaBrowser" {
		t.Errorf("first consumer = %q, want MediaBrowser", consumers[0].ConsumerName)
	}
	if !consumers[0].Enabled {
		t.Error("expected first consumer to be enabled")
	}
	if consumers[1].Enabled {
		t.Error("expected second consumer to be disabled")
	}
}

func TestGetMetadataConsumers_SingleObject(t *testing.T) {
	// Newer Lidarr versions return a single object instead of an array
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/api/v1/config/metadataprovider" {
			t.Errorf("path = %s, want /api/v1/config/metadataprovider", r.URL.Path)
		}
		if r.Header.Get("X-Api-Key") != "key" {
			t.Errorf("X-Api-Key = %q, want key", r.Header.Get("X-Api-Key"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":1,"metadataType":"Kodi (XBMC) / Emby","consumerId":1,"consumerName":"Kodi (XBMC) / Emby","enable":true}`))
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", srv.Client(), testLogger())
	consumers, err := c.GetMetadataConsumers(context.Background())
	if err != nil {
		t.Fatalf("GetMetadataConsumers (single object): %v", err)
	}
	if len(consumers) != 1 {
		t.Fatalf("got %d consumers, want 1", len(consumers))
	}
	if consumers[0].ConsumerName != "Kodi (XBMC) / Emby" {
		t.Errorf("consumer = %q, want Kodi (XBMC) / Emby", consumers[0].ConsumerName)
	}
	if !consumers[0].Enabled {
		t.Error("expected consumer to be enabled")
	}
}

func TestDisableMetadataConsumer(t *testing.T) {
	bodyCh := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/config/metadataprovider/2" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != http.MethodPut {
			t.Errorf("method = %s, want PUT", r.Method)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading body: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		bodyCh <- body
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", srv.Client(), testLogger())
	if err := c.DisableMetadataConsumer(context.Background(), 2); err != nil {
		t.Fatalf("DisableMetadataConsumer: %v", err)
	}

	receivedBody := <-bodyCh
	var cfg MetadataProviderConfig
	if err := json.Unmarshal(receivedBody, &cfg); err != nil {
		t.Fatalf("parsing sent body: %v", err)
	}
	if cfg.ID != 2 {
		t.Errorf("ID = %d, want 2", cfg.ID)
	}
	if cfg.Enable {
		t.Error("expected Enable to be false")
	}
}

// TestUpdateArtistPath_RoundTrip exercises the GET-modify-PUT cycle used by
// publish.Publisher.SyncRename (#1231). Lidarr's PUT must include the
// moveFiles=false query parameter so the peer treats the call as "the
// files already moved, just record the new path" rather than trying to
// move them itself (which would race the on-disk rename Stillwater just
// performed). The test asserts both the path overwrite and the query
// parameter on the outbound request.
func TestUpdateArtistPath_RoundTrip(t *testing.T) {
	bodyCh := make(chan map[string]any, 1)
	queryCh := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			// Existing artist with extra fields (qualityProfileId, monitored,
			// statistics) so the test can assert preservation through the
			// raw-map round-trip.
			_, _ = w.Write([]byte(`{
				"id": 42,
				"artistName": "Pink Floyd",
				"path": "/old/Pink Floyd",
				"foreignArtistId": "abc-mbid",
				"qualityProfileId": 1,
				"metadataProfileId": 1,
				"monitored": true,
				"statistics": {"trackCount": 100}
			}`))
		case http.MethodPut:
			queryCh <- r.URL.RawQuery
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			bodyCh <- body
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", srv.Client(), testLogger())
	if err := c.UpdateArtistPath(context.Background(), "42", "/new/Pink Floyd"); err != nil {
		t.Fatalf("UpdateArtistPath: %v", err)
	}

	q := <-queryCh
	if q != "moveFiles=false" {
		t.Errorf("query = %q, want moveFiles=false (rename already happened on disk)", q)
	}
	got := <-bodyCh
	if got["path"] != "/new/Pink Floyd" {
		t.Errorf("path = %v, want /new/Pink Floyd", got["path"])
	}
	if got["artistName"] != "Pink Floyd" {
		t.Errorf("artistName preservation failed: %v", got["artistName"])
	}
	// Unknown / unmodeled fields (statistics) must survive: the typed
	// Artist struct in types.go does not include them, so the raw-map
	// round-trip is the only thing keeping them intact.
	if _, present := got["statistics"]; !present {
		t.Errorf("statistics field lost; raw-map round-trip should preserve unknown fields")
	}
}

// TestUpdateArtistPath_PeerError verifies that a non-2xx PUT surfaces a
// wrapped error including the operation label. Mirrors the Emby test so a
// regression in either client's error wrap is caught by the matching
// publish.SyncRename per-platform Error assertions.
func TestUpdateArtistPath_PeerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":42,"artistName":"X","path":"/old/X"}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", srv.Client(), testLogger())
	err := c.UpdateArtistPath(context.Background(), "42", "/new/X")
	if err == nil {
		t.Fatal("expected error on 500 response")
	}
}

// TestUpdateArtistPath_GetError covers the fetch-step failure path: when
// the initial GET returns 500, UpdateArtistPath must wrap with the
// "fetching artist for path update" prefix and never issue the PUT.
func TestUpdateArtistPath_GetError(t *testing.T) {
	var putCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		putCount++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", srv.Client(), testLogger())
	err := c.UpdateArtistPath(context.Background(), "42", "/new")
	if err == nil {
		t.Fatal("expected error on 500 GET")
	}
	if putCount != 0 {
		t.Errorf("PUT issued %d times despite GET failure; should fail fast", putCount)
	}
}

// TestUpdateArtistPath_EmptyBody guards against an empty / null Lidarr GET
// response. The map-based round-trip would otherwise PUT {"path":"/new"}
// with all the other required ArtistResource fields missing, which Lidarr
// rejects in confusing ways. Fast-fail at the boundary is clearer.
func TestUpdateArtistPath_EmptyBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// "null" decodes to a nil map[string]any
		_, _ = w.Write([]byte(`null`))
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", srv.Client(), testLogger())
	err := c.UpdateArtistPath(context.Background(), "42", "/new/X")
	if err == nil {
		t.Fatal("expected error on empty body")
	}
}

// TestUpdateArtistPath_EmptyNewPath rejects a blank target path before any
// GET / PUT so we never silently overwrite the Lidarr artist row with "".
func TestUpdateArtistPath_EmptyNewPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "key", srv.Client(), testLogger())
	if err := c.UpdateArtistPath(context.Background(), "42", ""); err == nil {
		t.Fatal("expected error on empty newPath")
	}
}

// TestUpdateArtistPath_AuthClass401 verifies that a 401 response on the
// PUT half wraps with the ErrAuthRequired sentinel (per issue #1639), so the
// publish layer can detect auth failures via errors.Is.
func TestUpdateArtistPath_AuthClass401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":42,"path":"/old/X"}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "k", srv.Client(), testLogger())
	err := c.UpdateArtistPath(context.Background(), "42", "/new/X")
	if err == nil {
		t.Fatal("expected error on 401")
	}
	if !errors.Is(err, ErrAuthRequired) {
		t.Errorf("errors.Is(err, ErrAuthRequired) = false; want true. err = %v", err)
	}
}

// TestUpdateArtistPath_AuthClass403 mirrors AuthClass401 for the 403 branch.
func TestUpdateArtistPath_AuthClass403(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":42,"path":"/old/X"}`))
			return
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "k", srv.Client(), testLogger())
	err := c.UpdateArtistPath(context.Background(), "42", "/new/X")
	if err == nil {
		t.Fatal("expected error on 403")
	}
	if !errors.Is(err, ErrAuthRequired) {
		t.Errorf("errors.Is(err, ErrAuthRequired) = false; want true. err = %v", err)
	}
}

// TestUpdateArtistPath_NonAuthErrorNotWrapped guards the negative branch
// so the publish layer routes 5xx away from the re-auth toast class.
func TestUpdateArtistPath_NonAuthErrorNotWrapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":42,"path":"/old/X"}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "k", srv.Client(), testLogger())
	err := c.UpdateArtistPath(context.Background(), "42", "/new/X")
	if err == nil {
		t.Fatal("expected error on 500")
	}
	if errors.Is(err, ErrAuthRequired) {
		t.Errorf("errors.Is(err, ErrAuthRequired) = true on 500; want false")
	}
}

// TestUpdateArtistPath_IssuesSingleGET pins the per-rename request shape:
// exactly one GET (the pre-PUT fetch) plus the PUT, for every client. Lidarr
// used to carry an opt-in follow-up GET that re-read the artist to confirm the
// path round-tripped; #2419 removed it as redundant, because the publish layer
// now read-backs every peer unconditionally (publish.Publisher.verifyPeerPath).
// A second GET observed here means an in-client read-back has returned.
func TestUpdateArtistPath_IssuesSingleGET(t *testing.T) {
	// atomic.Int32 because the counter is written from the httptest
	// handler goroutine and read from the test goroutine; a plain int
	// trips -race under the project's race-test rule.
	var getCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			getCount.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":42,"path":"/old/X"}`))
		case http.MethodPut:
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "k", srv.Client(), testLogger())
	if err := c.UpdateArtistPath(context.Background(), "42", "/new/X"); err != nil {
		t.Fatalf("UpdateArtistPath: %v", err)
	}
	if got := getCount.Load(); got != 1 {
		t.Errorf("GET count = %d, want 1 (the pre-PUT fetch only); "+
			"a second GET means an in-client read-back returned -- the publish "+
			"layer already verifies every peer, so it would be a duplicate", got)
	}
}

// TestGetMetadataProviderConfigs_AuthClass401 covers the hand-rolled GET on
// /api/v1/config/metadataprovider so a 401 there still routes through the
// ErrAuthRequired wrap. The function is unexported, so we exercise it
// through GetMetadataConsumers (its only caller) which forwards the error.
func TestGetMetadataProviderConfigs_AuthClass401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/config/metadataprovider" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "k", srv.Client(), testLogger())
	_, err := c.GetMetadataConsumers(context.Background())
	if err == nil {
		t.Fatal("expected error on 401")
	}
	if !errors.Is(err, ErrAuthRequired) {
		t.Errorf("errors.Is(err, ErrAuthRequired) = false; want true. err = %v", err)
	}
}

// TestGetMetadataConsumers_AuthClass401 covers the hand-rolled GET on
// /api/v1/metadata used by the conflict detection and snapshot/restore
// paths. A 401 here must wrap with ErrAuthRequired so peer-side credential
// rotation surfaces consistently across every Lidarr touchpoint.
func TestGetMetadataConsumers_AuthClass401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/metadata" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "k", srv.Client(), testLogger())
	// CheckNFOWriterEnabled forwards getMetadataConsumers' error directly.
	_, _, err := c.CheckNFOWriterEnabled(context.Background())
	if err == nil {
		t.Fatal("expected error on 401")
	}
	if !errors.Is(err, ErrAuthRequired) {
		t.Errorf("errors.Is(err, ErrAuthRequired) = false; want true. err = %v", err)
	}
}
