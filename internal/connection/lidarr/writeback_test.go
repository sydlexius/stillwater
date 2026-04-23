package lidarr

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeLidarrServer models the /api/v1/metadata surface: GET returns an
// array of consumer configs with a "fields" array, PUT /:id accepts the
// full consumer body back. Tests drive the server through the same code
// path the real client exercises.
type fakeLidarrServer struct {
	mu        sync.Mutex
	consumers []map[string]any
	lastPUT   map[int]map[string]any
}

func newFakeLidarrServer(consumers []map[string]any) (*httptest.Server, *fakeLidarrServer) {
	f := &fakeLidarrServer{consumers: consumers, lastPUT: map[int]map[string]any{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/metadata":
			f.mu.Lock()
			defer f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(f.consumers)
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/api/v1/metadata/"):
			idStr := strings.TrimPrefix(r.URL.Path, "/api/v1/metadata/")
			var id int
			_, _ = fmt.Sscanf(idStr, "%d", &id)
			body, _ := io.ReadAll(r.Body)
			var payload map[string]any
			if err := json.Unmarshal(body, &payload); err != nil {
				http.Error(w, "bad json", http.StatusBadRequest)
				return
			}
			f.mu.Lock()
			f.lastPUT[id] = payload
			for i, m := range f.consumers {
				if int(m["id"].(float64)) == id {
					f.consumers[i] = payload
				}
			}
			f.mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
		default:
			http.NotFound(w, r)
		}
	}))
	return srv, f
}

// makeConsumer builds the raw map shape Lidarr's /api/v1/metadata returns.
// Fields is a heterogeneous list because real Lidarr responses include
// unrelated entries (e.g. albumMetadata, trackMetadata) we must preserve.
// The id is always 1 in current tests, and lidarr's numeric id comes back
// from JSON as float64, so we hardcode 1.0 rather than take a parameter.
func makeConsumer(name string, enable, artistMetadata, artistImages, albumMetadata bool) map[string]any {
	return map[string]any{
		"id":     float64(1),
		"name":   name,
		"enable": enable,
		"fields": []any{
			map[string]any{"name": "artistMetadata", "value": artistMetadata},
			map[string]any{"name": "albumMetadata", "value": albumMetadata},
			map[string]any{"name": "artistImages", "value": artistImages},
			map[string]any{"name": "albumImages", "value": true},
		},
	}
}

func TestLidarrCheckNFO_DetectsArtistMetadataField(t *testing.T) {
	srv, _ := newFakeLidarrServer([]map[string]any{
		makeConsumer("Kodi (XBMC) / Emby", true, true, false, true),
	})
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "k", srv.Client(), testLogger())
	on, name, err := c.CheckNFOWriterEnabled(context.Background())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !on {
		t.Error("artistMetadata=true on an enabled consumer should register as NFO conflict")
	}
	if name != "Kodi (XBMC) / Emby" {
		t.Errorf("name = %q, want Kodi (XBMC) / Emby", name)
	}
}

func TestLidarrCheckImage_DetectsArtistImagesField(t *testing.T) {
	srv, _ := newFakeLidarrServer([]map[string]any{
		makeConsumer("Kodi", true, false, true, false),
	})
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "k", srv.Client(), testLogger())
	on, _, err := c.CheckImageSaverEnabled(context.Background())
	if err != nil || !on {
		t.Errorf("want image=true err=nil, got %v %v", on, err)
	}
}

func TestLidarrCheckNFO_DisabledConsumerIsIgnored(t *testing.T) {
	srv, _ := newFakeLidarrServer([]map[string]any{
		makeConsumer("Kodi", false, true, true, true),
	})
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "k", srv.Client(), testLogger())
	nfo, _, _ := c.CheckNFOWriterEnabled(context.Background())
	img, _, _ := c.CheckImageSaverEnabled(context.Background())
	if nfo || img {
		t.Errorf("disabled consumer should not trigger any conflict, got nfo=%v img=%v", nfo, img)
	}
}

func TestLidarrDisableFileWriteBack_ClearsArtistFieldsOnly(t *testing.T) {
	srv, fake := newFakeLidarrServer([]map[string]any{
		makeConsumer("Kodi", true, true, true, true),
	})
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "k", srv.Client(), testLogger())
	if err := c.DisableFileWriteBack(context.Background()); err != nil {
		t.Fatalf("disable err = %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	got, ok := fake.lastPUT[1]
	if !ok {
		t.Fatal("no PUT recorded")
	}
	if got["enable"] != true {
		t.Error("master enable flag should be preserved, not touched")
	}
	if !hasField(got, "artistMetadata", false) {
		t.Error("artistMetadata should be false in PUT body")
	}
	if !hasField(got, "artistImages", false) {
		t.Error("artistImages should be false in PUT body")
	}
	if !hasField(got, "albumMetadata", true) {
		t.Error("albumMetadata must be left alone (was true)")
	}
}

func TestLidarrSnapshotThenRestoreRoundTrip(t *testing.T) {
	srv, fake := newFakeLidarrServer([]map[string]any{
		makeConsumer("Kodi", true, true, true, false),
	})
	defer srv.Close()

	c := NewWithHTTPClient(srv.URL, "k", srv.Client(), testLogger())

	snapJSON, err := c.SnapshotLibraryOptions(context.Background())
	if err != nil {
		t.Fatalf("snap err = %v", err)
	}

	if err := c.DisableFileWriteBack(context.Background()); err != nil {
		t.Fatalf("disable err = %v", err)
	}
	// Sanity: fields were indeed turned off.
	if got := fake.lastPUT[1]; !hasField(got, "artistMetadata", false) {
		t.Fatalf("disable should have zeroed artistMetadata: %v", got)
	}

	if err := c.RestoreLibraryOptions(context.Background(), snapJSON); err != nil {
		t.Fatalf("restore err = %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if !hasField(fake.lastPUT[1], "artistMetadata", true) {
		t.Error("restore should have flipped artistMetadata back to true")
	}
	if !hasField(fake.lastPUT[1], "artistImages", true) {
		t.Error("restore should have flipped artistImages back to true")
	}
}

func TestLidarrRestoreRejectsUnknownVersion(t *testing.T) {
	srv, _ := newFakeLidarrServer(nil)
	defer srv.Close()
	c := NewWithHTTPClient(srv.URL, "k", srv.Client(), testLogger())
	err := c.RestoreLibraryOptions(context.Background(), `{"version":42,"consumers":[]}`)
	if err == nil || !strings.Contains(err.Error(), "unsupported snapshot version") {
		t.Errorf("want version error, got %v", err)
	}
}

// hasField checks that the raw consumer body has a field with the given
// name and value. The fields list is a []any of maps, so we walk it.
func hasField(m map[string]any, name string, want bool) bool {
	fields, _ := m["fields"].([]any)
	for _, raw := range fields {
		f, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if f["name"] == name {
			v, _ := f["value"].(bool)
			return v == want
		}
	}
	return false
}
