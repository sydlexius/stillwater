package emby

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/connection"
)

// TestBuildProviderIDs covers the #1084 mapping from ArtistPushData external
// IDs into Emby's canonical ProviderIds dictionary keys. Empty IDs must be
// omitted so a missing-in-Stillwater value never clears a Jellyfin/Emby-side
// value via "".
func TestBuildProviderIDs(t *testing.T) {
	t.Run("all four IDs map to canonical Emby keys", func(t *testing.T) {
		got := buildProviderIDs(connection.ArtistPushData{
			MusicBrainzID: "mb-1",
			AudioDBID:     "adb-2",
			DiscogsID:     "dsc-3",
			SpotifyID:     "spo-4",
		})
		want := map[string]string{
			"MusicBrainzArtist": "mb-1",
			"TheAudioDb":        "adb-2",
			"Discogs":           "dsc-3",
			"Spotify":           "spo-4",
		}
		if len(got) != len(want) {
			t.Fatalf("expected %d keys, got %d: %+v", len(want), len(got), got)
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("key %q: want %q, got %q", k, v, got[k])
			}
		}
	})

	t.Run("partial input only emits non-empty IDs", func(t *testing.T) {
		got := buildProviderIDs(connection.ArtistPushData{
			MusicBrainzID: "mb-1",
			SpotifyID:     "spo-4",
		})
		if len(got) != 2 {
			t.Fatalf("expected 2 keys, got %d: %+v", len(got), got)
		}
		if got["MusicBrainzArtist"] != "mb-1" {
			t.Errorf("MusicBrainzArtist missing or wrong: %+v", got)
		}
		if got["Spotify"] != "spo-4" {
			t.Errorf("Spotify missing or wrong: %+v", got)
		}
		if _, present := got["TheAudioDb"]; present {
			t.Errorf("TheAudioDb must be omitted when AudioDBID is empty: %+v", got)
		}
		if _, present := got["Discogs"]; present {
			t.Errorf("Discogs must be omitted when DiscogsID is empty: %+v", got)
		}
	})

	t.Run("empty input yields empty map", func(t *testing.T) {
		got := buildProviderIDs(connection.ArtistPushData{})
		if len(got) != 0 {
			t.Errorf("expected empty map, got %+v", got)
		}
	})
}

func TestDeleteImageAtIndex_IssuesIndexedDelete(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotAuth = r.Header.Get("X-Emby-Token")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := New(srv.URL, "api-key", "user-1", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := c.DeleteImageAtIndex(context.Background(), "artist-42", "fanart", 3); err != nil {
		t.Fatalf("DeleteImageAtIndex: %v", err)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("method = %s, want DELETE", gotMethod)
	}
	if want := "/Items/artist-42/Images/Backdrop/3"; gotPath != want {
		t.Errorf("path = %s, want %s", gotPath, want)
	}
	if gotAuth != "api-key" {
		t.Errorf("X-Emby-Token = %q, want %q", gotAuth, "api-key")
	}
}

func TestDeleteImageAtIndex_RejectsNegativeIndex(t *testing.T) {
	c := New("http://example.invalid", "k", "u", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := c.DeleteImageAtIndex(context.Background(), "a", "fanart", -1); err == nil {
		t.Fatal("want error for negative index, got nil")
	}
}

// TestDeleteImageAtIndex_UnsupportedType covers the mapImageType("") branch:
// an image type with no Emby mapping must fail fast without issuing an HTTP
// request against the platform.
func TestDeleteImageAtIndex_UnsupportedType(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := New(srv.URL, "api-key", "user-1", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := c.DeleteImageAtIndex(context.Background(), "artist-42", "bogustype", 0); err == nil {
		t.Fatal("want error for unsupported image type, got nil")
	}
	if hit {
		t.Error("unsupported image type must not issue an HTTP request")
	}
}

// TestDeleteImageAtIndex_ServerError covers the resp.StatusCode >= 300 branch:
// a platform-side failure must surface as a non-nil error carrying the
// status/body detail (via ReadBoundedStatusError + wrapAuthIfStatusAuth).
func TestDeleteImageAtIndex_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer srv.Close()

	c := New(srv.URL, "api-key", "user-1", slog.New(slog.NewTextHandler(io.Discard, nil)))
	err := c.DeleteImageAtIndex(context.Background(), "artist-42", "fanart", 0)
	if err == nil {
		t.Fatal("want error for 500 response, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error = %q, want it to contain the status code %q", err.Error(), "500")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error = %q, want it to contain the response body %q", err.Error(), "boom")
	}
}

// TestUploadImageAtIndex_IssuesIndexedUpload covers the happy path: a
// correct indexed POST path, an auth header, and a base64-encoded body.
// UploadImageAtIndex had no dedicated unit test before this refactor (only
// indirect coverage via other paths); this closes that gap symmetrically
// with the pre-existing TestDeleteImageAtIndex_IssuesIndexedDelete.
func TestUploadImageAtIndex_IssuesIndexedUpload(t *testing.T) {
	data := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	var gotMethod, gotPath, gotAuth, gotContentType string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotAuth = r.Header.Get("X-Emby-Token")
		gotContentType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(srv.URL, "api-key", "user-1", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := c.UploadImageAtIndex(context.Background(), "artist-42", "fanart", 3, data, "image/jpeg"); err != nil {
		t.Fatalf("UploadImageAtIndex: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if want := "/Items/artist-42/Images/Backdrop/3"; gotPath != want {
		t.Errorf("path = %s, want %s", gotPath, want)
	}
	if gotAuth != "api-key" {
		t.Errorf("X-Emby-Token = %q, want %q", gotAuth, "api-key")
	}
	if gotContentType != "image/jpeg" {
		t.Errorf("Content-Type = %q, want image/jpeg", gotContentType)
	}
	decoded, err := base64.StdEncoding.DecodeString(string(gotBody))
	if err != nil {
		t.Fatalf("body is not valid base64: %v", err)
	}
	if !bytes.Equal(decoded, data) {
		t.Errorf("decoded body = %v, want %v", decoded, data)
	}
}

func TestUploadImageAtIndex_RejectsNegativeIndex(t *testing.T) {
	c := New("http://example.invalid", "k", "u", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := c.UploadImageAtIndex(context.Background(), "a", "fanart", -1, []byte{1}, "image/jpeg"); err == nil {
		t.Fatal("want error for negative index, got nil")
	}
}

// TestUploadImageAtIndex_UnsupportedType covers the mapImageType("") branch:
// an image type with no Emby mapping must fail fast without issuing an HTTP
// request against the platform.
func TestUploadImageAtIndex_UnsupportedType(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(srv.URL, "api-key", "user-1", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := c.UploadImageAtIndex(context.Background(), "artist-42", "bogustype", 0, []byte{1}, "image/jpeg"); err == nil {
		t.Fatal("want error for unsupported image type, got nil")
	}
	if hit {
		t.Error("unsupported image type must not issue an HTTP request")
	}
}

// TestUploadImageAtIndex_ServerError covers the resp.StatusCode >= 300
// branch: a platform-side failure must surface as a non-nil error carrying
// the status/body detail.
func TestUploadImageAtIndex_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer srv.Close()

	c := New(srv.URL, "api-key", "user-1", slog.New(slog.NewTextHandler(io.Discard, nil)))
	err := c.UploadImageAtIndex(context.Background(), "artist-42", "fanart", 0, []byte{1}, "image/jpeg")
	if err == nil {
		t.Fatal("want error for 500 response, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error = %q, want it to contain the status code %q", err.Error(), "500")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error = %q, want it to contain the response body %q", err.Error(), "boom")
	}
}
