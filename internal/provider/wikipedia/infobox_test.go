package wikipedia

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("loading fixture %s: %v", name, err)
	}
	return string(data)
}

func TestParseInfobox_Band(t *testing.T) {
	wikitext := loadFixture(t, "infobox_band.txt")
	data := parseInfobox(wikitext)
	if data == nil {
		t.Fatal("expected non-nil InfoboxData")
	}

	if data.Origin != "Abingdon, Oxfordshire, England" {
		t.Errorf("Origin = %q, want %q", data.Origin, "Abingdon, Oxfordshire, England")
	}

	wantGenres := []string{"Alternative rock", "art rock", "experimental rock", "electronic"}
	if len(data.Genres) != len(wantGenres) {
		t.Fatalf("Genres count = %d, want %d: %v", len(data.Genres), len(wantGenres), data.Genres)
	}
	for i, g := range data.Genres {
		if g != wantGenres[i] {
			t.Errorf("Genres[%d] = %q, want %q", i, g, wantGenres[i])
		}
	}

	wantMembers := []string{"Thom Yorke", "Jonny Greenwood", "Colin Greenwood", "Ed O'Brien", "Philip Selway"}
	if len(data.Members) != len(wantMembers) {
		t.Fatalf("Members count = %d, want %d: %v", len(data.Members), len(wantMembers), data.Members)
	}
	for i, m := range data.Members {
		if m != wantMembers[i] {
			t.Errorf("Members[%d] = %q, want %q", i, m, wantMembers[i])
		}
	}

	if len(data.PastMembers) != 0 {
		t.Errorf("PastMembers = %v, want empty", data.PastMembers)
	}

	// YearsActive should contain "1985" and "present".
	if data.YearsActive == "" {
		t.Error("YearsActive is empty")
	}
}

func TestParseInfobox_Solo(t *testing.T) {
	wikitext := loadFixture(t, "infobox_solo.txt")
	data := parseInfobox(wikitext)
	if data == nil {
		t.Fatal("expected non-nil InfoboxData")
	}

	if data.Origin != "Houston, Texas, U.S." {
		t.Errorf("Origin = %q, want %q", data.Origin, "Houston, Texas, U.S.")
	}

	// hlist genres should be parsed.
	if len(data.Genres) < 3 {
		t.Errorf("expected at least 3 genres, got %d: %v", len(data.Genres), data.Genres)
	}

	if len(data.Members) != 0 {
		t.Errorf("Members = %v, want empty for solo artist", data.Members)
	}
}

func TestParseInfobox_Person(t *testing.T) {
	wikitext := loadFixture(t, "infobox_person.txt")
	data := parseInfobox(wikitext)
	if data == nil {
		t.Fatal("expected non-nil InfoboxData")
	}

	if data.Origin != "Bonn, Electorate of Cologne" {
		t.Errorf("Origin = %q, want %q", data.Origin, "Bonn, Electorate of Cologne")
	}

	// No music-specific fields for Infobox person (unless added).
	if len(data.Genres) != 0 {
		t.Errorf("Genres = %v, want empty for Infobox person", data.Genres)
	}
}

func TestParseInfobox_PastMembers(t *testing.T) {
	wikitext := loadFixture(t, "infobox_with_past_members.txt")
	data := parseInfobox(wikitext)
	if data == nil {
		t.Fatal("expected non-nil InfoboxData")
	}

	wantCurrent := []string{"David Gilmour", "Nick Mason"}
	if len(data.Members) != len(wantCurrent) {
		t.Fatalf("Members count = %d, want %d: %v", len(data.Members), len(wantCurrent), data.Members)
	}
	for i, m := range data.Members {
		if m != wantCurrent[i] {
			t.Errorf("Members[%d] = %q, want %q", i, m, wantCurrent[i])
		}
	}

	wantPast := []string{"Syd Barrett", "Roger Waters", "Richard Wright"}
	if len(data.PastMembers) != len(wantPast) {
		t.Fatalf("PastMembers count = %d, want %d: %v", len(data.PastMembers), len(wantPast), data.PastMembers)
	}
	for i, m := range data.PastMembers {
		if m != wantPast[i] {
			t.Errorf("PastMembers[%d] = %q, want %q", i, m, wantPast[i])
		}
	}
}

func TestParseInfobox_HlistGenres(t *testing.T) {
	wikitext := loadFixture(t, "infobox_hlist_genres.txt")
	data := parseInfobox(wikitext)
	if data == nil {
		t.Fatal("expected non-nil InfoboxData")
	}

	// hlist with pipe-separated items after unwrapping.
	wantGenres := []string{"Electronic", "house", "French house", "synth-pop", "disco"}
	if len(data.Genres) != len(wantGenres) {
		t.Fatalf("Genres count = %d, want %d: %v", len(data.Genres), len(wantGenres), data.Genres)
	}
	for i, g := range data.Genres {
		if g != wantGenres[i] {
			t.Errorf("Genres[%d] = %q, want %q", i, g, wantGenres[i])
		}
	}
}

func TestParseInfobox_BRMembers(t *testing.T) {
	wikitext := loadFixture(t, "infobox_br_members.txt")
	data := parseInfobox(wikitext)
	if data == nil {
		t.Fatal("expected non-nil InfoboxData")
	}

	wantMembers := []string{"Agnetha Faltskog", "Bjorn Ulvaeus", "Benny Andersson", "Anni-Frid Lyngstad"}
	if len(data.Members) != len(wantMembers) {
		t.Fatalf("Members count = %d, want %d: %v", len(data.Members), len(wantMembers), data.Members)
	}
	for i, m := range data.Members {
		if m != wantMembers[i] {
			t.Errorf("Members[%d] = %q, want %q", i, m, wantMembers[i])
		}
	}
}

func TestParseInfobox_NoInfobox(t *testing.T) {
	wikitext := "This article has no infobox template at all. Just plain text about music."
	data := parseInfobox(wikitext)
	if data != nil {
		t.Errorf("expected nil for text without infobox, got %+v", data)
	}
}

func TestParseInfobox_EmptyWikitext(t *testing.T) {
	data := parseInfobox("")
	if data != nil {
		t.Errorf("expected nil for empty wikitext, got %+v", data)
	}
}

func TestParseInfobox_EmptyFields(t *testing.T) {
	wikitext := `{{Infobox musical artist
| name            = Test Artist
| origin          =
| genre           =
| years_active    =
| current_members =
| past_members    =
}}`
	data := parseInfobox(wikitext)
	// All fields are empty, so should return nil.
	if data != nil {
		t.Errorf("expected nil for infobox with all empty fields, got %+v", data)
	}
}

func TestCleanMarkup(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"wikilink with display", "[[Abingdon, Oxfordshire|Abingdon]]", "Abingdon"},
		{"simple wikilink", "[[London]]", "London"},
		{"nested ref", "text<ref>citation</ref>more", "textmore"},
		{"self-closing ref", "text<ref name=foo />more", "textmore"},
		{"html tags", "<b>bold</b> and <i>italic</i>", "bold and italic"},
		{"plain text", "just plain text", "just plain text"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cleanMarkup(tt.input)
			if got != tt.want {
				t.Errorf("cleanMarkup(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestResolveWikilinks(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"display text", "[[Alternative rock|Alt rock]]", "Alt rock"},
		{"no display text", "[[Rock music]]", "Rock music"},
		{"multiple links", "[[London]], [[England]]", "London, England"},
		{"unclosed link", "[[broken", "[[broken"},
		{"no links", "plain text", "plain text"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveWikilinks(tt.input)
			if got != tt.want {
				t.Errorf("resolveWikilinks(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestStripRefs(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"ref block", "text<ref>citation info</ref>more", "textmore"},
		{"self-closing ref", "text<ref name=\"foo\" />more", "textmore"},
		{"multiple refs", "a<ref>x</ref>b<ref>y</ref>c", "abc"},
		{"no refs", "plain text", "plain text"},
		{"br inside block ref", "<ref>foo<br />bar</ref>rest", "rest"},
		{"br inside named block ref", "<ref name=\"x\">foo<br />bar</ref>rest", "rest"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripRefs(tt.input)
			if got != tt.want {
				t.Errorf("stripRefs(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestUnwrapListTemplates(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			"flatlist",
			"{{flatlist|\n* [[Alternative rock]]\n* [[Art rock]]\n}}",
			"\n* [[Alternative rock]]\n* [[Art rock]]\n",
		},
		{
			"hlist",
			"{{hlist|[[Rock]]|[[Pop]]|[[Jazz]]}}",
			"[[Rock]]|[[Pop]]|[[Jazz]]",
		},
		{
			"no template",
			"just text",
			"just text",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := unwrapListTemplates(tt.input)
			if got != tt.want {
				t.Errorf("unwrapListTemplates(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseListField(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{
			"bullet list",
			"\n* [[Thom Yorke]]\n* [[Jonny Greenwood]]\n* [[Colin Greenwood]]",
			[]string{"Thom Yorke", "Jonny Greenwood", "Colin Greenwood"},
		},
		{
			"br separated",
			"[[Agnetha]]<br />[[Bjorn]]<br />[[Benny]]",
			[]string{"Agnetha", "Bjorn", "Benny"},
		},
		{
			"comma separated",
			"[[Rock]], [[Pop]], [[Jazz]]",
			[]string{"Rock", "Pop", "Jazz"},
		},
		{
			"single value",
			"[[Alternative rock]]",
			[]string{"Alternative rock"},
		},
		{
			"hlist with class param",
			"{{hlist|class=nowrap|[[Electronic music|Electronic]]|[[House music|house]]}}",
			[]string{"Electronic", "house"},
		},
		{
			"comma inside wikilink preserved",
			"[[Crosby, Stills, Nash & Young]], [[The Beatles]]",
			[]string{"Crosby, Stills, Nash & Young", "The Beatles"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseListField(tt.input)
			if len(got) != len(tt.want) {
				t.Fatalf("parseListField count = %d, want %d: %v", len(got), len(tt.want), got)
			}
			for i, item := range got {
				if item != tt.want[i] {
					t.Errorf("parseListField[%d] = %q, want %q", i, item, tt.want[i])
				}
			}
		})
	}
}

func TestParseInfobox_Singer(t *testing.T) {
	wikitext := loadFixture(t, "infobox_singer.txt")
	data := parseInfobox(wikitext)
	if data == nil {
		t.Fatal("expected non-nil infoboxData for Infobox singer")
	}
	if data.Origin != "Tottenham, London, England" {
		t.Errorf("Origin = %q, want %q", data.Origin, "Tottenham, London, England")
	}
	if len(data.Genres) < 2 {
		t.Errorf("expected at least 2 genres, got %d: %v", len(data.Genres), data.Genres)
	}
}

func TestParseInfobox_Composer(t *testing.T) {
	wikitext := loadFixture(t, "infobox_composer.txt")
	data := parseInfobox(wikitext)
	if data == nil {
		t.Fatal("expected non-nil infoboxData for Infobox composer")
	}
	if data.Origin != "Eisenach, Saxe-Eisenach" {
		t.Errorf("Origin = %q, want %q", data.Origin, "Eisenach, Saxe-Eisenach")
	}
}

func TestParseInfobox_MalformedBraces(t *testing.T) {
	// Unmatched braces -- findInfoboxBlock should return empty string.
	wikitext := `{{Infobox musical artist
| name = Test
| origin = [[London]]
| genre = {{flatlist|
* [[Rock]]
* [[Pop]]
`
	data := parseInfobox(wikitext)
	if data != nil {
		t.Errorf("expected nil for malformed wikitext with unmatched braces, got %+v", data)
	}
}

func TestStripSimpleTemplates_PartialMatch(t *testing.T) {
	// "smallcaps" should not be matched by the "small" template stripper.
	got := stripSimpleTemplates("{{smallcaps|text}}")
	// The unknown-template fallback strips it, keeping the last pipe segment.
	if got != "text" {
		t.Errorf("stripSimpleTemplates(smallcaps) = %q, want %q", got, "text")
	}
}

func TestUnwrapListTemplates_Nested(t *testing.T) {
	input := "{{flatlist|\n* {{hlist|a|b}}\n* c\n}}"
	got := unwrapListTemplates(input)
	// The outer flatlist is unwrapped first, then the inner hlist.
	if !strings.Contains(got, "a") || !strings.Contains(got, "b") || !strings.Contains(got, "c") {
		t.Errorf("nested template unwrap failed: %q", got)
	}
}

func TestCleanYearsActive(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"simple range", "1985-present", "1985-present"},
		{"en-dash", "1985\u2013present", "1985-present"},
		{"with start date template", "{{start date|1985}}-present", "1985-present"},
		{"with ref", "1985-present<ref>source</ref>", "1985-present"},
		{"named entity ndash", "1987&ndash;present", "1987-present"},
		{"numeric entity ndash", "1987&#8211;present", "1987-present"},
		{"amp entity", "rock &amp; roll", "rock & roll"},
		{"realistic wikipedia value", "{{start date|1987}}&ndash;present<ref>source</ref>", "1987-present"},
		{"multiple ranges with entities", "1972&#8211;1982, 2018&#8211;2022", "1972-1982, 2018-2022"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cleanYearsActive(tt.input)
			if got != tt.want {
				t.Errorf("cleanYearsActive(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
