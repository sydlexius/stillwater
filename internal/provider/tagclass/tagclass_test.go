package tagclass

import (
	"testing"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		tag  string
		want TagClass
	}{
		// Moods
		{"melancholic", TagClassMood},
		{"Melancholy", TagClassMood},
		{"CHILL", TagClassMood},
		{"energetic", TagClassMood},
		{"atmospheric", TagClassMood},
		{"dreamy", TagClassMood},
		{"dark", TagClassMood},
		{"happy", TagClassMood},
		{"sad", TagClassMood},
		{"peaceful", TagClassMood},
		{"brooding", TagClassMood},
		{"nostalgic", TagClassMood},
		{"euphoric", TagClassMood},
		{"tender", TagClassMood},

		// Styles
		{"shoegaze", TagClassStyle},
		{"Post-Rock", TagClassStyle},
		{"trip-hop", TagClassStyle},
		{"lo-fi", TagClassStyle},
		{"dream pop", TagClassStyle},
		{"post-punk", TagClassStyle},
		{"art rock", TagClassStyle},
		{"grunge", TagClassStyle},
		{"britpop", TagClassStyle},
		{"idm", TagClassStyle},
		{"vaporwave", TagClassStyle},
		{"synthwave", TagClassStyle},
		{"dubstep", TagClassStyle},
		{"ambient", TagClassStyle},
		{"afrobeat", TagClassStyle},
		{"doom metal", TagClassStyle},
		{"black metal", TagClassStyle},

		// Ignore
		{"seen live", TagClassIgnore},
		{"favorites", TagClassIgnore},
		{"Favourite", TagClassIgnore}, //nolint:misspell // British English Last.fm tag
		{"awesome", TagClassIgnore},
		{"beautiful", TagClassIgnore},
		{"my collection", TagClassIgnore},
		{"spotify", TagClassIgnore},

		// Unknown defaults to genre
		{"rock", TagClassGenre},
		{"electronic", TagClassGenre},
		{"alternative", TagClassGenre},
		{"hip-hop", TagClassGenre},
		{"jazz", TagClassGenre},
		{"pop", TagClassGenre},
		{"metal", TagClassGenre},
		{"classical", TagClassGenre},

		// Whitespace trimming
		{"  shoegaze  ", TagClassStyle},
		{"  dark  ", TagClassMood},
		{"  seen live  ", TagClassIgnore},

		// Empty string
		{"", TagClassIgnore},
		{"   ", TagClassIgnore},
	}

	for _, tt := range tests {
		name := tt.tag
		if name == "" {
			name = "<empty>"
		}
		t.Run(name, func(t *testing.T) {
			got := Classify(tt.tag)
			if got != tt.want {
				t.Errorf("Classify(%q) = %d, want %d", tt.tag, got, tt.want)
			}
		})
	}
}

func TestClassifyTags(t *testing.T) {
	tags := []string{
		"rock",        // genre (unknown -> default)
		"electronic",  // genre
		"shoegaze",    // style
		"post-rock",   // style
		"melancholic", // mood
		"dark",        // mood
		"seen live",   // ignore
		"favorites",   // ignore
		"",            // empty -> skipped
	}

	genres, styles, moods := ClassifyTags(tags)

	// Genres: rock, electronic
	if len(genres) != 2 {
		t.Errorf("expected 2 genres, got %d: %v", len(genres), genres)
	}
	if genres[0] != "rock" {
		t.Errorf("expected first genre 'rock', got %q", genres[0])
	}
	if genres[1] != "electronic" {
		t.Errorf("expected second genre 'electronic', got %q", genres[1])
	}

	// Styles: shoegaze, post-rock
	if len(styles) != 2 {
		t.Errorf("expected 2 styles, got %d: %v", len(styles), styles)
	}
	if styles[0] != "shoegaze" {
		t.Errorf("expected first style 'shoegaze', got %q", styles[0])
	}
	if styles[1] != "post-rock" {
		t.Errorf("expected second style 'post-rock', got %q", styles[1])
	}

	// Moods: melancholic, dark
	if len(moods) != 2 {
		t.Errorf("expected 2 moods, got %d: %v", len(moods), moods)
	}
	if moods[0] != "melancholic" {
		t.Errorf("expected first mood 'melancholic', got %q", moods[0])
	}
	if moods[1] != "dark" {
		t.Errorf("expected second mood 'dark', got %q", moods[1])
	}
}

func TestClassifyTags_AllGenres(t *testing.T) {
	// Tags that are all unrecognized should default to genres.
	tags := []string{"rock", "pop", "jazz"}
	genres, styles, moods := ClassifyTags(tags)

	if len(genres) != 3 {
		t.Errorf("expected 3 genres, got %d", len(genres))
	}
	if len(styles) != 0 {
		t.Errorf("expected 0 styles, got %d", len(styles))
	}
	if len(moods) != 0 {
		t.Errorf("expected 0 moods, got %d", len(moods))
	}
}

func TestClassifyTags_Empty(t *testing.T) {
	genres, styles, moods := ClassifyTags(nil)
	if genres != nil || styles != nil || moods != nil {
		t.Errorf("expected all nil for empty input, got genres=%v styles=%v moods=%v",
			genres, styles, moods)
	}
}

func TestClassifyTags_CaseInsensitive(t *testing.T) {
	tags := []string{"SHOEGAZE", "Dark", "SEEN LIVE"}
	genres, styles, moods := ClassifyTags(tags)

	if len(genres) != 0 {
		t.Errorf("expected 0 genres, got %d: %v", len(genres), genres)
	}
	if len(styles) != 1 {
		t.Errorf("expected 1 style, got %d: %v", len(styles), styles)
	}
	if len(moods) != 1 {
		t.Errorf("expected 1 mood, got %d: %v", len(moods), moods)
	}
}

func TestClassifyTags_PreservesOriginalCase(t *testing.T) {
	tags := []string{"SHOEGAZE", "Dark"}
	_, styles, moods := ClassifyTags(tags)

	// The original tag text should be preserved, not lowercased.
	if len(styles) > 0 && styles[0] != "SHOEGAZE" {
		t.Errorf("expected style to preserve case 'SHOEGAZE', got %q", styles[0])
	}
	if len(moods) > 0 && moods[0] != "Dark" {
		t.Errorf("expected mood to preserve case 'Dark', got %q", moods[0])
	}
}
