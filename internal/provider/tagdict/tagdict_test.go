package tagdict

import (
	"testing"
)

func TestCanonical_KnownVariants(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"synthpop", "Synth-Pop"},
		{"synth pop", "Synth-Pop"},
		{"Synth-Pop", "Synth-Pop"},
		{"SYNTHPOP", "Synth-Pop"},
		{"lofi", "Lo-Fi"},
		{"lo fi", "Lo-Fi"},
		{"Lo-Fi", "Lo-Fi"},
		{"hip hop", "Hip-Hop"},
		{"hiphop", "Hip-Hop"},
		{"Hip-Hop", "Hip-Hop"},
		{"alt rock", "Alternative Rock"},
		{"alt-rock", "Alternative Rock"},
		{"kpop", "K-Pop"},
		{"k-pop", "K-Pop"},
		{"melancholy", "Melancholic"},
		{"chillout", "Chill"},
		{"neo soul", "Neo-Soul"},
		{"rnb", "R&B"},
		{"r & b", "R&B"},
		{"rhythm and blues", "R&B"},
		{"synth_pop", "Synth-Pop"},
		{"prog rock", "Progressive Rock"},
		{"idm", "IDM"},
		{"intelligent dance music", "IDM"},
	}
	for _, tc := range cases {
		got := Canonical(tc.input)
		if got != tc.want {
			t.Errorf("Canonical(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestCanonical_UnknownTagPassthrough(t *testing.T) {
	// Tags with no canonical entry should be returned unchanged.
	cases := []string{
		"Grunge",
		"Ambient Drone",
		"some-totally-unknown-genre",
		"",
	}
	for _, tc := range cases {
		got := Canonical(tc)
		if got != tc {
			t.Errorf("Canonical(%q) = %q, want original %q", tc, got, tc)
		}
	}
}

func TestMergeAndDeduplicate_Basic(t *testing.T) {
	existing := []string{"Rock", "Jazz"}
	incoming := []string{"Blues", "Jazz"}
	got := MergeAndDeduplicate(existing, incoming)
	// "Jazz" should appear only once; Blues should be appended.
	if len(got) != 3 {
		t.Fatalf("expected 3 tags, got %d: %v", len(got), got)
	}
	if got[0] != "Rock" || got[1] != "Jazz" || got[2] != "Blues" {
		t.Errorf("unexpected order or values: %v", got)
	}
}

func TestMergeAndDeduplicate_CaseInsensitiveDedup(t *testing.T) {
	// "Synth-Pop" from existing and "synthpop" from incoming should deduplicate.
	existing := []string{"Synth-Pop"}
	incoming := []string{"synthpop", "Electronic"}
	got := MergeAndDeduplicate(existing, incoming)
	if len(got) != 2 {
		t.Fatalf("expected 2 tags (dedup), got %d: %v", len(got), got)
	}
	if got[0] != "Synth-Pop" {
		t.Errorf("expected Synth-Pop first, got %q", got[0])
	}
	if got[1] != "Electronic" {
		t.Errorf("expected Electronic second, got %q", got[1])
	}
}

func TestMergeAndDeduplicate_CanonicalizesIncoming(t *testing.T) {
	// Incoming "hip hop" should be stored as "Hip-Hop".
	existing := []string{}
	incoming := []string{"hip hop", "lofi"}
	got := MergeAndDeduplicate(existing, incoming)
	if len(got) != 2 {
		t.Fatalf("expected 2 tags, got %d: %v", len(got), got)
	}
	if got[0] != "Hip-Hop" {
		t.Errorf("expected Hip-Hop, got %q", got[0])
	}
	if got[1] != "Lo-Fi" {
		t.Errorf("expected Lo-Fi, got %q", got[1])
	}
}

func TestMergeAndDeduplicate_PreservesFirstSeenOrder(t *testing.T) {
	// Existing tags should appear before incoming tags; order within each
	// group should be preserved.
	existing := []string{"Folk", "Blues", "Jazz"}
	incoming := []string{"Rock", "Folk", "Country"}
	got := MergeAndDeduplicate(existing, incoming)
	// Expected: Folk, Blues, Jazz, Rock, Country (Folk deduped from incoming)
	want := []string{"Folk", "Blues", "Jazz", "Rock", "Country"}
	if len(got) != len(want) {
		t.Fatalf("expected %d tags, got %d: %v", len(want), len(got), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("position %d: got %q, want %q (full: %v)", i, got[i], w, got)
		}
	}
}

func TestMergeAndDeduplicate_EmptySlices(t *testing.T) {
	// Both nil and empty slices should work without panic.
	got := MergeAndDeduplicate(nil, nil)
	if len(got) != 0 {
		t.Errorf("expected empty result for nil inputs, got %v", got)
	}

	got = MergeAndDeduplicate([]string{}, []string{})
	if len(got) != 0 {
		t.Errorf("expected empty result for empty inputs, got %v", got)
	}

	got = MergeAndDeduplicate(nil, []string{"Rock"})
	if len(got) != 1 || got[0] != "Rock" {
		t.Errorf("expected [Rock] for nil existing, got %v", got)
	}

	got = MergeAndDeduplicate([]string{"Rock"}, nil)
	if len(got) != 1 || got[0] != "Rock" {
		t.Errorf("expected [Rock] for nil incoming, got %v", got)
	}
}

func TestMergeAndDeduplicate_TrimsWhitespace(t *testing.T) {
	// Padded unknown tags must be trimmed before storage; padded known tags
	// must canonicalize correctly and not store the padded form.
	incoming := []string{"  Grunge  ", "  hip hop  ", "\t  Jazz\t  "}
	got := MergeAndDeduplicate(nil, incoming)
	want := []string{"Grunge", "Hip-Hop", "Jazz"}
	if len(got) != len(want) {
		t.Fatalf("expected %d tags, got %d: %v", len(want), len(got), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("position %d: got %q, want %q", i, got[i], w)
		}
	}
}

func TestMergeAndDeduplicate_FiltersEmptyStrings(t *testing.T) {
	// Empty and whitespace-only strings embedded in a mixed slice are dropped.
	incoming := []string{"Rock", "", "  ", "Jazz"}
	got := MergeAndDeduplicate(nil, incoming)
	want := []string{"Rock", "Jazz"}
	if len(got) != len(want) {
		t.Fatalf("expected %d tags, got %d: %v", len(want), len(got), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("position %d: got %q, want %q", i, got[i], w)
		}
	}
}

func TestMergeAndDeduplicate_CrossProviderDedup(t *testing.T) {
	// Simulate two providers returning the same genre under different spellings.
	// Provider A returns "Hip-Hop", Provider B returns "hip hop" and "Rap".
	providerA := []string{"Hip-Hop", "Electronic"}
	providerB := []string{"hip hop", "Rap", "Electronic"}
	got := MergeAndDeduplicate(providerA, providerB)
	// Hip-Hop and Electronic should be deduped; Rap should be added.
	want := []string{"Hip-Hop", "Electronic", "Rap"}
	if len(got) != len(want) {
		t.Fatalf("expected %d tags, got %d: %v", len(want), len(got), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("position %d: got %q, want %q", i, got[i], w)
		}
	}
}

// -- locale-aware tests --

func TestLocalizeTag_English(t *testing.T) {
	// English locale and empty locale both return the canonical EN form.
	cases := []struct {
		locale string
		input  string
		want   string
	}{
		{"en", "hip hop", "Hip-Hop"},
		{"", "lofi", "Lo-Fi"},
		{"en", "Jazz", "Jazz"},
	}
	for _, tc := range cases {
		got := LocalizeTag(tc.locale, tc.input)
		if got != tc.want {
			t.Errorf("LocalizeTag(%q, %q) = %q, want %q", tc.locale, tc.input, got, tc.want)
		}
	}
}

func TestLocalizeTag_Japanese(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"Jazz", "ジャズ"},
		{"hip hop", "ヒップホップ"}, // normalized via Canonical first
		{"Classical", "クラシック"},
		{"Electronic", "エレクトロニック"},
		{"Shoegaze", "シューゲイザー"},
	}
	for _, tc := range cases {
		got := LocalizeTag("ja", tc.input)
		if got != tc.want {
			t.Errorf("LocalizeTag(ja, %q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestLocalizeTag_JapaneseRegionSubtag(t *testing.T) {
	// Region subtag "ja-JP" should hit the same table as "ja".
	got := LocalizeTag("ja-JP", "Jazz")
	if got != "ジャズ" {
		t.Errorf("LocalizeTag(ja-JP, Jazz) = %q, want Japanese form", got)
	}
}

func TestLocalizeTag_French(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"Classical", "Classique"},
		{"Folk", "Folk"},
		{"Hip-Hop", "Hip-hop"},
		{"Soul", "Soul"},
	}
	for _, tc := range cases {
		got := LocalizeTag("fr", tc.input)
		if got != tc.want {
			t.Errorf("LocalizeTag(fr, %q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestLocalizeTag_UnknownLocaleReturnsCanonical(t *testing.T) {
	// A locale without a translation table returns the EN canonical form.
	got := LocalizeTag("de", "Jazz")
	if got != "Jazz" {
		t.Errorf("LocalizeTag(de, Jazz) = %q, want %q", got, "Jazz")
	}
}

func TestLocalizeTag_UnknownTagReturnsAsIs(t *testing.T) {
	// A tag not in the canonical map and not in any translation table is
	// returned unchanged.
	got := LocalizeTag("ja", "SomeTotallyObscureGenre")
	if got != "SomeTotallyObscureGenre" {
		t.Errorf("LocalizeTag(ja, SomeTotallyObscureGenre) = %q, want unchanged", got)
	}
}

func TestMergeAndDeduplicateLocale_EnglishBehavesLikeBase(t *testing.T) {
	// With locale "en", behavior must be identical to MergeAndDeduplicate.
	existing := []string{"Rock", "Jazz"}
	incoming := []string{"Blues", "Jazz"}
	got := MergeAndDeduplicateLocale(existing, incoming, "en")
	want := MergeAndDeduplicate(existing, incoming)
	if len(got) != len(want) {
		t.Fatalf("en locale: got %d tags, want %d: %v vs %v", len(got), len(want), got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("position %d: got %q, want %q", i, got[i], w)
		}
	}
}

func TestMergeAndDeduplicateLocale_CrossLanguageDedup(t *testing.T) {
	// MusicBrainz returns "Jazz" (EN), Wikidata returns "ジャズ" (ja).
	// With locale "ja", both should collapse to one entry. The EN forms arrive
	// first (existing), so the localized forms survive in first-seen position.
	mbTags := []string{"Jazz", "Electronic"}
	wdTags := []string{"ジャズ", "エレクトロニック"}
	got := MergeAndDeduplicateLocale(mbTags, wdTags, "ja")
	want := []string{"ジャズ", "エレクトロニック"}
	if len(got) != len(want) {
		t.Fatalf("expected %d tags after cross-language dedup, got %d: %v", len(want), len(got), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("position %d: got %q, want %q (full: %v)", i, got[i], w, got)
		}
	}
}

func TestMergeAndDeduplicateLocale_PreferredFormSurvives(t *testing.T) {
	// When the localized form arrives before the EN form, the localized form
	// should survive as the deduplicated entry.
	wdFirst := []string{"ジャズ"}   // Wikidata ja form arrives first (existing)
	mbSecond := []string{"Jazz"} // MB EN form arrives second (incoming)
	got := MergeAndDeduplicateLocale(wdFirst, mbSecond, "ja")
	if len(got) != 1 {
		t.Fatalf("expected 1 tag (dedup), got %d: %v", len(got), got)
	}
	if got[0] != "ジャズ" {
		t.Errorf("expected Japanese form %q to survive, got %q", "ジャズ", got[0])
	}
}

func TestMergeAndDeduplicateLocale_UnknownTagNotDropped(t *testing.T) {
	// Tags not in the translation table must still be kept; locale-aware dedup
	// must not silently discard tags it does not recognize.
	existing := []string{"SomeObscureGenre"}
	incoming := []string{"AnotherObscureGenre"}
	got := MergeAndDeduplicateLocale(existing, incoming, "ja")
	if len(got) != 2 {
		t.Fatalf("expected 2 unknown tags kept, got %d: %v", len(got), got)
	}
}

func TestMergeAndDeduplicateLocale_EmptyLocale(t *testing.T) {
	// Empty locale falls back to MergeAndDeduplicate behavior.
	got := MergeAndDeduplicateLocale([]string{"Rock"}, []string{"Rock"}, "")
	if len(got) != 1 {
		t.Fatalf("expected 1 (dedup), got %d: %v", len(got), got)
	}
}

func TestMergeAndDeduplicateLocale_RegionSubtag(t *testing.T) {
	// A region subtag ("ja-JP") must be stripped to its primary subtag so it
	// hits the same "ja" table for cross-language dedup as bare "ja" would.
	got := MergeAndDeduplicateLocale([]string{"Jazz"}, []string{"ジャズ"}, "ja-JP")
	if len(got) != 1 {
		t.Fatalf("expected 1 tag after region-subtag dedup, got %d: %v", len(got), got)
	}
	if got[0] != "ジャズ" {
		t.Errorf("expected localized form %q to survive, got %q", "ジャズ", got[0])
	}
}

func TestMergeAndDeduplicateLocale_UnknownLocale(t *testing.T) {
	// A non-empty locale with no translation table falls back to EN-keyed
	// dedup: same-concept tags still collapse and tags are stored in EN form.
	got := MergeAndDeduplicateLocale([]string{"Jazz"}, []string{"jazz", "Rock"}, "de")
	want := []string{"Jazz", "Rock"}
	if len(got) != len(want) {
		t.Fatalf("expected %d tags, got %d: %v", len(want), len(got), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("position %d: got %q, want %q", i, got[i], w)
		}
	}
}

func TestMergeAndDeduplicateLocale_RockConcept(t *testing.T) {
	// "rock" is the canonical cross-locale example cited throughout this file:
	// a MusicBrainz "rock" tag and a Wikidata "ロック" tag must collapse to one
	// localized entry. This guards the concept's presence in both the canonical
	// map and the locale translation tables.
	got := MergeAndDeduplicateLocale([]string{"rock"}, []string{"ロック"}, "ja")
	if len(got) != 1 {
		t.Fatalf("expected 1 tag after rock/ロック dedup, got %d: %v", len(got), got)
	}
	if got[0] != "ロック" {
		t.Errorf("expected localized form %q, got %q", "ロック", got[0])
	}
}

func TestTranslationTables_KeysAreCanonical(t *testing.T) {
	// Every inner-map key must be a canonical EN form: LocalizeTag and
	// localeConceptKey look up Canonical(tag) against these keys, so a
	// non-canonical key would be dead data that never matches.
	for locale, table := range translations {
		for key := range table {
			if got := Canonical(key); got != key {
				t.Errorf("translations[%q] key %q is not canonical: Canonical(%q) = %q",
					locale, key, key, got)
			}
		}
	}
}

func TestTranslationTables_NoLocalizedFormCollision(t *testing.T) {
	// localeConceptKey reverse-maps an already-localized form to its EN concept
	// by an exact normalized-string match. If two distinct EN concepts shared a
	// localized form, that reverse mapping would be ambiguous and dedup would
	// become map-iteration-order dependent. Assert each locale table is free of
	// such collisions.
	for locale, table := range translations {
		seen := make(map[string]string, len(table))
		for enForm, localizedForm := range table {
			norm := normalizeKey(localizedForm)
			if prev, dup := seen[norm]; dup {
				t.Errorf("translations[%q]: EN concepts %q and %q share localized form %q",
					locale, prev, enForm, localizedForm)
			}
			seen[norm] = enForm
		}
	}
}

func TestLocaleConceptKey_EnglishAndEmpty(t *testing.T) {
	// localeConceptKey documents that English and empty locales key on the
	// normalized canonical EN form. The exported merge path guards these cases
	// before ever calling in, so exercise that documented contract directly.
	enKey := localeConceptKey("en", "hip hop")
	emptyKey := localeConceptKey("", "Hip-Hop")
	if enKey != emptyKey {
		t.Errorf("en and empty locale keys differ: %q vs %q", enKey, emptyKey)
	}
	if enKey == "" {
		t.Error("expected a non-empty concept key for a known tag")
	}
}

func TestMergeAndDeduplicateLocale_FiltersEmptyStrings(t *testing.T) {
	// Empty and whitespace-only tags are dropped under the locale-aware path
	// just as they are in the base merge.
	got := MergeAndDeduplicateLocale([]string{"Jazz", ""}, []string{"  ", "Classical"}, "ja")
	want := []string{"ジャズ", "クラシック"}
	if len(got) != len(want) {
		t.Fatalf("expected %d tags, got %d: %v", len(want), len(got), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("position %d: got %q, want %q", i, got[i], w)
		}
	}
}
