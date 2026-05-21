package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/provider/tagdict"
)

// TestHandleGetVocab_Default verifies that GET /api/v1/settings/vocab returns
// the default config (empty exclude list, zero caps) when the setting has
// never been saved.
func TestHandleGetVocab_Default(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/settings/vocab", nil)
	w := httptest.NewRecorder()
	r.handleGetVocab(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var cfg tagdict.VocabConfig
	if err := json.NewDecoder(w.Body).Decode(&cfg); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	if len(cfg.Exclude) != 0 {
		t.Errorf("default exclude list should be empty, got %v", cfg.Exclude)
	}
	if cfg.MaxGenres != 0 || cfg.MaxStyles != 0 || cfg.MaxMoods != 0 {
		t.Errorf("default caps should all be 0, got g=%d s=%d m=%d",
			cfg.MaxGenres, cfg.MaxStyles, cfg.MaxMoods)
	}
}

// TestHandleGetVocab_DefaultExcludeNotNull verifies the default GET response
// serializes "exclude" as [] rather than null (so UI clients can iterate it).
func TestHandleGetVocab_DefaultExcludeNotNull(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/settings/vocab", nil)
	w := httptest.NewRecorder()
	r.handleGetVocab(w, req)

	if !strings.Contains(w.Body.String(), `"exclude":[]`) {
		t.Errorf("expected exclude to serialize as [], body: %s", w.Body.String())
	}
}

// TestHandlePutVocab_RoundTrip verifies that a PUT followed by a GET returns
// the same config that was PUT.
func TestHandlePutVocab_RoundTrip(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	body := `{"exclude":["christian","*core"],"max_genres":5,"max_styles":0,"max_moods":3}`
	putReq := httptest.NewRequest(http.MethodPut, "/api/v1/settings/vocab", strings.NewReader(body))
	putW := httptest.NewRecorder()
	r.handlePutVocab(putW, putReq)
	if putW.Code != http.StatusOK {
		t.Fatalf("PUT returned %d: %s", putW.Code, putW.Body.String())
	}

	getReq := httptest.NewRequest(http.MethodGet, "/api/v1/settings/vocab", nil)
	getW := httptest.NewRecorder()
	r.handleGetVocab(getW, getReq)
	if getW.Code != http.StatusOK {
		t.Fatalf("GET returned %d: %s", getW.Code, getW.Body.String())
	}

	var cfg tagdict.VocabConfig
	if err := json.NewDecoder(getW.Body).Decode(&cfg); err != nil {
		t.Fatalf("decoding GET response: %v", err)
	}
	if len(cfg.Exclude) != 2 || cfg.Exclude[0] != "christian" || cfg.Exclude[1] != "*core" {
		t.Errorf("unexpected exclude list: %v", cfg.Exclude)
	}
	if cfg.MaxGenres != 5 || cfg.MaxStyles != 0 || cfg.MaxMoods != 3 {
		t.Errorf("unexpected caps: g=%d s=%d m=%d", cfg.MaxGenres, cfg.MaxStyles, cfg.MaxMoods)
	}
}

// TestHandlePutVocab_UnknownField verifies that an unknown top-level key in
// the request body is rejected with 400 (DisallowUnknownFields).
func TestHandlePutVocab_UnknownField(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	body := `{"excludes": ["x"]}` // typo: "excludes" instead of "exclude"
	req := httptest.NewRequest(http.MethodPut, "/api/v1/settings/vocab", strings.NewReader(body))
	w := httptest.NewRecorder()
	r.handlePutVocab(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown field, got %d: %s", w.Code, w.Body.String())
	}
}

// TestHandlePutVocab_TrailingJSON verifies that a body with a valid object
// followed by trailing JSON is rejected with 400 (strict single-object decode).
func TestHandlePutVocab_TrailingJSON(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	body := `{"max_genres":1}{"junk":1}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/settings/vocab", strings.NewReader(body))
	w := httptest.NewRecorder()
	r.handlePutVocab(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for trailing JSON after the first object, got %d: %s", w.Code, w.Body.String())
	}
}

// TestHandlePutVocab_InvalidJSON verifies that a malformed JSON body is
// rejected with 400.
func TestHandlePutVocab_InvalidJSON(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/settings/vocab", strings.NewReader(`{invalid`))
	w := httptest.NewRecorder()
	r.handlePutVocab(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid JSON, got %d: %s", w.Code, w.Body.String())
	}
}

// TestHandlePutVocab_BlankExcludePattern verifies a blank exclude pattern is
// rejected with 400.
func TestHandlePutVocab_BlankExcludePattern(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	body := `{"exclude":["rock","  "]}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/settings/vocab", strings.NewReader(body))
	w := httptest.NewRecorder()
	r.handlePutVocab(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for blank exclude pattern, got %d: %s", w.Code, w.Body.String())
	}
}

// TestHandlePutVocab_NegativeCap verifies a negative per-field cap is rejected
// with 400.
func TestHandlePutVocab_NegativeCap(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	body := `{"max_genres":-1}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/settings/vocab", strings.NewReader(body))
	w := httptest.NewRecorder()
	r.handlePutVocab(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for negative cap, got %d: %s", w.Code, w.Body.String())
	}
}

// TestHandlePutVocab_EmptyBodyIsNoOp verifies that an empty JSON object is a
// valid PUT producing the default no-op config.
func TestHandlePutVocab_EmptyBodyIsNoOp(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/settings/vocab", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	r.handlePutVocab(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for empty object, got %d: %s", w.Code, w.Body.String())
	}

	getReq := httptest.NewRequest(http.MethodGet, "/api/v1/settings/vocab", nil)
	getW := httptest.NewRecorder()
	r.handleGetVocab(getW, getReq)

	var cfg tagdict.VocabConfig
	if err := json.NewDecoder(getW.Body).Decode(&cfg); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(cfg.Exclude) != 0 || cfg.MaxGenres != 0 {
		t.Errorf("empty PUT did not produce a no-op config: %+v", cfg)
	}
}

// TestHandlePutVocab_FormEncoded_RoundTrip verifies that the form-encoded PUT
// path persists patterns and caps and that a subsequent GET returns the same
// values. This is the code path used by the Settings > Providers > Tag Sources
// HTMX form.
func TestHandlePutVocab_FormEncoded_RoundTrip(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	// Submit the form the way an HTMX hx-put would: URL-encoded body with the
	// exclude textarea content and three integer cap fields.
	body := "exclude=christian%0A*core&max_genres=5&max_styles=0&max_moods=3"
	putReq := httptest.NewRequest(http.MethodPut, "/api/v1/settings/vocab", strings.NewReader(body))
	putReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	putW := httptest.NewRecorder()
	r.handlePutVocab(putW, putReq)
	if putW.Code != http.StatusOK {
		t.Fatalf("form PUT returned %d: %s", putW.Code, putW.Body.String())
	}

	// Verify the persisted config matches what was sent.
	getReq := httptest.NewRequest(http.MethodGet, "/api/v1/settings/vocab", nil)
	getW := httptest.NewRecorder()
	r.handleGetVocab(getW, getReq)
	if getW.Code != http.StatusOK {
		t.Fatalf("GET returned %d: %s", getW.Code, getW.Body.String())
	}

	var cfg tagdict.VocabConfig
	if err := json.NewDecoder(getW.Body).Decode(&cfg); err != nil {
		t.Fatalf("decoding GET response: %v", err)
	}
	if len(cfg.Exclude) != 2 || cfg.Exclude[0] != "christian" || cfg.Exclude[1] != "*core" {
		t.Errorf("form PUT: unexpected exclude list: %v", cfg.Exclude)
	}
	if cfg.MaxGenres != 5 || cfg.MaxStyles != 0 || cfg.MaxMoods != 3 {
		t.Errorf("form PUT: unexpected caps: g=%d s=%d m=%d", cfg.MaxGenres, cfg.MaxStyles, cfg.MaxMoods)
	}
}

// TestHandlePutVocab_FormEncoded_BlankLinesIgnored verifies that blank lines
// in the exclude textarea are stripped rather than persisted as blank patterns.
// This is the expected behavior since blank patterns are meaningless, and the
// form path strips them before validation so the request always succeeds when
// only blank lines are present.
func TestHandlePutVocab_FormEncoded_BlankLinesIgnored(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	// Two real patterns with a blank line in between (as a user might type).
	body := "exclude=rock%0A%0A%0Apop&max_genres=0&max_styles=0&max_moods=0"
	putReq := httptest.NewRequest(http.MethodPut, "/api/v1/settings/vocab", strings.NewReader(body))
	putReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	putW := httptest.NewRecorder()
	r.handlePutVocab(putW, putReq)
	if putW.Code != http.StatusOK {
		t.Fatalf("form PUT returned %d: %s", putW.Code, putW.Body.String())
	}

	// Only the non-blank patterns should be persisted.
	cfg := r.loadVocabConfig(context.Background())
	if len(cfg.Exclude) != 2 {
		t.Errorf("expected 2 patterns after stripping blanks, got %d: %v", len(cfg.Exclude), cfg.Exclude)
	}
}

// TestHandlePutVocab_FormEncoded_NegativeCap verifies that a negative cap
// value in a form PUT is rejected with 400.
func TestHandlePutVocab_FormEncoded_NegativeCap(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	body := "exclude=&max_genres=-1&max_styles=0&max_moods=0"
	putReq := httptest.NewRequest(http.MethodPut, "/api/v1/settings/vocab", strings.NewReader(body))
	putReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	putW := httptest.NewRecorder()
	r.handlePutVocab(putW, putReq)

	if putW.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for negative cap via form, got %d: %s", putW.Code, putW.Body.String())
	}
}

// TestHandlePutVocab_FormEncoded_EmptyFormIsNoOp verifies that submitting the
// form with an empty exclude textarea and all caps at 0 stores the no-op
// default config.
func TestHandlePutVocab_FormEncoded_EmptyFormIsNoOp(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	body := "exclude=&max_genres=0&max_styles=0&max_moods=0"
	putReq := httptest.NewRequest(http.MethodPut, "/api/v1/settings/vocab", strings.NewReader(body))
	putReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	putW := httptest.NewRecorder()
	r.handlePutVocab(putW, putReq)
	if putW.Code != http.StatusOK {
		t.Fatalf("expected 200 for empty form, got %d: %s", putW.Code, putW.Body.String())
	}

	cfg := r.loadVocabConfig(context.Background())
	if len(cfg.Exclude) != 0 || cfg.MaxGenres != 0 || cfg.MaxStyles != 0 || cfg.MaxMoods != 0 {
		t.Errorf("empty form PUT should produce no-op config, got: %+v", cfg)
	}
}

// TestHandlePutVocab_FormEncoded_NonIntegerCapRejected verifies that a
// non-integer cap value in a form PUT is rejected with 400.
func TestHandlePutVocab_FormEncoded_NonIntegerCapRejected(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	body := "exclude=&max_genres=abc&max_styles=0&max_moods=0"
	putReq := httptest.NewRequest(http.MethodPut, "/api/v1/settings/vocab", strings.NewReader(body))
	putReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	putW := httptest.NewRecorder()
	r.handlePutVocab(putW, putReq)

	if putW.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for non-integer cap, got %d: %s", putW.Code, putW.Body.String())
	}
}

// TestLoadVocabConfig_CorruptBlobDegrades verifies that a corrupt stored
// metadata_vocab blob makes loadVocabConfig (the fetch-path loader) degrade to
// the default no-op config rather than failing -- so a bad blob never breaks
// metadata fetches. (handleGetVocab deliberately returns 500 instead; that
// divergence is asserted by the separate handler tests.)
func TestLoadVocabConfig_CorruptBlobDegrades(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)
	ctx := context.Background()

	_, err := r.db.ExecContext(ctx,
		`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)`,
		SettingMetadataVocab, "{not valid json", "2026-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("seeding corrupt setting: %v", err)
	}

	cfg := r.loadVocabConfig(ctx)
	if cfg == nil {
		t.Fatal("loadVocabConfig should return a non-nil config for a corrupt blob")
	}
	if len(cfg.Exclude) != 0 || cfg.MaxGenres != 0 || cfg.MaxStyles != 0 || cfg.MaxMoods != 0 {
		t.Errorf("expected the default no-op config for a corrupt blob, got %+v", cfg)
	}
}

// TestHandlePutVocab_HTMXReturnsFragment verifies that an HTMX request gets an
// HTML fragment (swapped into the Tag Sources form status span) rather than the
// raw JSON body a plain API client receives.
func TestHandlePutVocab_HTMXReturnsFragment(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/settings/vocab", strings.NewReader(`{"max_genres":3}`))
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	r.handlePutVocab(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "<span") {
		t.Errorf("HTMX response should be an HTML fragment, got: %s", body)
	}
	if strings.Contains(body, `"status"`) {
		t.Errorf("HTMX response should not be the raw JSON body, got: %s", body)
	}
}

// TestHandlePutVocab_FormEncoded_CRLF verifies the textarea split tolerates
// CRLF line endings (browsers normalize <textarea> values to CRLF on submit).
func TestHandlePutVocab_FormEncoded_CRLF(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	putReq := httptest.NewRequest(http.MethodPut, "/api/v1/settings/vocab",
		strings.NewReader("exclude=rock%0D%0Apop")) // rock\r\npop
	putReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	putW := httptest.NewRecorder()
	r.handlePutVocab(putW, putReq)
	if putW.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", putW.Code, putW.Body.String())
	}

	getReq := httptest.NewRequest(http.MethodGet, "/api/v1/settings/vocab", nil)
	getW := httptest.NewRecorder()
	r.handleGetVocab(getW, getReq)
	var cfg tagdict.VocabConfig
	if err := json.NewDecoder(getW.Body).Decode(&cfg); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(cfg.Exclude) != 2 || cfg.Exclude[0] != "rock" || cfg.Exclude[1] != "pop" {
		t.Fatalf("a CRLF body should yield [rock pop] with no stray CR, got %q", cfg.Exclude)
	}
}

// TestHandlePutVocab_FormEncoded_HTMXFragment verifies the real production
// path: a form-encoded body plus the HX-Request header returns the HTML
// fragment (swapped into the form status span), not JSON.
func TestHandlePutVocab_FormEncoded_HTMXFragment(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/settings/vocab",
		strings.NewReader("max_genres=4"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	r.handlePutVocab(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "<span") {
		t.Errorf("a form+HTMX request should get an HTML fragment, got: %s", w.Body.String())
	}
}

// TestHandlePutVocab_FormEncoded_WhitespaceOnlyLineDropped verifies that a
// line of only spaces in the textarea is dropped, not stored and not rejected.
func TestHandlePutVocab_FormEncoded_WhitespaceOnlyLineDropped(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	putReq := httptest.NewRequest(http.MethodPut, "/api/v1/settings/vocab",
		strings.NewReader("exclude=rock%0A%20%20%20%0Apop")) // "rock", "   ", "pop"
	putReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	putW := httptest.NewRecorder()
	r.handlePutVocab(putW, putReq)
	if putW.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", putW.Code, putW.Body.String())
	}

	getReq := httptest.NewRequest(http.MethodGet, "/api/v1/settings/vocab", nil)
	getW := httptest.NewRecorder()
	r.handleGetVocab(getW, getReq)
	var cfg tagdict.VocabConfig
	if err := json.NewDecoder(getW.Body).Decode(&cfg); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(cfg.Exclude) != 2 {
		t.Fatalf("a whitespace-only line should be dropped, expected 2 patterns, got %q", cfg.Exclude)
	}
}

// TestHandlePutVocab_FormEncoded_UnknownFieldRejected verifies the form path
// rejects an unknown form field with 400, matching the JSON path's strict-key
// contract (and the openapi additionalProperties: false declaration).
func TestHandlePutVocab_FormEncoded_UnknownFieldRejected(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/settings/vocab",
		strings.NewReader("max_genres=2&bogus=1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.handlePutVocab(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an unknown form field, got %d: %s", w.Code, w.Body.String())
	}
}
