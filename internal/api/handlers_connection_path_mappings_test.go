package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/connection"
)

func postPathMappings(t *testing.T, r *Router, id string, contentType, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/connections/"+id+"/path-mappings", strings.NewReader(body))
	req.SetPathValue("id", id)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	w := httptest.NewRecorder()
	r.handleSetPathMappings(w, req)
	return w
}

// TestHandleSetPathMappings_JSONRoundTrip is the core acceptance test: a JSON
// body of mappings persists across a re-read and the response reflects them;
// a subsequent empty list clears them.
func TestHandleSetPathMappings_JSONRoundTrip(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)
	id := seedLidarrConn(t, r)

	w := postPathMappings(t, r, id, "application/json",
		`{"path_mappings":[{"host_prefix":"/music","platform_prefix":"/data/media"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("set: status %d, body %s", w.Code, w.Body.String())
	}
	var resp connectionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.PathMappings) != 1 || resp.PathMappings[0].HostPrefix != "/music" || resp.PathMappings[0].PlatformPrefix != "/data/media" {
		t.Fatalf("response mappings = %+v, want one /music->/data/media", resp.PathMappings)
	}

	got, err := r.connectionService.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(got.GetPathMappings()) != 1 {
		t.Fatalf("persisted mappings = %+v, want 1", got.GetPathMappings())
	}

	// Empty list clears.
	w = postPathMappings(t, r, id, "application/json", `{"path_mappings":[]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("clear: status %d, body %s", w.Code, w.Body.String())
	}
	got, err = r.connectionService.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("reload after clear: %v", err)
	}
	if len(got.GetPathMappings()) != 0 {
		t.Errorf("mappings after clear = %+v, want none", got.GetPathMappings())
	}
}

// TestHandleSetPathMappings_FormEncoded covers the HTMX form path: parallel
// host_prefix / platform_prefix fields (including a trailing blank row that
// must be dropped) persist as the sanitized set.
func TestHandleSetPathMappings_FormEncoded(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)
	id := seedLidarrConn(t, r)

	form := url.Values{}
	form.Add("host_prefix", "/music")
	form.Add("platform_prefix", "/data")
	// Trailing blank row the UI always emits; sanitize must drop it.
	form.Add("host_prefix", "")
	form.Add("platform_prefix", "")

	w := postPathMappings(t, r, id, "application/x-www-form-urlencoded", form.Encode())
	if w.Code != http.StatusOK {
		t.Fatalf("set: status %d, body %s", w.Code, w.Body.String())
	}
	got, err := r.connectionService.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(got.GetPathMappings()) != 1 || got.GetPathMappings()[0].HostPrefix != "/music" {
		t.Fatalf("persisted mappings = %+v, want single /music->/data", got.GetPathMappings())
	}
}

// TestHandleSetPathMappings_HalfMappingRejected pins that a pair with exactly
// one side filled is a 400, never silently persisted as a corrupting mapping.
func TestHandleSetPathMappings_HalfMappingRejected(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)
	id := seedLidarrConn(t, r)

	w := postPathMappings(t, r, id, "application/json",
		`{"path_mappings":[{"host_prefix":"/music","platform_prefix":""}]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("half mapping: status %d, want 400 (body %s)", w.Code, w.Body.String())
	}
	got, err := r.connectionService.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(got.GetPathMappings()) != 0 {
		t.Errorf("half mapping should not persist; got %+v", got.GetPathMappings())
	}
}

// TestHandleSetPathMappings_RootPrefixRejected pins that a prefix that is
// empty or a bare "/" (reducing to "" after MapArtistPath's TrimRight) is a
// 400, not a silently-saved no-op. Both sides are covered.
func TestHandleSetPathMappings_RootPrefixRejected(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)
	id := seedLidarrConn(t, r)

	for _, body := range []string{
		`{"path_mappings":[{"host_prefix":"/","platform_prefix":"/data"}]}`,
		`{"path_mappings":[{"host_prefix":"/music","platform_prefix":"/"}]}`,
	} {
		w := postPathMappings(t, r, id, "application/json", body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("root prefix %s: status %d, want 400 (body %s)", body, w.Code, w.Body.String())
		}
	}
	got, err := r.connectionService.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(got.GetPathMappings()) != 0 {
		t.Errorf("root prefix should not persist; got %+v", got.GetPathMappings())
	}
}

// TestHandleSetPathMappings_NonLidarrRejected pins the 400 for an Emby
// connection: the field is unrepresentable on non-Lidarr sub-configs.
func TestHandleSetPathMappings_NonLidarrRejected(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)
	id := seedEmbyConn(t, r)

	w := postPathMappings(t, r, id, "application/json",
		`{"path_mappings":[{"host_prefix":"/music","platform_prefix":"/data"}]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("emby connection: status %d, want 400 (body %s)", w.Code, w.Body.String())
	}
}

// TestHandleSetPathMappings_UnknownID returns 404 without allocating a mutex
// entry (the existence gate runs before LoadOrStore).
func TestHandleSetPathMappings_UnknownID(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)

	w := postPathMappings(t, r, "does-not-exist", "application/json", `{"path_mappings":[]}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown id: status %d, want 404 (body %s)", w.Code, w.Body.String())
	}
}

// TestSanitizePathMappings covers the pure validation helper directly.
func TestSanitizePathMappings(t *testing.T) {
	t.Parallel()

	got, err := sanitizePathMappings([]connection.PathMapping{
		{HostPrefix: " /music ", PlatformPrefix: " /data "},
		{HostPrefix: "  ", PlatformPrefix: ""},
	})
	if err != nil {
		t.Fatalf("sanitize: %v", err)
	}
	if len(got) != 1 || got[0].HostPrefix != "/music" || got[0].PlatformPrefix != "/data" {
		t.Fatalf("sanitize trimmed = %+v, want single trimmed /music->/data", got)
	}

	if _, err := sanitizePathMappings([]connection.PathMapping{{HostPrefix: "/music", PlatformPrefix: ""}}); err == nil {
		t.Error("half mapping should error")
	}

	// A prefix that is only slashes reduces to "" after TrimRight and must error
	// on either side rather than persist as a no-op.
	for _, m := range []connection.PathMapping{
		{HostPrefix: "/", PlatformPrefix: "/data"},
		{HostPrefix: "/music", PlatformPrefix: "/"},
		{HostPrefix: "//", PlatformPrefix: "/data"},
	} {
		if _, err := sanitizePathMappings([]connection.PathMapping{m}); err == nil {
			t.Errorf("root prefix %+v should error", m)
		}
	}

	if got, err := sanitizePathMappings(nil); err != nil || got != nil {
		t.Errorf("nil input = %+v, %v; want nil, nil", got, err)
	}
}
