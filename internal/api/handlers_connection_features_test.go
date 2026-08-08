package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/connection"
)

// #2579. PATCH /connections/{id}/features accepted the three feature toggles
// on a Lidarr connection and answered 200 {"status":"updated"} for a value the
// read path can never surface: scanConnection maps the feature columns onto
// the Emby/Jellyfin sub-configs only, so a Lidarr row's stored 1 has nowhere
// to land and GetFeatureImageWrite falls to its default:false case.
//
// READING THE GETTERS PROVES NOTHING HERE. On a Lidarr connection every
// GetFeature* accessor returns false unconditionally, so an assertion built on
// them passes identically before and after the fix -- vacuously. Each test
// below reads the RAW connections columns instead, which is where the measured
// pre-fix damage actually was (200 OK, columns written to 1, getters false).

// rawConnectionFeatureColumns returns the three feature columns as stored,
// bypassing scanConnection's type-discriminated mapping.
func rawConnectionFeatureColumns(t *testing.T, r *Router, id string) (imageWrite, metadataPush, triggerRefresh int) {
	t.Helper()
	row := r.db.QueryRow(`
		SELECT feature_image_write, feature_metadata_push, feature_trigger_refresh
		FROM connections WHERE id = ?`, id)
	if err := row.Scan(&imageWrite, &metadataPush, &triggerRefresh); err != nil {
		t.Fatalf("reading raw feature columns for %s: %v", id, err)
	}
	return imageWrite, metadataPush, triggerRefresh
}

// patchFeatures issues a features PATCH with the given body against id.
func patchFeatures(t *testing.T, r *Router, id string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshaling body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPatch,
		"/api/v1/connections/"+id+"/features", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", id)
	return serveValidated(t, http.HandlerFunc(r.handleUpdateConnectionFeatures), req)
}

// TestHandleUpdateConnectionFeatures_LidarrRejectsAllThree is the #2579
// regression proper: all three toggles on a Lidarr connection must be refused
// at the boundary, and nothing may reach the columns.
func TestHandleUpdateConnectionFeatures_LidarrRejectsAllThree(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)
	id := seedLidarrConn(t, r)

	// PRECONDITION: the columns start at 0, so a post-call reading of 0 is
	// attributable to the rejection rather than to a fixture that could
	// never have shown the damage.
	if iw, mp, tr := rawConnectionFeatureColumns(t, r, id); iw != 0 || mp != 0 || tr != 0 {
		t.Fatalf("precondition: seeded lidarr columns = %d/%d/%d, want 0/0/0", iw, mp, tr)
	}

	w := patchFeatures(t, r, id, map[string]any{
		"feature_image_write":     true,
		"feature_metadata_push":   true,
		"feature_trigger_refresh": true,
	})

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (body %s)", w.Code, w.Body.String())
	}

	// The error must name the fields. An operator who gets a bare "bad
	// request" is only marginally better off than one who got a false 200.
	var errBody struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &errBody); err != nil {
		t.Fatalf("decoding error body %q: %v", w.Body.String(), err)
	}
	for _, field := range []string{"feature_image_write", "feature_metadata_push", "feature_trigger_refresh"} {
		if !strings.Contains(errBody.Error, field) {
			t.Errorf("error %q does not name %q", errBody.Error, field)
		}
	}
	if !strings.Contains(errBody.Error, "lidarr") {
		t.Errorf("error %q does not name the connection type", errBody.Error)
	}
	// The verb agrees with the field count. Every other assertion here is a
	// Contains() on a field name, which passes under either verb -- live UAT
	// surfaced "feature_image_write, feature_metadata_push,
	// feature_trigger_refresh IS not supported" with all of them green.
	if !strings.Contains(errBody.Error, "are not supported") {
		t.Errorf("error %q should use the plural verb for a 3-field list", errBody.Error)
	}

	if iw, mp, tr := rawConnectionFeatureColumns(t, r, id); iw != 0 || mp != 0 || tr != 0 {
		t.Errorf("columns after rejected PATCH = %d/%d/%d, want 0/0/0 (the #2579 phantom write)", iw, mp, tr)
	}
}

// TestHandleUpdateConnectionFeatures_LidarrRejectsEachFieldIndependently
// covers the issue's scope note: image_write was the reported field, but
// metadata_push and trigger_refresh go through the same handler and the same
// accessor shape. A gate that only inspects one of the three leaves the other
// two writing phantom columns, so each is exercised ALONE -- a combined body
// would let one rejected field mask two accepted ones.
func TestHandleUpdateConnectionFeatures_LidarrRejectsEachFieldIndependently(t *testing.T) {
	t.Parallel()
	for _, field := range []string{"feature_image_write", "feature_metadata_push", "feature_trigger_refresh"} {
		t.Run(field, func(t *testing.T) {
			t.Parallel()
			r := newConnectionTestRouter(t)
			id := seedLidarrConn(t, r)

			w := patchFeatures(t, r, id, map[string]any{field: true})
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (body %s)", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), field) {
				t.Errorf("error %q does not name %q", w.Body.String(), field)
			}
			if iw, mp, tr := rawConnectionFeatureColumns(t, r, id); iw != 0 || mp != 0 || tr != 0 {
				t.Errorf("columns = %d/%d/%d, want 0/0/0", iw, mp, tr)
			}
		})
	}
}

// TestHandleUpdateConnectionFeatures_LidarrRejectsFalseToo keys the gate on
// the field being PRESENT, not on its value. `{"feature_image_write": false}`
// is just as unsupported as true: answering 200 "updated" would still assert a
// stored setting that does not exist for this connection type.
func TestHandleUpdateConnectionFeatures_LidarrRejectsFalseToo(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)
	id := seedLidarrConn(t, r)

	w := patchFeatures(t, r, id, map[string]any{"feature_image_write": false})
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (body %s)", w.Code, w.Body.String())
	}
}

// TestHandleUpdateConnectionFeatures_LidarrEmptyBodyStillSucceeds is the
// anti-overreach guard: the gate must fire on the inapplicable FIELDS, not on
// the connection type. A body carrying none of them asks for nothing
// unsupported and must not be turned into an error.
func TestHandleUpdateConnectionFeatures_LidarrEmptyBodyStillSucceeds(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)
	id := seedLidarrConn(t, r)

	w := patchFeatures(t, r, id, map[string]any{})
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
}

// TestHandleUpdateConnection_LidarrRejectsFeatureToggles covers the SECOND
// write surface. PUT /connections/{id} accepts the same three fields and had
// the same defect by a different mechanism: it routes through
// Connection.SetFeatures, which has no Lidarr arm, so the assignment was a
// silent no-op and the handler still answered 200.
//
// This test exists because deleting the gate in handleUpdateConnection left
// the ENTIRE internal/api suite green -- the features-PATCH tests above cannot
// see this handler at all. Two fixes, two independent proofs.
func TestHandleUpdateConnection_LidarrRejectsFeatureToggles(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)
	id := seedLidarrConn(t, r)

	body, err := json.Marshal(map[string]any{"feature_image_write": true})
	if err != nil {
		t.Fatalf("marshaling body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPut, "/api/v1/connections/"+id, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", id)

	w := serveValidated(t, http.HandlerFunc(r.handleUpdateConnection), req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (body %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "feature_image_write") {
		t.Errorf("error %q does not name the field", w.Body.String())
	}
	if iw, mp, tr := rawConnectionFeatureColumns(t, r, id); iw != 0 || mp != 0 || tr != 0 {
		t.Errorf("columns = %d/%d/%d, want 0/0/0", iw, mp, tr)
	}
}

// TestHandleUpdateConnection_TypeChangeToLidarrRejectsFeatureToggles pins the
// gate's POSITION. It sits after the type reassignment, so a body that both
// switches a connection to Lidarr and sets a toggle is judged against the type
// it is BECOMING. Moved above that assignment, the check would consult the old
// type, pass, and let SetFeatures silently drop the value -- the original bug,
// reachable through a type change.
func TestHandleUpdateConnection_TypeChangeToLidarrRejectsFeatureToggles(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)

	c := &connection.Connection{
		Name: "WasEmby", Type: connection.TypeEmby,
		URL: "http://emby.local:8096", APIKey: "k", Enabled: true,
	}
	newConnectionTestConn(t, r, c)

	body, err := json.Marshal(map[string]any{
		"type":                connection.TypeLidarr,
		"url":                 "http://lidarr.local:8686",
		"feature_image_write": true,
	})
	if err != nil {
		t.Fatalf("marshaling body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPut, "/api/v1/connections/"+c.ID, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", c.ID)

	w := serveValidated(t, http.HandlerFunc(r.handleUpdateConnection), req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (body %s)", w.Code, w.Body.String())
	}
}

// TestHandleUpdateConnection_InvalidTypeReportsTheTypeNotTheToggle covers the
// #2975 review finding: an unknown type paired with a feature toggle was
// answered "feature_image_write is not supported for plex connections", which
// blames the toggle and implicitly treats "plex" as a real connection type,
// masking the actual input error.
//
// The sub-cases are asserted SEPARATELY because they failed differently before
// the fix -- with a toggle it was a misleading 400, without one it was a 500
// (Validate rejected the type deep in the write path, where the handler could
// only render "internal error"). A single combined case would have hidden the
// second.
func TestHandleUpdateConnection_InvalidTypeReportsTheTypeNotTheToggle(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body map[string]any
	}{
		{"with a feature toggle", map[string]any{"type": "plex", "feature_image_write": true}},
		{"type alone", map[string]any{"type": "plex"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newConnectionTestRouter(t)
			c := &connection.Connection{
				Name: "Peer", Type: connection.TypeEmby,
				URL: "http://emby.local:8096", APIKey: "k", Enabled: true,
			}
			newConnectionTestConn(t, r, c)

			raw, err := json.Marshal(tc.body)
			if err != nil {
				t.Fatalf("marshaling body: %v", err)
			}
			req := httptest.NewRequest(http.MethodPut, "/api/v1/connections/"+c.ID, bytes.NewReader(raw))
			req.Header.Set("Content-Type", "application/json")
			req.SetPathValue("id", c.ID)
			w := httptest.NewRecorder()
			r.handleUpdateConnection(w, req)

			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (body %s)", w.Code, w.Body.String())
			}
			// The message must name the TYPE problem, not the toggle.
			if !strings.Contains(w.Body.String(), "type must be one of") {
				t.Errorf("error %q does not report the unsupported type", w.Body.String())
			}
			if strings.Contains(w.Body.String(), "feature_image_write") {
				t.Errorf("error %q blames the toggle instead of the type", w.Body.String())
			}
			// The bogus type must not have been persisted.
			got, err := r.connectionService.GetByID(context.Background(), c.ID)
			if err != nil {
				t.Fatalf("GetByID: %v", err)
			}
			if got.Type != connection.TypeEmby {
				t.Errorf("Type = %q, want it unchanged at emby", got.Type)
			}
		})
	}
}

// TestHandleUpdateConnection_ValidTypeChangeStillWorks is the anti-overreach
// guard on the type check: a legitimate type change must still be accepted, or
// the assertions above would pass with the endpoint refusing every type change.
func TestHandleUpdateConnection_ValidTypeChangeStillWorks(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)
	c := &connection.Connection{
		Name: "Peer", Type: connection.TypeEmby,
		URL: "http://emby.local:8096", APIKey: "k", Enabled: true,
	}
	newConnectionTestConn(t, r, c)

	raw, err := json.Marshal(map[string]any{
		"type": connection.TypeJellyfin,
		"url":  "http://jellyfin.local:8096",
	})
	if err != nil {
		t.Fatalf("marshaling body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPut, "/api/v1/connections/"+c.ID, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", c.ID)
	w := serveValidated(t, http.HandlerFunc(r.handleUpdateConnection), req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	got, err := r.connectionService.GetByID(context.Background(), c.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Type != connection.TypeJellyfin {
		t.Errorf("Type = %q, want jellyfin", got.Type)
	}
}

// TestHandleUpdateConnection_LidarrWithoutFeatureTogglesSucceeds is the
// anti-overreach guard for this surface: an ordinary Lidarr edit carrying none
// of the three fields must still work. Without it, a gate keyed on the
// connection TYPE rather than on the FIELDS would make Lidarr connections
// uneditable and every assertion above would still pass.
func TestHandleUpdateConnection_LidarrWithoutFeatureTogglesSucceeds(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)
	id := seedLidarrConn(t, r)

	body, err := json.Marshal(map[string]any{"name": "Renamed Lidarr"})
	if err != nil {
		t.Fatalf("marshaling body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPut, "/api/v1/connections/"+id, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", id)

	w := serveValidated(t, http.HandlerFunc(r.handleUpdateConnection), req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	got, err := r.connectionService.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Name != "Renamed Lidarr" {
		t.Errorf("Name = %q, want Renamed Lidarr", got.Name)
	}
}

// TestHandleUpdateConnectionFeatures_SupportedTypesUnaffected is the mutation
// guard on the gate's DIRECTION. A predicate inverted (or widened to every
// type) still passes every Lidarr assertion above while silently breaking the
// platforms the toggles exist for, so both supported types are asserted to
// still persist all three flags.
func TestHandleUpdateConnectionFeatures_SupportedTypesUnaffected(t *testing.T) {
	t.Parallel()
	for _, connType := range []string{connection.TypeEmby, connection.TypeJellyfin} {
		t.Run(connType, func(t *testing.T) {
			t.Parallel()
			r := newConnectionTestRouter(t)
			c := &connection.Connection{
				Name: "Feat " + connType, Type: connType,
				URL: "http://" + connType + ".local:8096", APIKey: "k", Enabled: true,
			}
			newConnectionTestConn(t, r, c)

			w := patchFeatures(t, r, c.ID, map[string]any{
				"feature_image_write":     true,
				"feature_metadata_push":   true,
				"feature_trigger_refresh": true,
			})
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
			}
			got, err := r.connectionService.GetByID(context.Background(), c.ID)
			if err != nil {
				t.Fatalf("GetByID: %v", err)
			}
			if !got.GetFeatureImageWrite() || !got.GetFeatureMetadataPush() || !got.GetFeatureTriggerRefresh() {
				t.Errorf("features not persisted for %s: %+v", connType, got)
			}
		})
	}
}
