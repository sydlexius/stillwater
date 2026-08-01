package api

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/connection"
)

// TestHandleUpdateConnection_AcceptsFormEncodedBody covers the settings-page
// edit form, which submits application/x-www-form-urlencoded (#2886). The
// handler decoded JSON unconditionally, so every edit -- name, URL, API key --
// failed with 400 "invalid request body" before any work happened. The sibling
// handleCreateConnection has always branched on Content-Type, which is why an
// operator could add a connection through the UI but never edit one.
func TestHandleUpdateConnection_AcceptsFormEncodedBody(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)

	c := &connection.Connection{
		Name: "Before", Type: connection.TypeEmby,
		URL: "http://emby.local:8096", APIKey: "old-key", Enabled: true,
	}
	newConnectionTestConn(t, r, c)
	// Non-default starting state for all three feature flags, so the
	// leave-unchanged assertions below cannot pass by coinciding with the
	// zero value a naive form path would collapse them to.
	c.SetFeatures(true, true, true)
	if err := r.connectionService.Update(context.Background(), c); err != nil {
		t.Fatalf("seeding features: %v", err)
	}

	form := url.Values{}
	form.Set("name", "After")
	form.Set("url", "http://emby.local:9096")
	form.Set("api_key", "new-key")

	req := httptest.NewRequest(http.MethodPut, "/api/v1/connections/"+c.ID, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("id", c.ID)

	w := serveValidated(t, http.HandlerFunc(r.handleUpdateConnection), req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}

	got, err := r.connectionService.GetByID(context.Background(), c.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "After" {
		t.Errorf("Name = %q, want %q", got.Name, "After")
	}
	if got.URL != "http://emby.local:9096" {
		t.Errorf("URL = %q, want %q", got.URL, "http://emby.local:9096")
	}
	if got.APIKey != "new-key" {
		t.Errorf("APIKey = %q, want %q", got.APIKey, "new-key")
	}

	// The edit form renders no checkbox for Enabled or the three feature
	// toggles. The JSON path models that with *bool ("absent means leave
	// unchanged"), and the form path must preserve the distinction rather
	// than collapsing an unrendered field into false.
	if !got.Enabled {
		t.Error("Enabled = false, want true (absent field must leave it unchanged)")
	}
	if !got.GetFeatureImageWrite() {
		t.Error("FeatureImageWrite = false, want true (absent field must leave it unchanged)")
	}
	if !got.GetFeatureMetadataPush() {
		t.Error("FeatureMetadataPush = false, want true (absent field must leave it unchanged)")
	}
	if !got.GetFeatureTriggerRefresh() {
		t.Error("FeatureTriggerRefresh = false, want true (absent field must leave it unchanged)")
	}
}

// TestHandleUpdateConnection_FormBoolsAreApplied is the counterpart to the
// leave-unchanged assertions above: a form field that IS present must take
// effect. Without this, a fix that ignored form booleans entirely would pass
// the absent-field test vacuously.
func TestHandleUpdateConnection_FormBoolsAreApplied(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)

	c := &connection.Connection{
		Name: "Toggle", Type: connection.TypeEmby,
		URL: "http://emby.local:8096", APIKey: "k", Enabled: true,
	}
	newConnectionTestConn(t, r, c)
	c.SetFeatures(true, false, true)
	if err := r.connectionService.Update(context.Background(), c); err != nil {
		t.Fatalf("seeding features: %v", err)
	}

	// Each field is driven AWAY from its seeded value, so a fix that wrote a
	// constant, or cross-wired two columns, fails rather than passing.
	form := url.Values{}
	form.Set("enabled", "false")
	form.Set("feature_image_write", "false")
	form.Set("feature_metadata_push", "true")
	form.Set("feature_trigger_refresh", "false")

	req := httptest.NewRequest(http.MethodPut, "/api/v1/connections/"+c.ID, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("id", c.ID)

	w := serveValidated(t, http.HandlerFunc(r.handleUpdateConnection), req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}

	got, err := r.connectionService.GetByID(context.Background(), c.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Enabled {
		t.Error("Enabled = true, want false")
	}
	if got.GetFeatureImageWrite() {
		t.Error("FeatureImageWrite = true, want false")
	}
	if !got.GetFeatureMetadataPush() {
		t.Error("FeatureMetadataPush = false, want true")
	}
	if got.GetFeatureTriggerRefresh() {
		t.Error("FeatureTriggerRefresh = true, want false")
	}
}

// TestHandleUpdateConnection_MalformedFormBoolLeavesValueUnchanged covers the
// deliberate leniency in formBoolPtr: a present-but-unparsable boolean must
// leave the stored value alone rather than collapsing to false, because a
// garbled value must never disable a connection or clear its write features.
// The handler logs it; the operator's configuration survives.
func TestHandleUpdateConnection_MalformedFormBoolLeavesValueUnchanged(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)

	c := &connection.Connection{
		Name: "Garbled", Type: connection.TypeEmby,
		URL: "http://emby.local:8096", APIKey: "k", Enabled: true,
	}
	newConnectionTestConn(t, r, c)
	c.SetFeatures(true, true, true)
	if err := r.connectionService.Update(context.Background(), c); err != nil {
		t.Fatalf("seeding features: %v", err)
	}

	form := url.Values{}
	form.Set("name", "Garbled")
	form.Set("enabled", "xyz")
	form.Set("feature_image_write", "")

	req := httptest.NewRequest(http.MethodPut, "/api/v1/connections/"+c.ID, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("id", c.ID)

	// Deliberately NOT served through serveValidated. The OpenAPI wrapper is a
	// test-only harness -- no such middleware runs in production (nothing
	// outside _test.go references validateExchange) -- so it would reject the
	// malformed value before the handler ever saw it and this test would
	// assert the spec's behavior instead of the handler's. A real client can
	// and does reach the handler with a garbled boolean, which is exactly the
	// path being pinned here.
	w := httptest.NewRecorder()
	r.handleUpdateConnection(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}

	got, err := r.connectionService.GetByID(context.Background(), c.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.Enabled {
		t.Error("Enabled = false, want true (malformed value must not disable the connection)")
	}
	if !got.GetFeatureImageWrite() {
		t.Error("FeatureImageWrite = false, want true (empty value must not clear the feature)")
	}
}

// TestHandleUpdateConnection_QueryParamsCannotMutate pins that the request
// BODY is the only sanctioned source for an update.
//
// req.FormValue merges the URL query into the body, so reading fields through
// it would let a query string rewrite a connection's URL or API key on a
// request whose body never mentioned them -- and a query-driven `type` change
// additionally clears the platform sub-config, taking all three feature flags
// with it. The handler reads req.PostForm for that reason; this test fails if
// anyone switches it back.
func TestHandleUpdateConnection_QueryParamsCannotMutate(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)

	c := &connection.Connection{
		Name: "Original", Type: connection.TypeEmby,
		URL: "http://emby.local:8096", APIKey: "real-key", Enabled: true,
	}
	newConnectionTestConn(t, r, c)
	c.SetFeatures(true, true, true)
	if err := r.connectionService.Update(context.Background(), c); err != nil {
		t.Fatalf("seeding features: %v", err)
	}

	// A legitimate body edit, with every mutable field also present in the
	// query string. Only the body values may land.
	form := url.Values{}
	form.Set("name", "Renamed By Body")

	target := "/api/v1/connections/" + c.ID +
		"?url=http://attacker.local&api_key=WIPED&type=jellyfin&enabled=false"
	req := httptest.NewRequest(http.MethodPut, target, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("id", c.ID)

	w := httptest.NewRecorder()
	r.handleUpdateConnection(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}

	got, err := r.connectionService.GetByID(context.Background(), c.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "Renamed By Body" {
		t.Errorf("Name = %q, want the body's value", got.Name)
	}
	if got.URL != "http://emby.local:8096" {
		t.Errorf("URL = %q, want unchanged: a query parameter must not rewrite it", got.URL)
	}
	if got.APIKey != "real-key" {
		t.Error("APIKey was overwritten from the query string")
	}
	if got.Type != connection.TypeEmby {
		t.Errorf("Type = %q, want unchanged: a query-driven type change clears the sub-config", got.Type)
	}
	if !got.Enabled {
		t.Error("Enabled = false: a query parameter disabled the connection")
	}
	if !got.GetFeatureImageWrite() || !got.GetFeatureMetadataPush() || !got.GetFeatureTriggerRefresh() {
		t.Error("feature flags were cleared via the query string")
	}
}

// TestHandleUpdateConnection_ContentTypeBranching pins which media types are
// treated as form submissions.
//
// The form branch is an explicit allowlist and JSON is the fallback, because
// the failure modes are asymmetric: form-parsing a JSON body SUCCEEDS (the
// document becomes one key with an empty value), so every field reads empty,
// every guard skips, and the handler would answer 200 OK having written
// nothing. Decoding a form body as JSON fails loudly instead. An unclassified
// body must therefore produce an error, never a silently discarded write.
func TestHandleUpdateConnection_ContentTypeBranching(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		contentType string
		body        string
		wantCode    int
		wantName    string
	}{
		{"plain json", "application/json", `{"name":"ViaJSON"}`, http.StatusOK, "ViaJSON"},
		{"json with charset", "application/json; charset=utf-8", `{"name":"ViaJSON"}`, http.StatusOK, "ViaJSON"},
		{"json uppercase", "APPLICATION/JSON", `{"name":"ViaJSON"}`, http.StatusOK, "ViaJSON"},
		{"form", "application/x-www-form-urlencoded", "name=ViaForm", http.StatusOK, "ViaForm"},
		{"form with charset", "application/x-www-form-urlencoded; charset=utf-8", "name=ViaForm", http.StatusOK, "ViaForm"},
		// A JSON body with no Content-Type, or an unrecognized one, must NOT be
		// form-parsed into a silent no-op. It falls to JSON and succeeds here;
		// a body that is neither is rejected by the case below.
		{"absent content-type with json body", "", `{"name":"ViaJSON"}`, http.StatusOK, "ViaJSON"},
		{"unrecognized content-type with json body", "text/plain", `{"name":"ViaJSON"}`, http.StatusOK, "ViaJSON"},
		{"unclassifiable body is rejected loudly", "text/plain", "name=ViaForm", http.StatusBadRequest, "Before"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newConnectionTestRouter(t)
			c := &connection.Connection{
				Name: "Before", Type: connection.TypeEmby,
				URL: "http://emby.local:8096", APIKey: "k", Enabled: true,
			}
			newConnectionTestConn(t, r, c)

			req := httptest.NewRequest(http.MethodPut, "/api/v1/connections/"+c.ID, strings.NewReader(tc.body))
			if tc.contentType != "" {
				req.Header.Set("Content-Type", tc.contentType)
			}
			req.SetPathValue("id", c.ID)

			w := httptest.NewRecorder()
			r.handleUpdateConnection(w, req)
			if w.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d (body %s)", w.Code, tc.wantCode, w.Body.String())
			}

			got, err := r.connectionService.GetByID(context.Background(), c.ID)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if got.Name != tc.wantName {
				t.Errorf("Name = %q, want %q", got.Name, tc.wantName)
			}
		})
	}
}

// TestHandleUpdateConnection_MalformedBoolIsLogged pins the visible half of
// the lenient-but-logged contract. Leniency without the log is an ordinary
// silent failure, and nothing else in the suite proves the caller consumes
// formBoolPtr's well-formed flag.
func TestHandleUpdateConnection_MalformedBoolIsLogged(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)

	var logBuf bytes.Buffer
	r.logger = slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	c := &connection.Connection{
		Name: "Logged", Type: connection.TypeEmby,
		URL: "http://emby.local:8096", APIKey: "k", Enabled: true,
	}
	newConnectionTestConn(t, r, c)

	form := url.Values{}
	form.Set("name", "Logged")
	form.Set("enabled", "banana")

	req := httptest.NewRequest(http.MethodPut, "/api/v1/connections/"+c.ID, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("id", c.ID)

	w := httptest.NewRecorder()
	r.handleUpdateConnection(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}

	logged := logBuf.String()
	if !strings.Contains(logged, "malformed boolean") {
		t.Errorf("no warning logged for a malformed boolean; leniency without the log is a silent failure.\nlog: %s", logged)
	}
	if !strings.Contains(logged, "enabled") {
		t.Errorf("the warning does not name the offending field.\nlog: %s", logged)
	}
}

// TestFormBoolPtr covers the checkbox encoding directly. A checked HTML
// checkbox with no value attribute posts the literal string "on", which
// strconv.ParseBool rejects, so without the special case every checkbox in a
// form body would read as malformed and be ignored.
func TestFormBoolPtr(t *testing.T) {
	t.Parallel()

	newReq := func(body string) *http.Request {
		req := httptest.NewRequest(http.MethodPut, "/x", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return req
	}

	tests := []struct {
		name           string
		body           string
		wantVal        *bool
		wantWellFormed bool
	}{
		{"checked checkbox posts on", "flag=on", ptrTo(true), true},
		{"uppercase ON is accepted", "flag=ON", ptrTo(true), true},
		{"explicit true", "flag=true", ptrTo(true), true},
		{"explicit false", "flag=false", ptrTo(false), true},
		{"absent key leaves unchanged", "other=1", nil, true},
		{"present but empty is malformed", "flag=", nil, false},
		{"garbage is malformed", "flag=banana", nil, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, wellFormed := formBoolPtr(newReq(tc.body), "flag")
			if wellFormed != tc.wantWellFormed {
				t.Errorf("wellFormed = %v, want %v", wellFormed, tc.wantWellFormed)
			}
			switch {
			case tc.wantVal == nil && got != nil:
				t.Errorf("value = %v, want nil", *got)
			case tc.wantVal != nil && got == nil:
				t.Errorf("value = nil, want %v", *tc.wantVal)
			case tc.wantVal != nil && *got != *tc.wantVal:
				t.Errorf("value = %v, want %v", *got, *tc.wantVal)
			}
		})
	}
}

func ptrTo[T any](v T) *T { return &v }

// TestHandleUpdateConnection_RejectsMalformedJSON pins the JSON path's
// existing behavior so the Content-Type branch cannot silently turn a
// malformed JSON body into a form parse that succeeds with empty values.
func TestHandleUpdateConnection_RejectsMalformedJSON(t *testing.T) {
	t.Parallel()
	r := newConnectionTestRouter(t)

	c := &connection.Connection{
		Name: "Keep", Type: connection.TypeEmby,
		URL: "http://emby.local:8096", APIKey: "k", Enabled: true,
	}
	newConnectionTestConn(t, r, c)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/connections/"+c.ID, strings.NewReader("{not json"))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", c.ID)

	w := httptest.NewRecorder()
	r.handleUpdateConnection(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}

	got, err := r.connectionService.GetByID(context.Background(), c.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "Keep" {
		t.Errorf("Name = %q, want %q (rejected body must not mutate)", got.Name, "Keep")
	}
}
