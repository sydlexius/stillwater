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
