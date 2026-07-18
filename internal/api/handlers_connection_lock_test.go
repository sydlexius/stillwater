package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/sydlexius/stillwater/internal/connection"
)

// TestConcurrentConnectionWrites_NoLostUpdate is the #2324 regression test: a
// concurrent PUT to handleUpdateConnection (a full-row read-modify-write of
// the Name field) racing a concurrent POST to handleSetPathMappings (a
// read-modify-write of the PathMappings field) on the SAME connection must
// never lose either side's change. Before the fix these two handlers
// serialized on different locks (handleUpdateConnection took none at all,
// handleSetPathMappings took a dedicated pathMappingsMu), so a full-row
// UPDATE from handleUpdateConnection's stale in-memory snapshot could commit
// after handleSetPathMappings's write and silently revert PathMappings back
// to empty (last-writer-wins on the non-overlapping field). With both
// handlers now serialized on the SAME per-connection lock (connWriteMu via
// lockConnection), whichever write commits last re-reads the connection
// under the lock first, so it observes and preserves the other write's
// already-committed change.
//
// Run with -race (see CLAUDE.md capture rule): the race detector also
// catches any unsynchronized access to the in-memory Connection struct these
// handlers share via the service layer.
func TestConcurrentConnectionWrites_NoLostUpdate(t *testing.T) {
	t.Parallel()

	const rounds = 25
	for round := 0; round < rounds; round++ {
		r := newConnectionTestRouter(t)
		// Enabled: false (unlike seedLidarrConn) so handleUpdateConnection's
		// post-write applyInferredPathMappingsIfEmpty best-effort call takes
		// its "conn.Enabled" early-return and never attempts a real network
		// round-trip to the fixture's non-routable Lidarr URL -- that path is
		// exercised elsewhere; this test is only about the write-lock race.
		c := &connection.Connection{
			Name: "Before", Type: connection.TypeLidarr,
			URL: "http://lidarr.local:8686", APIKey: "k", Enabled: false,
		}
		newConnectionTestConn(t, r, c)
		id := c.ID

		wantName := fmt.Sprintf("Renamed-%d", round)
		wantHost := fmt.Sprintf("/music-%d", round)
		wantPlatform := fmt.Sprintf("/data-%d", round)

		var wg sync.WaitGroup
		wg.Add(2)

		// Writer A: full-row update via handleUpdateConnection, changing Name.
		go func() {
			defer wg.Done()
			body, err := json.Marshal(map[string]any{"name": wantName})
			if err != nil {
				t.Errorf("round %d: marshal PUT body: %v", round, err)
				return
			}
			req := httptest.NewRequest(http.MethodPut, "/api/v1/connections/"+id, bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.SetPathValue("id", id)
			w := httptest.NewRecorder()
			r.handleUpdateConnection(w, req)
			if w.Code != http.StatusOK {
				t.Errorf("round %d: handleUpdateConnection status = %d, body %s", round, w.Code, w.Body.String())
			}
		}()

		// Writer B: path-mappings write via handleSetPathMappings, changing
		// PathMappings. Disjoint field from writer A -- neither request body
		// mentions the other's field, mirroring two different settings-page
		// panels saved back to back by an operator.
		go func() {
			defer wg.Done()
			payload := fmt.Sprintf(`{"path_mappings":[{"host_prefix":%q,"platform_prefix":%q}]}`, wantHost, wantPlatform)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/connections/"+id+"/path-mappings", bytes.NewReader([]byte(payload)))
			req.Header.Set("Content-Type", "application/json")
			req.SetPathValue("id", id)
			w := httptest.NewRecorder()
			r.handleSetPathMappings(w, req)
			if w.Code != http.StatusOK {
				t.Errorf("round %d: handleSetPathMappings status = %d, body %s", round, w.Code, w.Body.String())
			}
		}()

		wg.Wait()

		got, err := r.connectionService.GetByID(context.Background(), id)
		if err != nil {
			t.Fatalf("round %d: reload: %v", round, err)
		}
		if got.Name != wantName {
			t.Errorf("round %d: LOST UPDATE: Name = %q, want %q (writer A's change was clobbered)", round, got.Name, wantName)
		}
		mappings := got.GetPathMappings()
		if len(mappings) != 1 || mappings[0].HostPrefix != wantHost || mappings[0].PlatformPrefix != wantPlatform {
			t.Errorf("round %d: LOST UPDATE: PathMappings = %+v, want one %s->%s (writer B's change was clobbered)",
				round, mappings, wantHost, wantPlatform)
		}
	}
}

// TestConcurrentConnectionWrites_FeaturesAndPathMappings covers the second
// pairing named in #2324: handleUpdateConnectionFeatures (a partial
// read-modify-write of the three feature-flag columns) racing
// handleSetPathMappings on the same connection. Both must land.
func TestConcurrentConnectionWrites_FeaturesAndPathMappings(t *testing.T) {
	t.Parallel()

	const rounds = 25
	for round := 0; round < rounds; round++ {
		r := newConnectionTestRouter(t)
		id := seedEmbyConn(t, r)

		wantHost := fmt.Sprintf("/music-%d", round)
		wantPlatform := fmt.Sprintf("/data-%d", round)

		var wg sync.WaitGroup
		wg.Add(2)

		go func() {
			defer wg.Done()
			body, err := json.Marshal(map[string]any{"feature_image_write": true})
			if err != nil {
				t.Errorf("round %d: marshal PATCH body: %v", round, err)
				return
			}
			req := httptest.NewRequest(http.MethodPatch, "/api/v1/connections/"+id+"/features", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.SetPathValue("id", id)
			w := httptest.NewRecorder()
			r.handleUpdateConnectionFeatures(w, req)
			if w.Code != http.StatusOK {
				t.Errorf("round %d: handleUpdateConnectionFeatures status = %d, body %s", round, w.Code, w.Body.String())
			}
		}()

		go func() {
			defer wg.Done()
			payload := fmt.Sprintf(`{"path_mappings":[{"host_prefix":%q,"platform_prefix":%q}]}`, wantHost, wantPlatform)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/connections/"+id+"/path-mappings", bytes.NewReader([]byte(payload)))
			req.Header.Set("Content-Type", "application/json")
			req.SetPathValue("id", id)
			w := httptest.NewRecorder()
			r.handleSetPathMappings(w, req)
			if w.Code != http.StatusOK {
				t.Errorf("round %d: handleSetPathMappings status = %d, body %s", round, w.Code, w.Body.String())
			}
		}()

		wg.Wait()

		got, err := r.connectionService.GetByID(context.Background(), id)
		if err != nil {
			t.Fatalf("round %d: reload: %v", round, err)
		}
		if !got.GetFeatureImageWrite() {
			t.Errorf("round %d: LOST UPDATE: feature_image_write = false, want true", round)
		}
		mappings := got.GetPathMappings()
		if len(mappings) != 1 || mappings[0].HostPrefix != wantHost || mappings[0].PlatformPrefix != wantPlatform {
			t.Errorf("round %d: LOST UPDATE: PathMappings = %+v, want one %s->%s", round, mappings, wantHost, wantPlatform)
		}
	}
}
