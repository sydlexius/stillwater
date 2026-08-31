package api

import (
	"encoding/json"
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

// TestPlatformBackdropDuplicatesPrune_RejectsAMalformedFormBoolean. A
// malformed "dry_run" must not read as false: that would turn a rehearsal
// into a real, irreversible delete, and it would look like a success.
// Asserted alongside a malformed all_artists so the strictness is not
// accidentally specific to one field.
func TestPlatformBackdropDuplicatesPrune_RejectsAMalformedFormBoolean(t *testing.T) {
	t.Parallel()
	for _, body := range []string{
		"all_artists=sure",
		"all_artists=maybe",
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
				t.Errorf("status = %d, want 400 for %q", w.Code, body)
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
