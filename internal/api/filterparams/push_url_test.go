package filterparams

import (
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestWriteHXPushURL(t *testing.T) {
	cases := []struct {
		name     string
		basePath string
		vals     url.Values
		want     string
	}{
		{
			name:     "empty values root base",
			basePath: "",
			vals:     url.Values{},
			want:     "/",
		},
		{
			name:     "empty values rooted base",
			basePath: "/",
			vals:     url.Values{},
			want:     "/",
		},
		{
			name:     "empty values with subpath, no trailing slash",
			basePath: "/stillwater",
			vals:     url.Values{},
			want:     "/stillwater/",
		},
		{
			name:     "single key",
			basePath: "",
			vals:     url.Values{"severity": []string{"error"}},
			want:     "/?severity=error",
		},
		{
			name:     "multi key deterministic order",
			basePath: "",
			vals:     url.Values{"severity": []string{"error"}, "library_id": []string{"abc"}},
			// url.Values.Encode sorts keys alphabetically.
			want: "/?library_id=abc&severity=error",
		},
		{
			name:     "with basePath suffix",
			basePath: "/sub",
			vals:     url.Values{"category": []string{"image"}},
			want:     "/sub/?category=image",
		},
		{
			name:     "basePath already trailing-slash",
			basePath: "/sub/",
			vals:     url.Values{"fixable": []string{"yes"}},
			want:     "/sub/?fixable=yes",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			WriteHXPushURL(w, tc.basePath, tc.vals)
			got := w.Header().Get("HX-Push-Url")
			if got != tc.want {
				t.Errorf("HX-Push-Url = %q; want %q", got, tc.want)
			}
		})
	}
}

// TestWriteHXPushURLForPath covers the channel-aware variant that pushes a
// verbatim, fully-qualified screen path (e.g. the next/ dashboard at
// "$basePath/next/dashboard") instead of the application root. Unlike
// WriteHXPushURL it never forces a trailing slash: the path is emitted as-is,
// with a "?"-joined query only when there are values.
func TestWriteHXPushURLForPath(t *testing.T) {
	cases := []struct {
		name string
		path string
		vals url.Values
		want string
	}{
		{
			name: "empty values emits the path verbatim",
			path: "/next/dashboard",
			vals: url.Values{},
			want: "/next/dashboard",
		},
		{
			name: "nil values also emits the path verbatim",
			path: "/next/dashboard",
			vals: nil,
			want: "/next/dashboard",
		},
		{
			name: "single value appends the query",
			path: "/next/dashboard",
			vals: url.Values{"severity": []string{"error"}},
			want: "/next/dashboard?severity=error",
		},
		{
			name: "multi value sorts keys alphabetically",
			path: "/next/dashboard",
			vals: url.Values{"severity": []string{"warning"}, "fixable": []string{"yes"}},
			// url.Values.Encode sorts keys alphabetically.
			want: "/next/dashboard?fixable=yes&severity=warning",
		},
		{
			name: "subpath deployment keeps the basePath prefix verbatim",
			path: "/stillwater/next/dashboard",
			vals: url.Values{"category": []string{"image"}},
			want: "/stillwater/next/dashboard?category=image",
		},
		{
			// Defensive: an empty path normalizes to "/" before the header is
			// written (matching WriteHXPushURL), so a caller that passes "" never
			// emits a relative/empty HX-Push-Url that the browser would resolve
			// against the internal fetch endpoint.
			name: "empty path normalizes to root, no values",
			path: "",
			vals: url.Values{},
			want: "/",
		},
		{
			name: "empty path normalizes to root, with values",
			path: "",
			vals: url.Values{"severity": []string{"error"}},
			want: "/?severity=error",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			WriteHXPushURLForPath(w, tc.path, tc.vals)
			got := w.Header().Get("HX-Push-Url")
			if got != tc.want {
				t.Errorf("HX-Push-Url = %q; want %q", got, tc.want)
			}
		})
	}
}
