package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
)

// #2415. /images/fetch and /images/upload answer an aspect-ratio mismatch with
// a 200 that SAVED NOTHING. That is a legitimate contract -- the body carries
// needs_crop plus the image data so the client can prompt for a crop -- but it
// is also the shape of Stillwater's dominant bug class: a success response for
// work that did not happen. It used to be the one image-save outcome that
// logged nothing at all, so an operator had no way to tell it apart from a real
// save.
//
// These tests therefore assert the OUTCOME (no file on disk) rather than the
// status code, and pin the log line that makes the short-circuit observable.
// Asserting the 200 would be guarding the bug.

// captureLogs points the router's logger at a buffer and returns the decoded
// records. Records are decoded lazily by the returned func so the caller reads
// them after the request has run.
func captureLogs(t *testing.T, r *Router) func() []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	r.logger = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return func() []map[string]any {
		t.Helper()
		var out []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			if line == "" {
				continue
			}
			var rec map[string]any
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatalf("decoding log line %q: %v", line, err)
			}
			out = append(out, rec)
		}
		return out
	}
}

// findNeedsCropLog returns the short-circuit log record, or nil if the handler
// stayed silent about not saving.
func findNeedsCropLog(recs []map[string]any) map[string]any {
	for _, rec := range recs {
		if m, _ := rec["msg"].(string); m == needsCropLogMsg {
			return rec
		}
	}
	return nil
}

// imageFilesIn lists the entries of an artist's image directory. A needs_crop
// short-circuit must leave it exactly as it found it.
func imageFilesIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading image dir: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

const needsCropLogMsg = "image not saved: aspect ratio needs cropping"

func TestHandleImageFetch_NeedsCrop_SavesNothingAndSaysSo(t *testing.T) {
	t.Parallel()
	r, svc := newImageHandlerTestServer(t)
	dir := t.TempDir()
	a := &artist.Artist{Name: "NeedsCropFetch", SortName: "NeedsCropFetch", Path: dir}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	readLogs := captureLogs(t, r)

	// thumb wants a square; 1000x100 is a hard aspect mismatch.
	r.ssrfClient = &http.Client{Transport: &stubRoundTripper{body: testJPEG(t, 1000, 100)}}

	if got := imageFilesIn(t, dir); len(got) != 0 {
		t.Fatalf("precondition: image dir must start empty, has %v", got)
	}

	body := strings.NewReader(`{"url":"https://8.8.8.8/wide.jpg","type":"thumb"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/fetch", body)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", a.ID)

	w := serveValidated(t, http.HandlerFunc(r.handleImageFetch), req)

	// The outcome, not the status: nothing was written. This is what makes the
	// needs_crop signal load-bearing -- if the client ignores it, the user's
	// image is simply gone.
	if got := imageFilesIn(t, dir); len(got) != 0 {
		t.Fatalf("a needs_crop response must save NOTHING, but the image dir holds %v", got)
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if needsCrop, _ := resp["needs_crop"].(bool); !needsCrop {
		t.Fatalf("needs_crop = %v, want true (nothing was saved, so the client MUST be told)", resp["needs_crop"])
	}
	if data, _ := resp["image_data"].(string); !strings.HasPrefix(data, "data:image/") {
		t.Errorf("image_data = %.30q, want a data: URI the client can load into the cropper", data)
	}

	rec := findNeedsCropLog(readLogs())
	if rec == nil {
		t.Fatalf("no %q log line: the one save outcome that writes nothing must not also be the one that logs nothing", needsCropLogMsg)
	}
	if got := rec["level"]; got != "INFO" {
		t.Errorf("log level = %v, want INFO (a short-circuit is expected, not an error)", got)
	}
	if got := rec["artist_id"]; got != a.ID {
		t.Errorf("log artist_id = %v, want %q", got, a.ID)
	}
	if got := rec["type"]; got != "thumb" {
		t.Errorf("log type = %v, want thumb", got)
	}
	if got, ok := rec["actual_ratio"].(float64); !ok || got < 9 || got > 11 {
		t.Errorf("log actual_ratio = %v, want ~10 (1000x100)", rec["actual_ratio"])
	}
}

func TestHandleImageFetch_NeedsCrop_LogsTheFanartSlot(t *testing.T) {
	t.Parallel()
	r, svc := newImageHandlerTestServer(t)
	dir := t.TempDir()
	a := &artist.Artist{Name: "SlotLog", SortName: "SlotLog", Path: dir, FanartExists: true}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	// An explicit slot must already exist on disk (validateFanartSlot runs
	// before the fetch), so seed the primary backdrop.
	if err := os.WriteFile(dir+"/fanart.jpg", testJPEG(t, 1920, 1080), 0o600); err != nil {
		t.Fatalf("seeding fanart: %v", err)
	}
	readLogs := captureLogs(t, r)

	// fanart wants 16:9; a square is a mismatch.
	r.ssrfClient = &http.Client{Transport: &stubRoundTripper{body: testJPEG(t, 800, 800)}}

	body := strings.NewReader(`{"url":"https://8.8.8.8/sq.jpg","type":"fanart","slot":0}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/fetch", body)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", a.ID)

	serveValidated(t, http.HandlerFunc(r.handleImageFetch), req)

	rec := findNeedsCropLog(readLogs())
	if rec == nil {
		t.Fatalf("no %q log line", needsCropLogMsg)
	}
	if got, ok := rec["slot"].(float64); !ok || int(got) != 0 {
		t.Errorf("log slot = %v, want 0 -- the operator needs to know WHICH backdrop was not replaced", rec["slot"])
	}
}

func TestHandleImageUpload_NeedsCrop_SavesNothingAndSaysSo(t *testing.T) {
	t.Parallel()
	r, svc := newImageHandlerTestServer(t)
	dir := t.TempDir()
	a := &artist.Artist{Name: "NeedsCropUpload", SortName: "NeedsCropUpload", Path: dir}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	readLogs := captureLogs(t, r)

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if err := mw.WriteField("type", "thumb"); err != nil {
		t.Fatalf("writing type field: %v", err)
	}
	hdr := make(map[string][]string)
	hdr["Content-Disposition"] = []string{`form-data; name="file"; filename="wide.jpg"`}
	hdr["Content-Type"] = []string{"image/jpeg"}
	part, err := mw.CreatePart(hdr)
	if err != nil {
		t.Fatalf("creating part: %v", err)
	}
	if _, err := part.Write(testJPEG(t, 1000, 100)); err != nil {
		t.Fatalf("writing part: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("closing writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/upload", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.SetPathValue("id", a.ID)

	w := httptest.NewRecorder()
	r.handleImageUpload(w, req)

	if got := imageFilesIn(t, dir); len(got) != 0 {
		t.Fatalf("a needs_crop upload must save NOTHING, but the image dir holds %v", got)
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if needsCrop, _ := resp["needs_crop"].(bool); !needsCrop {
		t.Fatalf("needs_crop = %v, want true", resp["needs_crop"])
	}

	rec := findNeedsCropLog(readLogs())
	if rec == nil {
		t.Fatalf("no %q log line on the upload path -- it short-circuits exactly like fetch", needsCropLogMsg)
	}
	if got := rec["artist_id"]; got != a.ID {
		t.Errorf("log artist_id = %v, want %q", got, a.ID)
	}
	if _, present := rec["slot"]; present {
		t.Errorf("an upload carries no fanart slot, so the log must not claim one: %v", rec["slot"])
	}
}

func TestHandleImageFetch_NormalSave_DoesNotLogNeedsCrop(t *testing.T) {
	t.Parallel()
	r, svc := newImageHandlerTestServer(t)
	dir := t.TempDir()
	a := &artist.Artist{Name: "CleanSave", SortName: "CleanSave", Path: dir}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	readLogs := captureLogs(t, r)

	// A square thumb needs no crop, so this one really does save.
	r.ssrfClient = &http.Client{Transport: &stubRoundTripper{body: testJPEG(t, 500, 500)}}

	body := strings.NewReader(`{"url":"https://8.8.8.8/sq.jpg","type":"thumb"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/fetch", body)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", a.ID)

	serveValidated(t, http.HandlerFunc(r.handleImageFetch), req)

	// Keeps the short-circuit tests above honest: the log line tracks the
	// short-circuit, not merely "a fetch happened".
	if got := imageFilesIn(t, dir); len(got) == 0 {
		t.Fatalf("precondition: a matching aspect ratio must actually save the image")
	}
	if rec := findNeedsCropLog(readLogs()); rec != nil {
		t.Errorf("a real save must not log the needs_crop short-circuit: %v", rec)
	}
}
