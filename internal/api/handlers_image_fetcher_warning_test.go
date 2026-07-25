package api

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

// peerServing stands up a fake peer returning the given
// /Library/VirtualFolders payload, and a Connection pointing at it. The
// warning builders construct their own client from the connection's URL, so
// this is all the wiring they need.
func peerServing(t *testing.T, connType, body string) (*Router, *connection.Connection) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	r := &Router{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	conn := &connection.Connection{
		ID: "conn-1", Name: "Test Peer", URL: srv.URL, APIKey: "key",
		Type: connType,
	}
	return r, conn
}

// unconfiguredLibrary is a music library with no MusicArtist TypeOption --
// the #2719 defaulted case. internetProviders is rendered into the payload
// so the Jellyfin gate can be exercised.
func unconfiguredLibrary(internetProviders string) string {
	return `[
		{
			"Name":"Music","CollectionType":"music","ItemId":"lib-001",
			"LibraryOptions":{
				"SaveLocalMetadata":false,"MetadataSavers":[],
				` + internetProviders + `
				"TypeOptions":[
					{"Type":"MusicAlbum","ImageFetchers":["TheAudioDb"],"MetadataFetchers":[]}
				]
			}
		}
	]`
}

// explicitFetcherLibrary has fetchers turned on by name.
func explicitFetcherLibrary(internetProviders string) string {
	return `[
		{
			"Name":"Music","CollectionType":"music","ItemId":"lib-001",
			"LibraryOptions":{
				"SaveLocalMetadata":false,"MetadataSavers":[],
				` + internetProviders + `
				"TypeOptions":[
					{"Type":"MusicArtist","ImageFetchers":["TheAudioDb","FanArt"],"MetadataFetchers":[]}
				]
			}
		}
	]`
}

// TestEmbyWarning_DefaultedCopyDiffersFromExplicitCopy is the operator-facing
// half of #2719. Detecting the defaulted state is only useful if the person
// reading the banner is told something they can ACT on, and the two states
// need different actions:
//
//   - explicit fetchers -> "these named fetchers are on, switch them off"
//   - defaulted -> "nothing is saved, so the defaults are running; open the
//     library settings and save an explicit choice"
//
// Telling a defaulted operator to "disable the listed fetchers" sends them
// looking for a list that does not exist.
func TestEmbyWarning_DefaultedCopyDiffersFromExplicitCopy(t *testing.T) {
	r, conn := peerServing(t, "emby", unconfiguredLibrary(""))
	defaulted := r.checkEmbyImageFetchers(context.Background(), conn)

	if len(defaulted) != 1 {
		t.Fatalf("got %d warnings, want 1 for an unconfigured library", len(defaulted))
	}
	if !defaulted[0].Defaulted {
		t.Fatal("Defaulted = false; the warning must carry the axis through to the API consumer")
	}

	r2, conn2 := peerServing(t, "emby", explicitFetcherLibrary(""))
	explicit := r2.checkEmbyImageFetchers(context.Background(), conn2)
	if len(explicit) != 1 {
		t.Fatalf("got %d warnings, want 1 for an explicit fetcher list", len(explicit))
	}
	if explicit[0].Defaulted {
		t.Fatal("Defaulted = true for an explicitly configured library")
	}

	assertDefaultedCopyIsSubstantive(t, "Emby", defaulted[0].Message, explicit[0].Message)

	// The explicit message must still name the fetchers, or the operator
	// cannot find the setting to switch off.
	if !strings.Contains(explicit[0].Message, "TheAudioDb") {
		t.Errorf("explicit copy does not name the configured fetchers: %q", explicit[0].Message)
	}
}

// assertDefaultedCopyIsSubstantive checks the defaulted warning is genuinely
// its own prose rather than the explicit template run with an empty name
// list.
//
// A bare "the two messages differ" assertion is VACUOUS here, and this was
// measured rather than guessed: disabling the defaulted branch makes the
// defaulted case fall through to the explicit template, which formats an
// empty fetcher list as "image fetchers () are enabled". That string still
// DIFFERS from the explicit one, so an inequality check passes while the
// operator is shown a broken sentence naming no fetchers and prescribing a
// remediation that does not apply. The assertions below pin the substance
// instead: no empty-parens artifact, no fabricated fetcher names, and the
// presence of the guidance that only the defaulted branch produces.
func assertDefaultedCopyIsSubstantive(t *testing.T, platform, defaultedMsg, explicitMsg string) {
	t.Helper()

	if defaultedMsg == explicitMsg {
		t.Errorf("%s: defaulted and explicit warnings render IDENTICAL copy:\n%q", platform, defaultedMsg)
	}
	// The tell of a collapsed branch: the explicit template's parenthesised
	// fetcher list rendered with nothing in it.
	if strings.Contains(defaultedMsg, "()") {
		t.Errorf("%s: defaulted copy contains an empty fetcher list '()', which means it was "+
			"rendered by the EXPLICIT template with no names to fill in rather than by the "+
			"defaulted branch: %q", platform, defaultedMsg)
	}
	// It must not name fetchers the peer never reported.
	for _, invented := range []string{"TheAudioDb", "FanArt"} {
		if strings.Contains(defaultedMsg, invented) {
			t.Errorf("%s: defaulted copy names %q, but the peer never reports which fetchers "+
				"its DEFAULTS use -- that name is fabricated: %q", platform, invented, defaultedMsg)
		}
	}
	// And it must carry the guidance unique to this state: that nothing is
	// saved, so the defaults are what is running.
	for _, required := range []string{"no artist image settings saved", "defaults"} {
		if !strings.Contains(defaultedMsg, required) {
			t.Errorf("%s: defaulted copy is missing %q. The operator has to be told that "+
				"NOTHING is configured and the platform's defaults are therefore active; "+
				"without that they will go looking for a fetcher list to switch off and find "+
				"none: %q", platform, required, defaultedMsg)
		}
	}
}

// TestJellyfinWarning_DefaultedCopyDiffersFromExplicitCopy mirrors the Emby
// case. Internet providers are ON here, which is what makes the defaults
// live and the warning correct.
func TestJellyfinWarning_DefaultedCopyDiffersFromExplicitCopy(t *testing.T) {
	const providersOn = `"EnableInternetProviders":true,`

	r, conn := peerServing(t, "jellyfin", unconfiguredLibrary(providersOn))
	defaulted := r.checkJellyfinImageFetchers(context.Background(), conn)
	if len(defaulted) != 1 {
		t.Fatalf("got %d warnings, want 1 for an unconfigured library with providers on", len(defaulted))
	}
	if !defaulted[0].Defaulted {
		t.Fatal("Defaulted = false; the axis did not reach the warning")
	}

	r2, conn2 := peerServing(t, "jellyfin", explicitFetcherLibrary(providersOn))
	explicit := r2.checkJellyfinImageFetchers(context.Background(), conn2)
	if len(explicit) != 1 {
		t.Fatalf("got %d warnings, want 1", len(explicit))
	}

	assertDefaultedCopyIsSubstantive(t, "Jellyfin", defaulted[0].Message, explicit[0].Message)

	// Jellyfin's defaulted copy offers the internet-providers escape hatch,
	// which is real on this platform and absent on Emby.
	if !strings.Contains(defaulted[0].Message, "internet providers") {
		t.Errorf("Jellyfin defaulted copy should mention the internet providers switch as a "+
			"second way out; it is a real Jellyfin control: %q", defaulted[0].Message)
	}
}

// TestJellyfinWarning_ProvidersOffProducesNoWarning is the divergence guard
// at the API layer. The whole chain -- client, helper, handler -- must stay
// silent, or the operator sees a banner about a fetcher that cannot fire.
func TestJellyfinWarning_ProvidersOffProducesNoWarning(t *testing.T) {
	r, conn := peerServing(t, "jellyfin", unconfiguredLibrary(`"EnableInternetProviders":false,`))
	warnings := r.checkJellyfinImageFetchers(context.Background(), conn)

	if len(warnings) != 0 {
		t.Errorf("got %d warnings, want 0. With internet providers off Jellyfin cannot fetch "+
			"anything for this library, so an absent MusicArtist entry is harmless. A warning "+
			"here is a false positive that sends the operator chasing a setting that is "+
			"already safe: %+v", len(warnings), warnings)
	}
}

// TestEmbyWarning_ConfiguredEmptyLibraryProducesNoWarning pins that the fix
// did not turn every correctly-configured install into a warning factory.
func TestEmbyWarning_ConfiguredEmptyLibraryProducesNoWarning(t *testing.T) {
	r, conn := peerServing(t, "emby", `[
		{
			"Name":"Music","CollectionType":"music","ItemId":"lib-001",
			"LibraryOptions":{
				"SaveLocalMetadata":false,"MetadataSavers":[],
				"TypeOptions":[
					{"Type":"MusicArtist","ImageFetchers":[],"MetadataFetchers":[]}
				]
			}
		}
	]`)
	warnings := r.checkEmbyImageFetchers(context.Background(), conn)

	if len(warnings) != 0 {
		t.Errorf("got %d warnings, want 0 for a deliberately configured library with no "+
			"fetchers. Warning here would penalize operators who did exactly the right "+
			"thing: %+v", len(warnings), warnings)
	}
}
