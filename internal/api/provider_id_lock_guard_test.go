package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
		name       string
		lockField  string
		body       string
		call       func(*Router, http.ResponseWriter, *http.Request)
		seed       func(*testing.T, *Router, string)
		storedWant func(t *testing.T, r *Router, id string)
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

			req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/link", strings.NewReader(tc.body))
			req.SetPathValue("id", a.ID)
			req.Header.Set("Content-Type", "application/json")
			req = req.WithContext(testI18nCtx(t, req.Context()))
			w := httptest.NewRecorder()
			tc.call(r, w, req)

			if w.Code != http.StatusConflict {
				t.Fatalf("status = %d, want %d; a 200 tells the operator the link succeeded while the chokepoint silently reverted it. body=%s",
					w.Code, http.StatusConflict, w.Body.String())
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

	if w.Code == http.StatusConflict {
		t.Fatalf("status = 409 on an UNLOCKED field; the lock check must not refuse an ordinary link. body=%s", w.Body.String())
	}
	after, err := artistSvc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("reloading: %v", err)
	}
	if after.DeezerID != "4050205" {
		t.Errorf("deezer_id = %q, want the link to land", after.DeezerID)
	}
}
