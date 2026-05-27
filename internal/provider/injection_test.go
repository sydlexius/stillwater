package provider

import (
	"testing"
)

func TestParseInjectedSet_Unset(t *testing.T) {
	m := parseInjectedSet("")
	if len(m) != 0 {
		t.Errorf("expected empty set for empty input, got %v", m)
	}
}

func TestParseInjectedSet_Single(t *testing.T) {
	m := parseInjectedSet("musicbrainz")
	if _, ok := m["musicbrainz"]; !ok {
		t.Error("expected musicbrainz in set")
	}
	if len(m) != 1 {
		t.Errorf("expected 1 entry, got %d", len(m))
	}
}

func TestParseInjectedSet_CommaSeparated(t *testing.T) {
	m := parseInjectedSet("musicbrainz,fanarttv,discogs")
	for _, name := range []string{"musicbrainz", "fanarttv", "discogs"} {
		if _, ok := m[name]; !ok {
			t.Errorf("expected %q in set", name)
		}
	}
	if len(m) != 3 {
		t.Errorf("expected 3 entries, got %d", len(m))
	}
}

func TestParseInjectedSet_Whitespace(t *testing.T) {
	m := parseInjectedSet(" musicbrainz , fanarttv ")
	for _, name := range []string{"musicbrainz", "fanarttv"} {
		if _, ok := m[name]; !ok {
			t.Errorf("expected %q in set after whitespace trim", name)
		}
	}
}

func TestParseInjectedSet_MixedCase(t *testing.T) {
	m := parseInjectedSet("MusicBrainz,FanartTV")
	for _, name := range []string{"musicbrainz", "fanarttv"} {
		if _, ok := m[name]; !ok {
			t.Errorf("expected %q (lowercased) in set", name)
		}
	}
}

func TestParseInjectedSet_EmptySegments(t *testing.T) {
	// Leading/trailing commas and consecutive commas must not produce empty keys.
	m := parseInjectedSet(",musicbrainz,,fanarttv,")
	if _, ok := m[""]; ok {
		t.Error("empty string must not be a valid key in the injected set")
	}
	if len(m) != 2 {
		t.Errorf("expected 2 entries, got %d: %v", len(m), m)
	}
}

func TestShouldInjectFailure_EmptySet(t *testing.T) {
	t.Cleanup(func() { SetInjectedProviders(nil) })

	SetInjectedProviders(nil)
	if ShouldInjectFailure(NameMusicBrainz) {
		t.Error("ShouldInjectFailure must return false when injectedSet is empty")
	}
}

func TestShouldInjectFailure_Match(t *testing.T) {
	t.Cleanup(func() { SetInjectedProviders(nil) })

	SetInjectedProviders([]string{"musicbrainz", "fanarttv"})

	if !ShouldInjectFailure(NameMusicBrainz) {
		t.Error("ShouldInjectFailure should return true for musicbrainz when it is in the set")
	}
	if !ShouldInjectFailure(NameFanartTV) {
		t.Error("ShouldInjectFailure should return true for fanarttv when it is in the set")
	}
	if ShouldInjectFailure(NameLastFM) {
		t.Error("ShouldInjectFailure should return false for lastfm when it is not in the set")
	}
}

func TestShouldInjectFailure_CaseInsensitive(t *testing.T) {
	t.Cleanup(func() { SetInjectedProviders(nil) })

	// Env var value in mixed case maps correctly to the lowercased provider name.
	SetInjectedProviders([]string{"MusicBrainz"})
	if !ShouldInjectFailure(NameMusicBrainz) {
		t.Errorf("ShouldInjectFailure must match case-insensitively; NameMusicBrainz=%q", NameMusicBrainz)
	}
}

func TestShouldInjectFailure_AllProviders(t *testing.T) {
	t.Cleanup(func() { SetInjectedProviders(nil) })

	// Derive the list from AllProviderNames so adding a new provider can't
	// silently drop it from coverage.
	allNames := AllProviderNames()
	all := make([]string, len(allNames))
	for i, name := range allNames {
		all[i] = string(name)
	}
	SetInjectedProviders(all)

	for _, name := range allNames {
		if !ShouldInjectFailure(name) {
			t.Errorf("ShouldInjectFailure should return true for %q when all providers are injected", name)
		}
	}
}
