package mediabrowser

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sydlexius/stillwater/internal/connection/httpclient"
)

// TestNewAppliesPlatformAuthScheme is the load-bearing test for the shared
// client seam: it pins the exact wire credential each platform sends.
//
// Both the presence of the expected header AND the absence of the other
// platform's header are asserted. A profile that set both headers would
// authenticate successfully against either peer and so would pass a
// presence-only assertion, while silently leaking a credential in a header
// the peer never asked for.
func TestNewAppliesPlatformAuthScheme(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		platform     string
		apiKey       string
		wantHeader   string
		wantValue    string
		absentHeader string
	}{
		{
			name:         "emby uses the X-Emby-Token header",
			platform:     PlatformEmby,
			apiKey:       "emby-secret",
			wantHeader:   "X-Emby-Token",
			wantValue:    "emby-secret",
			absentHeader: "Authorization",
		},
		{
			name:         "jellyfin uses the MediaBrowser Authorization scheme",
			platform:     PlatformJellyfin,
			apiKey:       "jf-secret",
			wantHeader:   "Authorization",
			wantValue:    `MediaBrowser Token="jf-secret"`,
			absentHeader: "X-Emby-Token",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var got http.Header
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.Header.Clone()
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{}`))
			}))
			defer srv.Close()

			c, err := New(tt.platform, srv.URL, tt.apiKey, "user-1", srv.Client(), testLogger())
			if err != nil {
				t.Fatalf("New(%q) returned error: %v", tt.platform, err)
			}

			var out map[string]any
			if err := c.Get(context.Background(), "/System/Info", &out); err != nil {
				t.Fatalf("Get returned error: %v", err)
			}

			if v := got.Get(tt.wantHeader); v != tt.wantValue {
				t.Errorf("%s header = %q, want %q", tt.wantHeader, v, tt.wantValue)
			}
			if _, ok := got[http.CanonicalHeaderKey(tt.absentHeader)]; ok {
				t.Errorf("%s header was sent but must not be present for %s", tt.absentHeader, tt.platform)
			}
		})
	}
}

// TestNewSetsIdentityAndProfile pins the fields the per-platform packages
// read off the shared client: the integration tag handed to
// httpclient.NewBase (which feeds log/metric attribution) and the exported
// UserID the user-scoped item endpoints interpolate.
func TestNewSetsIdentityAndProfile(t *testing.T) {
	t.Parallel()

	c, err := New(PlatformEmby, "http://example.invalid", "key", "user-42", http.DefaultClient, testLogger())
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	if c.UserID != "user-42" {
		t.Errorf("UserID = %q, want %q", c.UserID, "user-42")
	}
	if c.Profile.Integration != PlatformEmby {
		t.Errorf("Profile.Integration = %q, want %q", c.Profile.Integration, PlatformEmby)
	}
	if c.APIKey != "key" {
		t.Errorf("APIKey = %q, want %q", c.APIKey, "key")
	}
}

// TestProfileForUnknownPlatform verifies the resolver fails loudly rather
// than returning a zero Profile whose nil ApplyAuth would produce a client
// issuing unauthenticated requests.
func TestProfileForUnknownPlatform(t *testing.T) {
	t.Parallel()

	p, err := ProfileFor("plex")
	if err == nil {
		t.Fatal("ProfileFor(\"plex\") returned nil error; want ErrUnknownPlatform")
	}
	if !errors.Is(err, ErrUnknownPlatform) {
		t.Errorf("error = %v, want errors.Is ErrUnknownPlatform", err)
	}
	if p.ApplyAuth != nil {
		t.Error("ProfileFor returned a profile with a non-nil ApplyAuth for an unknown platform")
	}

	if _, err := New("plex", "http://example.invalid", "key", "u", http.DefaultClient, testLogger()); !errors.Is(err, ErrUnknownPlatform) {
		t.Errorf("New(\"plex\") error = %v, want errors.Is ErrUnknownPlatform", err)
	}
}

// TestProfileForKnownPlatforms guards against a profile entry losing its
// auth function, which would make every request from that platform
// unauthenticated.
func TestProfileForKnownPlatforms(t *testing.T) {
	t.Parallel()

	for _, platform := range []string{PlatformEmby, PlatformJellyfin} {
		p, err := ProfileFor(platform)
		if err != nil {
			t.Fatalf("ProfileFor(%q) returned error: %v", platform, err)
		}
		if p.ApplyAuth == nil {
			t.Errorf("ProfileFor(%q) returned a nil ApplyAuth", platform)
		}
		if p.Integration != platform {
			t.Errorf("ProfileFor(%q).Integration = %q, want %q", platform, p.Integration, platform)
		}
	}
}

// TestClassifyAuthError covers the shared 401/403 classifier. Two distinct
// sentinels are exercised so a regression that collapsed them into one
// shared value (which would misattribute a re-auth prompt to the wrong
// connection) fails here.
func TestClassifyAuthError(t *testing.T) {
	t.Parallel()

	embySentinel := errors.New("emby: authentication required")
	jellyfinSentinel := errors.New("jellyfin: authentication required")
	plain := errors.New("connection refused")

	tests := []struct {
		name     string
		err      error
		sentinel error
		wantWrap bool
	}{
		{name: "nil passes through", err: nil, sentinel: embySentinel, wantWrap: false},
		{name: "401 wraps", err: &httpclient.StatusError{StatusCode: 401, Body: "no"}, sentinel: embySentinel, wantWrap: true},
		{name: "403 wraps", err: &httpclient.StatusError{StatusCode: 403, Body: "no"}, sentinel: embySentinel, wantWrap: true},
		{name: "500 does not wrap", err: &httpclient.StatusError{StatusCode: 500, Body: "boom"}, sentinel: embySentinel, wantWrap: false},
		{name: "404 does not wrap", err: &httpclient.StatusError{StatusCode: 404, Body: "gone"}, sentinel: embySentinel, wantWrap: false},
		{name: "plain error does not wrap", err: plain, sentinel: embySentinel, wantWrap: false},
		{name: "401 wraps the jellyfin sentinel", err: &httpclient.StatusError{StatusCode: 401, Body: "no"}, sentinel: jellyfinSentinel, wantWrap: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := ClassifyAuthError(tt.err, tt.sentinel)

			if tt.err == nil {
				if got != nil {
					t.Fatalf("ClassifyAuthError(nil) = %v, want nil", got)
				}
				return
			}

			if errors.Is(got, tt.sentinel) != tt.wantWrap {
				t.Errorf("errors.Is(result, sentinel) = %v, want %v (result: %v)", !tt.wantWrap, tt.wantWrap, got)
			}
			// The original error must remain reachable either way, so
			// publish.classifyPushErr's substring contract on
			// err.Error() keeps matching.
			if !errors.Is(got, tt.err) {
				t.Errorf("original error no longer reachable via errors.Is: %v", got)
			}

			// The other platform's sentinel must never match.
			other := jellyfinSentinel
			if errors.Is(tt.sentinel, jellyfinSentinel) {
				other = embySentinel
			}
			if errors.Is(got, other) {
				t.Errorf("result matched the other platform's sentinel: %v", got)
			}
		})
	}
}
