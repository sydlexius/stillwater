package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"testing/fstest"
)

// TestIsArtistDetailPath verifies the canonical Artist Detail route match
// used by assetsFor() to gate loading the guided-tour assets (#2228
// fix-round). It must match exactly "/artists/{id}" and reject the list
// page, sibling routes carrying an extra path segment (images,
// artwork-modal), and lookalike prefixes -- mirroring tour.js's own
// getCurrentScreen() artistDetail regex so the two never drift apart.
func TestIsArtistDetailPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		path string
		want bool
	}{
		{"artist detail with a plain id", "/artists/abc123", true},
		{"artist detail with a uuid-shaped id", "/artists/6f2b1e2a-...-9c", true},
		{"artists list page (no id)", "/artists", false},
		{"artists list page with trailing slash", "/artists/", false},
		{"artist images sub-route has an extra segment", "/artists/abc123/images", false},
		{"artist artwork-modal sub-route has an extra segment", "/artists/abc123/artwork-modal", false},
		{"unrelated root path", "/", false},
		{"unrelated prefix lookalike", "/artists-archive/abc123", false},
		{"empty path", "", false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isArtistDetailPath(tc.path); got != tc.want {
				t.Errorf("isArtistDetailPath(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

// TestAssetsForBasePath verifies assetsFor() still loads the guided-tour
// assets (DriverJS/DriverCSS/TourJS) on the promoted tour-eligible routes
// when the server is deployed under a non-empty basePath (e.g.
// "/stillwater"), and that the returned asset paths are prefixed with that
// basePath. assetsFor() strips r.basePath from req.URL.Path before matching
// the tour-eligible route switch (see handlers.go), so a request path that
// still carries the basePath prefix -- as it would from a real mounted
// request -- must match the same routes as an unprefixed deployment; a
// regression here would silently disable the guided tour for every
// sub-path-deployed instance (#2244 Codoki follow-up).
func TestAssetsForBasePath(t *testing.T) {
	t.Parallel()
	const basePath = "/stillwater"

	r := &Router{
		logger:       slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		basePath:     basePath,
		staticAssets: NewStaticAssets(fstest.MapFS{}, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))),
	}

	cases := []struct {
		name string
		path string
	}{
		{"promoted dashboard root", basePath + "/"},
		{"promoted artist detail", basePath + "/artists/42"},
		{"guide route", basePath + "/guide"},
		{"legacy next lane", basePath + "/next/artists"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			a := r.assetsFor(req)

			if a.DriverJS == "" || a.DriverCSS == "" || a.TourJS == "" {
				t.Fatalf("path %q: expected tour assets to be set, got DriverJS=%q DriverCSS=%q TourJS=%q",
					tc.path, a.DriverJS, a.DriverCSS, a.TourJS)
			}
			for name, assetPath := range map[string]string{
				"DriverJS":  a.DriverJS,
				"DriverCSS": a.DriverCSS,
				"TourJS":    a.TourJS,
			} {
				if !strings.HasPrefix(assetPath, basePath) {
					t.Errorf("path %q: %s = %q, want prefix %q", tc.path, name, assetPath, basePath)
				}
			}
			if a.BasePath != basePath {
				t.Errorf("path %q: BasePath = %q, want %q", tc.path, a.BasePath, basePath)
			}
		})
	}

	t.Run("non-tour route under basePath does not load tour assets", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequest(http.MethodGet, basePath+"/settings", nil)
		a := r.assetsFor(req)
		if a.DriverJS != "" || a.DriverCSS != "" || a.TourJS != "" {
			t.Errorf("expected no tour assets on /settings, got DriverJS=%q DriverCSS=%q TourJS=%q", a.DriverJS, a.DriverCSS, a.TourJS)
		}
	})
}

func TestHandleLogout(t *testing.T) {
	t.Parallel()
	t.Run("returns 200 with JSON body and HX-Redirect header", func(t *testing.T) {
		r := &Router{
			logger:   slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
			basePath: "/sw",
		}

		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
		rec := httptest.NewRecorder()

		r.handleLogout(rec, req)

		resp := rec.Result()
		defer resp.Body.Close()

		// Verify HTTP 200 status.
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected status 200, got %d", resp.StatusCode)
		}

		// Verify Content-Type is JSON.
		ct := resp.Header.Get("Content-Type")
		if ct != "application/json" {
			t.Fatalf("expected Content-Type application/json, got %q", ct)
		}

		// Verify the HX-Redirect header includes basePath + "/".
		redirect := resp.Header.Get("HX-Redirect")
		if redirect != "/sw/" {
			t.Fatalf("expected HX-Redirect %q, got %q", "/sw/", redirect)
		}

		// Verify JSON body contains {"status":"ok"}.
		var body map[string]string
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("failed to decode JSON body: %v", err)
		}
		if body["status"] != "ok" {
			t.Fatalf("expected status ok, got %q", body["status"])
		}
	})

	t.Run("clears session cookie", func(t *testing.T) {
		r := &Router{
			logger:   slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
			basePath: "",
		}

		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
		rec := httptest.NewRecorder()

		r.handleLogout(rec, req)

		resp := rec.Result()
		defer resp.Body.Close()

		// Verify session cookie is cleared (MaxAge -1).
		var found bool
		for _, c := range resp.Cookies() {
			if c.Name == "session" {
				found = true
				if c.MaxAge != -1 {
					t.Fatalf("expected session cookie MaxAge -1, got %d", c.MaxAge)
				}
				if c.Value != "" {
					t.Fatalf("expected empty session cookie value, got %q", c.Value)
				}
			}
		}
		if !found {
			t.Fatal("expected session cookie in response")
		}

		// Verify HX-Redirect with empty basePath.
		redirect := resp.Header.Get("HX-Redirect")
		if redirect != "/" {
			t.Fatalf("expected HX-Redirect %q, got %q", "/", redirect)
		}
	})

	t.Run("normalizes basePath trailing slash", func(t *testing.T) {
		r := &Router{
			logger:   slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
			basePath: "/sw/",
		}

		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
		rec := httptest.NewRecorder()

		r.handleLogout(rec, req)

		resp := rec.Result()
		defer resp.Body.Close()

		redirect := resp.Header.Get("HX-Redirect")
		if redirect != "/sw/" {
			t.Fatalf("expected HX-Redirect %q, got %q", "/sw/", redirect)
		}
	})
}

func TestBuildDashboardInitialQuery(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input url.Values
		want  string
	}{
		{
			name:  "empty query returns empty string",
			input: url.Values{},
			want:  "",
		},
		{
			name: "single known key",
			input: url.Values{
				"severity": []string{"warning"},
			},
			want: "?severity=warning",
		},
		{
			name: "multiple known keys are preserved and encoded",
			input: url.Values{
				"search":   []string{"bad artist"},
				"severity": []string{"error"},
				"category": []string{"image"},
			},
			// url.Values.Encode() sorts keys alphabetically so the output is
			// deterministic across runs.
			want: "?category=image&search=bad+artist&severity=error",
		},
		{
			name: "unknown keys are discarded",
			input: url.Values{
				"severity": []string{"warning"},
				"foo":      []string{"bar"},
				"debug":    []string{"true"},
			},
			want: "?severity=warning",
		},
		{
			name: "empty values are skipped",
			input: url.Values{
				"severity": []string{""},
				"category": []string{"image"},
			},
			want: "?category=image",
		},
		{
			name: "all six supported keys round-trip",
			input: url.Values{
				"search":   []string{"a"},
				"severity": []string{"b"},
				"category": []string{"c"},
				"library":  []string{"d"},
				"rule":     []string{"e"},
				"fixable":  []string{"yes"},
			},
			want: "?category=c&fixable=yes&library=d&rule=e&search=a&severity=b",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := buildDashboardInitialQuery(tc.input)
			if got != tc.want {
				t.Errorf("buildDashboardInitialQuery(%v) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}
