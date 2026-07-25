package api

// handlers_name_collision_guard_test.go -- API-boundary guards for #2730.
//
// The service-layer tests (internal/artist/name_collision_test.go) prove the
// DETECTION is right. These prove the POLICY: that handleFieldUpdate actually
// consults it, refuses the write, and hands the operator a next step.
//
// Every test here asserts the DATABASE STATE, not just the status code. A
// handler that answered 409 and wrote anyway would pass a code-only check,
// and "the write happened despite the warning" is precisely the #2730 defect.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
)

// patchName issues a form-encoded PATCH of the name field, mirroring what the
// artist detail page's inline editor sends.
func patchName(t *testing.T, r *Router, artistID, newName string, htmx bool) *httptest.ResponseRecorder {
	t.Helper()
	body := strings.NewReader("value=" + newName)
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/artists/"+artistID+"/fields/name", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if htmx {
		req.Header.Set("HX-Request", "true")
	}
	req.SetPathValue("id", artistID)
	req.SetPathValue("field", "name")
	// Production routes run behind i18n.Middleware, which puts a Translator in
	// the request context. Injecting it here keeps the test on the same path:
	// without it the fragment renders bare i18n keys, which is a rendering
	// artifact of the harness rather than a real defect.
	req = req.WithContext(testI18nCtx(t, req.Context()))
	w := httptest.NewRecorder()
	r.handleFieldUpdate(w, req)
	return w
}

// nameOf reads an artist's stored name straight back from the service, so the
// assertions measure persisted state rather than the handler's response body.
func nameOf(t *testing.T, svc *artist.Service, id string) string {
	t.Helper()
	a, err := svc.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("GetByID(%s): %v", id, err)
	}
	return a.Name
}

// addPlatformOnlyArtist creates an artist with NO filesystem path, which is
// what an Emby/Jellyfin populate produces for an artist that has no directory.
// addTestArtist always sets a path, so this variant is required to reproduce
// the exact pair the issue describes.
func addPlatformOnlyArtist(t *testing.T, svc *artist.Service, name string) *artist.Artist {
	t.Helper()
	a := &artist.Artist{Name: name, SortName: name, Type: "group", Path: ""}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating platform-only artist %s: %v", name, err)
	}
	return a
}

// TestHandleFieldUpdate_NameCollision_RefusesWrite is the headline #2730 case:
// renaming a platform-only artist onto an existing artist's name must be
// refused with 409 AND must leave the stored name untouched.
func TestHandleFieldUpdate_NameCollision_RefusesWrite(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	existing := addTestArtist(t, artistSvc, "Northfield Chorale")
	platformOnly := addPlatformOnlyArtist(t, artistSvc, "Northfield Chorale Live")

	// Precondition: the two names are genuinely distinct to begin with, so a
	// blanket-reject bug cannot masquerade as a correct guard.
	if nameOf(t, artistSvc, existing.ID) == nameOf(t, artistSvc, platformOnly.ID) {
		t.Fatal("precondition: the seeded artists already share a name; the fixture is not exercising a rename")
	}

	w := patchName(t, r, platformOnly.ID, "Northfield Chorale", false)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d (a colliding rename must be refused); body: %s",
			w.Code, http.StatusConflict, w.Body.String())
	}
	// The load-bearing assertion: the write did NOT happen.
	if got := nameOf(t, artistSvc, platformOnly.ID); got != "Northfield Chorale Live" {
		t.Errorf("stored name = %q, want it unchanged at %q: the guard answered 409 but the write still landed",
			got, "Northfield Chorale Live")
	}
	// And the other artist was not disturbed either.
	if got := nameOf(t, artistSvc, existing.ID); got != "Northfield Chorale" {
		t.Errorf("existing artist name = %q, want %q", got, "Northfield Chorale")
	}
}

// TestHandleFieldUpdate_NameCollision_JSONBodyNamesPartner asserts the API
// response carries enough for a client to act: the sentinel text plus the
// colliding artist's ID. A bare 409 would tell the operator nothing.
func TestHandleFieldUpdate_NameCollision_JSONBodyNamesPartner(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	existing := addTestArtist(t, artistSvc, "Northfield Chorale")
	platformOnly := addPlatformOnlyArtist(t, artistSvc, "Southgate Winds")

	w := patchName(t, r, platformOnly.ID, "Northfield Chorale", false)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusConflict, w.Body.String())
	}

	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding response body %q: %v", w.Body.String(), err)
	}
	if body["error"] != artist.ErrNameCollision.Error() {
		t.Errorf("error = %q, want %q", body["error"], artist.ErrNameCollision.Error())
	}
	if body["existing_artist_id"] != existing.ID {
		t.Errorf("existing_artist_id = %q, want %q", body["existing_artist_id"], existing.ID)
	}
	if body["existing_name"] != "Northfield Chorale" {
		t.Errorf("existing_name = %q, want %q", body["existing_name"], "Northfield Chorale")
	}
}

// TestHandleFieldUpdate_NameCollision_HTMXPointsAtMerge asserts the HTMX body
// is the warning fragment and that it links to the duplicates report. The link
// is the actionable half of the acceptance criterion: without it the operator
// is told "no" with no path forward.
func TestHandleFieldUpdate_NameCollision_HTMXPointsAtMerge(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	addTestArtist(t, artistSvc, "Northfield Chorale")
	platformOnly := addPlatformOnlyArtist(t, artistSvc, "Southgate Winds")

	w := patchName(t, r, platformOnly.ID, "Northfield Chorale", true)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusConflict, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html for an HTMX request", ct)
	}

	body := w.Body.String()
	if !strings.Contains(body, "/reports/duplicates") {
		t.Errorf("body does not link to the duplicates report, so the operator has no next step:\n%s", body)
	}
	if !strings.Contains(body, "Northfield Chorale") {
		t.Errorf("body does not name the colliding artist:\n%s", body)
	}
	// The fragment must render translated copy, not a bare key. A missing
	// en.json entry would otherwise ship as literal "name_collision.merge_link".
	if strings.Contains(body, "name_collision.") {
		t.Errorf("body contains an untranslated i18n key (missing en.json entry):\n%s", body)
	}
}

// TestHandleFieldUpdate_NameCollision_PlatformOnlyPartnerNoted covers the
// reverse direction and the platform-only annotation: renaming the
// path-bearing artist onto the platform-only row must explain that the other
// record has no folder on disk, since that is why the operator cannot simply
// look for its directory.
func TestHandleFieldUpdate_NameCollision_PlatformOnlyPartnerNoted(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	local := addTestArtist(t, artistSvc, "Northfield Chorale")
	addPlatformOnlyArtist(t, artistSvc, "Southgate Winds")

	w := patchName(t, r, local.ID, "Southgate Winds", true)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusConflict, w.Body.String())
	}
	if body := w.Body.String(); !strings.Contains(body, "only on a connected platform") {
		t.Errorf("body omits the platform-only note for a path-less partner:\n%s", body)
	}
	if got := nameOf(t, artistSvc, local.ID); got != "Northfield Chorale" {
		t.Errorf("stored name = %q, want it unchanged: the refused rename still wrote", got)
	}
}

// TestHandleFieldUpdate_NameCollision_AllowsNonColliding is the over-blocking
// control. A guard that rejected every name change would pass all the tests
// above; this one fails unless legitimate edits still succeed.
func TestHandleFieldUpdate_NameCollision_AllowsNonColliding(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	addTestArtist(t, artistSvc, "Northfield Chorale")
	subject := addPlatformOnlyArtist(t, artistSvc, "Southgate Winds")

	w := patchName(t, r, subject.ID, "Brackenmoor Ensemble", false)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d for a non-colliding rename; body: %s",
			w.Code, http.StatusOK, w.Body.String())
	}
	if got := nameOf(t, artistSvc, subject.ID); got != "Brackenmoor Ensemble" {
		t.Errorf("stored name = %q, want %q: a safe rename must still be written", got, "Brackenmoor Ensemble")
	}
}

// TestHandleFieldUpdate_NameCollision_OtherFieldsUnguarded confirms the guard
// is scoped to the name field. Running an artist-table scan on every biography
// edit would be a real cost, and only the name determines identity.
func TestHandleFieldUpdate_NameCollision_OtherFieldsUnguarded(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	addTestArtist(t, artistSvc, "Northfield Chorale")
	subject := addPlatformOnlyArtist(t, artistSvc, "Southgate Winds")

	// A biography whose text happens to equal another artist's name must not
	// trip the guard.
	body := strings.NewReader("value=Northfield Chorale")
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/artists/"+subject.ID+"/fields/biography", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("id", subject.ID)
	req.SetPathValue("field", "biography")
	w := httptest.NewRecorder()
	r.handleFieldUpdate(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: the collision guard must not apply to non-name fields; body: %s",
			w.Code, http.StatusOK, w.Body.String())
	}
	if got := nameOf(t, artistSvc, subject.ID); got != "Southgate Winds" {
		t.Errorf("name = %q, want it untouched by a biography edit", got)
	}
}

// --- fail-closed guard -------------------------------------------------

// collisionCheckFailingRepo forces GetByID to fail so FindNameCollision cannot
// complete. Embedding the artist.Repository INTERFACE keeps the decorator to a
// single overridden method; every other call delegates to the real repository.
//
// GetByID is the right lever because it is the FIRST thing FindNameCollision
// does, and nothing in handleFieldUpdate reads the artist before the guard
// runs. So the failure lands squarely on the guard rather than on some earlier
// step, which is what makes the resulting 500 attributable.
type collisionCheckFailingRepo struct {
	artist.Repository
}

func (r *collisionCheckFailingRepo) GetByID(_ context.Context, _ string) (*artist.Artist, error) {
	return nil, errors.New("simulated repository failure")
}

// TestHandleFieldUpdate_NameCollision_FailsClosed is the regression guard for
// the fail-closed contract: when the collision check itself cannot run, the
// handler must REFUSE the write, not fall through to it.
//
// This is the branch most at risk from a well-meaning refactor. Turning the
// 500 into a logged warning and a fall-through would look like graceful
// degradation and would keep every other test in this file green, while
// silently restoring the exact #2730 defect -- because "the guard could not
// run" is not evidence that the rename is safe.
//
// The write assertion is the load-bearing half. A status-only check would pass
// against a handler that answers 500 AFTER having already written the field.
func TestHandleFieldUpdate_NameCollision_FailsClosed(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	subject := addTestArtist(t, artistSvc, "Southgate Winds")

	// A second artist whose name is the rename target. Its presence means a
	// WORKING guard would have refused this write anyway (409), so the test
	// cannot pass merely because the rename was harmless.
	addTestArtist(t, artistSvc, "Northfield Chorale")

	// Reader built on the same DB with REAL repos, so the post-condition read
	// is unaffected by the fault injection below.
	reader := artist.NewService(r.db)

	// Precondition: the field holds its original value before the attempt.
	if got := nameOf(t, reader, subject.ID); got != "Southgate Winds" {
		t.Fatalf("precondition: name = %q, want %q", got, "Southgate Winds")
	}

	// Swap the router's artist service for one whose repository fails the
	// lookup the guard depends on.
	artists, providers, members, aliases, images, platformIDs, completeness := artist.NewDefaultRepos(r.db)
	r.artistService = artist.NewServiceWithRepos(
		&collisionCheckFailingRepo{Repository: artists},
		providers, members, aliases, images, platformIDs, completeness)

	w := patchName(t, r, subject.ID, "Northfield Chorale", false)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d: a collision check that cannot run must refuse the write, "+
			"never fall through to it; body: %s", w.Code, http.StatusInternalServerError, w.Body.String())
	}
	// The assertion that actually pins fail-closed behavior.
	if got := nameOf(t, reader, subject.ID); got != "Southgate Winds" {
		t.Errorf("stored name = %q, want it unchanged at %q: the write landed despite the guard failing, "+
			"which is the #2730 defect re-opened through the error path",
			got, "Southgate Winds")
	}
}

// TestHandleFieldUpdate_NameCollision_ReportsStoredNameNotTypedName pins the
// one piece of information the 409 message exists to carry: WHICH artist is
// already using the name.
//
// The fixture is built so the REQUESTED name and the STORED name are
// DIFFERENT strings that share an identity key -- "the northfield chorale"
// typed against a stored "The Northfield Chorale". Every other fixture in this
// file uses the same string for both, which makes collision.Name and newName
// indistinguishable: swapping one for the other passes those tests unchanged.
//
// If that regresses, the operator is told *Another artist named "the
// northfield chorale" already uses this name* -- their own typing echoed back,
// naming an artist that does not exist under that spelling. They would go
// looking for a record that is not there. Asserting the ABSENCE of the typed
// string is what makes this test able to catch the swap; asserting only the
// presence of the stored name would still pass if both were rendered.
func TestHandleFieldUpdate_NameCollision_ReportsStoredNameNotTypedName(t *testing.T) {
	t.Parallel()

	const (
		storedName = "The Northfield Chorale"
		typedName  = "the northfield chorale"
	)

	// Precondition: the two spellings must genuinely differ yet collide, or the
	// test degenerates into the same-string case it exists to escape.
	if storedName == typedName {
		t.Fatal("precondition: fixture names must differ, or collision.Name and newName are indistinguishable")
	}

	t.Run("json envelope", func(t *testing.T) {
		t.Parallel()
		r, artistSvc := testRouter(t)
		existing := addTestArtist(t, artistSvc, storedName)
		subject := addPlatformOnlyArtist(t, artistSvc, "Southgate Winds")

		w := patchName(t, r, subject.ID, typedName, false)
		if w.Code != http.StatusConflict {
			t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusConflict, w.Body.String())
		}

		var body map[string]string
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decoding response body %q: %v", w.Body.String(), err)
		}
		if body["existing_name"] != storedName {
			t.Errorf("existing_name = %q, want the STORED name %q; reporting the typed value names "+
				"an artist that does not exist under that spelling", body["existing_name"], storedName)
		}
		if body["existing_artist_id"] != existing.ID {
			t.Errorf("existing_artist_id = %q, want %q", body["existing_artist_id"], existing.ID)
		}
	})

	t.Run("rendered fragment", func(t *testing.T) {
		t.Parallel()
		r, artistSvc := testRouter(t)
		addTestArtist(t, artistSvc, storedName)
		subject := addPlatformOnlyArtist(t, artistSvc, "Southgate Winds")

		w := patchName(t, r, subject.ID, typedName, true)
		if w.Code != http.StatusConflict {
			t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusConflict, w.Body.String())
		}

		body := w.Body.String()
		if !strings.Contains(body, storedName) {
			t.Errorf("fragment does not name the STORED artist %q:\n%s", storedName, body)
		}
		// The discriminating assertion. templ HTML-escapes its interpolations,
		// so the typed string would appear verbatim if it were rendered.
		if strings.Contains(body, typedName) {
			t.Errorf("fragment echoes the operator's typed name %q back at them instead of naming "+
				"the existing artist; the message then points at a record that does not exist:\n%s",
				typedName, body)
		}
	})
}

// TestHandleFieldUpdate_NameCollision_DoesNotPromiseAMerge pins the copy fix
// for the two cases where the 409 points at a report that will not show the
// pair (#2730 review, #2798).
//
// The duplicates report fails to display a colliding pair in two unrelated
// ways that look identical to the operator:
//
//  1. Conflicting MBIDs -- DetectDuplicates refuses to group artists bound to
//     different non-empty MusicBrainz IDs, so no group ever forms.
//  2. An ignored group -- the page filters server-side ignores, so the group
//     exists but is hidden from the operator who ignored it.
//
// The guard cannot detect either without reading data it is deliberately kept
// away from, so the copy must be true WITHOUT distinguishing them. This test
// asserts the message does not make the unconditional promise, and still
// carries the part that is always true (which artist the collision is with).
//
// It is a copy guard, not a mechanism guard: it does not seed an MBID conflict
// or an ignore record, because the point is that the SAME message must serve
// every case. Reverting to "Merge them on the duplicates report" turns it RED.
func TestHandleFieldUpdate_NameCollision_DoesNotPromiseAMerge(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	addTestArtist(t, artistSvc, "Northfield Chorale")
	subject := addPlatformOnlyArtist(t, artistSvc, "Southgate Winds")

	w := patchName(t, r, subject.ID, "Northfield Chorale", true)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusConflict, w.Body.String())
	}
	body := w.Body.String()

	// Always true and always useful: WHICH artist holds the name.
	if !strings.Contains(body, "Northfield Chorale") {
		t.Errorf("message no longer names the colliding artist, which is the one fact that "+
			"holds in every case:\n%s", body)
	}

	// The unconditional promise must be gone. "Merge them on the duplicates
	// report" asserts an affordance that may not be displayed.
	if strings.Contains(body, "Merge them on") {
		t.Errorf("message promises a merge on the duplicates report unconditionally; the report "+
			"will not display the pair when the two carry conflicting MusicBrainz IDs, nor when "+
			"the group was previously ignored:\n%s", body)
	}

	// The operator is given somewhere else to look when the pair is absent.
	// Without this the message is accurate but leaves a dead end.
	if !strings.Contains(body, "ignored") {
		t.Errorf("message does not point at the ignored groups, so an operator whose pair is "+
			"hidden by a prior ignore has no next step:\n%s", body)
	}

	// Mechanics must stay out of operator-facing copy: nobody should need to
	// know what an MBID is to read this.
	for _, jargon := range []string{"MusicBrainz", "MBID", "identity key", "normalized"} {
		if strings.Contains(body, jargon) {
			t.Errorf("message exposes internal mechanics (%q) to the operator:\n%s", jargon, body)
		}
	}
}
