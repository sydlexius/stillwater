package emby

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

// TestCheckImageFetchersEnabled_AbsentMusicArtistIsDefaultedOn is the #2719
// false-clean guard for Emby.
//
// The library below has TypeOptions, but none of them is MusicArtist. Before
// the fix this reported ZERO statuses, which the UI renders as "no image
// fetcher conflicts" -- telling the operator they are safe while Emby's
// defaults are actively fetching artist images into a directory Stillwater
// claims to manage. An absent entry is not "off", it is "unconfigured, so
// defaults apply", and Emby's defaults fetch.
func TestCheckImageFetchersEnabled_AbsentMusicArtistIsDefaultedOn(t *testing.T) {
	srv := virtualFoldersServer(t, `[
		{
			"Name":"Music","CollectionType":"music","ItemId":"lib-001",
			"LibraryOptions":{
				"SaveLocalMetadata":false,"MetadataSavers":[],
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
		t.Fatalf("got %d statuses, want 1. A music library with NO MusicArtist TypeOption "+
			"must be reported: Emby applies its own defaults to an unconfigured type and those "+
			"defaults FETCH IMAGES. Reporting nothing here is the #2719 false clean -- it tells "+
			"the operator they are safe while the peer is writing artwork", len(statuses))
	}
	s := statuses[0]
	if !s.Defaulted {
		t.Errorf("Defaulted = false, want true. The operator needs to be told the fetchers are "+
			"on BY DEFAULT (nothing to switch off yet) rather than explicitly configured; the "+
			"remediation differs. FetcherNames=%v", s.FetcherNames)
	}
	if s.LibraryName != "Music" {
		t.Errorf("LibraryName = %q, want Music", s.LibraryName)
	}
	if s.LibraryID != "lib-001" {
		t.Errorf("LibraryID = %q, want lib-001", s.LibraryID)
	}
	if len(s.FetcherNames) != 0 {
		t.Errorf("FetcherNames = %v, want empty. Emby never reports which fetchers its DEFAULTS "+
			"use, so inventing names here would put a fabricated list in front of the operator", s.FetcherNames)
	}
	if s.RiskLevel != "warn" {
		t.Errorf("RiskLevel = %q, want warn (Emby adds missing images only)", s.RiskLevel)
	}
}

// TestCheckImageFetchersEnabled_NoTypeOptionsAtAllIsDefaultedOn covers the
// library that has no TypeOptions array whatsoever -- a fresh library the
// operator has never opened the settings for. Same reasoning as an absent
// MusicArtist entry, and a distinct input shape (missing key vs present
// array without the entry), so it gets its own case rather than being
// assumed equivalent.
func TestCheckImageFetchersEnabled_NoTypeOptionsAtAllIsDefaultedOn(t *testing.T) {
	srv := virtualFoldersServer(t, `[
		{
			"Name":"Music","CollectionType":"music","ItemId":"lib-001",
			"LibraryOptions":{"SaveLocalMetadata":false,"MetadataSavers":[]}
		}
	]`)

	c := NewWithHTTPClient(srv.URL, "key", "", srv.Client(), testLogger())
	statuses, err := c.CheckImageFetchersEnabled(context.Background())
	if err != nil {
		t.Fatalf("CheckImageFetchersEnabled: %v", err)
	}
	if len(statuses) != 1 {
		t.Fatalf("got %d statuses, want 1 (a library with no TypeOptions at all runs defaults)", len(statuses))
	}
	if !statuses[0].Defaulted {
		t.Error("Defaulted = false, want true for a library with no TypeOptions array")
	}
}

// TestCheckImageFetchersEnabled_PresentButEmptyIsGenuinelyClean pins the
// other side of the distinction. A PRESENT MusicArtist entry with an empty
// fetcher list is an operator who configured this library and chose no
// fetchers. That is a TRUE clean and must stay silent -- if the fix reported
// it, every correctly-configured install would grow a permanent false alarm.
func TestCheckImageFetchersEnabled_PresentButEmptyIsGenuinelyClean(t *testing.T) {
	srv := virtualFoldersServer(t, `[
		{
			"Name":"Music","CollectionType":"music","ItemId":"lib-001",
			"LibraryOptions":{
				"SaveLocalMetadata":false,"MetadataSavers":[],
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
			"list is a deliberately configured library -- reporting it would turn every "+
			"correctly-set-up install into a permanent false alarm: %+v", len(statuses), statuses)
	}
}

// TestCheckImageFetchersEnabled_PresentNonEmptyIsNotDefaulted checks the
// pre-existing dirty case still reports, and reports as EXPLICIT rather than
// defaulted. Without the Defaulted assertion a bug that marked everything
// defaulted would pass this test.
func TestCheckImageFetchersEnabled_PresentNonEmptyIsNotDefaulted(t *testing.T) {
	srv := virtualFoldersServer(t, `[
		{
			"Name":"Music","CollectionType":"music","ItemId":"lib-001",
			"LibraryOptions":{
				"SaveLocalMetadata":false,"MetadataSavers":[],
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
	if len(statuses) != 1 {
		t.Fatalf("got %d statuses, want 1", len(statuses))
	}
	if statuses[0].Defaulted {
		t.Error("Defaulted = true, want false. These fetchers are EXPLICITLY configured; " +
			"the operator can switch them off by name, so the copy must not say 'nothing is configured'")
	}
	if len(statuses[0].FetcherNames) != 2 {
		t.Errorf("FetcherNames = %v, want the 2 configured fetchers", statuses[0].FetcherNames)
	}
}

// TestCheckImageFetchersEnabled_NonMusicLibraryNeverDefaulted guards the
// blast radius of the fix. A movies library has no MusicArtist entry either,
// but it is not a music library and must never be reported -- otherwise
// every Emby install with a film collection grows a spurious artist-image
// warning.
func TestCheckImageFetchersEnabled_NonMusicLibraryNeverDefaulted(t *testing.T) {
	srv := virtualFoldersServer(t, `[
		{
			"Name":"Movies","CollectionType":"movies","ItemId":"lib-002",
			"LibraryOptions":{
				"SaveLocalMetadata":false,"MetadataSavers":[],
				"TypeOptions":[
					{"Type":"Movie","ImageFetchers":["TheMovieDb"],"MetadataFetchers":[]}
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
		t.Errorf("got %d statuses, want 0. A movies library also lacks a MusicArtist entry, "+
			"but it is not a music library -- reporting it would put an artist-image warning "+
			"in front of every operator who owns films: %+v", len(statuses), statuses)
	}
}

// TestCheckImageFetchersEnabled_BlankCollectionTypeAbsentEntryIsClean is the
// false-positive guard for the defaulted rule.
//
// The upstream library filter admits a library whose CollectionType is
// "music" OR BLANK, because some installs leave it blank on mixed or legacy
// folders that really do hold music. That leniency was free while absence
// was silent. Once absence became the TRIGGER it stopped being free: a
// non-music library (home videos, a mixed folder) has no MusicArtist entry
// precisely BECAUSE it is not a music library, so the defaulted rule would
// fire on every one of them.
//
// The operator consequence is what makes this worth a dedicated test: they
// would be told to open their home-video library and switch off artist image
// fetchers that do not exist there. A banner full of unactionable warnings
// teaches people to ignore the one that is real.
func TestCheckImageFetchersEnabled_BlankCollectionTypeAbsentEntryIsClean(t *testing.T) {
	srv := virtualFoldersServer(t, `[
		{
			"Name":"Home Videos","CollectionType":"","ItemId":"lib-003",
			"LibraryOptions":{
				"SaveLocalMetadata":false,"MetadataSavers":[],
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
		t.Errorf("got %d statuses, want 0. A blank-CollectionType library has no MusicArtist "+
			"entry because it is NOT a music library. Warning here tells the operator to go "+
			"switch off artist image fetchers in a home-video library, where no such setting "+
			"exists: %+v", len(statuses), statuses)
	}
}

// TestCheckImageFetchersEnabled_BlankCollectionTypeExplicitFetchersStillWarns
// is the other half, and it is what stops the fix above from over-correcting
// into a false NEGATIVE.
//
// A blank-CollectionType library carrying an actual MusicArtist entry with
// armed fetchers is a genuinely-music library the operator never labeled.
// The entry's existence is affirmative proof the peer treats it as holding
// artists, whatever the label says, and the fetchers really are armed. That
// must still warn. Suppressing it would trade the false positive above for a
// silent real conflict, which is the worse of the two.
func TestCheckImageFetchersEnabled_BlankCollectionTypeExplicitFetchersStillWarns(t *testing.T) {
	srv := virtualFoldersServer(t, `[
		{
			"Name":"Unlabeled Music","CollectionType":"","ItemId":"lib-004",
			"LibraryOptions":{
				"SaveLocalMetadata":false,"MetadataSavers":[],
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
		t.Fatalf("got %d statuses, want 1. Requiring CollectionType==music for the DEFAULTED "+
			"case must not suppress an EXPLICIT fetcher list on a blank-labeled library -- "+
			"that is a real conflict going silent", len(statuses))
	}
	if statuses[0].Defaulted {
		t.Error("Defaulted = true, want false; these fetchers are explicitly configured")
	}
}
