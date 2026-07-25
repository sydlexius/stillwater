package mediabrowser

import "testing"

// fakeLib/fakeOpt are minimal stand-ins for the two platforms' VirtualFolder
// and TypeOption DTOs, which stay separate types per package. Testing the
// generic helper directly means the three-state logic is pinned once, in the
// place it actually lives, rather than only through two platform clients.
type fakeLib struct {
	name       string
	id         string
	internetOn bool
	// collectionType drives the declaredMusic gate. Defaults to "" in
	// fixtures that predate that gate, so every such fixture sets it
	// explicitly to "music" -- a blank value now means "not declared music"
	// and would silently suppress the defaulted case.
	collectionType string
	typeOptions    []fakeOpt
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
		func(l fakeLib) bool { return DeclaredMusicCollectionType(l.collectionType) },
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
			got := collect([]fakeLib{{name: "Music", id: "m1", internetOn: true, collectionType: "music", typeOptions: tc.opts}}, false)
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
		name: "Music", id: "m1", collectionType: "music",
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
	got := collect([]fakeLib{{name: "Music", id: "lib-001", internetOn: true, collectionType: "music"}}, false)
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
		{name: "Configured", id: "m1", internetOn: true, collectionType: "music",
			typeOptions: []fakeOpt{{optType: "MusicArtist", fetchers: []string{}}}},
		{name: "Unconfigured", id: "m2", internetOn: true, collectionType: "music"},
		{name: "Explicit", id: "m3", internetOn: true, collectionType: "music",
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

// TestCollectImageFetcherEntriesRaw_DeclaredMusicGatesOnlyTheDefaultedCase
// pins the asymmetry between the two evidence standards at the helper level.
//
// The defaulted case is triggered by an ABSENCE, so it needs the peer to
// affirmatively declare the library as music -- blank is not a statement.
// The explicit case is triggered by a PRESENT entry with armed fetchers,
// which is itself proof the peer treats the library as holding artists, so
// blank is fine there.
//
// Both rows use the SAME blank CollectionType and differ only in whether the
// MusicArtist entry exists. If a change ever makes them agree, one of the two
// real behaviors has been lost.
func TestCollectImageFetcherEntriesRaw_DeclaredMusicGatesOnlyTheDefaultedCase(t *testing.T) {
	t.Run("blank type, absent entry -> silent", func(t *testing.T) {
		got := collect([]fakeLib{{
			name: "Home Videos", id: "hv1", internetOn: true, collectionType: "",
			typeOptions: []fakeOpt{{optType: "HomeVideos", fetchers: []string{"TheMovieDb"}}},
		}}, false)
		if len(got) != 0 {
			t.Errorf("got %+v, want none. An absent MusicArtist entry on a library the peer "+
				"never declared as music means 'not a music library', not 'defaults are "+
				"running' -- warning here is unactionable noise", got)
		}
	})

	t.Run("blank type, explicit fetchers -> reported", func(t *testing.T) {
		got := collect([]fakeLib{{
			name: "Unlabeled Music", id: "um1", internetOn: true, collectionType: "",
			typeOptions: []fakeOpt{{optType: "MusicArtist", fetchers: []string{"FanArt"}}},
		}}, false)
		if len(got) != 1 {
			t.Fatalf("got %+v, want 1. A MusicArtist entry with armed fetchers is affirmative "+
				"proof the peer treats this library as holding artists, whatever its label "+
				"says -- suppressing it trades a false positive for a silent real conflict", got)
		}
		if got[0].Defaulted {
			t.Error("Defaulted = true, want false for an explicit fetcher list")
		}
	})

	t.Run("declared music, absent entry -> reported", func(t *testing.T) {
		// The control: same absence as the first row, but declared music.
		// Without this row the first row could pass because the defaulted
		// case is broken outright rather than correctly gated.
		got := collect([]fakeLib{{
			name: "Music", id: "m1", internetOn: true, collectionType: "music",
		}}, false)
		if len(got) != 1 || !got[0].Defaulted {
			t.Errorf("got %+v, want 1 defaulted entry. This is the control proving the "+
				"defaulted case still fires when it should", got)
		}
	})
}

// TestCollectImageFetcherEntriesRaw_MusicArtistMatchIsCaseInsensitive guards
// EqualFold at this call site, where a case regression now has an INVERTED
// consequence compared with before the defaulted rule existed.
//
// Previously a case variant meant a missed dirty report -- bad, but
// conservative. Now a lowercase "musicartist" entry with an EMPTY list reads
// as ABSENT, which is interpreted as "defaults are running", producing a
// false warning against a library the operator configured correctly. The
// package documents that the peers are inconsistent about TypeOptions casing
// across versions, so this is a live input rather than a hypothetical.
func TestCollectImageFetcherEntriesRaw_MusicArtistMatchIsCaseInsensitive(t *testing.T) {
	for _, casing := range []string{"musicartist", "MUSICARTIST", "MusicArtist"} {
		t.Run(casing+"/empty list is clean", func(t *testing.T) {
			got := collect([]fakeLib{{
				name: "Music", id: "m1", internetOn: true, collectionType: "music",
				typeOptions: []fakeOpt{{optType: casing, fetchers: []string{}}},
			}}, false)
			if len(got) != 0 {
				t.Errorf("got %+v, want none. A %q entry with an empty list is a configured "+
					"library; a case-sensitive match would read it as ABSENT and warn that "+
					"defaults are running against an install that is already correct",
					got, casing)
			}
		})

		t.Run(casing+"/armed list is dirty", func(t *testing.T) {
			got := collect([]fakeLib{{
				name: "Music", id: "m1", internetOn: true, collectionType: "music",
				typeOptions: []fakeOpt{{optType: casing, fetchers: []string{"FanArt"}}},
			}}, false)
			if len(got) != 1 {
				t.Fatalf("got %+v, want 1 reported entry for casing %q", got, casing)
			}
			if got[0].Defaulted {
				t.Errorf("Defaulted = true for casing %q; the entry exists and is armed, so "+
					"this is an EXPLICIT conflict, not a defaulted one", casing)
			}
		})
	}
}
