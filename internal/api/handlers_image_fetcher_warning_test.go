package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/encryption"
	"github.com/sydlexius/stillwater/internal/library"
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

	// THE ACTIONABLE HALF. Everything above pins what the message must NOT
	// say plus two topic keywords, and that combination is satisfiable by
	// keyword soup: a message reduced to "<platform> has no artist image
	// settings saved for library 'X'. defaults" passes every assertion above
	// while telling the operator nothing to DO. Diagnosis without remediation
	// is exactly what the defaulted state cannot afford, because unlike the
	// explicit case there is no named fetcher to go find. Pin the imperative
	// itself.
	if !strings.Contains(defaultedMsg, "settings in "+platform) {
		t.Errorf("%s: defaulted copy does not tell the operator WHERE to go (expected it to "+
			"direct them to the library's settings in %s). A message that names the problem "+
			"and omits the remedy leaves them stuck: %q", platform, platform, defaultedMsg)
	}
	if !strings.Contains(defaultedMsg, "turn the artist image fetchers off") {
		t.Errorf("%s: defaulted copy does not tell the operator WHAT to do. The instruction "+
			"to turn the artist image fetchers off is the substance of this warning: %q",
			platform, defaultedMsg)
	}
	// Saving is the non-obvious step and the reason the warning exists at
	// all: an unsaved library keeps running the platform's defaults even if
	// the operator eyeballs the toggles and thinks they are off.
	if !strings.Contains(defaultedMsg, "save") && !strings.Contains(defaultedMsg, "stored") {
		t.Errorf("%s: defaulted copy never tells the operator to SAVE an explicit choice. "+
			"That is the whole point -- looking at the settings page is not enough, the "+
			"value has to be stored or the defaults keep applying: %q", platform, defaultedMsg)
	}

	// The copy must be about THIS platform. Rendering Jellyfin's message from
	// the Emby path would warn about EXIF stripping and offer an
	// internet-providers switch that does not exist on Emby, sending the
	// operator to look for a control their server does not have.
	if !strings.Contains(defaultedMsg, platform) {
		t.Errorf("%s: defaulted copy never names %s. Cross-wired platform copy sends the "+
			"operator hunting for controls their server does not have: %q",
			platform, platform, defaultedMsg)
	}
	if !strings.Contains(explicitMsg, platform) {
		t.Errorf("%s: explicit copy never names %s: %q", platform, platform, explicitMsg)
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
	if explicit[0].Defaulted {
		t.Fatal("Defaulted = true for an explicitly configured library")
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

// peerFailing stands up a fake peer that refuses every request with 500, and
// a Connection pointing at it. This is what drives the two "could not check"
// error paths: the client's CheckImageFetchersEnabled returns an error, and
// the handler builds a warning with no fetcher names at all.
func peerFailing(t *testing.T, connType string) (*Router, *connection.Connection) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "peer is down", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	r := &Router{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	conn := &connection.Connection{
		ID: "conn-1", Name: "Test Peer", URL: srv.URL, APIKey: "key",
		Type: connType,
	}
	return r, conn
}

// TestWarning_ErrorPathsSerializeFetcherNamesAsArray covers the three paths
// that build a warning WITHOUT ever having a fetcher list to put in it: the
// connection lookup failing, and the Emby / Jellyfin checks failing against
// an unreachable peer. They reach exactly the same JSON contract as the
// success paths -- "always an array, never null" -- and they are the paths a
// strict generated client is most likely to meet first, because they fire
// whenever a peer is down.
//
// Each case is driven through the real code path rather than by constructing
// the struct, and asserts on the marshaled bytes. The message precondition
// is what keeps it honest: without it, a case that quietly took the SUCCESS
// path would still pass, since the success paths normalize too.
func TestWarning_ErrorPathsSerializeFetcherNamesAsArray(t *testing.T) {
	for _, tc := range []struct {
		name string
		// wantMessage identifies the error path. If the code took some
		// other branch, this fails and the JSON assertion below is not
		// credited to a path it never exercised.
		wantMessage string
		produce     func(*testing.T) []connection.ImageFetcherWarning
	}{
		{
			name:        "connection lookup fails",
			wantMessage: "Could not load connection settings",
			produce: func(t *testing.T) []connection.ImageFetcherWarning {
				t.Helper()
				db := newTestDB(t)
				enc, _, err := encryption.NewEncryptor("")
				if err != nil {
					t.Fatalf("encryptor: %v", err)
				}
				r := &Router{
					logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
					connectionService: connection.NewService(db, enc),
				}
				// No connection row with this ID exists, so GetByID errors.
				return r.collectImageFetcherWarnings(context.Background(),
					[]library.Library{{ID: "lib-1", ConnectionID: "does-not-exist"}})
			},
		},
		{
			name:        "emby check fails",
			wantMessage: "Could not check Emby image fetcher settings",
			produce: func(t *testing.T) []connection.ImageFetcherWarning {
				t.Helper()
				r, conn := peerFailing(t, "emby")
				return r.checkEmbyImageFetchers(context.Background(), conn)
			},
		},
		{
			name:        "jellyfin check fails",
			wantMessage: "Could not check Jellyfin image fetcher settings",
			produce: func(t *testing.T) []connection.ImageFetcherWarning {
				t.Helper()
				r, conn := peerFailing(t, "jellyfin")
				return r.checkJellyfinImageFetchers(context.Background(), conn)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			warnings := tc.produce(t)
			if len(warnings) != 1 {
				t.Fatalf("got %d warnings, want 1", len(warnings))
			}
			// Precondition: confirm this really is the error path. A test
			// that silently took the success path would assert nothing.
			if !strings.Contains(warnings[0].Message, tc.wantMessage) {
				t.Fatalf("precondition: expected the error path (message containing %q), got %q",
					tc.wantMessage, warnings[0].Message)
			}

			buf, err := json.Marshal(warnings[0])
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if strings.Contains(string(buf), `"fetcher_names":null`) {
				t.Errorf("fetcher_names serialized as null on an error path: %s\nThe OpenAPI "+
					"schema declares it type: array without nullable: true, and the description "+
					"promises it is always an array. An unreachable peer is the most likely way "+
					"a client meets this response", buf)
			}
			if !strings.Contains(string(buf), `"fetcher_names":[]`) {
				t.Errorf("fetcher_names is not an empty array: %s", buf)
			}
		})
	}
}

// TestWarning_FetcherNamesSerializeAsArrayNotNull pins the wire shape.
//
// A defaulted warning has no fetcher names, and a nil Go slice marshals to
// JSON null. The OpenAPI schema declares fetcher_names as type: array with no
// nullable: true, so a strict client generated from that spec rejects null
// where it expects []. The two must agree, and an empty array is the honest
// value: the list is genuinely a list in every state, only its length varies.
func TestWarning_FetcherNamesSerializeAsArrayNotNull(t *testing.T) {
	for _, tc := range []struct {
		name     string
		connType string
		body     string
		check    func(*Router, *connection.Connection) []connection.ImageFetcherWarning
	}{
		{
			name: "emby defaulted", connType: "emby", body: unconfiguredLibrary(""),
			check: func(r *Router, c *connection.Connection) []connection.ImageFetcherWarning {
				return r.checkEmbyImageFetchers(context.Background(), c)
			},
		},
		{
			name: "jellyfin defaulted", connType: "jellyfin",
			body: unconfiguredLibrary(`"EnableInternetProviders":true,`),
			check: func(r *Router, c *connection.Connection) []connection.ImageFetcherWarning {
				return r.checkJellyfinImageFetchers(context.Background(), c)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, conn := peerServing(t, tc.connType, tc.body)
			warnings := tc.check(r, conn)
			if len(warnings) != 1 {
				t.Fatalf("got %d warnings, want 1", len(warnings))
			}
			// Precondition: this must actually be the defaulted case, or the
			// test passes against an explicit warning that has names anyway.
			if !warnings[0].Defaulted {
				t.Fatal("precondition: expected the DEFAULTED case, which is the one with " +
					"no fetcher names and therefore the one that can serialize as null")
			}

			buf, err := json.Marshal(warnings[0])
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if strings.Contains(string(buf), `"fetcher_names":null`) {
				t.Errorf("fetcher_names serialized as null: %s\nThe OpenAPI schema declares "+
					"it type: array without nullable: true, so a strict generated client "+
					"rejects this response", buf)
			}
			if !strings.Contains(string(buf), `"fetcher_names":[]`) {
				t.Errorf("fetcher_names is not an empty array: %s", buf)
			}
		})
	}
}
