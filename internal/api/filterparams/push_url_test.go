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
