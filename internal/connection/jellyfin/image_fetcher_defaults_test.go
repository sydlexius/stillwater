package jellyfin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// virtualFoldersServer serves a canned /Library/VirtualFolders payload.
func virtualFoldersServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestCheckImageFetchersEnabled_AbsentMusicArtistWithProvidersOnIsDefaultedOn
// is the #2719 false-clean guard for Jellyfin. Note EnableInternetProviders
// is TRUE here: that is what makes Jellyfin's defaults live, and therefore
// what makes an absent MusicArtist entry dangerous.
func TestCheckImageFetchersEnabled_AbsentMusicArtistWithProvidersOnIsDefaultedOn(t *testing.T) {
	srv := virtualFoldersServer(t, `[
		{
			"Name":"Music","CollectionType":"music","ItemId":"lib-001",
			"LibraryOptions":{
				"SaveLocalMetadata":false,"MetadataSavers":[],
				"EnableInternetProviders":true,
				"TypeOptions":[
					{"Type":"MusicAlbum","ImageFetchers":["TheAudioDb"],"MetadataFetchers":[]}
				]
			}
		}
	]`)

	c := NewWithHTTPClient(srv.URL, "key", "", srv.Client(), testLogger())
	statuses, err := c.CheckImageFetchersEnabled(context.Background())
	if err != nil {
		t.Fatalf("CheckImageFetchersEnabled: %v", err)
	}

	if len(statuses) != 1 {
		t.Fatalf("got %d statuses, want 1. Internet providers are ENABLED and there is no "+
			"MusicArtist configuration, so Jellyfin's defaults are live and fetching. "+
			"Reporting nothing is the #2719 false clean", len(statuses))
	}
	if !statuses[0].Defaulted {
		t.Errorf("Defaulted = false, want true (nothing configured, so defaults apply)")
	}
	if statuses[0].RiskLevel != "critical" {
		t.Errorf("RiskLevel = %q, want critical (Jellyfin can REPLACE existing images and "+
			"strip EXIF, unlike Emby which only adds missing ones)", statuses[0].RiskLevel)
	}
	if len(statuses[0].FetcherNames) != 0 {
		t.Errorf("FetcherNames = %v, want empty; Jellyfin never reports which fetchers its "+
			"defaults use", statuses[0].FetcherNames)
	}
}

// TestCheckImageFetchersEnabled_AbsentMusicArtistWithProvidersOffIsClean is
// THE DIVERGENCE GUARD, and it deliberately asserts the OPPOSITE of the Emby
// absent-entry test.
//
// Same input as the test above in every respect except
// EnableInternetProviders=false. On Jellyfin that switch kills the whole
// library's fetching regardless of TypeOptions, so the defaults cannot run
// and silence is a TRUE clean.
//
// This is the specific way the #2719 fix could go wrong: hoisting the
// absent-entry check above the EnableInternetProviders gate would fix Emby's
// false NEGATIVE while manufacturing a Jellyfin false POSITIVE, sending
// operators to chase a fetcher that cannot fire. Emby has no equivalent
// switch, which is why its absent-entry case is unconditional. If this test
// and the Emby one ever assert the same thing, the divergence has been
// unified and one of them is vacuous.
func TestCheckImageFetchersEnabled_AbsentMusicArtistWithProvidersOffIsClean(t *testing.T) {
	srv := virtualFoldersServer(t, `[
		{
			"Name":"Music","CollectionType":"music","ItemId":"lib-001",
			"LibraryOptions":{
				"SaveLocalMetadata":false,"MetadataSavers":[],
				"EnableInternetProviders":false,
				"TypeOptions":[
					{"Type":"MusicAlbum","ImageFetchers":["TheAudioDb"],"MetadataFetchers":[]}
				]
			}
		}
	]`)

	c := NewWithHTTPClient(srv.URL, "key", "", srv.Client(), testLogger())
	statuses, err := c.CheckImageFetchersEnabled(context.Background())
	if err != nil {
		t.Fatalf("CheckImageFetchersEnabled: %v", err)
	}

	if len(statuses) != 0 {
		t.Errorf("got %d statuses, want 0. EnableInternetProviders=false switches off ALL "+
			"fetching for this library, so an absent MusicArtist entry is harmless -- its "+
			"defaults cannot run. Reporting it is a false POSITIVE, which is the failure mode "+
			"introduced by hoisting the absent-entry check above the internet-providers gate. "+
			"Emby has no such switch and correctly reports the same input as dirty: %+v",
			len(statuses), statuses)
	}
}

// TestCheckImageFetchersEnabled_ExplicitFetchersWithProvidersOffIsClean is
// the pre-existing behavior, re-asserted because the fix touches the same
// loop. Even an EXPLICIT fetcher list is inert when internet providers are
// off, so the library stays unreported.
func TestCheckImageFetchersEnabled_ExplicitFetchersWithProvidersOffIsClean(t *testing.T) {
	srv := virtualFoldersServer(t, `[
		{
			"Name":"Music","CollectionType":"music","ItemId":"lib-001",
			"LibraryOptions":{
				"SaveLocalMetadata":false,"MetadataSavers":[],
				"EnableInternetProviders":false,
				"TypeOptions":[
					{"Type":"MusicArtist","ImageFetchers":["TheAudioDb","FanArt"],"MetadataFetchers":[]}
				]
			}
		}
	]`)

	c := NewWithHTTPClient(srv.URL, "key", "", srv.Client(), testLogger())
	statuses, err := c.CheckImageFetchersEnabled(context.Background())
	if err != nil {
		t.Fatalf("CheckImageFetchersEnabled: %v", err)
	}
	if len(statuses) != 0 {
		t.Errorf("got %d statuses, want 0 (internet providers off makes even an explicit "+
			"fetcher list inert): %+v", len(statuses), statuses)
	}
}

// TestCheckImageFetchersEnabled_PresentButEmptyIsGenuinelyClean mirrors the
// Emby case: a configured library with no fetchers chosen is a true clean.
func TestCheckImageFetchersEnabled_PresentButEmptyIsGenuinelyClean(t *testing.T) {
	srv := virtualFoldersServer(t, `[
		{
			"Name":"Music","CollectionType":"music","ItemId":"lib-001",
			"LibraryOptions":{
				"SaveLocalMetadata":false,"MetadataSavers":[],
				"EnableInternetProviders":true,
				"TypeOptions":[
					{"Type":"MusicArtist","ImageFetchers":[],"MetadataFetchers":["TheAudioDb"]}
				]
			}
		}
	]`)

	c := NewWithHTTPClient(srv.URL, "key", "", srv.Client(), testLogger())
	statuses, err := c.CheckImageFetchersEnabled(context.Background())
	if err != nil {
		t.Fatalf("CheckImageFetchersEnabled: %v", err)
	}
	if len(statuses) != 0 {
		t.Errorf("got %d statuses, want 0. A PRESENT MusicArtist entry with an empty fetcher "+
			"list is deliberately configured, even with internet providers on: %+v",
			len(statuses), statuses)
	}
}

// TestCheckImageFetchersEnabled_PresentNonEmptyIsNotDefaulted keeps the
// explicit dirty case distinguishable from the defaulted one.
func TestCheckImageFetchersEnabled_PresentNonEmptyIsNotDefaulted(t *testing.T) {
	srv := virtualFoldersServer(t, `[
		{
			"Name":"Music","CollectionType":"music","ItemId":"lib-001",
			"LibraryOptions":{
				"SaveLocalMetadata":false,"MetadataSavers":[],
				"EnableInternetProviders":true,
				"TypeOptions":[
					{"Type":"MusicArtist","ImageFetchers":["TheAudioDb"],"MetadataFetchers":[]}
				]
			}
		}
	]`)

	c := NewWithHTTPClient(srv.URL, "key", "", srv.Client(), testLogger())
	statuses, err := c.CheckImageFetchersEnabled(context.Background())
	if err != nil {
		t.Fatalf("CheckImageFetchersEnabled: %v", err)
	}
	if len(statuses) != 1 {
		t.Fatalf("got %d statuses, want 1", len(statuses))
	}
	if statuses[0].Defaulted {
		t.Error("Defaulted = true, want false for an explicitly configured fetcher list")
	}
}

// TestCheckImageFetchersEnabled_BlankCollectionTypeAbsentEntryIsClean mirrors
// the Emby guard: a blank-CollectionType library has no MusicArtist entry
// because it is not a music library, so the defaulted rule must not fire.
// Internet providers are ON here to isolate the CollectionType axis -- the
// providers gate is already covered separately, and leaving it off would let
// this test pass for the wrong reason.
func TestCheckImageFetchersEnabled_BlankCollectionTypeAbsentEntryIsClean(t *testing.T) {
	srv := virtualFoldersServer(t, `[
		{
			"Name":"Home Videos","CollectionType":"","ItemId":"lib-003",
			"LibraryOptions":{
				"SaveLocalMetadata":false,"MetadataSavers":[],
				"EnableInternetProviders":true,
				"TypeOptions":[
					{"Type":"HomeVideos","ImageFetchers":["TheMovieDb"],"MetadataFetchers":[]}
				]
			}
		}
	]`)

	c := NewWithHTTPClient(srv.URL, "key", "", srv.Client(), testLogger())
	statuses, err := c.CheckImageFetchersEnabled(context.Background())
	if err != nil {
		t.Fatalf("CheckImageFetchersEnabled: %v", err)
	}
	if len(statuses) != 0 {
		t.Errorf("got %d statuses, want 0. Internet providers are ON, so this is isolating "+
			"the CollectionType axis: a non-music library must not produce an artist-image "+
			"warning: %+v", len(statuses), statuses)
	}
}

// TestCheckImageFetchersEnabled_BlankCollectionTypeExplicitFetchersStillWarns
// guards against over-correcting into a false negative, as on Emby.
func TestCheckImageFetchersEnabled_BlankCollectionTypeExplicitFetchersStillWarns(t *testing.T) {
	srv := virtualFoldersServer(t, `[
		{
			"Name":"Unlabeled Music","CollectionType":"","ItemId":"lib-004",
			"LibraryOptions":{
				"SaveLocalMetadata":false,"MetadataSavers":[],
				"EnableInternetProviders":true,
				"TypeOptions":[
					{"Type":"MusicArtist","ImageFetchers":["TheAudioDb"],"MetadataFetchers":[]}
				]
			}
		}
	]`)

	c := NewWithHTTPClient(srv.URL, "key", "", srv.Client(), testLogger())
	statuses, err := c.CheckImageFetchersEnabled(context.Background())
	if err != nil {
		t.Fatalf("CheckImageFetchersEnabled: %v", err)
	}
	if len(statuses) != 1 {
		t.Fatalf("got %d statuses, want 1 (an explicit fetcher list on a blank-labeled "+
			"library is a real conflict and must not go silent)", len(statuses))
	}
	if statuses[0].Defaulted {
		t.Error("Defaulted = true, want false")
	}
}
