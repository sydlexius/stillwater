package jellyfin

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/connection"
)

// TestBuildProviderIDUpdates covers the #1084 mapping from ArtistPushData
// external IDs into Jellyfin's canonical ProviderIds dictionary keys. Empty
// IDs must be omitted so a missing-in-Stillwater value never overwrites an
// existing-on-Jellyfin value via "".
func TestBuildProviderIDUpdates(t *testing.T) {
	t.Run("all four IDs map to canonical Jellyfin keys", func(t *testing.T) {
		got := buildProviderIDUpdates(connection.ArtistPushData{
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
		got := buildProviderIDUpdates(connection.ArtistPushData{
			AudioDBID: "adb-only",
		})
		if len(got) != 1 || got["TheAudioDb"] != "adb-only" {
			t.Errorf("expected only TheAudioDb=adb-only, got %+v", got)
		}
	})

	t.Run("empty input yields empty map", func(t *testing.T) {
		got := buildProviderIDUpdates(connection.ArtistPushData{})
		if len(got) != 0 {
			t.Errorf("expected empty map, got %+v", got)
		}
	})
}

// TestBuildPeopleEntries covers the #1085 mapping from Stillwater band
// members into Jellyfin's People array shape. Each entry must carry
// Type=Person; Role is included only when non-empty; entries with no Name
// are dropped at the boundary.
func TestBuildPeopleEntries(t *testing.T) {
	t.Run("Name and Role propagate, Type is always Person", func(t *testing.T) {
		got := buildPeopleEntries([]connection.ArtistPersonRef{
			{Name: "Ann", Role: "Vocals (lead); Guitar"},
			{Name: "Bob", Role: "Drums"},
		})
		if len(got) != 2 {
			t.Fatalf("expected 2 entries, got %d: %+v", len(got), got)
		}
		if got[0]["Name"] != "Ann" || got[0]["Type"] != "Person" || got[0]["Role"] != "Vocals (lead); Guitar" {
			t.Errorf("entry 0 mismatch: %+v", got[0])
		}
		if got[1]["Name"] != "Bob" || got[1]["Type"] != "Person" || got[1]["Role"] != "Drums" {
			t.Errorf("entry 1 mismatch: %+v", got[1])
		}
	})

	t.Run("empty Role is omitted from entry", func(t *testing.T) {
		got := buildPeopleEntries([]connection.ArtistPersonRef{
			{Name: "Solo", Role: ""},
		})
		if len(got) != 1 {
			t.Fatalf("expected 1 entry, got %d", len(got))
		}
		if _, present := got[0]["Role"]; present {
			t.Errorf("Role must be omitted when empty, got %+v", got[0])
		}
		if got[0]["Type"] != "Person" {
			t.Errorf("Type must be Person even with empty Role: %+v", got[0])
		}
	})

	t.Run("entries with empty Name are dropped", func(t *testing.T) {
		got := buildPeopleEntries([]connection.ArtistPersonRef{
			{Name: "Real", Role: "Guitar"},
			{Name: "", Role: "Drums"},
		})
		if len(got) != 1 {
			t.Fatalf("expected 1 entry, got %d: %+v", len(got), got)
		}
		if got[0]["Name"] != "Real" {
			t.Errorf("wrong surviving entry: %+v", got[0])
		}
	})

	t.Run("nil or empty input yields empty slice (non-nil)", func(t *testing.T) {
		if got := buildPeopleEntries(nil); got == nil || len(got) != 0 {
			t.Errorf("nil input should yield non-nil empty slice, got %v", got)
		}
		if got := buildPeopleEntries([]connection.ArtistPersonRef{}); got == nil || len(got) != 0 {
			t.Errorf("empty input should yield non-nil empty slice, got %v", got)
		}
	})
}

func TestDeleteImageAtIndex_IssuesIndexedDelete(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotAuth = r.Header.Get("Authorization")
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
	if want := `MediaBrowser Token="api-key"`; gotAuth != want {
		t.Errorf("Authorization = %q, want %q", gotAuth, want)
	}
}

func TestDeleteImageAtIndex_RejectsNegativeIndex(t *testing.T) {
	c := New("http://example.invalid", "k", "u", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := c.DeleteImageAtIndex(context.Background(), "a", "fanart", -1); err == nil {
		t.Fatal("want error for negative index, got nil")
	}
}

// TestDeleteImageAtIndex_UnsupportedType covers the mapImageType("") branch:
// an image type with no Jellyfin mapping must fail fast without issuing an
// HTTP request against the platform.
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
