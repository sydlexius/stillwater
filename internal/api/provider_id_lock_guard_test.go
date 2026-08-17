package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
)

// TestLinkFlows_RefuseALockedProviderID pins F5 as a CLASS rather than a site.
//
// Since #3037 widened the persist chokepoint to the provider IDs, any link or
// identify handler that mutates the artist and calls Service.Update on a plain
// request context has its write silently reverted -- and answers 200, telling
// the operator a link happened when nothing was stored. The fix is a visible
// 409, matching what the Discogs and TheAudioDB match-by-name flows already do.
//
// Each subtest asserts BOTH halves: the refusal status AND that the stored value
// is unchanged. A handler that 409s but wrote anyway would pass a status-only
// assertion.
func TestLinkFlows_RefuseALockedProviderID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	for _, tc := range []struct {
		name      string
		lockField string
		body      string
		call      func(*Router, http.ResponseWriter, *http.Request)
	}{
		{
			name:      "deezer_link",
			lockField: "deezer_id",
			body:      `{"deezer_id":"999"}`,
			call:      func(r *Router, w http.ResponseWriter, req *http.Request) { r.handleDeezerLink(w, req) },
		},
		{
			name:      "refresh_link_mbid",
			lockField: "musicbrainz_id",
			body:      `{"mbid":"11111111-2222-3333-4444-555555555555"}`,
			call:      func(r *Router, w http.ResponseWriter, req *http.Request) { r.handleRefreshLink(w, req) },
		},
		{
			name:      "refresh_link_discogs",
			lockField: "discogs_id",
			body:      `{"discogs_id":"777"}`,
			call:      func(r *Router, w http.ResponseWriter, req *http.Request) { r.handleRefreshLink(w, req) },
		},
		{
			// THE REPUDIATION AXIS. clear_ids=true with no mbid is a
			// re-identify that CLEARS musicbrainz_id -- a write to that field
			// carrying no body value for it. A guard keyed on the body skips
			// the field here, the chokepoint silently restores the pinned MBID,
			// and the handler answers 200 for a repudiation that never
			// happened. The three cases above all supply the field they lock,
			// so the table looked exhaustive while missing the axis that
			// mattered.
			name:      "refresh_link_reidentify_repudiates_pinned_mbid",
			lockField: "musicbrainz_id",
			body:      `{"discogs_id":"777","clear_ids":"true"}`,
			call:      func(r *Router, w http.ResponseWriter, req *http.Request) { r.handleRefreshLink(w, req) },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, artistSvc := testRouter(t)
			a := addTestArtist(t, artistSvc, "Locked "+tc.name)
			a.DeezerID = "dz-stored"
			a.DiscogsID = "dg-stored"
			a.MusicBrainzID = "mb-stored"
			if err := artistSvc.Update(ctx, a); err != nil {
				t.Fatalf("seeding ids: %v", err)
			}
			if err := artistSvc.SetLockedFields(ctx, a.ID, []string{tc.lockField}); err != nil {
				t.Fatalf("locking %s: %v", tc.lockField, err)
			}
			// Precondition: the lock and the stored IDs persisted, or a 409
			// could come from something other than the lock.
			seeded, err := artistSvc.GetByID(ctx, a.ID)
			if err != nil {
				t.Fatalf("reloading seed: %v", err)
			}
			if len(seeded.LockedFields) != 1 || seeded.LockedFields[0] != tc.lockField {
				t.Fatalf("precondition: locked_fields = %v, want [%s]", seeded.LockedFields, tc.lockField)
			}
			// The seeded IDs too: without this a fixture regression surfaces
			// below as "the refusal did not prevent the write", blaming the
			// guard for a broken seed.
			if seeded.DeezerID != "dz-stored" || seeded.DiscogsID != "dg-stored" || seeded.MusicBrainzID != "mb-stored" {
				t.Fatalf("precondition: seeded ids = deezer:%q discogs:%q mbid:%q, want all three persisted",
					seeded.DeezerID, seeded.DiscogsID, seeded.MusicBrainzID)
			}

			req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/link", strings.NewReader(tc.body))
			req.SetPathValue("id", a.ID)
			req.Header.Set("Content-Type", "application/json")
			req = req.WithContext(testI18nCtx(t, req.Context()))
			w := httptest.NewRecorder()
			tc.call(r, w, req)

			if w.Code != http.StatusLocked {
				t.Fatalf("status = %d, want %d (423 Locked); a 200 tells the operator the link succeeded while the chokepoint silently reverted it. body=%s",
					w.Code, http.StatusLocked, w.Body.String())
			}
			var got map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatalf("decoding body %q: %v", w.Body.String(), err)
			}
			if got["error"] != "field_locked" {
				t.Errorf("error = %v, want %q; clients discriminate on this key", got["error"], "field_locked")
			}
			if got["field"] != tc.lockField {
				t.Errorf("field = %v, want %q", got["field"], tc.lockField)
			}

			// The stored identity must be untouched.
			after, err := artistSvc.GetByID(ctx, a.ID)
			if err != nil {
				t.Fatalf("reloading: %v", err)
			}
			if after.DeezerID != "dz-stored" || after.DiscogsID != "dg-stored" || after.MusicBrainzID != "mb-stored" {
				t.Errorf("stored ids = deezer:%q discogs:%q mbid:%q, want all unchanged; the refusal did not prevent the write",
					after.DeezerID, after.DiscogsID, after.MusicBrainzID)
			}
		})
	}
}

// TestLinkFlows_UnlockedProviderIDStillLinks is the over-correction control for
// the check above. Refusing an unpinned link would break the ordinary identify
// journey while claiming to protect the pinned one.
func TestLinkFlows_UnlockedProviderIDStillLinks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Unlocked Link")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/deezer/link", strings.NewReader(`{"deezer_id":"4050205"}`))
	req.SetPathValue("id", a.ID)
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()
	r.handleDeezerLink(w, req)

	if w.Code == http.StatusLocked {
		t.Fatalf("status = 423 on an UNLOCKED field; the lock check must not refuse an ordinary link. body=%s", w.Body.String())
	}
	after, err := artistSvc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("reloading: %v", err)
	}
	if after.DeezerID != "4050205" {
		t.Errorf("deezer_id = %q, want the link to land", after.DeezerID)
	}
}

// TestWriteFieldLockRefusal covers the renderer directly, because its two
// production call sites cannot reach it: both wrap the context with a matching
// grant, so the service verb never refuses them (see the function's own comment).
// Testing it through a handler would therefore prove nothing, and testing it not
// at all would leave the mapping from a typed service error to an operator's
// response entirely unpinned.
//
// The non-refusal arm is the load-bearing half. A renderer that answered 423 for
// ANY error would swallow a database failure as a lock refusal and satisfy the
// positive arm on its own.
func TestWriteFieldLockRefusal(t *testing.T) {
	r := &Router{logger: testLogger()}

	t.Run("typed lock refusal renders 423 with the house body", func(t *testing.T) {
		w := httptest.NewRecorder()
		const wantReason = "the discogs_id field is locked; unlock it before changing this value"
		err := fmt.Errorf("updating artist: %w",
			&artist.FieldLockedError{Field: "discogs_id", Reason: wantReason})
		if !r.writeFieldLockRefusal(w, "a1", err) {
			t.Fatal("a wrapped *artist.FieldLockedError was not recognized; errors.As must see through a wrap")
		}
		if w.Code != http.StatusLocked {
			t.Errorf("status = %d, want 423", w.Code)
		}
		var body map[string]any
		if decErr := json.NewDecoder(w.Body).Decode(&body); decErr != nil {
			t.Fatalf("decoding body: %v", decErr)
		}
		// Identical key set to refuseLockedProviderIDs, so a client
		// discriminating on "error" handles both without knowing which layer
		// noticed.
		if body["error"] != "field_locked" || body["field"] != "discogs_id" {
			t.Errorf("body = %+v, want error=field_locked field=discogs_id", body)
		}
		// The reason must be EXACTLY the typed literal, not merely absent the
		// wrap prefix -- a changed, empty, or unrelated reason must fail this
		// too, since the property under test is that the operator-facing text
		// is the hand-authored FieldLockedError.Reason and nothing else.
		if reason, _ := body["reason"].(string); reason != wantReason {
			t.Errorf("reason = %q, want %q (the typed Reason, verbatim)", reason, wantReason)
		}
		// Kept alongside the equality check: it pins the SPECIFIC failure mode
		// (the wrap's own prefix leaking through) with a message a maintainer
		// can act on immediately, rather than a generic mismatch diff.
		if reason, _ := body["reason"].(string); strings.Contains(reason, "updating artist") {
			t.Errorf("reason = %q; it rendered the error chain instead of the typed Reason", reason)
		}
	})

	t.Run("an ordinary error is left to the caller", func(t *testing.T) {
		w := httptest.NewRecorder()
		if r.writeFieldLockRefusal(w, "a1", errors.New("database is unavailable")) {
			t.Fatal("an unrecognized error must return false so the caller still logs it and answers 500")
		}
		if w.Code != http.StatusOK || w.Body.Len() != 0 {
			t.Errorf("the recorder was written to (code=%d body=%q); it must be untouched", w.Code, w.Body.String())
		}
	})
}
