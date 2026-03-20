package provider

import "testing"

func TestIsJunkBiography(t *testing.T) {
	tests := []struct {
		name string
		bio  string
		want bool
	}{
		// Exact placeholder patterns
		{name: "question mark", bio: "?", want: true},
		{name: "double question mark", bio: "??", want: true},
		{name: "triple question mark", bio: "???", want: true},
		{name: "N/A", bio: "N/A", want: true},
		{name: "n/a", bio: "n/a", want: true},
		{name: "Unknown", bio: "Unknown", want: true},
		{name: "TBD", bio: "TBD", want: true},
		{name: "dash", bio: "-", want: true},
		{name: "double dash", bio: "--", want: true},
		{name: "dots", bio: "...", want: true},
		{name: "None", bio: "None", want: true},
		{name: "no description available", bio: "No description available", want: true},
		{name: "no biography available", bio: "No biography available.", want: true},

		// Case-insensitive matching (EqualFold)
		{name: "UNKNOWN uppercase", bio: "UNKNOWN", want: true},
		{name: "unknown mixed case", bio: "uNkNoWn", want: true},
		{name: "NONE uppercase", bio: "NONE", want: true},
		{name: "tbd lowercase", bio: "tbd", want: true},

		// Whitespace-padded junk
		{name: "padded question mark", bio: "  ?  ", want: true},
		{name: "padded N/A", bio: " N/A ", want: true},

		// Empty/whitespace
		{name: "empty string", bio: "", want: true},
		{name: "whitespace only", bio: "   ", want: true},

		// Too short (under 50 bytes) but not an exact pattern match
		{name: "short text", bio: "A rock band from California.", want: true},
		{name: "very short", bio: "Rock band", want: true},
		{name: "49 chars", bio: "This is a biography that is exactly forty-nine.xx", want: true},

		// Valid biographies (>= 50 bytes)
		{name: "valid biography", bio: "Noise Ratchet was an American rock band from Orange County, California.", want: false},
		{name: "exactly 50 chars", bio: "01234567890123456789012345678901234567890123456789", want: false},
		{name: "long biography", bio: "Radiohead are an English rock band formed in Abingdon, Oxfordshire, in 1985. The band consists of Thom Yorke, brothers Jonny and Colin Greenwood, Ed O'Brien, and Philip Selway.", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsJunkBiography(tt.bio)
			if got != tt.want {
				t.Errorf("IsJunkBiography(%q) = %v, want %v", tt.bio, got, tt.want)
			}
		})
	}
}

func TestIsJunkValue(t *testing.T) {
	tests := []struct {
		name  string
		field string
		value string
		want  bool
	}{
		// Biography uses the same logic as IsJunkBiography
		{name: "bio/junk pattern", field: "biography", value: "?", want: true},
		{name: "bio/too short", field: "biography", value: "Short bio", want: true},
		{name: "bio/valid", field: "biography", value: "A full biography that is definitely longer than fifty bytes of text.", want: false},

		// Generic text fields use defaultMinFieldLength=2
		{name: "other/empty", field: "years_active", value: "", want: true},
		{name: "other/single char", field: "born", value: "x", want: true},
		{name: "other/junk pattern", field: "formed", value: "N/A", want: true},
		{name: "other/valid", field: "born", value: "1985", want: false},

		// List fields (genres, styles, moods) have no minimum length
		{name: "genre/single word", field: "genres", value: "Rock", want: false},
		{name: "genre/junk pattern", field: "genres", value: "?", want: true},
		{name: "style/short valid", field: "styles", value: "Lo-Fi", want: false},
		{name: "mood/short valid", field: "moods", value: "Happy", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsJunkValue(tt.field, tt.value)
			if got != tt.want {
				t.Errorf("IsJunkValue(%q, %q) = %v, want %v", tt.field, tt.value, got, tt.want)
			}
		})
	}
}
