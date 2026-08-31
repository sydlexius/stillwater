package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/publish"
	"github.com/sydlexius/stillwater/web/templates"
)

// TestPlatformBackdropDuplicatesPrune_RefusesAnUnscopedRequest is #3139's
// handler-side half: a request naming neither one artist nor the whole
// library must be a 400 rather than a library-wide delete.
//
// The 400 is asserted BEFORE the singleton, deliberately: a malformed request
// that claimed the prune slot would 409 every subsequent legitimate prune
// until the slot released. So each case also asserts the slot is still free.
func TestPlatformBackdropDuplicatesPrune_RefusesAnUnscopedRequest(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		body string
	}{
		{"empty body", ``},
		{"empty object", `{}`},
		{"both scopes", `{"artist_id": "a1", "all_artists": true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := testRouterWithPlatformPublisher(t)
			req := httptest.NewRequestWithContext(adminContext(), http.MethodPost,
				"/api/v1/reports/platform-backdrop-duplicates/prune", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			r.handlePlatformBackdropDuplicatesPrune(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %q)", w.Code, tc.body)
			}
			r.platformPruneMu.Lock()
			claimed := r.platformPruneRunning
			r.platformPruneMu.Unlock()
			if claimed {
				t.Error("a rejected request left the prune singleton claimed; every later prune would 409")
			}
		})
	}
}

// TestPlatformBackdropDuplicatesPrune_AcceptsFormEncodedScope pins the
// encoding the REPORT PAGE actually uses. htmx posts hx-vals as a form body
// (no json-enc extension is vendored), so a JSON-only handler would leave both
// buttons permanently broken while every JSON-based test kept passing.
func TestPlatformBackdropDuplicatesPrune_AcceptsFormEncodedScope(t *testing.T) {
	t.Parallel()
	// Library scope only: this router's library is EMPTY, so a scoped
	// artist_id would correctly 500 on an artist that does not exist -- a
	// fact about the fixture, not about form decoding. The artist_id form
	// path is asserted where it is actually observable, on the decoder
	// itself: see TestDecodePlatformPruneRequest_FormCarriesTheArtistScope.
	for _, body := range []string{
		"all_artists=true",
	} {
		t.Run(body, func(t *testing.T) {
			t.Parallel()
			r := testRouterWithPlatformPublisher(t)
			req := httptest.NewRequestWithContext(adminContext(), http.MethodPost,
				"/api/v1/reports/platform-backdrop-duplicates/prune", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			w := httptest.NewRecorder()
			r.handlePlatformBackdropDuplicatesPrune(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 for %q; body: %s", w.Code, body, w.Body.String())
			}
		})
	}
}

// TestDecodePlatformPruneRequest_FormCarriesTheArtistScope asserts the PARSED
// VALUES of a form body, not the status code it produces (CodeRabbit, PR
// #3157).
//
// The per-row "Prune This Artist" button posts artist_id as a form field, and
// every other form test here asserts only a status. A decode path that
// dropped artist_id would leave every row button rejected as unscoped -- and
// nothing would fail, because a 200 for the library case and a 400 for a
// dropped artist case are both "the status I expected" to a test that never
// looks at what was decoded. That is the same vacuity shape this branch has
// now been bitten by twice: the malformed-boolean test that passed for the
// wrong reason, and the live bystander fixture that passed with the scope
// broken.
//
// Asserted against decodePlatformPruneRequest directly, because the parsed
// scope is the thing at risk and it is not observable from the handler's
// response.
func TestDecodePlatformPruneRequest_FormCarriesTheArtistScope(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, body     string
		wantArtistID   string
		wantAllArtists bool
	}{
		{"the per-row button", "artist_id=a1", "a1", false},
		{"the library button", "all_artists=true", "", true},
		// An id containing reserved characters must survive form decoding
		// intact: a mangled id names a DIFFERENT artist, and the prune would
		// then delete from the wrong one or refuse silently.
		{"an id needing escaping", "artist_id=a%2Bb+c", "a+b c", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequestWithContext(adminContext(), http.MethodPost,
				"/api/v1/reports/platform-backdrop-duplicates/prune", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			w := httptest.NewRecorder()

			// nil logger deliberately: it also exercises logPruneDecodeFailure's
			// nil guard, so a missing logger cannot panic on the error path.
			got, ok := decodePlatformPruneRequest(w, req, nil)
			if !ok {
				t.Fatalf("decode refused %q: %s", tc.body, w.Body.String())
			}
			if got.ArtistID != tc.wantArtistID {
				t.Errorf("ArtistID = %q, want %q -- a dropped or mangled artist_id leaves the per-row button rejected as unscoped", got.ArtistID, tc.wantArtistID)
			}
			if got.AllArtists != tc.wantAllArtists {
				t.Errorf("AllArtists = %v, want %v", got.AllArtists, tc.wantAllArtists)
			}
		})
	}
}

// TestPlatformBackdropDuplicatesPrune_RejectsAMalformedFormBoolean. An
// unparsable all_artists must be a 400 NAMING THE BOOLEAN, never silently
// read as false: on a path that deletes artwork, a malformed value must not
// become a different, narrower request than the client actually sent.
//
// THE ASSERTION IS THE MESSAGE, NOT THE STATUS, and the artist_id case is
// why (hostile review, Important 2). An earlier version tested only bodies
// carrying no artist_id, so under a lenient `v, _ := strconv.ParseBool(raw)`
// mutant AllArtists merely became false and the request 400'd anyway -- for
// the MISSING-SCOPE reason. Status alone cannot tell those two 400s apart,
// so the mutant survived. With artist_id PRESENT the scope is valid whatever
// the boolean decodes to, so only the strict parse can produce a 400 at all:
// under the mutant that request reaches the publisher and 500s.
func TestPlatformBackdropDuplicatesPrune_RejectsAMalformedFormBoolean(t *testing.T) {
	t.Parallel()
	for _, body := range []string{
		// No scope: 400 either way, but the MESSAGE must still name the
		// boolean rather than the missing scope.
		"all_artists=sure",
		"all_artists=maybe",
		// A VALID scope alongside the malformed boolean. Nothing but the
		// strict parse can reject this one.
		"artist_id=a1&all_artists=sure",
	} {
		t.Run(body, func(t *testing.T) {
			t.Parallel()
			r := testRouterWithPlatformPublisher(t)
			req := httptest.NewRequestWithContext(adminContext(), http.MethodPost,
				"/api/v1/reports/platform-backdrop-duplicates/prune", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			w := httptest.NewRecorder()
			r.handlePlatformBackdropDuplicatesPrune(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 for %q; body: %s", w.Code, body, w.Body.String())
			}
			var got map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatalf("decoding body for %q: %v", body, err)
			}
			if want := "invalid boolean for all_artists"; got["error"] != want {
				t.Errorf("error = %q, want %q -- a 400 for the wrong REASON is how a lenient parse hides", got["error"], want)
			}
		})
	}
}

// TestPlatformBackdropDuplicatesPage_ScopeIsOnEveryButton is a rendered-output
// check of the one thing that would break BOTH buttons silently (#3139): the
// endpoint refuses an unscoped request, so a button posting an empty body is a
// permanent 400 no Go-level handler test would notice. Both buttons are
// asserted from ONE render -- either alone would miss the other losing its
// scope and pruning nothing.
func TestPlatformBackdropDuplicatesPage_ScopeIsOnEveryButton(t *testing.T) {
	t.Parallel()
	view := buildPlatformBackdropDuplicatesView(publish.PlatformBackdropDupReport{
		ConnectionsAffected: 1,
		ArtistsAffected:     1,
		RedundantBackdrops:  2,
		PerArtist: []publish.ArtistPlatformBackdropDup{
			{ArtistID: "a1", Name: "Artist One", ConnectionID: "c-emby", Connection: "emby", Backdrops: 4, Redundant: 2},
		},
	})

	req := withI18nCtx(t, httptest.NewRequestWithContext(adminContext(), http.MethodGet, "/reports/platform-backdrop-duplicates", nil))
	rec := httptest.NewRecorder()
	renderTempl(rec, req, templates.PlatformBackdropDuplicatesPage(templates.AssetPaths{}, view))
	body := rec.Body.String()

	// The library-wide button must say so explicitly. There is no
	// library-wide default any more, so an omitted all_artists is a 400.
	if !strings.Contains(body, "all_artists") {
		t.Errorf("the library-wide prune button carries no all_artists scope, so it now posts an unscoped request and is refused with 400; body: %s", body)
	}
	// The per-row action, and the artist it is scoped to.
	if !strings.Contains(body, "sw-platform-backdrop-prune-artist") {
		t.Error("no per-artist prune action on the row; #3139's whole point is being able to rehearse on one artist")
	}
	if !strings.Contains(body, `artist_id`) || !strings.Contains(body, `a1`) {
		t.Errorf("the per-artist button does not carry its artist_id, so it would post an unscoped request; body: %s", body)
	}
}

// TestPlatformBackdropDuplicatesPage_RowButtonScopeSurvivesAQuotedArtistID.
// The artist id reaches an HTML attribute that htmx parses as JSON, so a
// value containing a quote must not be able to break out of the object. This
// asserts the raw injection is not present in the rendered page.
func TestPlatformBackdropDuplicatesPage_RowButtonScopeSurvivesAQuotedArtistID(t *testing.T) {
	t.Parallel()
	const nasty = `a1","all_artists":true,"x":"`
	view := buildPlatformBackdropDuplicatesView(publish.PlatformBackdropDupReport{
		RedundantBackdrops: 1,
		PerArtist: []publish.ArtistPlatformBackdropDup{
			{ArtistID: nasty, Name: "Artist One", ConnectionID: "c", Connection: "emby", Backdrops: 2, Redundant: 1},
		},
	})
	req := withI18nCtx(t, httptest.NewRequestWithContext(adminContext(), http.MethodGet, "/reports/platform-backdrop-duplicates", nil))
	rec := httptest.NewRecorder()
	renderTempl(rec, req, templates.PlatformBackdropDuplicatesPage(templates.AssetPaths{}, view))
	body := rec.Body.String()

	// An unescaped injection would put a literal `","all_artists":true` into
	// the row's hx-vals attribute, turning a single-artist rehearsal into a
	// library-wide delete -- the exact widening this feature must refuse.
	if strings.Contains(body, `"all_artists":true`) {
		t.Errorf("an artist id escaped its hx-vals JSON and injected a library-wide scope; body: %s", body)
	}
}

// TestPlatformBackdropDuplicatesPrune_ScopeErrorsAreFixedMessages pins the
// no-raw-error-leak contract on the 400 path AND that the refusals stay
// DISTINGUISHABLE. Collapsing them to one string satisfies "no internals leak"
// while making the endpoint harder to use; echoing err.Error() keeps them
// distinguishable while leaking the validator's internal text.
func TestPlatformBackdropDuplicatesPrune_ScopeErrorsAreFixedMessages(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, body string }{
		{"missing", `{}`},
		{"ambiguous", `{"artist_id": "a1", "all_artists": true}`},
	}
	seen := map[string]bool{}
	for _, tc := range cases {
		r := testRouterWithPlatformPublisher(t)
		req := httptest.NewRequestWithContext(adminContext(), http.MethodPost,
			"/api/v1/reports/platform-backdrop-duplicates/prune", strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.handlePlatformBackdropDuplicatesPrune(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400", tc.name, w.Code)
		}
		var got map[string]string
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("%s: decoding body: %v", tc.name, err)
		}
		if got["error"] == "" {
			t.Errorf("%s: a 400 with no error message tells the caller nothing", tc.name)
		}
		// The Go-internal phrasing the validator uses must not reach a client
		// message: those are written for the API, not for the struct.
		for _, internal := range []string{"AllArtists", "ArtistID"} {
			if strings.Contains(got["error"], internal) {
				t.Errorf("%s: the response names the Go field %q: %q", tc.name, internal, got["error"])
			}
		}
		seen[got["error"]] = true
	}
	if len(seen) != len(cases) {
		t.Errorf("the scope refusals collapsed to %d distinct messages; a caller cannot tell a missing scope from a contradictory one", len(seen))
	}
}

// TestPlatformBackdropDuplicatesPrune_FormEncodingConformsToTheSpec closes the
// gap between what the shipping UI POSTs and what openapi.yaml DECLARES.
//
// Every other form test in this file calls the handler directly, which
// bypasses the spec entirely -- so a requestBody declaring only
// application/json passed all of them while the report page's htmx buttons
// (platform_backdrop_duplicates.templ) emit form-encoded, the one media type
// the spec called non-conformant. Prose in a requestBody DESCRIPTION saying
// "form-encoded is accepted" is not a media type; only an entry under
// `content:` is.
//
// serveValidated is the point of this test: it validates the request against
// the spec BEFORE the handler runs, so this fails loudly if the
// x-www-form-urlencoded content entry is ever dropped again. The JSON case
// rides along so a regression that broke JSON while fixing form would not
// slip through.
func TestPlatformBackdropDuplicatesPrune_FormEncodingConformsToTheSpec(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, contentType, body string
	}{
		{"form-encoded, as the report page posts", "application/x-www-form-urlencoded", "all_artists=true"},
		{"json, as the API posts", "application/json", `{"all_artists": true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := testRouterWithPlatformPublisher(t)
			req := httptest.NewRequestWithContext(adminContext(), http.MethodPost,
				"/api/v1/reports/platform-backdrop-duplicates/prune", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", tc.contentType)

			w := serveValidated(t, http.HandlerFunc(r.handlePlatformBackdropDuplicatesPrune), req)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
			}
		})
	}
}

// TestPlatformBackdropDuplicatesPrune_SpecRejectsAnUnscopedPayload is the
// SPEC-side half of the scope invariant (CodeRabbit, PR #3157).
//
// The handler has refused an unscoped, ambiguous, or all_artists=false
// request since this branch began, but the published schema accepted all
// three: a flat object with two optional properties calls them valid. A
// generated client built from that schema would therefore emit payloads the
// server rejects, on the one endpoint whose entire purpose is refusing an
// unscoped destructive run.
//
// These assertions run against the SPEC, via validateExchange, and never
// reach the handler -- validateExchange returns its error before invoking it.
// That is the point: a test that only asserted the 400 would pass with the
// schema left loose, because the handler was always right. Only spec
// validation can fail here.
func TestPlatformBackdropDuplicatesPrune_SpecRejectsAnUnscopedPayload(t *testing.T) {
	t.Parallel()
	// A handler that would 200 anything, so a failure can only come from spec
	// validation rather than from the real handler's own refusal.
	permissive := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"artists_processed": 0, "backdrops_removed": 0,
			"skipped_changed": 0, "failures": 0,
		})
	})

	for _, tc := range []struct {
		name, contentType, body string
		wantValid               bool
	}{
		{"no scope at all", "application/json", `{}`, false},
		{"both scopes", "application/json", `{"artist_id":"a1","all_artists":true}`, false},
		{"all_artists explicitly false", "application/json", `{"all_artists":false}`, false},
		{"empty artist_id names nothing", "application/json", `{"artist_id":""}`, false},
		{"one artist", "application/json", `{"artist_id":"a1"}`, true},
		{"the whole library", "application/json", `{"all_artists":true}`, true},
		// The two encodings the endpoint declares must agree. A form-encoded
		// payload is what the report page's buttons actually post.
		{"form: one artist", "application/x-www-form-urlencoded", "artist_id=a1", true},
		{"form: the whole library", "application/x-www-form-urlencoded", "all_artists=true", true},
		{"form: no scope", "application/x-www-form-urlencoded", "", false},
		{"form: both scopes", "application/x-www-form-urlencoded", "artist_id=a1&all_artists=true", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequestWithContext(adminContext(), http.MethodPost,
				"/api/v1/reports/platform-backdrop-duplicates/prune", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", tc.contentType)

			_, err := validateExchange(loadSpec(t), permissive, req)
			if tc.wantValid && err != nil {
				t.Errorf("spec REJECTED a payload the handler accepts (%s): %v", tc.body, err)
			}
			if !tc.wantValid && err == nil {
				t.Errorf("spec ACCEPTED %q, which the handler refuses with a 400; a generated client would emit it against a destructive endpoint", tc.body)
			}
		})
	}
}

// TestPlatformBackdropDuplicatesPrune_DecodeFailuresLogWithoutLeaking pins
// BOTH halves of the decode-failure logging (CodeRabbit, PR #3157): the
// server records enough to diagnose the 400, and the client learns nothing
// it did not already know.
//
// The second half is the one that needs a test. An earlier round on this
// branch found raw err.Error() text leaking a rejected value into a response
// body, and "add a log line" is exactly the edit that reintroduces it -- the
// detail is right there in hand at the moment the response is written. So
// this asserts the offending VALUE appears in the log and NOT in the body,
// and that each response string is the fixed one that shipped before the
// logging existed.
func TestPlatformBackdropDuplicatesPrune_DecodeFailuresLogWithoutLeaking(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, contentType, body string
		wantResponse            string
		// secret is client-supplied text that must reach the LOG but never
		// the response.
		secret string
	}{
		{
			name:         "malformed form boolean",
			contentType:  "application/x-www-form-urlencoded",
			body:         "all_artists=NOTABOOLEANXYZ",
			wantResponse: "invalid boolean for all_artists",
			secret:       "NOTABOOLEANXYZ",
		},
		{
			name:         "malformed json",
			contentType:  "application/json",
			body:         `{"all_artists":`,
			wantResponse: "invalid JSON body",
		},
		{
			name:         "unknown json field",
			contentType:  "application/json",
			body:         `{"nonesuch": true}`,
			wantResponse: "invalid JSON body",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var logs bytes.Buffer
			r := testRouterWithPlatformPublisher(t)
			r.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))

			req := httptest.NewRequestWithContext(adminContext(), http.MethodPost,
				"/api/v1/reports/platform-backdrop-duplicates/prune", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", tc.contentType)
			w := httptest.NewRecorder()
			r.handlePlatformBackdropDuplicatesPrune(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
			}
			var got map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatalf("decoding body: %v", err)
			}
			// UNCHANGED CLIENT CONTRACT: the exact string that shipped before
			// this logging was added.
			if got["error"] != tc.wantResponse {
				t.Errorf("error = %q, want %q -- the response contract must not move when logging is added", got["error"], tc.wantResponse)
			}
			// The server must have SOMETHING to diagnose with.
			if !strings.Contains(logs.String(), "platform backdrop prune:") {
				t.Errorf("no warning logged for a rejected body; an operator seeing this 400 has nothing server-side to go on. logs: %s", logs.String())
			}
			if tc.secret != "" {
				if !strings.Contains(logs.String(), tc.secret) {
					t.Errorf("the rejected value %q is absent from the log, which is where the diagnostic detail belongs. logs: %s", tc.secret, logs.String())
				}
				if strings.Contains(w.Body.String(), tc.secret) {
					t.Errorf("the rejected value %q was REFLECTED into the response body: %s", tc.secret, w.Body.String())
				}
			}
		})
	}
}
