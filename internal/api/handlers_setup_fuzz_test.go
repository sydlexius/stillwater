package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/sydlexius/stillwater/internal/auth"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/encryption"
	"github.com/sydlexius/stillwater/internal/nfo"
	"github.com/sydlexius/stillwater/internal/rule"
)

// setupFuzzBody mirrors the anonymous struct decoded by handleSetup so the
// fuzz body can unmarshal arbitrary bytes the same way the production decoder
// does. Keeping the JSON tags identical to handlers.go is load-bearing: a
// drift here masks fuzz failures behind a different field map.
type setupFuzzBody struct {
	AuthMethod string `json:"auth_method"`
	Username   string `json:"username"`
	Password   string `json:"password"`
	ServerURL  string `json:"server_url"`
}

// FuzzHandleSetupLocal feeds arbitrary byte slices to the local-setup JSON
// decoder. The decoder must never panic regardless of input; returning an
// error or a 4xx response for invalid JSON / invalid fields is expected.
//
// Two paths are exercised per fuzz iteration:
//
//  1. json.Unmarshal into the same struct shape handleSetup decodes. This is
//     the cheap path and matches the pattern used in the webhook fuzzers.
//  2. End-to-end POST through handleSetup with auth_method forced to "local"
//     so the dispatched handler is handleSetupLocal. A fresh in-memory router
//     is constructed once per fuzz iteration; users are wiped between runs to
//     keep coverage of the handler's "no admin yet" gate.
//
// Path 2 catches panics inside handleSetupLocal that pure unmarshal-only
// fuzzing would miss (for example, slice-bound or pointer-deref issues
// reachable only when the handler actually executes the post-decode logic).
//
// Target: internal/api/handlers.go line 858 (handleSetupLocal) and the
// json.NewDecoder(req.Body).Decode at line 804 inside handleSetup.
func FuzzHandleSetupLocal(f *testing.F) {
	// --- Happy-path seeds ---
	f.Add([]byte(`{"auth_method":"local","username":"admin","password":"correcthorse"}`))
	f.Add([]byte(`{"auth_method":"local","username":"admin","password":"correcthorse","server_url":""}`))
	// Extra fields the decoder must ignore.
	f.Add([]byte(`{"auth_method":"local","username":"admin","password":"correcthorse","extra":"ignored","another":42}`))
	// Empty auth_method (handleSetup defaults to "local").
	f.Add([]byte(`{"auth_method":"","username":"admin","password":"correcthorse"}`))
	// Missing username (triggers 400).
	f.Add([]byte(`{"auth_method":"local","password":"correcthorse"}`))
	// Missing password (triggers 400).
	f.Add([]byte(`{"auth_method":"local","username":"admin"}`))
	// Short password (triggers 400).
	f.Add([]byte(`{"auth_method":"local","username":"admin","password":"1234"}`))

	// --- Malformed / hostile seeds ---
	// Bare null.
	f.Add([]byte(`null`))
	// Empty body.
	f.Add([]byte{})
	// Array where object expected.
	f.Add([]byte(`[{"auth_method":"local"}]`))
	// username as integer instead of string.
	f.Add([]byte(`{"auth_method":"local","username":12345,"password":"correcthorse"}`))
	// password as array.
	f.Add([]byte(`{"auth_method":"local","username":"admin","password":["a","b"]}`))
	// Deeply nested extra object.
	f.Add([]byte(`{"auth_method":"local","username":"admin","password":"correcthorse","extra":{"a":{"b":{"c":{"d":{"e":"deep"}}}}}}`))
	// NUL byte embedded in username.
	f.Add(append(
		[]byte(`{"auth_method":"local","username":"adm`),
		append([]byte{0x00}, []byte(`in","password":"correcthorse"}`)...)...))
	// Very long username to exercise allocator paths.
	bigUser := make([]byte, 0, 64+1024*1024)
	bigUser = append(bigUser, []byte(`{"auth_method":"local","username":"`)...)
	for i := 0; i < 1024*1024; i++ {
		bigUser = append(bigUser, 'A')
	}
	bigUser = append(bigUser, []byte(`","password":"correcthorse"}`)...)
	f.Add(bigUser)
	// Trailing garbage after a valid object (json.Decoder tolerates this, but
	// the decoder must still not panic).
	f.Add([]byte(`{"auth_method":"local","username":"admin","password":"correcthorse"}trailing garbage`))
	// Invalid escape inside username string.
	f.Add([]byte(`{"auth_method":"local","username":"ad\xZZmin","password":"correcthorse"}`))
	// auth_method as a number (forces the type-mismatch error path).
	f.Add([]byte(`{"auth_method":42,"username":"admin","password":"correcthorse"}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		// Path 1: pure decoder. Must not panic. Errors are expected for
		// malformed input.
		var body setupFuzzBody
		_ = json.Unmarshal(data, &body)

		// Path 2: drive the full handler. Fresh router per iteration so
		// state from earlier seeds cannot mask a panic that only fires
		// when users == 0.
		r := newSetupFuzzRouter(t)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/setup", bytes.NewReader(data))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		// handleSetup is the production entry point and dispatches to
		// handleSetupLocal for the "local" / empty auth_method cases.
		// A panic anywhere in this stack is a fuzz failure.
		r.handleSetup(w, req)
	})
}

// FuzzHandleSetupFederated feeds arbitrary byte slices to the federated-setup
// JSON decoder. Like FuzzHandleSetupLocal it exercises both pure decoder and
// full-handler paths, but the corpus is biased toward emby / jellyfin
// dispatch targets so the fuzzer mutates around the federated branches.
//
// Target: internal/api/handlers.go line 888 (handleSetupFederated) and the
// json.NewDecoder at line 804 inside handleSetup.
func FuzzHandleSetupFederated(f *testing.F) {
	// --- Happy-path seeds (one each for emby and jellyfin). The server URL
	// points at a placeholder loopback; the connection refusal exercises the
	// 502 BadGateway path through r.authenticateByName, which still must not
	// panic. ---
	f.Add([]byte(`{"auth_method":"emby","username":"embyadmin","password":"correcthorse","server_url":"http://127.0.0.1:1"}`))
	f.Add([]byte(`{"auth_method":"jellyfin","username":"jfadmin","password":"correcthorse","server_url":"http://127.0.0.1:1"}`))
	// Extra fields.
	f.Add([]byte(`{"auth_method":"emby","username":"a","password":"correcthorse","server_url":"http://127.0.0.1:1","extra":"ignored"}`))
	// Missing username (400).
	f.Add([]byte(`{"auth_method":"emby","password":"correcthorse","server_url":"http://127.0.0.1:1"}`))
	// Missing password (400).
	f.Add([]byte(`{"auth_method":"emby","username":"a","server_url":"http://127.0.0.1:1"}`))
	// Missing server_url (400).
	f.Add([]byte(`{"auth_method":"emby","username":"a","password":"correcthorse"}`))
	// Invalid scheme on server_url (400).
	f.Add([]byte(`{"auth_method":"emby","username":"a","password":"correcthorse","server_url":"ftp://nope"}`))
	// Embedded credentials in URL (rejected by ValidateBaseURL).
	f.Add([]byte(`{"auth_method":"emby","username":"a","password":"correcthorse","server_url":"http://user:pw@host"}`))
	// Server URL with query string (rejected).
	f.Add([]byte(`{"auth_method":"emby","username":"a","password":"correcthorse","server_url":"http://host?q=1"}`))
	// Unsupported auth method (400 from handleSetup's switch default).
	f.Add([]byte(`{"auth_method":"lidarr","username":"a","password":"correcthorse","server_url":"http://127.0.0.1:1"}`))

	// --- Malformed / hostile seeds ---
	f.Add([]byte(`null`))
	f.Add([]byte{})
	// Truncated JSON.
	f.Add([]byte(`{"auth_method":"emby","username":"a"`))
	// auth_method as number.
	f.Add([]byte(`{"auth_method":42,"username":"a","password":"correcthorse","server_url":"http://127.0.0.1:1"}`))
	// server_url as object.
	f.Add([]byte(`{"auth_method":"emby","username":"a","password":"correcthorse","server_url":{"nested":"value"}}`))
	// password as boolean.
	f.Add([]byte(`{"auth_method":"emby","username":"a","password":false,"server_url":"http://127.0.0.1:1"}`))
	// NUL byte in username.
	f.Add(append(
		[]byte(`{"auth_method":"emby","username":"em`),
		append([]byte{0x00}, []byte(`by","password":"correcthorse","server_url":"http://127.0.0.1:1"}`)...)...))
	// IPv6 host in URL (must round-trip through ValidateBaseURL without panic).
	f.Add([]byte(`{"auth_method":"emby","username":"a","password":"correcthorse","server_url":"http://[::1]:8096"}`))
	// Server URL with explicit fragment (rejected by ValidateBaseURL).
	f.Add([]byte(`{"auth_method":"emby","username":"a","password":"correcthorse","server_url":"http://host#frag"}`))
	// Trailing garbage.
	f.Add([]byte(`{"auth_method":"emby","username":"a","password":"correcthorse","server_url":"http://127.0.0.1:1"}garbage`))
	// Very long server_url to push allocator + parser.
	bigURL := make([]byte, 0, 256+8192)
	bigURL = append(bigURL, []byte(`{"auth_method":"emby","username":"a","password":"correcthorse","server_url":"http://`)...)
	for i := 0; i < 8192; i++ {
		bigURL = append(bigURL, 'x')
	}
	bigURL = append(bigURL, []byte(`"}`)...)
	f.Add(bigURL)
	// Mismatched quotes (parser error).
	f.Add([]byte(`{"auth_method":"emby,"username":"a","password":"correcthorse","server_url":"http://127.0.0.1:1"}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		// Path 1: pure decoder.
		var body setupFuzzBody
		_ = json.Unmarshal(data, &body)

		// Path 2: full handler. handleSetup dispatches by auth_method.
		// For "emby" / "jellyfin", authenticateByName is called and will
		// most often fail with a network error (no listener on the URL
		// from the seed), which exercises the 502 path. A panic anywhere
		// in this stack is a fuzz failure.
		r := newSetupFuzzRouter(t)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/setup", bytes.NewReader(data))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.handleSetup(w, req)
	})
}

// newSetupFuzzRouter builds a minimal Router for fuzz iterations. Each call
// gets a fresh on-disk SQLite file (under t.TempDir() so it's cleaned up
// automatically) with all migrations applied and zero users seeded -- the
// state required for handleSetup to proceed past its HasUsers gate.
//
// Logger is silenced (LevelError + io.Discard) so fuzz iterations don't
// flood stderr; the fuzz engine cares only about panics.
func newSetupFuzzRouter(t testing.TB) *Router {
	t.Helper()

	db := newTestDB(t)

	// Discard logs so fuzz iterations don't flood output.
	logger := slog.New(slog.NewTextHandler(discardWriter{}, &slog.HandlerOptions{Level: slog.LevelError}))

	enc, _, err := encryption.NewEncryptor("")
	if err != nil {
		t.Fatalf("encryptor: %v", err)
	}

	authSvc := auth.NewService(db)
	connSvc := connection.NewService(db, enc)
	ruleSvc := rule.NewService(db)
	// Skip SeedDefaults during fuzz -- it's expensive and not on the decoder
	// surface under test.
	nfoSnapSvc := nfo.NewSnapshotService(db)

	return NewRouter(RouterDeps{
		AuthService:        authSvc,
		ConnectionService:  connSvc,
		RuleService:        ruleSvc,
		NFOSnapshotService: nfoSnapSvc,
		DB:                 db,
		Logger:             logger,
		StaticFS:           os.DirFS("../../web/static"),
	})
}

// discardWriter is a no-op io.Writer that silences the slog handler during
// fuzz runs. Defined locally to avoid pulling in io.Discard (which would
// require an extra import) and to keep this file's surface self-contained.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
