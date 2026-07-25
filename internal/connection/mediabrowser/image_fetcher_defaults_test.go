package mediabrowser

import "testing"

// fakeLib/fakeOpt are minimal stand-ins for the two platforms' VirtualFolder
// and TypeOption DTOs, which stay separate types per package. Testing the
// generic helper directly means the three-state logic is pinned once, in the
// place it actually lives, rather than only through two platform clients.
type fakeLib struct {
	name        string
	id          string
	internetOn  bool
	typeOptions []fakeOpt
}

type fakeOpt struct {
	optType  string
	fetchers []string
}

// collect runs the helper with the Emby-shaped includeLibrary (constant
// true) unless gateOnInternet is set, in which case it uses the
// Jellyfin-shaped one. Both platforms' wiring is exercised through the same
// entry point so a divergence bug shows up as a behavior difference rather
// than a compile difference.
func collect(libs []fakeLib, gateOnInternet bool) []FetcherEntry {
	include := func(fakeLib) bool { return true }
	if gateOnInternet {
		include = func(l fakeLib) bool { return l.internetOn }
	}
	return CollectImageFetcherEntriesRaw(libs,
		include,
		func(l fakeLib) string { return l.name },
		func(l fakeLib) string { return l.id },
		func(l fakeLib) []fakeOpt { return l.typeOptions },
		func(o fakeOpt) string { return o.optType },
		func(o fakeOpt) []string { return o.fetchers },
	)
}

// TestCollectImageFetcherEntriesRaw_ThreeStates pins all three input states
// in one table so the distinctions cannot silently collapse into each other.
// Each row states the CONSEQUENCE of getting it wrong, because that is what
// makes a future reader hesitate before "simplifying" the logic.
func TestCollectImageFetcherEntriesRaw_ThreeStates(t *testing.T) {
	for _, tc := range []struct {
		name          string
		opts          []fakeOpt
		wantEntries   int
		wantDefaulted bool
		why           string
	}{
		{
			name:          "absent MusicArtist entry",
			opts:          []fakeOpt{{optType: "MusicAlbum", fetchers: []string{"TheAudioDb"}}},
			wantEntries:   1,
			wantDefaulted: true,
			why: "an absent entry means the peer has no stored config for this type, so its " +
				"DEFAULTS apply and those fetch images. Silence here is the #2719 false clean",
		},
		{
			name:          "no TypeOptions at all",
			opts:          nil,
			wantEntries:   1,
			wantDefaulted: true,
			why:           "a library never configured at all is the same defaults-apply case",
		},
		{
			name:          "present with empty fetcher list",
			opts:          []fakeOpt{{optType: "MusicArtist", fetchers: []string{}}},
			wantEntries:   0,
			wantDefaulted: false,
			why: "a present entry with no fetchers is a deliberately configured library -- " +
				"reporting it would make every correct install raise a permanent false alarm",
		},
		{
			name:          "present with fetchers",
			opts:          []fakeOpt{{optType: "MusicArtist", fetchers: []string{"FanArt"}}},
			wantEntries:   1,
			wantDefaulted: false,
			why:           "explicitly configured fetchers can be switched off by name; the copy differs",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := collect([]fakeLib{{name: "Music", id: "m1", internetOn: true, typeOptions: tc.opts}}, false)
			if len(got) != tc.wantEntries {
				t.Fatalf("entries = %d, want %d. %s", len(got), tc.wantEntries, tc.why)
			}
			if tc.wantEntries == 0 {
				return
			}
			if got[0].Defaulted != tc.wantDefaulted {
				t.Errorf("Defaulted = %v, want %v. %s", got[0].Defaulted, tc.wantDefaulted, tc.why)
			}
		})
	}
}

// TestCollectImageFetcherEntriesRaw_IncludeLibraryGatesTheDefaultedCase is
// the divergence guard at the shared-helper level. The SAME library, with
// the SAME absent MusicArtist entry, must be reported when includeLibrary
// passes and stay silent when it does not.
//
// This is what keeps the Jellyfin EnableInternetProviders=false case a true
// clean. Hoisting the absent-entry check above the includeLibrary gate would
// break exactly this assertion, so it fails loudly rather than turning into
// a false positive nobody notices until an operator reports a phantom
// warning.
func TestCollectImageFetcherEntriesRaw_IncludeLibraryGatesTheDefaultedCase(t *testing.T) {
	libWithoutArtistEntry := fakeLib{
		name: "Music", id: "m1",
		typeOptions: []fakeOpt{{optType: "MusicAlbum", fetchers: []string{"TheAudioDb"}}},
	}

	// Gate open (Jellyfin with internet providers on, or Emby always).
	open := libWithoutArtistEntry
	open.internetOn = true
	if got := collect([]fakeLib{open}, true); len(got) != 1 || !got[0].Defaulted {
		t.Fatalf("gate OPEN: got %+v, want 1 defaulted entry. Precondition for the closed-gate "+
			"assertion below -- if this side does not report, the test proves nothing", got)
	}

	// Gate closed (Jellyfin with internet providers off).
	closed := libWithoutArtistEntry
	closed.internetOn = false
	if got := collect([]fakeLib{closed}, true); len(got) != 0 {
		t.Errorf("gate CLOSED: got %+v, want 0 entries. A library whose fetching is switched "+
			"off wholesale cannot run its defaults, so the absent-entry rule MUST NOT fire -- "+
			"firing here converts the #2719 false negative into a false positive on Jellyfin", got)
	}
}

// TestCollectImageFetcherEntriesRaw_DefaultedEntryCarriesIdentity checks the
// defaulted entry is actually usable by the caller. An entry with no library
// name or ID cannot be rendered into a warning that tells the operator WHICH
// library to go fix.
func TestCollectImageFetcherEntriesRaw_DefaultedEntryCarriesIdentity(t *testing.T) {
	got := collect([]fakeLib{{name: "Music", id: "lib-001", internetOn: true}}, false)
	if len(got) != 1 {
		t.Fatalf("entries = %d, want 1", len(got))
	}
	if got[0].LibraryName != "Music" || got[0].LibraryID != "lib-001" {
		t.Errorf("defaulted entry = {name:%q id:%q}, want {Music lib-001}. A warning that "+
			"cannot name its library leaves the operator with nowhere to go",
			got[0].LibraryName, got[0].LibraryID)
	}
	if len(got[0].FetcherNames) != 0 {
		t.Errorf("FetcherNames = %v, want empty -- the peer never reports which fetchers its "+
			"defaults use, so any names here would be fabricated", got[0].FetcherNames)
	}
}

// TestCollectImageFetcherEntriesRaw_MultipleLibrariesReportIndependently
// guards against a single defaulted library suppressing or contaminating its
// siblings. A peer commonly has several music libraries in different states.
func TestCollectImageFetcherEntriesRaw_MultipleLibrariesReportIndependently(t *testing.T) {
	got := collect([]fakeLib{
		{name: "Configured", id: "m1", internetOn: true,
			typeOptions: []fakeOpt{{optType: "MusicArtist", fetchers: []string{}}}},
		{name: "Unconfigured", id: "m2", internetOn: true},
		{name: "Explicit", id: "m3", internetOn: true,
			typeOptions: []fakeOpt{{optType: "MusicArtist", fetchers: []string{"FanArt"}}}},
	}, false)

	if len(got) != 2 {
		t.Fatalf("entries = %d, want 2 (Unconfigured defaulted + Explicit configured; "+
			"Configured is a true clean): %+v", len(got), got)
	}
	byName := map[string]FetcherEntry{}
	for _, e := range got {
		byName[e.LibraryName] = e
	}
	if _, reported := byName["Configured"]; reported {
		t.Error("the deliberately-configured empty library was reported; it is a true clean")
	}
	if e, ok := byName["Unconfigured"]; !ok || !e.Defaulted {
		t.Errorf("Unconfigured library = %+v, want reported with Defaulted=true", e)
	}
	if e, ok := byName["Explicit"]; !ok || e.Defaulted {
		t.Errorf("Explicit library = %+v, want reported with Defaulted=false", e)
	}
}
