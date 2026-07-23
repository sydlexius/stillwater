package mediabrowser

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/sydlexius/stillwater/internal/connection/httpclient"
)

// Platform identifiers accepted by ProfileFor. These are the same short
// strings already threaded through the shared library-options helpers
// (GetMusicLibrariesRaw, PostLibraryOptionsRaw, DisableFileWriteBack,
// RestoreLibraryOptions) and passed to httpclient.NewBase as the
// integration tag for log and metric attribution, so a client built here
// logs under exactly the same tag it did when each package constructed its
// own BaseClient.
const (
	PlatformEmby     = "emby"
	PlatformJellyfin = "jellyfin"
)

// ErrUnknownPlatform is returned by ProfileFor and New when the platform
// string does not name a MediaBrowser-family peer we have a profile for.
// Surfacing an error (rather than a zero Profile) is deliberate: a zero
// Profile has a nil ApplyAuth, which would produce a client that silently
// issues unauthenticated requests and fails far from the mistake.
var ErrUnknownPlatform = errors.New("mediabrowser: unknown platform")

// Profile captures every way an Emby peer and a Jellyfin peer differ, as
// data rather than as duplicated code. One entry per platform, resolved
// once at construction and held by value on the Client.
//
// Knobs are added here only when the divergence they describe is actually
// consumed. An unused knob cannot be verified against real behavior and
// drifts from the thing it claims to describe.
type Profile struct {
	// Integration is the tag passed to httpclient.NewBase for structured
	// log and metric attribution. "emby" or "jellyfin".
	Integration string

	// ApplyAuth installs this platform's request credential. Emby uses the
	// X-Emby-Token header; Jellyfin uses Authorization: MediaBrowser
	// Token="...". The two schemes use different header NAMES, not just
	// different values, so this is a function rather than a header-name +
	// format-string pair.
	ApplyAuth func(req *http.Request, apiKey string)
}

// EmbyProfile and JellyfinProfile are the built-in capability profiles for
// each supported platform, exported so a caller holding a compile-time
// platform literal (the emby and jellyfin package constructors) can pass
// the profile value directly to NewWithProfile instead of round-tripping
// through the string-keyed, error-returning ProfileFor. That makes an
// unauthenticated client from a misspelled platform string impossible at
// compile time for those two callers, rather than merely unreachable at
// runtime.
var (
	EmbyProfile = Profile{
		Integration: PlatformEmby,
		ApplyAuth: func(req *http.Request, apiKey string) {
			req.Header.Set("X-Emby-Token", apiKey)
		},
	}
	JellyfinProfile = Profile{
		Integration: PlatformJellyfin,
		ApplyAuth: func(req *http.Request, apiKey string) {
			req.Header.Set("Authorization", fmt.Sprintf(`MediaBrowser Token="%s"`, apiKey))
		},
	}
)

// profiles holds the built-in capability profile for each supported
// platform, keyed by the platform string. Kept unexported so the set of
// platforms cannot be mutated at runtime; callers resolve through
// ProfileFor.
var profiles = map[string]Profile{
	PlatformEmby:     EmbyProfile,
	PlatformJellyfin: JellyfinProfile,
}

// ProfileFor returns the built-in capability profile for a
// MediaBrowser-family platform. An unknown platform returns
// ErrUnknownPlatform rather than a zero profile, so a misspelled platform
// cannot silently produce an unauthenticated client.
func ProfileFor(platform string) (Profile, error) {
	p, ok := profiles[platform]
	if !ok {
		return Profile{}, fmt.Errorf("%w: %q", ErrUnknownPlatform, platform)
	}
	return p, nil
}

// Client is the shared MediaBrowser-family client. It owns three things
// and nothing else: the HTTP transport (embedded httpclient.BaseClient),
// the per-platform auth scheme (installed from the Profile), and the
// peer identity plus capability profile.
//
// emby.Client and jellyfin.Client embed *Client, so every transport
// helper (Get, GetRaw, Post, PostJSON, PutJSON) and every BaseClient field
// (BaseURL, APIKey, Logger, HTTPClient, AuthFunc) remains reachable on
// those types by promotion, and both still satisfy Transport.
type Client struct {
	httpclient.BaseClient

	// UserID is the peer user whose scope some item endpoints require
	// (/Users/{UserID}/Items/...). Exported because the per-platform
	// packages read it from their own methods; an unexported field on
	// this type would not be reachable from package emby or jellyfin.
	UserID string

	// Profile is the resolved per-platform capability set.
	Profile Profile
}

// New builds a shared client for the named platform. It returns an error
// on an unknown platform; the two per-package constructors are the only
// callers and both pass a compile-time literal, so the error path is
// unreachable in practice, but returning a client with no auth scheme
// installed would be exactly the kind of silent failure that surfaces as
// an unexplained 401 much later.
func New(platform, baseURL, apiKey, userID string, httpClient *http.Client, logger *slog.Logger) (*Client, error) {
	profile, err := ProfileFor(platform)
	if err != nil {
		return nil, err
	}
	c := &Client{
		BaseClient: httpclient.NewBase(baseURL, apiKey, httpClient, logger, profile.Integration),
		UserID:     userID,
		Profile:    profile,
	}
	// BaseClient calls AuthFunc on every request it builds. Binding the
	// profile's ApplyAuth here is what replaces the per-package setAuth
	// methods; c.APIKey is read at request time, matching the previous
	// behavior where setAuth read the field off the live client.
	c.AuthFunc = func(req *http.Request) {
		profile.ApplyAuth(req, c.APIKey)
	}
	return c, nil
}

// NewWithProfile builds a shared client from an already-resolved Profile,
// with no error return. It exists for callers that hold a Profile value at
// compile time (EmbyProfile, JellyfinProfile) rather than a platform
// string: those callers cannot produce ErrUnknownPlatform, so unlike New,
// there is no error branch for them to carry (and no way to leave it
// unreachable-but-present). ProfileFor and New remain the right API for a
// caller keyed off a runtime platform string, such as
// library_options.go's plain `platform string` convention.
func NewWithProfile(profile Profile, baseURL, apiKey, userID string, httpClient *http.Client, logger *slog.Logger) *Client {
	c := &Client{
		BaseClient: httpclient.NewBase(baseURL, apiKey, httpClient, logger, profile.Integration),
		UserID:     userID,
		Profile:    profile,
	}
	c.AuthFunc = func(req *http.Request) {
		profile.ApplyAuth(req, c.APIKey)
	}
	return c
}

// ClassifyAuthError wraps a 401/403 httpclient.StatusError with the
// caller's platform sentinel and returns every other error unchanged.
//
// The sentinel is a parameter rather than a package-level value because
// internal/publish matches emby.ErrAuthRequired and
// jellyfin.ErrAuthRequired separately via errors.Is; collapsing them into
// one shared sentinel would change which connection a re-auth prompt is
// attributed to. The original error stays %w-wrapped so the existing
// substring contract on err.Error() ("status 401" / "HTTP 401", matched by
// publish.classifyPushErr) continues to hold.
func ClassifyAuthError(err error, sentinel error) error {
	if err == nil {
		return nil
	}
	var se *httpclient.StatusError
	if errors.As(err, &se) && se.IsAuth() {
		return fmt.Errorf("%w: %w", sentinel, err)
	}
	return err
}
