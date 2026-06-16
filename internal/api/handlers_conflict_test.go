package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/conflict"
	"github.com/sydlexius/stillwater/internal/connection"
)

// fakeRepo serves a canned Connection list to the conflict detector without
// involving the DB. Only the fields the detector reads are populated.
type fakeRepo struct {
	conns []connection.Connection
}

func (f *fakeRepo) List(context.Context) ([]connection.Connection, error) {
	return f.conns, nil
}

func testDiscardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newConflictHarness(t *testing.T, conns []connection.Connection) *Router {
	t.Helper()
	d := conflict.NewForTest(&fakeRepo{conns: conns}, testDiscardLogger())
	return &Router{
		logger:           testDiscardLogger(),
		conflictDetector: d,
		conflictGate:     conflict.NewGate(d),
	}
}

func TestHandleGetConflicts_ReturnsLedger(t *testing.T) {
	t.Parallel()
	r := newConflictHarness(t, []connection.Connection{
		{ID: "a", Name: "A", Type: connection.TypeEmby, Enabled: true},
	})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/conflicts", nil)
	w := httptest.NewRecorder()
	r.handleGetConflicts(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var body conflict.Ledger
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding body: %v", err)
	}
	if len(body.Connections) != 1 {
		t.Errorf("want 1 connection, got %d", len(body.Connections))
	}
}

func TestHandleGetConflicts_503WhenDetectorMissing(t *testing.T) {
	t.Parallel()
	r := &Router{logger: testDiscardLogger()}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/conflicts", nil)
	w := httptest.NewRecorder()
	r.handleGetConflicts(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

func TestHandleGetConflicts_RefreshInvalidatesCache(t *testing.T) {
	t.Parallel()
	r := newConflictHarness(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/conflicts?refresh=1", nil)
	w := httptest.NewRecorder()
	r.handleGetConflicts(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

func TestHandleGetConflictBanner_RendersHTML(t *testing.T) {
	t.Parallel()
	r := newConflictHarness(t, []connection.Connection{
		{ID: "a", Name: "A", Type: connection.TypeEmby, Enabled: true},
	})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/config/conflict-banner", nil)
	w := httptest.NewRecorder()
	r.handleGetConflictBanner(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	ct := w.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type = %q, want text/html", ct)
	}
}

func TestHandleGetConflictBanner_204WhenDetectorMissing(t *testing.T) {
	t.Parallel()
	r := &Router{logger: testDiscardLogger()}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/config/conflict-banner", nil)
	w := httptest.NewRecorder()
	r.handleGetConflictBanner(w, req)
	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", w.Code)
	}
}

func TestHandleGetConflictBanner_RefreshInvalidatesCache(t *testing.T) {
	t.Parallel()
	r := newConflictHarness(t, []connection.Connection{
		{ID: "a", Name: "A", Type: connection.TypeEmby, Enabled: true},
	})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/config/conflict-banner?refresh=1", nil)
	w := httptest.NewRecorder()
	r.handleGetConflictBanner(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type = %q, want text/html", ct)
	}
}

func TestHandleGetConnectionConflictDetail_RendersForKnownID(t *testing.T) {
	t.Parallel()
	r := newConflictHarness(t, []connection.Connection{
		{ID: "abc", Name: "Emby One", Type: connection.TypeEmby, Enabled: true},
	})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/connections/abc/conflict-detail", nil)
	req.SetPathValue("id", "abc")
	w := httptest.NewRecorder()
	r.handleGetConnectionConflictDetail(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	ct := w.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type = %q, want text/html", ct)
	}
}

func TestHandleGetConnectionConflictDetail_204WhenMissing(t *testing.T) {
	t.Parallel()
	r := &Router{logger: testDiscardLogger()}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/connections/abc/conflict-detail", nil)
	req.SetPathValue("id", "abc")
	w := httptest.NewRecorder()
	r.handleGetConnectionConflictDetail(w, req)
	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", w.Code)
	}
}

func TestHandleSetStillwaterManaged_RejectsMissingID(t *testing.T) {
	t.Parallel()
	r := newConflictHarness(t, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/connections//stillwater-managed", bytes.NewReader([]byte(`{"enabled":true}`)))
	req.SetPathValue("id", "")
	w := httptest.NewRecorder()
	r.handleSetStillwaterManaged(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestHandleSetStillwaterManaged_RejectsBadJSON(t *testing.T) {
	t.Parallel()
	r := newConflictHarness(t, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/connections/abc/stillwater-managed", bytes.NewReader([]byte(`{not json`)))
	req.SetPathValue("id", "abc")
	w := httptest.NewRecorder()
	r.handleSetStillwaterManaged(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// TestHandleSetStillwaterManaged_RejectsEmptyBody pins the strict-validation
// contract: an empty body with no query-param fallback must return 400, not
// silently coerce to enabled=false (which would be a destructive state change
// triggered by a dropped HTMX body or a curl typo).
func TestHandleSetStillwaterManaged_RejectsEmptyBody(t *testing.T) {
	t.Parallel()
	r := newConflictHarness(t, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/connections/abc/stillwater-managed", bytes.NewReader(nil))
	req.SetPathValue("id", "abc")
	w := httptest.NewRecorder()
	r.handleSetStillwaterManaged(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// TestHandleSetStillwaterManaged_RejectsJSONWithoutEnabled covers the case
// where the body is well-formed JSON but omits the "enabled" key. Coercing
// to false here would silently flip the toggle off on any caller that sent
// {"foo":"bar"} or {} -- exactly what strict validation is meant to prevent.
func TestHandleSetStillwaterManaged_RejectsJSONWithoutEnabled(t *testing.T) {
	t.Parallel()
	r := newConflictHarness(t, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/connections/abc/stillwater-managed", bytes.NewReader([]byte(`{"foo":"bar"}`)))
	req.SetPathValue("id", "abc")
	w := httptest.NewRecorder()
	r.handleSetStillwaterManaged(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// TestHandleSetStillwaterManaged_RejectsInvalidEnabledValue covers the case
// where "enabled" is present but not parseable as bool (string "maybe", a
// number, etc.).
func TestHandleSetStillwaterManaged_RejectsInvalidEnabledValue(t *testing.T) {
	t.Parallel()
	r := newConflictHarness(t, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/connections/abc/stillwater-managed", bytes.NewReader([]byte(`{"enabled":"maybe"}`)))
	req.SetPathValue("id", "abc")
	w := httptest.NewRecorder()
	r.handleSetStillwaterManaged(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// TestHandleSetStillwaterManaged_RejectsFormBodyWithoutEnabled covers the
// HTMX-style application/x-www-form-urlencoded path. A form body that omits
// "enabled" or sends a malformed value (e.g. enabled=maybe) must 400.
func TestHandleSetStillwaterManaged_RejectsFormBodyWithoutEnabled(t *testing.T) {
	t.Parallel()
	r := newConflictHarness(t, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/connections/abc/stillwater-managed", bytes.NewReader([]byte(`other=field`)))
	req.SetPathValue("id", "abc")
	w := httptest.NewRecorder()
	r.handleSetStillwaterManaged(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestWriteConflictError_EmitsExpectedShape(t *testing.T) {
	t.Parallel()
	w := httptest.NewRecorder()
	writeConflictError(w, &conflict.BlockedError{
		Axis:   conflict.AxisImage,
		Reason: "blocked for test",
	})
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response body: err = %v; body = %s", err, w.Body.String())
	}
	if body["axis"] != "image" {
		t.Errorf("axis = %v, want image", body["axis"])
	}
	if body["error"] != "image_write_blocked" {
		t.Errorf("error = %v, want image_write_blocked", body["error"])
	}
	if body["reason"] != "blocked for test" {
		t.Errorf("reason = %v, want %q", body["reason"], "blocked for test")
	}
	if _, ok := body["ledger"].(map[string]any); !ok {
		t.Errorf("ledger should be a JSON object, got %T (%v)", body["ledger"], body["ledger"])
	}
}

func TestConflictBannerView_PopulatesFromLedger(t *testing.T) {
	t.Parallel()
	l := conflict.Ledger{
		Connections: []conflict.ConnectionState{
			{ConnectionID: "a", ConnectionName: "A", ConnectionType: connection.TypeEmby,
				Enabled: true, ImageWriteback: true, NFOWriteback: false, LibraryName: "Music"},
			// managed, no error: should appear in ManagedConnections
			{ConnectionID: "b", ConnectionName: "B", ConnectionType: connection.TypeJellyfin,
				Enabled: true, ManageServerFiles: true, ImageWriteback: true},
			// disabled: filtered out of both lists
			{ConnectionID: "c", ConnectionName: "C", ConnectionType: connection.TypeLidarr,
				Enabled: false, NFOWriteback: true},
			// managed but with a detection error: omitted from ManagedConnections
			{ConnectionID: "d", ConnectionName: "D", ConnectionType: connection.TypeEmby,
				Enabled: true, ManageServerFiles: true, CheckErr: "connection refused"},
		},
		RoundTrips: []conflict.RoundTrip{{ConnectionAName: "A", ConnectionBName: "B", OverlappingPath: "/music"}},
	}
	view := conflictBannerView(l)
	if len(view.Connections) != 1 || view.Connections[0].ID != "a" {
		t.Errorf("connections should include only unmanaged enabled peers with conflicts: %+v", view.Connections)
	}
	if len(view.RoundTrips) != 1 || view.RoundTrips[0].Path != "/music" {
		t.Errorf("round trips should pass through: %+v", view.RoundTrips)
	}
	if len(view.ManagedConnections) != 1 || view.ManagedConnections[0].Name != "B" {
		t.Errorf("ManagedConnections should include only enabled error-free managed connections: %+v", view.ManagedConnections)
	}
}

func TestWriteConflictError_AllAxes(t *testing.T) {
	t.Parallel()
	for _, axis := range []conflict.Axis{conflict.AxisImage, conflict.AxisNFO, conflict.AxisRoundTrip} {
		w := httptest.NewRecorder()
		writeConflictError(w, &conflict.BlockedError{Axis: axis, Reason: "x"})
		if w.Code != http.StatusConflict {
			t.Errorf("axis=%s status=%d", axis, w.Code)
		}
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("axis=%s decode response: err = %v; body = %s", axis, err, w.Body.String())
		}
		if body["axis"] != string(axis) {
			t.Errorf("axis=%s got %v", axis, body["axis"])
		}
		if body["reason"] != "x" {
			t.Errorf("axis=%s reason = %v, want %q", axis, body["reason"], "x")
		}
		if _, ok := body["ledger"].(map[string]any); !ok {
			t.Errorf("axis=%s ledger should be a JSON object, got %T", axis, body["ledger"])
		}
	}
}

func TestGateImageWrite_AllowsCleanWrite(t *testing.T) {
	t.Parallel()
	r := newConflictHarness(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	if !r.gateImageWrite(w, req) {
		t.Error("clean gate should allow writes")
	}
}

func TestGateImageWrite_Blocks409(t *testing.T) {
	t.Parallel()
	d := conflict.NewBlockingForTest(testDiscardLogger())
	r := &Router{
		logger:           testDiscardLogger(),
		conflictDetector: d,
		conflictGate:     conflict.NewGate(d),
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	if r.gateImageWrite(w, req) {
		t.Fatal("blocking detector should refuse the write")
	}
	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", w.Code)
	}
}

func TestGateNFOWrite_Blocks409(t *testing.T) {
	t.Parallel()
	d := conflict.NewBlockingForTest(testDiscardLogger())
	r := &Router{
		logger:           testDiscardLogger(),
		conflictDetector: d,
		conflictGate:     conflict.NewGate(d),
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	if r.gateNFOWrite(w, req) {
		t.Fatal("blocking detector should refuse the write")
	}
	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", w.Code)
	}
}

func TestGateNFOWrite_AllowsCleanWrite(t *testing.T) {
	t.Parallel()
	// The blocked-write 409 shape is covered by TestWriteConflictError_EmitsExpectedShape
	// here and by TestGateBlocksOn{Image,NFO,RoundTrip}Conflict in the
	// conflict package. This test pins down the clean-allow path for the NFO
	// axis equivalent of gateImageWrite.
	r := newConflictHarness(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	if !r.gateNFOWrite(w, req) {
		t.Error("clean gate should allow NFO writes")
	}
}

// TestHandleSetStillwaterManaged_FormBodyInvalidValue covers the form-body
// branch where "enabled" is present but unparsable (e.g. "maybe"). This
// path returns 400 from inside parseBoolStrict.
func TestHandleSetStillwaterManaged_FormBodyInvalidValue(t *testing.T) {
	t.Parallel()
	r := newConflictHarness(t, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/connections/abc/stillwater-managed", bytes.NewReader([]byte(`enabled=maybe`)))
	req.SetPathValue("id", "abc")
	w := httptest.NewRecorder()
	r.handleSetStillwaterManaged(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// TestHandleSetStillwaterManaged_QueryParamInvalidValue covers the
// query-string fallback where ?enabled=??? is unparsable. The handler
// must reject with 400 from the URL branch, not silently fall through.
func TestHandleSetStillwaterManaged_QueryParamInvalidValue(t *testing.T) {
	t.Parallel()
	r := newConflictHarness(t, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/connections/abc/stillwater-managed?enabled=banana", nil)
	req.SetPathValue("id", "abc")
	w := httptest.NewRecorder()
	r.handleSetStillwaterManaged(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// TestRestoreLibraryOptions_RejectsUnsupportedType pins the default branch
// of the restore dispatch so an unsupported connection type can't silently
// no-op. The same default exists in snapshotLibraryOptions and
// disableFileWriteBack; testing one keeps the policy assertion local
// without standing up three near-identical fakes.
func TestRestoreLibraryOptions_RejectsUnsupportedType(t *testing.T) {
	t.Parallel()
	r := &Router{logger: testDiscardLogger()}
	err := r.restoreLibraryOptions(context.Background(), &connection.Connection{Type: "unknown"}, "{}")
	if err == nil {
		t.Fatal("restoreLibraryOptions on unsupported type should error")
	}
	got := fmt.Sprint(err)
	if !strings.Contains(got, "unsupported") {
		t.Errorf("error message = %q, should mention 'unsupported'", got)
	}
}

// TestParseBoolStrict exercises every branch of the strict bool parser:
// truthy forms, falsy forms, leading/trailing whitespace handling, and the
// rejection (false, false) signal that lets the handler distinguish a
// missing or garbled value from an explicit false.
func TestParseBoolStrict(t *testing.T) {
	t.Parallel()
	truthy := []string{"1", "true", "TRUE", "On", "yes", "y", "t", "  true  "}
	falsy := []string{"0", "false", "FALSE", "off", "no", "n", "f", " 0\n"}
	rejected := []string{"", "maybe", "2", "tr", "yes please"}

	for _, s := range truthy {
		got, ok := parseBoolStrict(s)
		if !ok || !got {
			t.Errorf("parseBoolStrict(%q) = (%v, %v), want (true, true)", s, got, ok)
		}
	}
	for _, s := range falsy {
		got, ok := parseBoolStrict(s)
		if !ok || got {
			t.Errorf("parseBoolStrict(%q) = (%v, %v), want (false, true)", s, got, ok)
		}
	}
	for _, s := range rejected {
		got, ok := parseBoolStrict(s)
		if ok {
			t.Errorf("parseBoolStrict(%q) = (%v, %v), want (_, false)", s, got, ok)
		}
	}
}

// TestStillwaterManagedErrorResponse pins the 502 vs 500 mapping: a
// peer-rejected error must surface with the caller-supplied peer message
// (which differs between apply and clear), a local-persist error must
// surface as 500 with a generic internal message, and any unwrapped error
// falls through to the historical 502 so unrelated regressions do not
// silently change the status code.
func TestStillwaterManagedErrorResponse(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		err      error
		peerMsg  string
		wantCode int
		wantMsg  string
	}{
		{
			name:     "peer-rejected uses peer message and 502",
			err:      fmt.Errorf("%w: snapshotting peer config: peer down", ErrConflictPeerRejected),
			peerMsg:  "peer rejected snapshot or disable; see server log",
			wantCode: http.StatusBadGateway,
			wantMsg:  "peer rejected snapshot or disable; see server log",
		},
		{
			name:     "local-persist uses internal message and 500",
			err:      fmt.Errorf("%w: persisting snapshot: db locked", ErrConflictLocalPersist),
			peerMsg:  "peer rejected snapshot or disable; see server log",
			wantCode: http.StatusInternalServerError,
			wantMsg:  "stillwater failed to persist managed-mode change; see server log",
		},
		{
			name:     "unwrapped error falls through to peer message and 502",
			err:      errors.New("plain"),
			peerMsg:  "peer rejected restore; see server log",
			wantCode: http.StatusBadGateway,
			wantMsg:  "peer rejected restore; see server log",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, msg := stillwaterManagedErrorResponse(c.err, c.peerMsg)
			if code != c.wantCode {
				t.Errorf("code = %d, want %d", code, c.wantCode)
			}
			if msg != c.wantMsg {
				t.Errorf("msg = %q, want %q", msg, c.wantMsg)
			}
		})
	}
}
