package provider

import "testing"

// TestSpotifyCapabilitiesExcludeGenres locks the drop of Spotify's genres
// capability: the Spotify API returns empty genres, so Spotify must not be
// advertised as a genres source anywhere providers are declared.
func TestSpotifyCapabilitiesExcludeGenres(t *testing.T) {
	caps, ok := ProviderCapabilities()[NameSpotify]
	if !ok {
		t.Fatalf("ProviderCapabilities has no entry for %q", NameSpotify)
	}

	found := false
	for _, f := range caps.SupportedFields {
		if f == "genres" {
			found = true
		}
	}
	if found {
		t.Errorf("ProviderCapabilities()[NameSpotify].SupportedFields contains %q, want it absent: %v", "genres", caps.SupportedFields)
	}

	found = false
	for _, f := range caps.SupportedFields {
		if f == "name" {
			found = true
		}
	}
	if !found {
		t.Errorf("ProviderCapabilities()[NameSpotify].SupportedFields = %v, want it to contain %q", caps.SupportedFields, "name")
	}

	for _, fp := range DefaultPriorities() {
		if fp.Field != "genres" {
			continue
		}
		if fp.Contains(NameSpotify) {
			t.Errorf("DefaultPriorities() %q row contains NameSpotify, want it absent: %v", fp.Field, fp.Providers)
		}
	}

	thumbFound := false
	for _, fp := range DefaultPriorities() {
		if fp.Field != "thumb" {
			continue
		}
		thumbFound = true
		if !fp.Contains(NameSpotify) {
			t.Errorf("DefaultPriorities() %q row = %v, want it to still contain NameSpotify", fp.Field, fp.Providers)
		}
	}
	if !thumbFound {
		t.Fatalf("DefaultPriorities() has no %q row", "thumb")
	}
}
