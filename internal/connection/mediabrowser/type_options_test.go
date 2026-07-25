package mediabrowser

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

// The fixtures below are the whole point of this file, so the design is
// spelled out rather than left implicit.
//
// A slice-replacement bug -- building the MusicArtist entry we want and
// POSTing TypeOptions as a one-element list -- deletes the operator's other
// entries on a peer that treats the POST as a full replace. To CATCH that,
// the surviving entries must differ from the merged one along EVERY axis the
// merge could plausibly collapse:
//
//   - different Type values (MusicVideo, MusicAlbum, not two of a kind)
//   - non-empty ImageFetchers, with DIFFERENT contents per entry, so a bug
//     that copies one entry's fetchers over another is visible
//   - a field MusicArtist does not carry at all (MetadataFetchers on
//     MusicVideo, LocalMetadataReaderOrder on MusicAlbum), so a bug that
//     rebuilds entries from a fixed shape drops something detectable
//   - a nested map (MusicAlbum's ImageOptions) so a shallow rebuild that
//     flattens structure is visible
//
// If two entries coincided on any of these, a mutation could pass and the
// test would prove nothing.
func operatorTypeOptions() []any {
	return []any{
		map[string]any{
			"Type":             "MusicVideo",
			"ImageFetchers":    []any{"TheMovieDb", "TheAudioDb"},
			"MetadataFetchers": []any{"TheMovieDb"},
		},
		map[string]any{
			"Type":                     "MusicAlbum",
			"ImageFetchers":            []any{"FanArt"},
			"LocalMetadataReaderOrder": []any{"Nfo"},
			"ImageOptions": map[string]any{
				"MinWidth": float64(400),
			},
		},
	}
}

// deepCopyRaw round-trips through JSON to produce a value that shares no
// backing memory with the original. Used to snapshot the "before" state so
// an in-place mutation of the caller's slice cannot hide from the comparison.
func deepCopyRaw(t *testing.T, v any) any {
	t.Helper()
	buf, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal for deep copy: %v", err)
	}
	var out any
	if err := json.Unmarshal(buf, &out); err != nil {
		t.Fatalf("unmarshal for deep copy: %v", err)
	}
	return out
}

// findOption returns the entry with the given Type, failing the test when
// absent -- an assertion helper, so a missing entry reports as a clear
// failure rather than a nil-map panic three lines later.
func findOption(t *testing.T, opts []any, typeName string) map[string]any {
	t.Helper()
	got, ok := FindTypeOptionRaw(opts, typeName)
	if !ok {
		t.Fatalf("TypeOptions entry %q not found in %+v", typeName, opts)
	}
	return got
}

// TestMergeTypeOptionRaw_NonTargetEntriesRoundTripByteIdentical is the
// trap-(b) guard. It asserts the operator's MusicVideo and MusicAlbum
// configuration comes out the far side EXACTLY as it went in.
//
// Byte-identical via JSON comparison rather than field spot-checks on
// purpose: a spot-check only catches the fields the author thought to list,
// and the failure mode here is losing a field nobody thought about.
func TestMergeTypeOptionRaw_NonTargetEntriesRoundTripByteIdentical(t *testing.T) {
	in := operatorTypeOptions()
	before := deepCopyRaw(t, in)

	// Precondition: the fixture must actually contain the entries whose
	// survival is being asserted. Without this the test would pass
	// vacuously against an empty fixture.
	if _, ok := FindTypeOptionRaw(in, "MusicVideo"); !ok {
		t.Fatal("precondition: fixture must contain a MusicVideo entry, or this test proves nothing")
	}
	if _, ok := FindTypeOptionRaw(in, "MusicAlbum"); !ok {
		t.Fatal("precondition: fixture must contain a MusicAlbum entry, or this test proves nothing")
	}

	out := MergeTypeOptionRaw(in, TypeMusicArtist, map[string]any{"ImageFetchers": []string{}})

	for _, typeName := range []string{"MusicVideo", "MusicAlbum"} {
		wantEntry, ok := FindTypeOptionRaw(before.([]any), typeName)
		if !ok {
			t.Fatalf("precondition: %q missing from the before-snapshot", typeName)
		}
		gotEntry := findOption(t, out, typeName)

		wantJSON, err := json.Marshal(wantEntry)
		if err != nil {
			t.Fatalf("marshal want: %v", err)
		}
		gotJSON, err := json.Marshal(gotEntry)
		if err != nil {
			t.Fatalf("marshal got: %v", err)
		}
		if string(wantJSON) != string(gotJSON) {
			t.Errorf("TypeOptions entry %q was MODIFIED by the merge.\n got: %s\nwant: %s\n"+
				"The peer treats the LibraryOptions POST as a full replace, so any drift here "+
				"silently reconfigures the operator's server and is NOT covered by the opt-out snapshot",
				typeName, gotJSON, wantJSON)
		}
	}
}

// TestMergeTypeOptionRaw_DoesNotDropEntries pins the count separately from
// the content. Byte-identity of the entries that survive says nothing about
// entries that vanished, which is precisely the slice-replacement bug.
func TestMergeTypeOptionRaw_DoesNotDropEntries(t *testing.T) {
	in := operatorTypeOptions()
	out := MergeTypeOptionRaw(in, TypeMusicArtist, map[string]any{"ImageFetchers": []string{}})

	// 2 operator entries + 1 created MusicArtist.
	if len(out) != 3 {
		t.Fatalf("TypeOptions length = %d, want 3 (MusicVideo + MusicAlbum + created MusicArtist). "+
			"A shorter list means operator configuration was discarded: %+v", len(out), out)
	}
}

// TestMergeTypeOptionRaw_CreatesEntryWhenAbsent covers the absent case, which
// is a lever left unpulled rather than a config loss. An absent entry means
// the peer's DEFAULTS apply, and the defaults have fetchers on -- so "no
// entry" must become "an explicit entry", not "nothing to do".
func TestMergeTypeOptionRaw_CreatesEntryWhenAbsent(t *testing.T) {
	in := []any{
		map[string]any{"Type": "MusicVideo", "ImageFetchers": []any{"TheMovieDb"}},
	}

	if _, ok := FindTypeOptionRaw(in, TypeMusicArtist); ok {
		t.Fatal("precondition: fixture must NOT contain a MusicArtist entry, or this test proves nothing")
	}

	out := MergeTypeOptionRaw(in, TypeMusicArtist, map[string]any{"ImageFetchers": []string{}})

	created := findOption(t, out, TypeMusicArtist)
	if created["Type"] != TypeMusicArtist {
		t.Errorf("created entry Type = %v, want %q", created["Type"], TypeMusicArtist)
	}
	fetchers, ok := created["ImageFetchers"].([]string)
	if !ok {
		t.Fatalf("created entry ImageFetchers = %#v, want an empty []string", created["ImageFetchers"])
	}
	if len(fetchers) != 0 {
		t.Errorf("created entry ImageFetchers = %v, want empty", fetchers)
	}
	// The sibling must still be there: creation is not an excuse to reset.
	if _, ok := FindTypeOptionRaw(out, "MusicVideo"); !ok {
		t.Error("creating the MusicArtist entry DROPPED the operator's MusicVideo entry")
	}
}

// TestMergeTypeOptionRaw_CreatesEntryOnEmptyInput covers the peer that sends
// TypeOptions absent, null, or empty. All three arrive here as an empty
// slice and must still yield an explicit MusicArtist entry.
func TestMergeTypeOptionRaw_CreatesEntryOnEmptyInput(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []any
	}{
		{"nil slice", nil},
		{"empty slice", []any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := MergeTypeOptionRaw(tc.in, TypeMusicArtist, map[string]any{"ImageFetchers": []string{}})
			if len(out) != 1 {
				t.Fatalf("length = %d, want 1 created entry: %+v", len(out), out)
			}
			if _, ok := FindTypeOptionRaw(out, TypeMusicArtist); !ok {
				t.Errorf("no MusicArtist entry was created from %s", tc.name)
			}
		})
	}
}

// TestMergeTypeOptionRaw_MergesIntoExistingEntry checks the update path
// keeps the target entry's OTHER fields. Clearing ImageFetchers must not
// take MetadataFetchers with it -- that is a different lever, and PR B
// pulls it deliberately or not at all.
func TestMergeTypeOptionRaw_MergesIntoExistingEntry(t *testing.T) {
	in := []any{
		map[string]any{
			"Type":             TypeMusicArtist,
			"ImageFetchers":    []any{"FanArt", "TheAudioDb"},
			"MetadataFetchers": []any{"MusicBrainz"},
			"ImageOptions":     map[string]any{"MinWidth": float64(300)},
		},
	}

	existing := findOption(t, in, TypeMusicArtist)
	if got := StringsFromRaw(existing["ImageFetchers"]); len(got) == 0 {
		t.Fatal("precondition: fixture MusicArtist must start WITH fetchers, or the clear proves nothing")
	}

	out := MergeTypeOptionRaw(in, TypeMusicArtist, map[string]any{"ImageFetchers": []string{}})

	if len(out) != 1 {
		t.Fatalf("length = %d, want 1 (merged in place, not appended): %+v", len(out), out)
	}
	got := findOption(t, out, TypeMusicArtist)

	fetchers, ok := got["ImageFetchers"].([]string)
	if !ok || len(fetchers) != 0 {
		t.Errorf("ImageFetchers = %#v, want empty -- the merge did not apply the field", got["ImageFetchers"])
	}
	// Untouched neighbors on the SAME entry.
	if md := StringsFromRaw(got["MetadataFetchers"]); len(md) != 1 || md[0] != "MusicBrainz" {
		t.Errorf("MetadataFetchers = %v, want [MusicBrainz] preserved. Clearing image fetchers "+
			"must not clear a lever nobody asked to pull", md)
	}
	if _, ok := got["ImageOptions"].(map[string]any); !ok {
		t.Errorf("ImageOptions was dropped from the merged entry: %#v", got["ImageOptions"])
	}
}

// TestMergeTypeOptionRaw_MatchesTypeCaseInsensitively guards the casing
// divergence the constant's doc comment records. A case-sensitive match
// would CREATE a duplicate entry alongside the peer's own, leaving two
// MusicArtist entries where the peer reads one -- so this asserts the
// length, not just that some entry got merged.
func TestMergeTypeOptionRaw_MatchesTypeCaseInsensitively(t *testing.T) {
	in := []any{
		map[string]any{"Type": "musicartist", "ImageFetchers": []any{"FanArt"}},
	}
	out := MergeTypeOptionRaw(in, TypeMusicArtist, map[string]any{"ImageFetchers": []string{}})

	if len(out) != 1 {
		t.Fatalf("length = %d, want 1. A case-sensitive match created a DUPLICATE "+
			"MusicArtist entry: %+v", len(out), out)
	}
	got, _ := out[0].(map[string]any)
	if fetchers, ok := got["ImageFetchers"].([]string); !ok || len(fetchers) != 0 {
		t.Errorf("lowercase 'musicartist' entry was not merged: %#v", got["ImageFetchers"])
	}
}

// TestMergeTypeOptionRaw_DoesNotMutateInput pins the no-side-effects
// contract. The snapshot path reads the peer's decoded options map and the
// disable path merges into it; if the merge mutated in place, the snapshot
// would capture post-merge state and the opt-out restore would hand back the
// values Stillwater imposed rather than the operator's originals.
func TestMergeTypeOptionRaw_DoesNotMutateInput(t *testing.T) {
	in := []any{
		map[string]any{"Type": TypeMusicArtist, "ImageFetchers": []any{"FanArt", "TheAudioDb"}},
		map[string]any{"Type": "MusicVideo", "ImageFetchers": []any{"TheMovieDb"}},
	}
	before := deepCopyRaw(t, in)

	_ = MergeTypeOptionRaw(in, TypeMusicArtist, map[string]any{"ImageFetchers": []string{}})

	if !reflect.DeepEqual(deepCopyRaw(t, in), before) {
		t.Errorf("MergeTypeOptionRaw MUTATED its input.\n after: %+v\nbefore: %+v\n"+
			"The snapshot path reads this same map; in-place mutation makes the snapshot "+
			"capture Stillwater's values instead of the operator's", in, before)
	}
}

// TestMergeTypeOptionRaw_PreservesNonObjectEntries covers a peer sending
// something unexpected in the list. Passing it through is the round-trip
// promise: we do not understand it, so we do not get to delete it.
func TestMergeTypeOptionRaw_PreservesNonObjectEntries(t *testing.T) {
	in := []any{
		"unexpected-string-entry",
		map[string]any{"Type": "MusicVideo", "ImageFetchers": []any{"TheMovieDb"}},
	}
	out := MergeTypeOptionRaw(in, TypeMusicArtist, map[string]any{"ImageFetchers": []string{}})

	var foundString bool
	for _, e := range out {
		if s, ok := e.(string); ok && s == "unexpected-string-entry" {
			foundString = true
		}
	}
	if !foundString {
		t.Errorf("a non-object TypeOptions entry was DROPPED: %+v", out)
	}
}

func TestFindTypeOptionRaw_DistinguishesAbsentFromEmpty(t *testing.T) {
	// This distinction is what PR C's false-clean fix turns on, so it is
	// pinned here at the helper level: present-with-no-fetchers and absent
	// are different answers, and a helper that collapsed them would make
	// the false-clean unfixable downstream.
	present := []any{
		map[string]any{"Type": TypeMusicArtist, "ImageFetchers": []any{}},
	}
	opt, ok := FindTypeOptionRaw(present, TypeMusicArtist)
	if !ok {
		t.Fatal("present-but-empty entry reported as ABSENT; these are different states")
	}
	if got := StringsFromRaw(opt["ImageFetchers"]); len(got) != 0 {
		t.Errorf("ImageFetchers = %v, want empty", got)
	}

	absent := []any{
		map[string]any{"Type": "MusicVideo"},
	}
	if _, ok := FindTypeOptionRaw(absent, TypeMusicArtist); ok {
		t.Error("absent entry reported as PRESENT; an absent entry means the peer's defaults apply")
	}
}

func TestStringsFromRaw(t *testing.T) {
	got := StringsFromRaw([]any{"a", float64(2), "b", nil})
	if !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("got %v, want [a b] with non-strings dropped", got)
	}

	// Non-nil on every path: a null list is a shape the peer rejects.
	for _, in := range []any{nil, "not-a-slice", []any{}} {
		if out := StringsFromRaw(in); out == nil {
			t.Errorf("StringsFromRaw(%#v) returned nil; must be an empty non-nil slice "+
				"so it serializes as [] rather than null", in)
		}
	}
}

func TestTypeOptionsFrom(t *testing.T) {
	withOpts := map[string]any{"TypeOptions": []any{map[string]any{"Type": "MusicVideo"}}}
	if got := TypeOptionsFrom(withOpts); len(got) != 1 {
		t.Errorf("length = %d, want 1", len(got))
	}

	// Absent, null, and wrong-typed all reduce to empty so the caller never
	// has to branch; MergeTypeOptionRaw then creates the entry.
	for name, opts := range map[string]map[string]any{
		"absent":     {},
		"null":       {"TypeOptions": nil},
		"wrong type": {"TypeOptions": "not-an-array"},
	} {
		if got := TypeOptionsFrom(opts); len(got) != 0 {
			t.Errorf("%s: length = %d, want 0", name, len(got))
		}
	}
}

// TestMergeTypeOptionRaw_SurvivesPeerJSONRoundTrip is the end-to-end shape
// check: the merged list has to serialize into something the peer accepts
// and read back with the operator's entries intact. The unit tests above
// work on Go values; this one proves the JSON on the wire is right, which is
// what the peer actually sees.
func TestMergeTypeOptionRaw_SurvivesPeerJSONRoundTrip(t *testing.T) {
	tr := newFakeTransport()
	opts := map[string]any{
		"SaveLocalMetadata": true,
		"TypeOptions":       operatorTypeOptions(),
	}
	opts["TypeOptions"] = MergeTypeOptionRaw(
		TypeOptionsFrom(opts), TypeMusicArtist, map[string]any{"ImageFetchers": []string{}})

	if err := PostLibraryOptionsRaw(context.Background(), tr, testLogger(), "emby", "m1", opts); err != nil {
		t.Fatalf("PostLibraryOptionsRaw: %v", err)
	}
	if len(tr.posts) != 1 {
		t.Fatalf("post count = %d, want 1", len(tr.posts))
	}

	var wrapper struct {
		LibraryOptions map[string]any `json:"LibraryOptions"`
	}
	if err := json.Unmarshal(tr.posts[0].body, &wrapper); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	sent, _ := wrapper.LibraryOptions["TypeOptions"].([]any)
	if len(sent) != 3 {
		t.Fatalf("wire TypeOptions length = %d, want 3: %+v", len(sent), sent)
	}

	// The operator's entries must be intact ON THE WIRE, with their own
	// distinct fetcher lists -- not each other's.
	mv := findOption(t, sent, "MusicVideo")
	if got := StringsFromRaw(mv["ImageFetchers"]); !reflect.DeepEqual(got, []string{"TheMovieDb", "TheAudioDb"}) {
		t.Errorf("MusicVideo ImageFetchers on the wire = %v, want [TheMovieDb TheAudioDb]", got)
	}
	ma := findOption(t, sent, "MusicAlbum")
	if got := StringsFromRaw(ma["ImageFetchers"]); !reflect.DeepEqual(got, []string{"FanArt"}) {
		t.Errorf("MusicAlbum ImageFetchers on the wire = %v, want [FanArt]", got)
	}
	if _, ok := ma["ImageOptions"].(map[string]any); !ok {
		t.Errorf("MusicAlbum nested ImageOptions did not survive serialization: %#v", ma["ImageOptions"])
	}

	// And the entry we created must serialize as [] rather than null.
	artist := findOption(t, sent, TypeMusicArtist)
	if fetchers, ok := artist["ImageFetchers"].([]any); !ok || len(fetchers) != 0 {
		t.Errorf("MusicArtist ImageFetchers on the wire = %#v, want an empty JSON array "+
			"(null is a shape the peer rejects)", artist["ImageFetchers"])
	}
}
