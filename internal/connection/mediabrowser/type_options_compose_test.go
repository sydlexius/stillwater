package mediabrowser

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestMergeThenFindThenStringsFromRaw_ComposesBothDirections is the guard for
// the compose defect found in hostile review.
//
// The three helpers are used as a pipeline by callers: merge fields in, find
// the entry back, read its list. Each worked in isolation and the pipeline
// silently lost data. MergeTypeOptionRaw copies caller-supplied field values
// VERBATIM, so passing a Go []string stored a []string; StringsFromRaw only
// handled []any, so the assertion failed, the slice came back nil, and a
// two-element fetcher list read as ZERO fetchers.
//
// Why that is dangerous rather than merely wrong: a false "empty" is
// indistinguishable from a genuine "the operator configured no fetchers", so
// the caller concludes the library is clean when it is not. That is the exact
// false-clean shape the surrounding work exists to eliminate.
//
// Both input shapes are exercised, because the map genuinely holds either
// depending on whether the peer or Stillwater wrote the key last.
func TestMergeThenFindThenStringsFromRaw_ComposesBothDirections(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
		want  []string
	}{
		{
			name:  "Stillwater-written Go []string",
			value: []string{"FanArt", "TheAudioDb"},
			want:  []string{"FanArt", "TheAudioDb"},
		},
		{
			name:  "peer-decoded []any",
			value: []any{"FanArt", "TheAudioDb"},
			want:  []string{"FanArt", "TheAudioDb"},
		},
		{
			name:  "Stillwater-written empty []string",
			value: []string{},
			want:  []string{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := MergeTypeOptionRaw(nil, TypeMusicArtist, map[string]any{"ImageFetchers": tc.value})

			opt, ok := FindTypeOptionRaw(out, TypeMusicArtist)
			if !ok {
				t.Fatalf("precondition: the merged entry must be findable, or the read half "+
					"of this pipeline proves nothing: %+v", out)
			}

			got := StringsFromRaw(opt["ImageFetchers"])
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("round-trip lost data: stored %#v, read back %#v, want %#v.\n"+
					"A fetcher list that reads back SHORTER than it is makes a configured "+
					"library look clean -- the false-clean bug this package exists to prevent",
					opt["ImageFetchers"], got, tc.want)
			}
		})
	}
}

// TestStringsFromRaw_HandlesBothSliceShapes pins the helper directly, since
// the pipeline test above could in principle be satisfied by a change on the
// merge side alone.
func TestStringsFromRaw_HandlesBothSliceShapes(t *testing.T) {
	if got := StringsFromRaw([]string{"a", "b"}); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("[]string input: got %#v, want [a b]. A Stillwater-written value must read "+
			"back at full length", got)
	}
	if got := StringsFromRaw([]any{"a", "b"}); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("[]any input: got %#v, want [a b]", got)
	}
	// Mixed members still drop non-strings.
	if got := StringsFromRaw([]any{"a", float64(2), nil, "b"}); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("mixed input: got %#v, want [a b]", got)
	}
	// Every shape returns non-nil so it serializes as [] rather than null.
	for _, in := range []any{nil, "not-a-slice", []any{}, []string{}, 42} {
		if out := StringsFromRaw(in); out == nil {
			t.Errorf("StringsFromRaw(%#v) returned nil; a null list is a shape the peer rejects", in)
		}
	}
}

// TestStringsFromRaw_ReturnedSliceDoesNotAliasInput checks the []string path
// copies rather than handing back the caller's own slice. Returning the input
// directly would let a caller's later append mutate the stored options map.
func TestStringsFromRaw_ReturnedSliceDoesNotAliasInput(t *testing.T) {
	// Extra capacity in the input is what makes this test meaningful: a
	// naive `return raw` hands back a slice whose spare capacity belongs to
	// the caller, so the append below would write into the input's backing
	// array rather than a copy of it.
	in := make([]string, 1, 4)
	in[0] = "FanArt"

	got := StringsFromRaw(in)
	appended := append(got, "Injected")

	if len(in) != 1 || in[0] != "FanArt" {
		t.Errorf("appending to the returned slice mutated the input: %v", in)
	}
	if len(appended) != 2 || appended[0] != "FanArt" || appended[1] != "Injected" {
		t.Errorf("appended = %v, want [FanArt Injected]", appended)
	}
	// The stored value must be unreachable through the returned slice's
	// spare capacity too.
	if cap(in) > 1 && len(in) == 1 && in[:cap(in)][1] == "Injected" {
		t.Error("the append reached the INPUT's backing array; StringsFromRaw returned a " +
			"slice aliasing its argument rather than a copy")
	}
}

// TestMergeTypeOptionRaw_MergesEveryDuplicateEntry pins the write-side
// decision on duplicates.
//
// A peer sending two MusicArtist entries gets BOTH merged. This is the
// deliberate choice: these fields are levers being disarmed, and since the
// package cannot know which entry the peer honors, a write that disarmed only
// the first would leave the lever half-pulled. Before this test nothing
// constrained the behavior -- a mutation adding a break after the first match
// passed the whole suite.
func TestMergeTypeOptionRaw_MergesEveryDuplicateEntry(t *testing.T) {
	in := []any{
		map[string]any{"Type": TypeMusicArtist, "ImageFetchers": []any{"FanArt"}, "Marker": "first"},
		map[string]any{"Type": "MusicVideo", "ImageFetchers": []any{"TheMovieDb"}},
		// Second MusicArtist entry, differing casing so this doubles as a
		// case-insensitivity check on the write side.
		map[string]any{"Type": "musicartist", "ImageFetchers": []any{"TheAudioDb"}, "Marker": "second"},
	}

	// Precondition: both entries must start ARMED, or "both disarmed" is
	// unprovable.
	for _, e := range []int{0, 2} {
		opt := in[e].(map[string]any)
		if len(StringsFromRaw(opt["ImageFetchers"])) == 0 {
			t.Fatalf("precondition: entry %d must start with fetchers armed", e)
		}
	}

	out := MergeTypeOptionRaw(in, TypeMusicArtist, map[string]any{"ImageFetchers": []string{}})

	var seen int
	for _, entry := range out {
		opt, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		name, _ := opt["Type"].(string)
		if name != TypeMusicArtist && name != "musicartist" {
			continue
		}
		seen++
		if got := StringsFromRaw(opt["ImageFetchers"]); len(got) != 0 {
			t.Errorf("duplicate entry %q (Marker=%v) was NOT disarmed: ImageFetchers=%v. "+
				"The write side must be exhaustive -- this package cannot know which of two "+
				"matching entries the peer honors, so leaving one armed leaves the lever "+
				"half-pulled", name, opt["Marker"], got)
		}
	}
	if seen != 2 {
		t.Errorf("found %d MusicArtist entries in the result, want 2. Merging must not drop "+
			"or collapse a duplicate: %+v", seen, out)
	}
	// The unrelated entry is still untouched.
	mv, ok := FindTypeOptionRaw(out, "MusicVideo")
	if !ok {
		t.Fatal("the operator's MusicVideo entry was dropped")
	}
	if got := StringsFromRaw(mv["ImageFetchers"]); !reflect.DeepEqual(got, []string{"TheMovieDb"}) {
		t.Errorf("MusicVideo ImageFetchers = %v, want [TheMovieDb]", got)
	}
}

// TestFindTypeOptionRaw_MatchesCaseInsensitively guards the read-side half of
// the casing claim in TypeMusicArtist's doc comment. The write side already
// had a case-insensitivity test; this side did not, and a mutation replacing
// strings.EqualFold with == passed the entire suite across all three
// connection packages because every fixture used canonical casing.
//
// This matters beyond tidiness: a case-sensitive read against a peer sending
// "musicartist" reports the entry ABSENT, and absent is interpreted as
// "no configuration, so defaults apply". The caller would take the
// defaults-apply branch on a library that is actually configured.
func TestFindTypeOptionRaw_MatchesCaseInsensitively(t *testing.T) {
	for _, casing := range []string{"musicartist", "MUSICARTIST", "MusicArtist", "mUsIcArTiSt"} {
		t.Run(casing, func(t *testing.T) {
			in := []any{
				map[string]any{"Type": casing, "ImageFetchers": []any{"FanArt"}},
			}
			opt, ok := FindTypeOptionRaw(in, TypeMusicArtist)
			if !ok {
				t.Fatalf("entry with Type=%q reported ABSENT. Absent is interpreted as "+
					"'nothing configured, defaults apply', so a case-sensitive match makes "+
					"the caller take the wrong branch on a configured library", casing)
			}
			if got := StringsFromRaw(opt["ImageFetchers"]); !reflect.DeepEqual(got, []string{"FanArt"}) {
				t.Errorf("matched the wrong entry: ImageFetchers = %v", got)
			}
		})
	}
}

// TestFindTypeOptionRaw_ReturnsFirstMatch pins the read-side first-match rule
// against the write side's merge-all, so the asymmetry is a recorded decision
// rather than an accident. The two entries carry distinct markers; a change to
// last-match or arbitrary-match fails here loudly.
func TestFindTypeOptionRaw_ReturnsFirstMatch(t *testing.T) {
	in := []any{
		map[string]any{"Type": TypeMusicArtist, "Marker": "first"},
		map[string]any{"Type": TypeMusicArtist, "Marker": "second"},
	}
	opt, ok := FindTypeOptionRaw(in, TypeMusicArtist)
	if !ok {
		t.Fatal("no entry found")
	}
	if opt["Marker"] != "first" {
		t.Errorf("Marker = %v, want \"first\". FindTypeOptionRaw returns the FIRST match, "+
			"matching what both platforms' GetLibrarySettings do; changing it would silently "+
			"change which entry every read-side caller trusts", opt["Marker"])
	}
}

// TestMergeTypeOptionRaw_ResultAliasesNestedValues documents the shallow-clone
// behavior as a TEST rather than only a comment, so the limit is executable.
// This asserts the CURRENT contract: the function does not mutate its input,
// but the result shares nested structure with it. If someone later makes the
// clone deep, this test fails and forces the doc comment to be updated with
// it, rather than the two drifting apart.
func TestMergeTypeOptionRaw_ResultAliasesNestedValues(t *testing.T) {
	nested := map[string]any{"MinWidth": float64(400)}
	in := []any{
		map[string]any{"Type": TypeMusicArtist, "ImageOptions": nested},
	}

	before, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	out := MergeTypeOptionRaw(in, TypeMusicArtist, map[string]any{"ImageFetchers": []string{}})

	// The function itself did not mutate the input.
	after, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("MergeTypeOptionRaw mutated its input:\nbefore %s\n after %s", before, after)
	}

	// But the result's nested map IS the input's nested map. Documenting the
	// aliasing here means a caller who mutates the result knows to copy first.
	gotOpt, ok := FindTypeOptionRaw(out, TypeMusicArtist)
	if !ok {
		t.Fatal("merged entry not found")
	}
	gotNested, ok := gotOpt["ImageOptions"].(map[string]any)
	if !ok {
		t.Fatalf("ImageOptions missing from the merged entry: %#v", gotOpt["ImageOptions"])
	}
	gotNested["MinWidth"] = float64(999)
	if nested["MinWidth"] != float64(999) {
		t.Errorf("the clone is now DEEP (input nested value unchanged at %v). That is a fine "+
			"change to make, but MergeTypeOptionRaw's doc comment states the clone is shallow "+
			"and that the result aliases -- update the comment with the behavior",
			nested["MinWidth"])
	}
}
