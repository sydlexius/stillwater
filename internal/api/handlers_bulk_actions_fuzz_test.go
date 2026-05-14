package api

import (
	"bytes"
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

// newBulkActionFuzzRouter builds a Router with the minimum services needed by
// handleBulkAction up to (but not through) the singleton-slot claim and the
// service-availability gates. The pipeline is intentionally nil so the
// "rule pipeline not configured" 503 branch fires on any happy-path body,
// keeping fuzz iterations fast and free of background goroutine work.
//
// The JSON decode + validActions + ID validation paths -- the surfaces under
// fuzz -- all run before the pipeline gate, so coverage of the decode
// boundary is unaffected.
func newBulkActionFuzzRouter(t testing.TB) *Router {
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

// FuzzHandleBulkAction feeds arbitrary byte slices to the JSON request body
// decoder used by handleBulkAction (POST /api/v1/artists/bulk-actions). The
// decoder uses DisallowUnknownFields + a trailing-data probe + ID regex
// validation, all of which must stay panic-free regardless of input.
//
// Target: internal/api/handlers_bulk_actions.go line 125 (json.NewDecoder
// into bulkActionRequest) plus the trailing-data probe at line 135 and the
// per-ID regex validation in the seen/dedup loop at line 165. The
// MaxBytesReader cap at line 124 is also exercised on oversized seeds.
//
// Action vocabulary is intentionally not in the seeds' enum value list so
// the fuzzer can discover any input that bypasses validActions's switch.
func FuzzHandleBulkAction(f *testing.F) {
	// Build the router once per fuzz run rather than per iteration.
	// Sharing is safe: the only iteration that successfully claims the
	// singleton bulk-action slot drops it via releaseSlot() before
	// returning (pipeline=nil here, so every claimed slot is released
	// inside the 503 branch). Bodies that fail JSON / validActions /
	// ID-regex validation never claim a slot, so no cross-iteration
	// state leaks. Sharing avoids replaying migrations per input and
	// lifts throughput substantially.
	r := newBulkActionFuzzRouter(f)

	// Happy-path: run_rules over two IDs.
	f.Add([]byte(`{"action":"run_rules","ids":["a1","b2"]}`))
	// Happy-path: re_identify (legacy alias).
	f.Add([]byte(`{"action":"re_identify","ids":["a1"]}`))
	// Happy-path: re_identify_auto canonical.
	f.Add([]byte(`{"action":"re_identify_auto","ids":["a1"]}`))
	// Happy-path: scan.
	f.Add([]byte(`{"action":"scan","ids":["a1"]}`))
	// Happy-path: fetch_images.
	f.Add([]byte(`{"action":"fetch_images","ids":["a1"]}`))
	// Empty ids array -> 400.
	f.Add([]byte(`{"action":"run_rules","ids":[]}`))
	// Missing action -> 400 (validActions returns false on "").
	f.Add([]byte(`{"ids":["a1"]}`))
	// Missing ids -> 400.
	f.Add([]byte(`{"action":"run_rules"}`))
	// Unknown action -> 400.
	f.Add([]byte(`{"action":"explode","ids":["a1"]}`))
	// Unknown field -> 400 via DisallowUnknownFields.
	f.Add([]byte(`{"action":"run_rules","ids":["a1"],"extra":42}`))
	// Trailing object -> 400 via trailing-data probe.
	f.Add([]byte(`{"action":"run_rules","ids":["a1"]}{"sneaky":true}`))
	// Action as integer -> type-mismatch decode error.
	f.Add([]byte(`{"action":42,"ids":["a1"]}`))
	// IDs as object -> type-mismatch decode error.
	f.Add([]byte(`{"action":"run_rules","ids":{"nope":"x"}}`))
	// IDs contains non-string -> decode error.
	f.Add([]byte(`{"action":"run_rules","ids":[1,2,3]}`))
	// ID with invalid characters -> regex rejects.
	f.Add([]byte(`{"action":"run_rules","ids":["bad id with spaces"]}`))
	// ID with NUL byte.
	f.Add(append(
		[]byte(`{"action":"run_rules","ids":["a`),
		append([]byte{0x00}, []byte(`b"]}`)...)...))
	// ID with newline (regex rejects).
	f.Add([]byte("{\"action\":\"run_rules\",\"ids\":[\"a\nb\"]}"))
	// Duplicate IDs (deduped by the seen map; not an error path but must
	// not panic).
	f.Add([]byte(`{"action":"run_rules","ids":["a","a","a"]}`))
	// Bare null.
	f.Add([]byte(`null`))
	// Empty body.
	f.Add([]byte{})
	// Array where object expected.
	f.Add([]byte(`[{"action":"run_rules","ids":["a"]}]`))
	// Truncated JSON.
	f.Add([]byte(`{"action":"run_rules","ids":["a`))
	// Deeply nested IDs structure.
	f.Add([]byte(`{"action":"run_rules","ids":["a"],"nest":{"a":{"b":{"c":"d"}}}}`))
	// Many IDs (under MaxBulkActionIDs).
	manyIDs := []byte(`{"action":"run_rules","ids":[`)
	for i := 0; i < 50; i++ {
		if i > 0 {
			manyIDs = append(manyIDs, ',')
		}
		manyIDs = append(manyIDs, []byte(`"id-`)...)
		manyIDs = appendIntBytes(manyIDs, i)
		manyIDs = append(manyIDs, '"')
	}
	manyIDs = append(manyIDs, []byte(`]}`)...)
	f.Add(manyIDs)
	// Over MaxBulkActionIDs (exercises the 400 too-many-ids branch). Build
	// MaxBulkActionIDs+1 trivial IDs.
	overCap := []byte(`{"action":"run_rules","ids":[`)
	for i := 0; i <= MaxBulkActionIDs; i++ {
		if i > 0 {
			overCap = append(overCap, ',')
		}
		overCap = append(overCap, []byte(`"id-`)...)
		overCap = appendIntBytes(overCap, i)
		overCap = append(overCap, '"')
	}
	overCap = append(overCap, []byte(`]}`)...)
	f.Add(overCap)
	// Body just under the 1 MiB MaxBytesReader cap to exercise the boundary
	// without crossing it (the cap itself is enforced by the reader, not
	// the decoder, so larger inputs return a body-read error from json
	// decode).
	bigBody := []byte(`{"action":"run_rules","ids":["a"],"pad":"`)
	for i := 0; i < 900_000; i++ {
		bigBody = append(bigBody, 'P')
	}
	bigBody = append(bigBody, []byte(`"}`)...)
	f.Add(bigBody)

	f.Fuzz(func(t *testing.T, data []byte) {
		req := httptest.NewRequest(http.MethodPost,
			"/api/v1/artists/bulk-actions", bytes.NewReader(data))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		// A panic anywhere in this stack is a fuzz failure. The body decode
		// + validActions + ID validation paths all run before service
		// availability gates, so the pipeline-nil 503 branch downstream
		// does not mask any decode-side bug.
		r.handleBulkAction(w, req)
	})
}

// appendIntBytes appends the decimal representation of n to b without
// pulling in strconv at the call sites and without allocating a fresh string
// for every fuzz seed. Keeps the seed builders tight.
func appendIntBytes(b []byte, n int) []byte {
	if n == 0 {
		return append(b, '0')
	}
	if n < 0 {
		b = append(b, '-')
		n = -n
	}
	// Build digits in reverse, then append in order. n is bounded by the
	// MaxBulkActionIDs cap so a small fixed-size scratch buffer is enough.
	var scratch [12]byte
	i := len(scratch)
	for n > 0 {
		i--
		scratch[i] = byte('0' + n%10)
		n /= 10
	}
	return append(b, scratch[i:]...)
}
