package mediabrowser

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestBuildSnapshotV2_StampsVersionTwoAndCarriesFetchers(t *testing.T) {
	entries := []LibrarySaverSnapshotEntry{{
		LibraryID: "m1", LibraryName: "Music",
		SaveLocalMetadata: true,
		MetadataSavers:    []string{"Nfo"},
		ImageFetchers:     []string{"FanArt", "TheAudioDb"},
	}}

	snapJSON, err := BuildSnapshotV2(entries)
	if err != nil {
		t.Fatalf("BuildSnapshotV2: %v", err)
	}

	var snap LibraryWriteBackSnapshot
	if err := json.Unmarshal([]byte(snapJSON), &snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if snap.Version != SnapshotVersionWithFetchers {
		t.Errorf("version = %d, want %d", snap.Version, SnapshotVersionWithFetchers)
	}
	if !reflect.DeepEqual(snap.Libraries, entries) {
		t.Errorf("entries round-trip mismatch:\n got %+v\nwant %+v", snap.Libraries, entries)
	}
	if snap.SnapshottedAt.IsZero() {
		t.Error("SnapshottedAt should be populated")
	}
}

// TestBuildSnapshot_StillStampsVersionOne pins that adding v2 did not
// silently promote the existing caller. BuildSnapshot's callers do not read
// fetchers off the peer, so stamping their output v2 would tell restore to
// clear fetcher lists it never recorded.
func TestBuildSnapshot_StillStampsVersionOne(t *testing.T) {
	snapJSON, err := BuildSnapshot([]LibrarySaverSnapshotEntry{{LibraryID: "m1"}})
	if err != nil {
		t.Fatalf("BuildSnapshot: %v", err)
	}
	var snap LibraryWriteBackSnapshot
	if err := json.Unmarshal([]byte(snapJSON), &snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if snap.Version != SnapshotVersionSavers {
		t.Errorf("version = %d, want %d -- BuildSnapshot must not be silently promoted to v2",
			snap.Version, SnapshotVersionSavers)
	}
}

// peerWithFetchers builds a stateful peer holding one music library whose
// MusicArtist entry has fetchers armed, alongside an operator-configured
// MusicVideo entry that must survive every restore path.
func peerWithFetchers() *statefulPeer {
	return newStatefulPeer(map[string]map[string]any{
		"m1": {
			"SaveLocalMetadata": false,
			"MetadataSavers":    []any{},
			"TypeOptions": []any{
				map[string]any{"Type": "MusicArtist", "ImageFetchers": []any{"FanArt"}},
				map[string]any{"Type": "MusicVideo", "ImageFetchers": []any{"TheMovieDb"}},
			},
		},
	})
}

// musicArtistFetchers reads back what the peer now holds for MusicArtist in
// the single-library fixtures this file uses.
func musicArtistFetchers(t *testing.T, p *statefulPeer) []string {
	t.Helper()
	const libID = "m1"
	raw, _ := p.libs[libID]["TypeOptions"].([]any)
	opt, ok := FindTypeOptionRaw(raw, TypeMusicArtist)
	if !ok {
		t.Fatalf("library %q has no MusicArtist TypeOption: %+v", libID, raw)
	}
	// The peer stores whatever shape was POSTed, which may be []string or
	// []any depending on path; normalize both.
	if typed, ok := opt["ImageFetchers"].([]string); ok {
		return typed
	}
	return StringsFromRaw(opt["ImageFetchers"])
}

// TestRestoreLibraryOptions_AcceptsV1AndLeavesFetchersAlone is the
// compatibility half. An install that snapshotted before this change has a
// v1 blob on its connection row; restoring it must still work, and must NOT
// invent a fetcher list it never recorded.
func TestRestoreLibraryOptions_AcceptsV1AndLeavesFetchersAlone(t *testing.T) {
	peer := peerWithFetchers()

	// Precondition: fetchers must start ARMED, or "left alone" is unprovable.
	if got := musicArtistFetchers(t, peer); len(got) == 0 {
		t.Fatal("precondition: MusicArtist must start with fetchers armed, or this test proves nothing")
	}

	snap := LibraryWriteBackSnapshot{
		Version: SnapshotVersionSavers,
		Libraries: []LibrarySaverSnapshotEntry{{
			LibraryID: "m1", SaveLocalMetadata: true, MetadataSavers: []string{"Nfo"},
		}},
	}
	buf, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if err := RestoreLibraryOptions(context.Background(), peer, testLogger(), "emby", string(buf)); err != nil {
		t.Fatalf("v1 snapshot must still restore: %v", err)
	}

	// The v1 fields it DID record are replayed.
	if peer.libs["m1"]["SaveLocalMetadata"] != true {
		t.Errorf("SaveLocalMetadata = %v, want true replayed", peer.libs["m1"]["SaveLocalMetadata"])
	}
	// The field it did NOT record is untouched -- not cleared.
	if got := musicArtistFetchers(t, peer); !reflect.DeepEqual(got, []string{"FanArt"}) {
		t.Errorf("v1 restore changed ImageFetchers to %v, want [FanArt] untouched. "+
			"A v1 snapshot has no record of fetchers, so restoring one must never clear them", got)
	}
}

// TestRestoreLibraryOptions_AcceptsV2AndReplaysFetchers is the forward half:
// a v2 snapshot's fetcher list IS authoritative and gets handed back.
func TestRestoreLibraryOptions_AcceptsV2AndReplaysFetchers(t *testing.T) {
	peer := newStatefulPeer(map[string]map[string]any{
		"m1": {
			"SaveLocalMetadata": false,
			"TypeOptions": []any{
				// Stillwater-managed state: fetchers cleared.
				map[string]any{"Type": "MusicArtist", "ImageFetchers": []any{}},
				map[string]any{"Type": "MusicVideo", "ImageFetchers": []any{"TheMovieDb"}},
			},
		},
	})

	if got := musicArtistFetchers(t, peer); len(got) != 0 {
		t.Fatalf("precondition: fetchers must start CLEARED so the replay is observable, got %v", got)
	}

	snap := LibraryWriteBackSnapshot{
		Version: SnapshotVersionWithFetchers,
		Libraries: []LibrarySaverSnapshotEntry{{
			LibraryID: "m1", SaveLocalMetadata: true,
			MetadataSavers: []string{"Nfo"},
			ImageFetchers:  []string{"FanArt", "TheAudioDb"},
		}},
	}
	buf, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if err := RestoreLibraryOptions(context.Background(), peer, testLogger(), "emby", string(buf)); err != nil {
		t.Fatalf("v2 restore: %v", err)
	}

	if got := musicArtistFetchers(t, peer); !reflect.DeepEqual(got, []string{"FanArt", "TheAudioDb"}) {
		t.Errorf("ImageFetchers = %v, want [FanArt TheAudioDb] replayed from the v2 snapshot", got)
	}
	// Trap (b) again, this time on the restore path.
	raw, _ := peer.libs["m1"]["TypeOptions"].([]any)
	mv, ok := FindTypeOptionRaw(raw, "MusicVideo")
	if !ok {
		t.Fatal("v2 restore DROPPED the operator's MusicVideo TypeOption")
	}
	if got := StringsFromRaw(mv["ImageFetchers"]); !reflect.DeepEqual(got, []string{"TheMovieDb"}) {
		t.Errorf("MusicVideo ImageFetchers = %v, want [TheMovieDb] untouched by an artist-scope restore", got)
	}
}

// TestRestoreLibraryOptions_V2EmptyFetchersClearsExplicitly is the case the
// version discriminator exists for. In a v2 snapshot an empty list means the
// operator genuinely had no fetchers, so restore must CLEAR them -- the
// opposite of what the same empty value means under v1.
func TestRestoreLibraryOptions_V2EmptyFetchersClearsExplicitly(t *testing.T) {
	peer := peerWithFetchers()

	if got := musicArtistFetchers(t, peer); len(got) == 0 {
		t.Fatal("precondition: fetchers must start armed so the clear is observable")
	}

	snap := LibraryWriteBackSnapshot{
		Version: SnapshotVersionWithFetchers,
		Libraries: []LibrarySaverSnapshotEntry{{
			LibraryID: "m1", SaveLocalMetadata: true,
			MetadataSavers: []string{"Nfo"},
			ImageFetchers:  []string{},
		}},
	}
	buf, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if err := RestoreLibraryOptions(context.Background(), peer, testLogger(), "emby", string(buf)); err != nil {
		t.Fatalf("v2 restore: %v", err)
	}

	if got := musicArtistFetchers(t, peer); len(got) != 0 {
		t.Errorf("ImageFetchers = %v, want CLEARED. Under v2 an empty list is authoritative -- "+
			"it records an operator who genuinely had no fetchers", got)
	}
}

// TestRestoreLibraryOptions_V2CreatesAbsentMusicArtistEntry: restoring onto a
// peer that has no MusicArtist entry must create one, for the same reason the
// disable path does. Leaving it absent means the peer's defaults apply, and
// the defaults are not what the snapshot recorded.
func TestRestoreLibraryOptions_V2CreatesAbsentMusicArtistEntry(t *testing.T) {
	peer := newStatefulPeer(map[string]map[string]any{
		"m1": {
			"SaveLocalMetadata": false,
			"TypeOptions": []any{
				map[string]any{"Type": "MusicVideo", "ImageFetchers": []any{"TheMovieDb"}},
			},
		},
	})

	raw, _ := peer.libs["m1"]["TypeOptions"].([]any)
	if _, ok := FindTypeOptionRaw(raw, TypeMusicArtist); ok {
		t.Fatal("precondition: peer must NOT have a MusicArtist entry, or this test proves nothing")
	}

	snap := LibraryWriteBackSnapshot{
		Version: SnapshotVersionWithFetchers,
		Libraries: []LibrarySaverSnapshotEntry{{
			LibraryID: "m1", SaveLocalMetadata: true, ImageFetchers: []string{"FanArt"},
		}},
	}
	buf, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if err := RestoreLibraryOptions(context.Background(), peer, testLogger(), "emby", string(buf)); err != nil {
		t.Fatalf("v2 restore: %v", err)
	}

	if got := musicArtistFetchers(t, peer); !reflect.DeepEqual(got, []string{"FanArt"}) {
		t.Errorf("ImageFetchers = %v, want [FanArt] on a created entry", got)
	}
}

// TestRestoreLibraryOptions_V2NilFetchersSerializeAsEmptyArray mirrors the
// existing nil-savers contract: a null list is a shape the peer rejects.
func TestRestoreLibraryOptions_V2NilFetchersSerializeAsEmptyArray(t *testing.T) {
	tr := newFakeTransport()
	tr.getResponses["/Library/VirtualFolders"] = []any{[]map[string]any{
		{"ItemId": "m1", "Name": "Music", "CollectionType": "music",
			"LibraryOptions": map[string]any{"TypeOptions": []any{}}},
	}}

	snap := LibraryWriteBackSnapshot{
		Version: SnapshotVersionWithFetchers,
		Libraries: []LibrarySaverSnapshotEntry{{
			LibraryID: "m1", SaveLocalMetadata: false, ImageFetchers: nil,
		}},
	}
	buf, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := RestoreLibraryOptions(context.Background(), tr, testLogger(), "emby", string(buf)); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if len(tr.posts) != 1 {
		t.Fatalf("post count = %d, want 1", len(tr.posts))
	}

	var wrapper struct {
		LibraryOptions map[string]any `json:"LibraryOptions"`
	}
	if err := json.Unmarshal(tr.posts[0].body, &wrapper); err != nil {
		t.Fatalf("decode: %v", err)
	}
	sent, _ := wrapper.LibraryOptions["TypeOptions"].([]any)
	artist, ok := FindTypeOptionRaw(sent, TypeMusicArtist)
	if !ok {
		t.Fatalf("no MusicArtist entry on the wire: %+v", sent)
	}
	if fetchers, isArray := artist["ImageFetchers"].([]any); !isArray || len(fetchers) != 0 {
		t.Errorf("ImageFetchers on the wire = %#v, want an empty JSON array; "+
			"null is a shape the peer rejects", artist["ImageFetchers"])
	}
}

// TestRestoreLibraryOptions_RejectsVersionAboveKnown keeps the forward-compat
// guard honest now that 2 is valid. A future v3 snapshot restored by today's
// binary would misapply levers it does not model, so it must be refused
// rather than partially applied.
func TestRestoreLibraryOptions_RejectsVersionAboveKnown(t *testing.T) {
	tr := newFakeTransport()
	err := RestoreLibraryOptions(context.Background(), tr, testLogger(), "emby",
		`{"version":3,"libraries":[{"library_id":"m1"}]}`)
	if err == nil || !strings.Contains(err.Error(), "unsupported snapshot version") {
		t.Errorf("expected unsupported-version error for v3, got %v", err)
	}
	if len(tr.posts) != 0 {
		t.Errorf("a rejected snapshot must not POST anything, got %d posts", len(tr.posts))
	}
}

// TestRestoreLibraryOptions_RejectsVersionZero covers the malformed/missing
// version field, which decodes to 0. Failing closed matters here: version 0
// is what a truncated or hand-edited blob looks like.
func TestRestoreLibraryOptions_RejectsVersionZero(t *testing.T) {
	tr := newFakeTransport()
	err := RestoreLibraryOptions(context.Background(), tr, testLogger(), "emby",
		`{"libraries":[{"library_id":"m1"}]}`)
	if err == nil || !strings.Contains(err.Error(), "unsupported snapshot version") {
		t.Errorf("expected unsupported-version error for a missing version field, got %v", err)
	}
	if len(tr.posts) != 0 {
		t.Errorf("a rejected snapshot must not POST anything, got %d posts", len(tr.posts))
	}
}

// TestSnapshotVersionSemantics_AreDeclaredPerVersion is a FORCING FUNCTION
// for the next person who adds a snapshot version, not a guard on the
// current fetcher-replay comparison.
//
// WHAT IT CANNOT DO, stated plainly so nobody mistakes it for more than it
// is. RestoreLibraryOptions rejects every version except 1 and 2, so for
// every input that reaches the comparison, `version >= 2` and `version == 2`
// are the SAME predicate. That is arithmetic, not a gap in coverage: no test
// driving the public API can tell the two operators apart, and one claiming
// to would be vacuous. Measured, not assumed -- reverting `==` to `>=` leaves
// this entire package, and all of internal/connection, green.
//
// WHAT IT DOES DO. It pins the two version SETS against each other:
// the set restore accepts, and the subset that replays fetchers. Those are
// separate decisions, and today the second is a strict subset of the first.
// Adding a version 3 to the accept gate without touching this table breaks
// the "everything else is rejected" row, which is precisely the moment
// someone must decide whether v3 replays fetchers. A `>=` comparison would
// have made that decision silently, by inheritance; `==` plus this table
// makes it a choice someone has to write down.
func TestSnapshotVersionSemantics_AreDeclaredPerVersion(t *testing.T) {
	// The peer holds an armed fetcher list, so "replayed" and "left alone"
	// are observably different outcomes rather than two flavors of nothing.
	const armedFetcher = "FanArt"

	for _, tc := range []struct {
		version         int
		accepted        bool
		replaysFetchers bool
		rationale       string
	}{
		{
			version: 0, accepted: false,
			rationale: "a missing or malformed version field decodes to 0; failing closed " +
				"is what stops a truncated blob being applied as if it were understood",
		},
		{
			version: SnapshotVersionSavers, accepted: true, replaysFetchers: false,
			rationale: "v1 predates the fetcher lever, so it has no record to replay and " +
				"must not clear what it never captured",
		},
		{
			version: SnapshotVersionWithFetchers, accepted: true, replaysFetchers: true,
			rationale: "v2 records fetchers, so its list is authoritative -- including when " +
				"it is empty",
		},
		{
			version: 3, accepted: false,
			rationale: "v3 does not exist yet. When it does, this row must be updated " +
				"DELIBERATELY, and updating it forces a decision about fetcher replay",
		},
		{
			version: 99, accepted: false,
			rationale: "a far-future snapshot restored by today's binary would misapply " +
				"levers it does not model",
		},
	} {
		t.Run(fmt.Sprintf("v%d", tc.version), func(t *testing.T) {
			peer := newStatefulPeer(map[string]map[string]any{
				"m1": {
					"SaveLocalMetadata": false,
					"TypeOptions": []any{
						map[string]any{"Type": "MusicArtist", "ImageFetchers": []any{armedFetcher}},
					},
				},
			})
			// Precondition: the fetcher must start armed, or "left alone"
			// and "cleared" look identical and every row passes vacuously.
			if got := musicArtistFetchers(t, peer); len(got) == 0 {
				t.Fatal("precondition: the peer must start with a fetcher armed")
			}

			// The snapshot carries an EMPTY fetcher list. Under v2 that is
			// authoritative and clears the peer's list; under v1 it is
			// "not recorded" and must leave the list alone. Empty is the
			// discriminating value: a populated list would be replayed to
			// the same visible end state either way for some fixtures.
			buf, err := json.Marshal(LibraryWriteBackSnapshot{
				Version: tc.version,
				Libraries: []LibrarySaverSnapshotEntry{{
					LibraryID: "m1", SaveLocalMetadata: true,
					MetadataSavers: []string{"Nfo"},
					ImageFetchers:  []string{},
				}},
			})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}

			err = RestoreLibraryOptions(context.Background(), peer, testLogger(), "emby", string(buf))

			if !tc.accepted {
				if err == nil || !strings.Contains(err.Error(), "unsupported snapshot version") {
					t.Fatalf("v%d was ACCEPTED, want rejected. %s.\nIf this version is now "+
						"meant to be supported, updating the accept gate is only half the "+
						"change: decide explicitly whether it replays fetchers and add it to "+
						"this table with that decision recorded. err = %v",
						tc.version, tc.rationale, err)
				}
				return
			}

			if err != nil {
				t.Fatalf("v%d was REJECTED, want accepted. %s. err = %v", tc.version, tc.rationale, err)
			}

			got := musicArtistFetchers(t, peer)
			if tc.replaysFetchers {
				if len(got) != 0 {
					t.Errorf("v%d did NOT replay the snapshot's empty fetcher list "+
						"(peer still has %v). %s", tc.version, got, tc.rationale)
				}
				return
			}
			if !reflect.DeepEqual(got, []string{armedFetcher}) {
				t.Errorf("v%d CHANGED the peer's fetcher list to %v, want [%s] untouched. %s",
					tc.version, got, armedFetcher, tc.rationale)
			}
		})
	}
}
