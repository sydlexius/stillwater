package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/platform"
)

// The per-slot error paths of handlePushImages: the branches that record a
// failure against ONE image and let the loop carry on to the next type.
//
// These are the counterweight to the distrust branches this PR added. Those
// branches abort the whole loop because the read did not happen for a reason
// that applies to every remaining slot; these must NOT abort, because a bad
// slot says nothing about the others. A fix that over-propagated -- routing an
// ordinary per-slot failure through writeCanceledPush -- would turn one
// unreadable file into a whole failed push, and every assertion below is
// written to catch that: each checks HTTP 200 with the failure carried in the
// "errors" list, not an abort status.
//
// The "errors" JSON KEY is asserted explicitly. This PR renamed the Go
// identifier behind it (the local slice shadowed the errors package once the
// file began calling errors.Is), and the key is the wire contract clients read.
// A rename that reached the struct literal would be invisible to a test that
// only counted failures.

// pushErrorsResponse is the 200-with-errors shape all of these assert against.
type pushErrorsResponse struct {
	Uploaded []string `json:"uploaded"`
	Errors   []string `json:"errors"`
}

// pushImagesForErrors runs handlePushImages against dir for the given types and
// returns the decoded body, failing unless the handler answered 200.
//
// The 200 assertion is a PRECONDITION, not a formality: an abort answers 4xx or
// 5xx with no "errors" key at all, so decoding an aborted response would yield
// an empty list and every "want one error" assertion below would report a
// missing error rather than the over-propagated abort that actually happened.
func pushImagesForErrors(t *testing.T, dir string, imageTypes string, name string) pushErrorsResponse {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	r, artistSvc := testRouter(t)
	r.platformService = platform.NewService(r.db)

	a := &artist.Artist{Name: name, SortName: name, Path: dir}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	addTestConnectionWithURL(t, r, "conn-emby", "Emby", "emby", srv.URL)

	body := `{"connection_id":"conn-emby","platform_artist_id":"emby-1","image_types":[` + imageTypes + `]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/push/images",
		strings.NewReader(body))
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handlePushImages(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; a per-slot failure must not abort the push. body: %s",
			w.Code, w.Body.String())
	}
	var resp pushErrorsResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	return resp
}

// TestHandlePushImages_InvalidTypeIsRecordedAndTheRestProceed covers the
// validImageTypes rejection. The second half is the point: an unknown type is
// the operator's typo in ONE list entry and must not cost them the valid ones.
func TestHandlePushImages_InvalidTypeIsRecordedAndTheRestProceed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// A readable, uploadable thumb alongside the bogus type, so the test can
	// prove the loop CONTINUED rather than merely that it did not crash.
	if err := os.WriteFile(filepath.Join(dir, "folder.jpg"), []byte("fake-image"), 0o600); err != nil {
		t.Fatal(err)
	}

	resp := pushImagesForErrors(t, dir, `"not-an-image-type","thumb"`, "Invalid Type")

	if len(resp.Errors) != 1 {
		t.Fatalf("errors = %v, want exactly one (the invalid type)", resp.Errors)
	}
	if !strings.Contains(resp.Errors[0], "invalid image type") {
		t.Errorf("error %q does not name the invalid type as the problem", resp.Errors[0])
	}
	// The valid slot that FOLLOWS the invalid one must still have been pushed.
	// Ordered after it deliberately: a `return` where the code has `continue`
	// leaves this empty, which is the regression this assertion exists for.
	if len(resp.Uploaded) != 1 || resp.Uploaded[0] != "thumb" {
		t.Errorf("uploaded = %v, want [thumb]; the valid slot after an invalid one was skipped",
			resp.Uploaded)
	}
}

// TestHandlePushImages_FanartDiscoveryFailureIsRecorded covers the
// DiscoverFanart error branch: the directory itself could not be listed.
//
// This is deliberately NOT routed through the distrust predicate. Discovery
// failing is a fact about ONE directory, so the remaining types -- which resolve
// through a different lookup -- are still worth attempting. The unreadable
// directory is the whole artist path here, so the other slots simply find
// nothing; what matters is that the handler reported the fanart failure and
// still answered 200.
func TestHandlePushImages_FanartDiscoveryFailureIsRecorded(t *testing.T) {
	t.Parallel()
	// A path that does not exist: os.ReadDir fails, so DiscoverFanart returns
	// an error rather than an empty set. An empty set is the SILENT case and
	// would take the loop's no-fanart path instead, testing nothing.
	dir := filepath.Join(t.TempDir(), "no-such-directory")

	resp := pushImagesForErrors(t, dir, `"fanart"`, "Discovery Fail")

	if len(resp.Errors) != 1 {
		t.Fatalf("errors = %v, want exactly one (the failed directory read)", resp.Errors)
	}
	if !strings.Contains(resp.Errors[0], "failed to read directory") {
		t.Errorf("error %q does not tell the operator the fanart DIRECTORY could not be read",
			resp.Errors[0])
	}
	// The sanitization rule the sibling read-failure test also pins: the raw OS
	// error and the on-disk path must not reach the client.
	for _, leak := range []string{"no such file", "no-such-directory", "permission denied"} {
		if strings.Contains(resp.Errors[0], leak) {
			t.Errorf("error leaks raw detail %q: %q", leak, resp.Errors[0])
		}
	}
	if len(resp.Uploaded) != 0 {
		t.Errorf("uploaded = %v, want empty; nothing was readable", resp.Uploaded)
	}
}

// TestHandlePushImages_NonFanartReadFailureIsRecorded covers the read-failure
// branch of the NON-fanart path, which is a separate read reached through
// FindExistingImage rather than DiscoverFanart.
//
// It needs its own test for the same reason it needs its own guard in the
// handler: the two branches drifted apart once already, with the non-fanart one
// carrying a comment claiming parity it did not have.
func TestHandlePushImages_NonFanartReadFailureIsRecorded(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// A dangling symlink at the thumb name: FindExistingImage's stat follows it
	// and reports not-found, so instead plant a DIRECTORY at the filename. It
	// stats fine (so the slot is "found") and the read then fails with EISDIR --
	// an ordinary read failure that is emphatically not a stalled mount.
	if err := os.Mkdir(filepath.Join(dir, "folder.jpg"), 0o700); err != nil {
		t.Fatal(err)
	}

	resp := pushImagesForErrors(t, dir, `"thumb"`, "Non Fanart Read Fail")

	if len(resp.Errors) != 1 {
		t.Fatalf("errors = %v, want exactly one (the failed thumb read)", resp.Errors)
	}
	if !strings.Contains(resp.Errors[0], "thumb: read failed") {
		t.Errorf("error %q does not name the thumb slot as the failure", resp.Errors[0])
	}
	// The distinction this PR turns on: an ordinary read failure must NOT be
	// reported as a stalled mount. If it were, an operator with one bad file
	// would be sent to debug their NAS.
	if strings.Contains(resp.Errors[0], "mount is not responding") {
		t.Errorf("an ordinary read failure was reported as a stalled mount: %q", resp.Errors[0])
	}
	for _, leak := range []string{"is a directory", "no such file", dir} {
		if strings.Contains(resp.Errors[0], leak) {
			t.Errorf("error leaks raw detail %q: %q", leak, resp.Errors[0])
		}
	}
}

// TestHandlePushImages_NonFanartUploadFailureIsRecorded covers the upload-failure
// branch of the non-fanart path: the file read fine, the PEER rejected it.
//
// Distinct from the read failure above in the direction that matters to an
// operator -- their file is fine and the platform is the problem -- and the
// message must say so.
func TestHandlePushImages_NonFanartUploadFailureIsRecorded(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "folder.jpg"), []byte("fake-image"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A peer that fails every upload. Built inline rather than through the
	// shared helper because that one answers 200 to everything.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	r, artistSvc := testRouter(t)
	r.platformService = platform.NewService(r.db)

	a := &artist.Artist{Name: "Non Fanart Upload Fail", SortName: "Non Fanart Upload Fail", Path: dir}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	addTestConnectionWithURL(t, r, "conn-emby", "Emby", "emby", srv.URL)

	body := `{"connection_id":"conn-emby","platform_artist_id":"emby-1","image_types":["thumb"]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/push/images",
		strings.NewReader(body))
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handlePushImages(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; a failed upload is reported per-slot. body: %s",
			w.Code, w.Body.String())
	}
	var resp pushErrorsResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	if len(resp.Errors) != 1 {
		t.Fatalf("errors = %v, want exactly one (the failed thumb upload)", resp.Errors)
	}
	if !strings.Contains(resp.Errors[0], "thumb: upload failed") {
		t.Errorf("error %q does not name the thumb UPLOAD as the failure", resp.Errors[0])
	}
	// The read succeeded, so blaming the read would send the operator to the
	// wrong end of the system -- the same class of misreport this PR exists to
	// fix, in the opposite direction.
	if strings.Contains(resp.Errors[0], "read failed") {
		t.Errorf("a failed upload was reported as a failed read: %q", resp.Errors[0])
	}
	if strings.Contains(resp.Errors[0], "500") {
		t.Errorf("error leaks the peer's HTTP status: %q", resp.Errors[0])
	}
	if len(resp.Uploaded) != 0 {
		t.Errorf("uploaded = %v, want empty; the peer rejected it", resp.Uploaded)
	}
}
