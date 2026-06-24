package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sydlexius/stillwater/internal/connection"
)

// seedLidarrConn persists a Lidarr fixture with skip-test (no outbound call)
// and returns its ID. VerifyPathAfterUpdate starts false (the safe default).
func seedLidarrConn(t *testing.T, r *Router) string {
	t.Helper()
	c := &connection.Connection{
		Name:    "Lidarr Test",
		Type:    connection.TypeLidarr,
		URL:     "http://lidarr.local:8686",
		APIKey:  "k",
		Enabled: true,
	}
	newConnectionTestConn(t, r, c)
	return c.ID
}

func seedEmbyConn(t *testing.T, r *Router) string {
	t.Helper()
	c := &connection.Connection{
		Name:    "Emby Test",
		Type:    connection.TypeEmby,
		URL:     "http://emby.local:8096",
		APIKey:  "k",
		Enabled: true,
	}
	newConnectionTestConn(t, r, c)
	return c.ID
}

func postVerifyPath(t *testing.T, r *Router, id string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/connections/"+id+"/verify-path-after-update", bytes.NewReader(body))
	req.SetPathValue("id", id)
	w := httptest.NewRecorder()
	r.handleSetVerifyPathAfterUpdate(w, req)
	return w
}

// TestHandleSetVerifyPathAfterUpdate_PersistRoundTrip is the core acceptance
// test: enabling the toggle persists across a re-read, the response reflects
// the new state, and disabling reverses it.
func TestHandleSetVerifyPathAfterUpdate_PersistRoundTrip(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)
	id := seedLidarrConn(t, r)

	// Enable.
	w := postVerifyPath(t, r, id, []byte(`{"enabled":true}`))
	if w.Code != http.StatusOK {
		t.Fatalf("enable status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp connectionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.VerifyPathAfterUpdate {
		t.Errorf("response VerifyPathAfterUpdate = false, want true")
	}

	// Persisted: a fresh read sees true.
	got, err := r.connectionService.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if !got.GetVerifyPathAfterUpdate() {
		t.Errorf("persisted VerifyPathAfterUpdate = false, want true")
	}

	// Disable reverses it.
	w = postVerifyPath(t, r, id, []byte(`{"enabled":false}`))
	if w.Code != http.StatusOK {
		t.Fatalf("disable status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	got, err = r.connectionService.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("re-read after disable: %v", err)
	}
	if got.GetVerifyPathAfterUpdate() {
		t.Errorf("persisted VerifyPathAfterUpdate = true after disable, want false")
	}
}

// TestHandleSetVerifyPathAfterUpdate_FormBody covers the HTMX form-encoded path
// (what the settings toggle actually sends).
func TestHandleSetVerifyPathAfterUpdate_FormBody(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)
	id := seedLidarrConn(t, r)

	w := postVerifyPath(t, r, id, []byte(`enabled=true`))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	got, err := r.connectionService.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if !got.GetVerifyPathAfterUpdate() {
		t.Errorf("VerifyPathAfterUpdate = false, want true")
	}
}

// TestHandleSetVerifyPathAfterUpdate_RejectsNonLidarr pins the Lidarr-only
// guard: an Emby connection must be rejected with 400 (the field is
// unrepresentable on Emby, so silently accepting would be a no-op masquerading
// as success).
func TestHandleSetVerifyPathAfterUpdate_RejectsNonLidarr(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)
	id := seedEmbyConn(t, r)

	w := postVerifyPath(t, r, id, []byte(`{"enabled":true}`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleSetVerifyPathAfterUpdate_MissingEnabled pins the strict-parse
// contract: a well-formed JSON body without "enabled" must 400, never coerce to
// false (a dropped HTMX body must not silently disable the safety check).
func TestHandleSetVerifyPathAfterUpdate_MissingEnabled(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)
	id := seedLidarrConn(t, r)

	w := postVerifyPath(t, r, id, []byte(`{"foo":"bar"}`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleSetVerifyPathAfterUpdate_EmptyBody covers an empty body with no
// query-param fallback: strict validation must 400.
func TestHandleSetVerifyPathAfterUpdate_EmptyBody(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)
	id := seedLidarrConn(t, r)

	w := postVerifyPath(t, r, id, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleSetVerifyPathAfterUpdate_InvalidEnabledValue covers a present but
// unparsable "enabled".
func TestHandleSetVerifyPathAfterUpdate_InvalidEnabledValue(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)
	id := seedLidarrConn(t, r)

	w := postVerifyPath(t, r, id, []byte(`{"enabled":"maybe"}`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleSetVerifyPathAfterUpdate_FormInvalidValue covers the form-encoded
// path with an unparsable value (enabled=maybe).
func TestHandleSetVerifyPathAfterUpdate_FormInvalidValue(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)
	id := seedLidarrConn(t, r)

	w := postVerifyPath(t, r, id, []byte(`enabled=maybe`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleSetVerifyPathAfterUpdate_MalformedForm covers a form body with
// invalid percent-encoding, which url.ParseQuery rejects.
func TestHandleSetVerifyPathAfterUpdate_MalformedForm(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)
	id := seedLidarrConn(t, r)

	w := postVerifyPath(t, r, id, []byte(`enabled=%zz`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleSetVerifyPathAfterUpdate_QueryParam covers the ?enabled= fallback
// with an empty body: the query param supplies the value and persists.
func TestHandleSetVerifyPathAfterUpdate_QueryParam(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)
	id := seedLidarrConn(t, r)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/connections/"+id+"/verify-path-after-update?enabled=true", nil)
	req.SetPathValue("id", id)
	w := httptest.NewRecorder()
	r.handleSetVerifyPathAfterUpdate(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	got, err := r.connectionService.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if !got.GetVerifyPathAfterUpdate() {
		t.Errorf("VerifyPathAfterUpdate = false, want true")
	}
}

// TestHandleSetVerifyPathAfterUpdate_QueryParamInvalid covers an unparsable
// query-param override.
func TestHandleSetVerifyPathAfterUpdate_QueryParamInvalid(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)
	id := seedLidarrConn(t, r)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/connections/"+id+"/verify-path-after-update?enabled=banana", nil)
	req.SetPathValue("id", id)
	w := httptest.NewRecorder()
	r.handleSetVerifyPathAfterUpdate(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleSetVerifyPathAfterUpdate_QueryOverridesBody locks the URL-over-body
// precedence: a JSON body of {"enabled":false} with ?enabled=true must end in
// final state TRUE. A refactor that drops the query override would flip this.
func TestHandleSetVerifyPathAfterUpdate_QueryOverridesBody(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)
	id := seedLidarrConn(t, r)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/connections/"+id+"/verify-path-after-update?enabled=true", bytes.NewReader([]byte(`{"enabled":false}`)))
	req.SetPathValue("id", id)
	w := httptest.NewRecorder()
	r.handleSetVerifyPathAfterUpdate(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	got, err := r.connectionService.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if !got.GetVerifyPathAfterUpdate() {
		t.Errorf("VerifyPathAfterUpdate = false, want true (query ?enabled=true must override body enabled=false)")
	}
}

// TestSetVerifyPathAfterUpdate_AllocatesNilLidarr covers the nil-allocation
// branch of the pure mutation helper: a lidarr Connection whose Lidarr
// sub-config is nil (e.g. a legacy/hand-built value) must be allocated and the
// flag set with no nil-deref. scanConnection always allocates Lidarr for a real
// lidarr row, so this branch is unreachable through the service and must be
// pinned at the unit level.
func TestSetVerifyPathAfterUpdate_AllocatesNilLidarr(t *testing.T) {
	t.Parallel()
	c := &connection.Connection{ID: "l-nil", Type: connection.TypeLidarr} // Lidarr left nil
	if c.Lidarr != nil {
		t.Fatalf("precondition: Lidarr should start nil")
	}

	setVerifyPathAfterUpdate(c, true)

	if c.Lidarr == nil {
		t.Fatal("Lidarr was not allocated")
	}
	if !c.Lidarr.VerifyPathAfterUpdate {
		t.Errorf("VerifyPathAfterUpdate = false, want true")
	}

	// And it flips back off without re-allocating a distinct config.
	setVerifyPathAfterUpdate(c, false)
	if c.Lidarr == nil || c.Lidarr.VerifyPathAfterUpdate {
		t.Errorf("expected VerifyPathAfterUpdate=false on non-nil Lidarr after disable")
	}
}

// TestHandleSetVerifyPathAfterUpdate_BadJSON covers a malformed JSON body.
func TestHandleSetVerifyPathAfterUpdate_BadJSON(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)
	id := seedLidarrConn(t, r)

	w := postVerifyPath(t, r, id, []byte(`{not json`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleSetVerifyPathAfterUpdate_MissingID rejects an empty path id.
func TestHandleSetVerifyPathAfterUpdate_MissingID(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/connections//verify-path-after-update", bytes.NewReader([]byte(`{"enabled":true}`)))
	req.SetPathValue("id", "")
	w := httptest.NewRecorder()
	r.handleSetVerifyPathAfterUpdate(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// TestHandleSetVerifyPathAfterUpdate_UnknownConnection returns 404 for a
// well-formed body but a connection that does not exist.
func TestHandleSetVerifyPathAfterUpdate_UnknownConnection(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)

	w := postVerifyPath(t, r, "does-not-exist", []byte(`{"enabled":true}`))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

// TestToConnectionResponse_VerifyPathAfterUpdate covers the DTO mapping from
// the nil-safe getter for both Lidarr (true) and a non-Lidarr connection
// (false, getter returns false for a nil Lidarr sub-config).
func TestToConnectionResponse_VerifyPathAfterUpdate(t *testing.T) {
	t.Parallel()

	lidarr := connection.Connection{
		ID:     "l1",
		Type:   connection.TypeLidarr,
		Lidarr: &connection.LidarrConfig{VerifyPathAfterUpdate: true},
	}
	if got := toConnectionResponse(lidarr).VerifyPathAfterUpdate; !got {
		t.Errorf("lidarr VerifyPathAfterUpdate = false, want true")
	}

	emby := connection.Connection{
		ID:   "e1",
		Type: connection.TypeEmby,
		Emby: &connection.EmbyConfig{},
	}
	if got := toConnectionResponse(emby).VerifyPathAfterUpdate; got {
		t.Errorf("emby VerifyPathAfterUpdate = true, want false")
	}
}
