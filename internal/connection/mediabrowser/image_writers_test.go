package mediabrowser

import (
	"context"
	"errors"
	"testing"
)

// noopClassifier passes the error through unchanged, matching what a real
// wrapAuthIfStatusAuth binding does for a non-auth-class error. Tests that
// care about auth wrapping supply their own classifier.
func noopClassifier(err error) error { return err }

func TestUploadImageRaw_IssuesPostAtPlainPath(t *testing.T) {
	tr := &rawTransport{}
	if err := UploadImageRaw(context.Background(), tr, testLogger(), "emby", "artist1", "Primary", "thumb", []byte{1, 2, 3}, "image/jpeg", noopClassifier); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tr.gotDoMethod != "POST" {
		t.Errorf("method = %q, want POST", tr.gotDoMethod)
	}
	if want := "/Items/artist1/Images/Primary"; tr.gotDoPath != want {
		t.Errorf("path = %q, want %q", tr.gotDoPath, want)
	}
	if tr.gotDoContentType != "image/jpeg" {
		t.Errorf("content-type = %q, want image/jpeg", tr.gotDoContentType)
	}
}

func TestUploadImageRaw_UnsupportedType(t *testing.T) {
	tr := &rawTransport{}
	err := UploadImageRaw(context.Background(), tr, testLogger(), "emby", "artist1", "", "bogus", []byte{1}, "image/jpeg", noopClassifier)
	if err == nil {
		t.Fatal("expected error for unsupported image type")
	}
	if tr.gotDoMethod != "" {
		t.Error("unsupported image type must not issue a request")
	}
}

// TestUploadImageAtIndexRaw_IssuesIndexedPath is the revert-and-rerun
// canary: it asserts the exact indexed path format, so a wrong-format
// production change (e.g. dropping the index or reordering type/index) is
// caught RED before it ships. See the manual revert-and-rerun proof
// (temporarily corrupting the path format string) reported alongside this
// PR.
func TestUploadImageAtIndexRaw_IssuesIndexedPath(t *testing.T) {
	tr := &rawTransport{}
	if err := UploadImageAtIndexRaw(context.Background(), tr, testLogger(), "jellyfin", "artist7", "Backdrop", "fanart", 4, []byte{9, 9}, "image/png", noopClassifier); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tr.gotDoMethod != "POST" {
		t.Errorf("method = %q, want POST", tr.gotDoMethod)
	}
	if want := "/Items/artist7/Images/Backdrop/4"; tr.gotDoPath != want {
		t.Errorf("path = %q, want %q", tr.gotDoPath, want)
	}
}

func TestUploadImageAtIndexRaw_RejectsNegativeIndex(t *testing.T) {
	tr := &rawTransport{}
	if err := UploadImageAtIndexRaw(context.Background(), tr, testLogger(), "emby", "artist1", "Backdrop", "fanart", -1, []byte{1}, "image/jpeg", noopClassifier); err == nil {
		t.Fatal("expected error for negative index")
	}
	if tr.gotDoMethod != "" {
		t.Error("negative index must not issue a request")
	}
}

func TestDeleteImageRaw_IssuesDeleteAtPlainPath(t *testing.T) {
	tr := &rawTransport{}
	if err := DeleteImageRaw(context.Background(), tr, testLogger(), "emby", "artist1", "Primary", "thumb", noopClassifier); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tr.gotDoMethod != "DELETE" {
		t.Errorf("method = %q, want DELETE", tr.gotDoMethod)
	}
	if want := "/Items/artist1/Images/Primary"; tr.gotDoPath != want {
		t.Errorf("path = %q, want %q", tr.gotDoPath, want)
	}
}

func TestDeleteImageAtIndexRaw_IssuesIndexedDelete(t *testing.T) {
	tr := &rawTransport{}
	if err := DeleteImageAtIndexRaw(context.Background(), tr, testLogger(), "jellyfin", "artist7", "Backdrop", "fanart", 2, noopClassifier); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tr.gotDoMethod != "DELETE" {
		t.Errorf("method = %q, want DELETE", tr.gotDoMethod)
	}
	if want := "/Items/artist7/Images/Backdrop/2"; tr.gotDoPath != want {
		t.Errorf("path = %q, want %q", tr.gotDoPath, want)
	}
}

func TestDeleteImageRaw_UnsupportedType(t *testing.T) {
	tr := &rawTransport{}
	err := DeleteImageRaw(context.Background(), tr, testLogger(), "emby", "artist1", "", "bogus", noopClassifier)
	if err == nil {
		t.Fatal("expected error for unsupported image type")
	}
	if tr.gotDoMethod != "" {
		t.Error("unsupported image type must not issue a request")
	}
}

func TestDeleteImageAtIndexRaw_RejectsNegativeIndex(t *testing.T) {
	tr := &rawTransport{}
	if err := DeleteImageAtIndexRaw(context.Background(), tr, testLogger(), "emby", "artist1", "Backdrop", "fanart", -1, noopClassifier); err == nil {
		t.Fatal("expected error for negative index")
	}
}

func TestDeleteImageAtIndexRaw_UnsupportedType(t *testing.T) {
	tr := &rawTransport{}
	err := DeleteImageAtIndexRaw(context.Background(), tr, testLogger(), "emby", "artist1", "", "bogus", 0, noopClassifier)
	if err == nil {
		t.Fatal("expected error for unsupported image type")
	}
	if tr.gotDoMethod != "" {
		t.Error("unsupported image type must not issue a request")
	}
}

// imageWriterCase exercises one of the four shared free functions through a
// single call shape, letting the three table-driven tests below cover all
// four functions' unsupported-type, Do-error, and server-error branches
// without four near-identical hand-written tests apiece.
type imageWriterCase struct {
	name string
	call func(tr *rawTransport, classify AuthErrorClassifier) error
}

var imageWriterCases = []imageWriterCase{
	{
		name: "UploadImageRaw",
		call: func(tr *rawTransport, classify AuthErrorClassifier) error {
			return UploadImageRaw(context.Background(), tr, testLogger(), "emby", "artist1", "Primary", "thumb", []byte{1}, "image/jpeg", classify)
		},
	},
	{
		name: "UploadImageAtIndexRaw",
		call: func(tr *rawTransport, classify AuthErrorClassifier) error {
			return UploadImageAtIndexRaw(context.Background(), tr, testLogger(), "emby", "artist1", "Backdrop", "fanart", 2, []byte{1}, "image/jpeg", classify)
		},
	},
	{
		name: "DeleteImageRaw",
		call: func(tr *rawTransport, classify AuthErrorClassifier) error {
			return DeleteImageRaw(context.Background(), tr, testLogger(), "emby", "artist1", "Primary", "thumb", classify)
		},
	},
	{
		name: "DeleteImageAtIndexRaw",
		call: func(tr *rawTransport, classify AuthErrorClassifier) error {
			return DeleteImageAtIndexRaw(context.Background(), tr, testLogger(), "emby", "artist1", "Backdrop", "fanart", 2, classify)
		},
	},
}

// TestImageWriters_DoErrorPropagates covers the "executing ... : %w" wrap
// branch in all four functions: a transport-level failure (network error,
// not a status code) must come back through errors.Is unchanged.
func TestImageWriters_DoErrorPropagates(t *testing.T) {
	for _, tc := range imageWriterCases {
		t.Run(tc.name, func(t *testing.T) {
			wantErr := errors.New("network boom")
			tr := &rawTransport{doErr: wantErr}
			err := tc.call(tr, noopClassifier)
			if !errors.Is(err, wantErr) {
				t.Errorf("expected wrapped Do error, got %v", err)
			}
		})
	}
}

// TestImageWriters_ServerErrorRoutesThroughClassifier covers the
// resp.StatusCode >= 300 branch in all four functions: the formatted status
// error must be handed to the caller's classifier (which routes 401/403 to
// a platform sentinel in production).
func TestImageWriters_ServerErrorRoutesThroughClassifier(t *testing.T) {
	wantSentinel := errors.New("sentinel")
	classify := func(err error) error {
		if err == nil {
			return nil
		}
		return errors.Join(err, wantSentinel)
	}
	for _, tc := range imageWriterCases {
		t.Run(tc.name, func(t *testing.T) {
			tr := &rawTransport{doStatus: 500, doBody: "boom"}
			err := tc.call(tr, classify)
			if err == nil {
				t.Fatal("expected error for 500 response")
			}
			if !errors.Is(err, wantSentinel) {
				t.Error("expected classifier's sentinel to be reachable via errors.Is")
			}
		})
	}
}
