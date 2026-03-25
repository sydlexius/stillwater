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
