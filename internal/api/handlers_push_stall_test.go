package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	img "github.com/sydlexius/stillwater/internal/image"
)

// #2976 review. writeCanceledPush answered HTTP 499 "the request ended" for
// BOTH causes that stop a push early. They are not the same event:
//
//   - a cancellation IS the client ending the request (499 is right);
//   - a stalled-read cap refusal is the SERVER unable to read its own library
//     while the request context is still perfectly alive.
//
// Reporting the second as 499 tells an operator their browser gave up when
// their mount actually stopped answering, sending them to debug the wrong end
// of the system. The cause is available -- the sentinel reaches this function
// as `cause` -- so nothing but the classification was missing.

func newPushTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// TestWriteCanceledPush_DistinguishesStalledMountFromCancellation is the
// regression: each cause gets its own status, and the body names the right one.
func TestWriteCanceledPush_DistinguishesStalledMountFromCancellation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		cause      error
		wantStatus int
		// wantBody is a phrase that must appear; wantAbsent must not.
		wantBody   string
		wantAbsent string
	}{
		{
			name:       "a stalled mount is a server condition",
			cause:      img.ErrTooManyStalledReads,
			wantStatus: http.StatusServiceUnavailable,
			wantBody:   "mount is not responding",
			wantAbsent: "the request ended",
		},
		{
			name:       "a wrapped cap error is still recognized",
			cause:      fmt.Errorf("reading fanart: %w", img.ErrTooManyStalledReads),
			wantStatus: http.StatusServiceUnavailable,
			wantBody:   "mount is not responding",
			wantAbsent: "the request ended",
		},
		{
			name:       "a cancellation is still the client",
			cause:      errors.New("context canceled"),
			wantStatus: StatusClientClosedRequest,
			wantBody:   "the request ended",
			wantAbsent: "mount is not responding",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := httptest.NewRecorder()
			uploaded := []string{"poster", "banner"}
			writeCanceledPush(w, newPushTestLogger(), "Some Artist", uploaded, tc.cause)

			if w.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", w.Code, tc.wantStatus)
			}
			body := w.Body.String()
			if !strings.Contains(body, tc.wantBody) {
				t.Errorf("body %q does not contain %q", body, tc.wantBody)
			}
			if strings.Contains(body, tc.wantAbsent) {
				t.Errorf("body %q blames the wrong cause (contains %q)", body, tc.wantAbsent)
			}

			// The partial progress survives on BOTH paths. An operator who
			// retries needs to know which images already landed, and that is
			// as true of a stalled mount as of a cancellation.
			var decoded struct {
				Uploaded []string `json:"uploaded"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &decoded); err != nil {
				t.Fatalf("decoding body %q: %v", body, err)
			}
			if len(decoded.Uploaded) != len(uploaded) {
				t.Errorf("uploaded = %v, want %v -- partial progress must survive both paths",
					decoded.Uploaded, uploaded)
			}
		})
	}
}

// TestWriteCanceledPush_StalledMountBodyLeaksNoPath guards the sanitization.
// The 503 arm is a new operator-facing string on an authenticated but
// error-prone path; it must name the CONDITION without carrying a filesystem
// path or an internal error chain into the response.
func TestWriteCanceledPush_StalledMountBodyLeaksNoPath(t *testing.T) {
	t.Parallel()
	w := httptest.NewRecorder()
	cause := fmt.Errorf("reading /srv/media/Some Artist/fanart3.jpg: %w", img.ErrTooManyStalledReads)
	writeCanceledPush(w, newPushTestLogger(), "Some Artist", nil, cause)

	body := w.Body.String()
	for _, leak := range []string{"/srv/media", "fanart3.jpg"} {
		if strings.Contains(body, leak) {
			t.Errorf("response body leaks %q: %s", leak, body)
		}
	}
}
