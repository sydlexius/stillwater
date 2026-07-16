package emby

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
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
}
