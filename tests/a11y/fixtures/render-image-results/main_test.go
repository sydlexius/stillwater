package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// failWriter rejects every write, standing in for a closed stdout pipe so the
// render-error branch of run is reachable.
type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("write refused") }

func TestRunUsageErrors(t *testing.T) {
	cases := map[string][]string{
		"no args":       nil,
		"one arg":       {"artist"},
		"too many args": {"a", "b", "images", "extra"},
		"unknown kind":  {"artist", "http://x/y.png", "banner"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if code := run(args, &out, &errOut); code != 2 {
				t.Fatalf("exit code = %d, want 2", code)
			}
			if out.Len() != 0 {
				t.Errorf("usage error wrote to stdout: %q", out.String())
			}
			if !strings.Contains(errOut.String(), "usage:") && !strings.Contains(errOut.String(), "unknown fragment") {
				t.Errorf("stderr missing guidance: %q", errOut.String())
			}
		})
	}
}

// TestRunRendersFragments pins what the a11y spec depends on: the card count,
// the meta shapes, and the provider-status banner in each fragment.
func TestRunRendersFragments(t *testing.T) {
	const url = "http://127.0.0.1/img.png"
	cases := []struct {
		name    string
		args    []string
		wantAll []string
		cards   int
	}{
		{"images default", []string{"artist-1", url}, []string{"42 likes", "data-sw-providers-skipped", "data-sw-provider-errored"}, 5},
		{"images explicit", []string{"artist-1", url, "images"}, []string{"42 likes", "data-sw-providers-skipped", "data-sw-provider-errored"}, 5},
		{"fanart", []string{"artist-1", url, "fanart"}, []string{"fanart-search-results", "data-sw-providers-skipped", "data-sw-provider-errored"}, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if code := run(tc.args, &out, &errOut); code != 0 {
				t.Fatalf("exit code = %d, stderr %q", code, errOut.String())
			}
			html := out.String()
			// Every case renders the provider-status banner (the program always
			// passes one skipped and one errored provider), so BOTH marker lines
			// must be present and carry the AA-safe amber (amber-700 paints under
			// the 5.0 floor in the light theme). A missing marker or opening tag
			// FAILS: skipping the class check would let the banner regress to a
			// failing color while this test stayed green.
			for _, marker := range []string{"data-sw-providers-skipped", "data-sw-provider-errored"} {
				i := strings.Index(html, marker)
				if i < 0 {
					t.Errorf("banner marker %q missing from the fragment", marker)
					continue
				}
				start := strings.LastIndex(html[:i], "<p")
				if start < 0 {
					t.Errorf("banner marker %q has no opening <p tag before it", marker)
					continue
				}
				if tag := html[start:i]; !strings.Contains(tag, "text-amber-800") {
					t.Errorf("banner %s lacks text-amber-800: %q", marker, tag)
				}
			}
			for _, want := range tc.wantAll {
				if !strings.Contains(html, want) {
					t.Errorf("fragment missing %q", want)
				}
			}
			if got := strings.Count(html, "data-img-url="); got != tc.cards {
				t.Errorf("card count = %d, want %d", got, tc.cards)
			}
		})
	}
}

func TestRunRenderErrors(t *testing.T) {
	for _, kind := range []string{"images", "fanart"} {
		t.Run(kind, func(t *testing.T) {
			var errOut bytes.Buffer
			if code := run([]string{"artist-1", "http://127.0.0.1/img.png", kind}, failWriter{}, &errOut); code != 1 {
				t.Fatalf("exit code = %d, want 1", code)
			}
			if !strings.Contains(errOut.String(), "rendering:") {
				t.Errorf("stderr = %q, want a rendering error", errOut.String())
			}
		})
	}
}
