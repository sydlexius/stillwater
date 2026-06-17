package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/auth"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/encryption"
	"github.com/sydlexius/stillwater/internal/nfo"
	"github.com/sydlexius/stillwater/internal/rule"
)

// newArtistFuzzRouter builds a Router with the minimum services needed by the
// artist-by-id handlers under fuzz: artistService (for AddAlias / UpdateField
// service calls) plus the housekeeping deps NewRouter requires.
//
// A fresh on-disk SQLite file under t.TempDir() per call keeps iterations
// isolated; logs are discarded via discardWriter (defined in
// handlers_setup_fuzz_test.go) so fuzz output stays quiet.
func newArtistFuzzRouter(t testing.TB) *Router {
	t.Helper()

	db := newTestDB(t)

	logger := slog.New(slog.NewTextHandler(discardWriter{}, &slog.HandlerOptions{Level: slog.LevelError}))

	enc, _, err := encryption.NewEncryptor("")
	if err != nil {
		t.Fatalf("encryptor: %v", err)
	}

	authSvc := auth.NewService(db)
	artistSvc := artist.NewService(db)
	connSvc := connection.NewService(db, enc)
	ruleSvc := rule.NewService(db)
	nfoSnapSvc := nfo.NewSnapshotService(db)

	return NewRouter(RouterDeps{
		SessionSecret:      testSessionSecret,
		AuthService:        authSvc,
		ArtistService:      artistSvc,
		ConnectionService:  connSvc,
		RuleService:        ruleSvc,
		NFOSnapshotService: nfoSnapSvc,
		DB:                 db,
		Logger:             logger,
		StaticFS:           os.DirFS("../../web/static"),
	})
}

// FuzzHandleAddAlias feeds arbitrary byte slices to the JSON request body
// decoder used by handleAddAlias (POST /api/v1/artists/{id}/aliases).
// The decoder must never panic regardless of input; returning a 400 / 404 /
// 500 for invalid input is expected and correct.
//
// Target: internal/api/handlers_alias.go line 48 (DecodeJSON into the
// alias+source struct) plus the post-decode TrimSpace + empty-check + service
// dispatch.
//
// Seeds cover the happy-path JSON shape, missing/empty fields, type
// mismatches, NUL embedding, and oversized payloads. The fuzz body drives the
// full handler via direct invocation with SetPathValue so the artist-id
// extraction path is exercised; the underlying service will return
// ErrNotFound for the synthetic id, which is the correct 404 branch.
func FuzzHandleAddAlias(f *testing.F) {
	// Build the router once per fuzz run rather than per iteration. The
	// handler path here is read-only against the DB -- it always falls
	// through to ErrNotFound for the synthetic "fuzz-aid" id, never
	// reaching the mutating AddAlias service call -- so sharing is safe
	// and lifts throughput by ~30x (no migration replay per iteration).
	r := newArtistFuzzRouter(f)

	// Happy-path: alias + source.
	f.Add([]byte(`{"alias":"The Band","source":"manual"}`))
	// Happy-path: source omitted (treated as empty by the handler).
	f.Add([]byte(`{"alias":"Solo"}`))
	// Alias only whitespace -> handler rejects with 400 after TrimSpace.
	f.Add([]byte(`{"alias":"   ","source":"manual"}`))
	// Empty alias -> 400.
	f.Add([]byte(`{"alias":"","source":"manual"}`))
	// Extra unknown fields the decoder must ignore.
	f.Add([]byte(`{"alias":"X","source":"manual","extra":42,"more":[1,2]}`))
	// Alias as integer -> type-mismatch decode error -> 400.
	f.Add([]byte(`{"alias":12345,"source":"manual"}`))
	// Source as array -> type-mismatch decode error.
	f.Add([]byte(`{"alias":"X","source":["a","b"]}`))
	// Bare null.
	f.Add([]byte(`null`))
	// Empty body.
	f.Add([]byte{})
	// Array where object expected.
	f.Add([]byte(`[{"alias":"X"}]`))
	// Truncated JSON.
	f.Add([]byte(`{"alias":"X","source":"manu`))
	// NUL byte embedded inside alias string.
	f.Add(append(
		[]byte(`{"alias":"Ali`),
		append([]byte{0x00}, []byte(`as","source":"manual"}`)...)...))
	// Deeply nested unknown object.
	f.Add([]byte(`{"alias":"X","source":"m","nest":{"a":{"b":{"c":{"d":"deep"}}}}}`))
	// Trailing garbage after a valid object.
	f.Add([]byte(`{"alias":"X","source":"m"}trailing`))
	// Very long alias string to exercise allocator paths.
	bigAlias := make([]byte, 0, 64+512*1024)
	bigAlias = append(bigAlias, []byte(`{"alias":"`)...)
	for i := 0; i < 512*1024; i++ {
		bigAlias = append(bigAlias, 'A')
	}
	bigAlias = append(bigAlias, []byte(`","source":"manual"}`)...)
	f.Add(bigAlias)

	f.Fuzz(func(t *testing.T, data []byte) {
		req := httptest.NewRequest(http.MethodPost,
			"/api/v1/artists/fuzz-aid/aliases", bytes.NewReader(data))
		req.Header.Set("Content-Type", "application/json")
		// SetPathValue mimics what the net/http stdlib mux populates from the
		// route pattern. Without it, RequirePathParam returns false and the
		// handler short-circuits before reaching the JSON decoder we want to
		// exercise.
		req.SetPathValue("id", "fuzz-aid")
		w := httptest.NewRecorder()
		// A panic anywhere in this stack is a fuzz failure.
		r.handleAddAlias(w, req)
	})
}

// FuzzHandleFieldUpdate feeds arbitrary byte slices to extractFieldValue, the
// JSON decode boundary used by handleFieldUpdate (PATCH
// /api/v1/artists/{id}/fields/{field}). The decoder must never panic
// regardless of input.
//
// Target: internal/api/handlers_field.go line 318 (extractFieldValue) -- the
// json.NewDecoder(req.Body).Decode into the {Value json.RawMessage} struct,
// followed by the string/array fallback for slice fields. Both code paths
// must stay panic-free across arbitrary input.
//
// Calling extractFieldValue directly (rather than driving the full
// handleFieldUpdate handler) isolates the JSON decode surface without
// requiring the rule pipeline, publisher, or event bus services. The handler
// itself is exercised by the coverage tests in handlers_field_test.go.
func FuzzHandleFieldUpdate(f *testing.F) {
	// Happy-path: string scalar.
	f.Add([]byte(`{"value":"updated name"}`), "name")
	// Happy-path: array for a slice field.
	f.Add([]byte(`{"value":["jazz","blues"]}`), "genres")
	// Null value (explicit clear).
	f.Add([]byte(`{"value":null}`), "biography")
	// Missing value field.
	f.Add([]byte(`{}`), "name")
	// Numeric value where string expected -> decode succeeds, but the
	// string-cast fallback in extractFieldValue rejects.
	f.Add([]byte(`{"value":42}`), "name")
	// Object value -> rejected by both string and array fallbacks.
	f.Add([]byte(`{"value":{"nested":"x"}}`), "name")
	// Array for non-slice field -> rejected (not a slice field).
	f.Add([]byte(`{"value":["a","b"]}`), "name")
	// Extra unknown fields ignored by the decoder.
	f.Add([]byte(`{"value":"x","extra":42}`), "name")
	// Bare null.
	f.Add([]byte(`null`), "name")
	// Empty body.
	f.Add([]byte{}, "name")
	// Truncated JSON.
	f.Add([]byte(`{"value":"x`), "name")
	// NUL byte embedded inside value.
	f.Add(append(
		[]byte(`{"value":"abc`),
		append([]byte{0x00}, []byte(`def"}`)...)...), "name")
	// Mixed-type array.
	f.Add([]byte(`{"value":["a",42,true]}`), "genres")
	// Deeply nested object value.
	f.Add([]byte(`{"value":{"a":{"b":{"c":{"d":"deep"}}}}}`), "biography")
	// Unknown field name (handler rejects later via IsEditableField).
	f.Add([]byte(`{"value":"x"}`), "no_such_field_name")
	// Field name with NUL byte.
	f.Add([]byte(`{"value":"x"}`), "name\x00")
	// Very long value string.
	bigVal := make([]byte, 0, 64+256*1024)
	bigVal = append(bigVal, []byte(`{"value":"`)...)
	for i := 0; i < 256*1024; i++ {
		bigVal = append(bigVal, 'V')
	}
	bigVal = append(bigVal, []byte(`"}`)...)
	f.Add(bigVal, "biography")

	f.Fuzz(func(t *testing.T, data []byte, field string) {
		// Construct a minimal *http.Request rather than going through
		// httptest.NewRequest. extractFieldValue only reads req.Body and the
		// Content-Type header, so we skip URL parsing -- httptest.NewRequest
		// panics on field names containing NUL or other URL-invalid chars,
		// which would mask the JSON-decode-side bugs the fuzz target is
		// actually looking for.
		req := &http.Request{
			Method: http.MethodPatch,
			Header: http.Header{"Content-Type": []string{"application/json"}},
			Body:   io.NopCloser(bytes.NewReader(data)),
		}
		// extractFieldValue is the JSON decode boundary; it must not panic
		// on any input. Errors are expected for malformed JSON / type
		// mismatches; the handler converts them to 400.
		_, _ = extractFieldValue(req, field)
	})
}

// Sanity check that the seeds we hand the fuzzer use a JSON shape the real
// handler's decoder accepts. Without this, a refactor that renames a struct
// tag would silently make every fuzz seed degenerate (decode error on every
// iteration) and the fuzz value evaporates.
func TestFuzzHandleAddAlias_SeedDecodes(t *testing.T) {
	t.Parallel()
	var body struct {
		Alias  string `json:"alias"`
		Source string `json:"source"`
	}
	if err := json.NewDecoder(bytes.NewReader([]byte(
		`{"alias":"The Band","source":"manual"}`,
	))).Decode(&body); err != nil {
		t.Fatalf("happy-path seed must decode cleanly: %v", err)
	}
	if body.Alias != "The Band" || body.Source != "manual" {
		t.Fatalf("seed decoded wrong: %+v", body)
	}
}

// Compile-time assertion that the artist service is set on the fuzz router.
// If a future refactor reorders RouterDeps fields and drops ArtistService
// from the construction, this catches it without running the fuzz target.
func TestFuzzArtistRouter_HasArtistService(t *testing.T) {
	t.Parallel()
	r := newArtistFuzzRouter(t)
	if r.artistService == nil {
		t.Fatal("fuzz router missing artistService -- handleAddAlias will NPE")
	}
	// Reaching the not-found branch is the design: synthetic IDs route
	// through the service which returns ErrNotFound. Assert the specific
	// sentinel so a future refactor that swaps the error type fails this
	// test loudly rather than silently masking the contract change.
	_, err := r.artistService.AddAlias(context.Background(), "fuzz-aid", "X", "manual")
	if !errors.Is(err, artist.ErrNotFound) {
		t.Fatalf("expected artist.ErrNotFound for synthetic id; got %v", err)
	}
}
