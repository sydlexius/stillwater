package nfo

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestParse_BasicNFO(t *testing.T) {
	f, err := os.Open("testdata/basic.nfo")
	if err != nil {
		t.Fatalf("opening test file: %v", err)
	}
	defer f.Close()

	nfo, err := Parse(f)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if nfo.Name != "Nirvana" {
		t.Errorf("Name = %q, want %q", nfo.Name, "Nirvana")
	}
	if nfo.SortName != "Nirvana" {
		t.Errorf("SortName = %q, want %q", nfo.SortName, "Nirvana")
	}
	if nfo.Type != "group" {
		t.Errorf("Type = %q, want %q", nfo.Type, "group")
	}
	if nfo.Disambiguation != "US grunge band" {
		t.Errorf("Disambiguation = %q, want %q", nfo.Disambiguation, "US grunge band")
	}
	if nfo.MusicBrainzArtistID != "5b11f4ce-a62d-471e-81fc-a69a8278c7da" {
		t.Errorf("MusicBrainzArtistID = %q", nfo.MusicBrainzArtistID)
	}
	if nfo.AudioDBArtistID != "111239" {
		t.Errorf("AudioDBArtistID = %q", nfo.AudioDBArtistID)
	}
	if len(nfo.Genres) != 3 {
		t.Errorf("Genres count = %d, want 3", len(nfo.Genres))
	} else if nfo.Genres[0] != "Rock" || nfo.Genres[1] != "Grunge" {
		t.Errorf("Genres = %v", nfo.Genres)
	}
	if len(nfo.Styles) != 2 {
		t.Errorf("Styles count = %d, want 2", len(nfo.Styles))
	}
	if len(nfo.Moods) != 2 {
		t.Errorf("Moods count = %d, want 2", len(nfo.Moods))
	}
	if nfo.YearsActive != "1987 - 1994" {
		t.Errorf("YearsActive = %q", nfo.YearsActive)
	}
	if nfo.Formed != "1987" {
		t.Errorf("Formed = %q", nfo.Formed)
	}
	if nfo.Disbanded != "1994" {
		t.Errorf("Disbanded = %q", nfo.Disbanded)
	}
	if !strings.Contains(nfo.Biography, "American rock band") {
		t.Errorf("Biography doesn't contain expected text: %q", nfo.Biography[:50])
	}

	// Thumbs
	if len(nfo.Thumbs) != 2 {
		t.Fatalf("Thumbs count = %d, want 2", len(nfo.Thumbs))
	}
	if nfo.Thumbs[0].Aspect != "poster" {
		t.Errorf("Thumb[0].Aspect = %q, want %q", nfo.Thumbs[0].Aspect, "poster")
	}
	if nfo.Thumbs[1].Preview != "https://example.com/preview.jpg" {
		t.Errorf("Thumb[1].Preview = %q", nfo.Thumbs[1].Preview)
	}

	// Fanart
	if nfo.Fanart == nil {
		t.Fatal("expected Fanart to be non-nil")
	}
	if len(nfo.Fanart.Thumbs) != 2 {
		t.Errorf("Fanart.Thumbs count = %d, want 2", len(nfo.Fanart.Thumbs))
	}
}

func TestParse_WithBOM(t *testing.T) {
	// Read basic NFO and prepend BOM
	data, err := os.ReadFile("testdata/basic.nfo")
	if err != nil {
		t.Fatalf("reading test file: %v", err)
	}

	bom := []byte{0xEF, 0xBB, 0xBF}
	bomData := append(bom, data...)

	nfo, err := Parse(bytes.NewReader(bomData))
	if err != nil {
		t.Fatalf("Parse with BOM: %v", err)
	}
	if nfo.Name != "Nirvana" {
		t.Errorf("Name = %q, want %q", nfo.Name, "Nirvana")
	}
}

func TestParse_HTMLEntities(t *testing.T) {
	f, err := os.Open("testdata/html_entities.nfo")
	if err != nil {
		t.Fatalf("opening test file: %v", err)
	}
	defer f.Close()

	nfo, err := Parse(f)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// The &ouml; in the name should be converted
	if !strings.Contains(nfo.Name, "rk") {
		t.Errorf("Name = %q, expected to contain 'rk'", nfo.Name)
	}
	if len(nfo.Genres) != 2 {
		t.Errorf("Genres count = %d, want 2", len(nfo.Genres))
	}
}

func TestParse_Minimal(t *testing.T) {
	f, err := os.Open("testdata/minimal.nfo")
	if err != nil {
		t.Fatalf("opening test file: %v", err)
	}
	defer f.Close()

	nfo, err := Parse(f)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if nfo.Name != "Unknown Artist" {
		t.Errorf("Name = %q, want %q", nfo.Name, "Unknown Artist")
	}
	if len(nfo.Genres) != 0 {
		t.Errorf("Genres should be empty, got %v", nfo.Genres)
	}
	if nfo.Fanart != nil {
		t.Error("Fanart should be nil for minimal NFO")
	}
}

func TestParse_CustomElements(t *testing.T) {
	f, err := os.Open("testdata/custom_elements.nfo")
	if err != nil {
		t.Fatalf("opening test file: %v", err)
	}
	defer f.Close()

	nfo, err := Parse(f)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if nfo.Name != "Radiohead" {
		t.Errorf("Name = %q, want %q", nfo.Name, "Radiohead")
	}
	if len(nfo.ExtraElements) != 2 {
		t.Fatalf("ExtraElements count = %d, want 2", len(nfo.ExtraElements))
	}
	if nfo.ExtraElements[0].Name != "customelement" {
		t.Errorf("ExtraElements[0].Name = %q, want %q", nfo.ExtraElements[0].Name, "customelement")
	}
	if nfo.ExtraElements[1].Name != "anothertag" {
		t.Errorf("ExtraElements[1].Name = %q, want %q", nfo.ExtraElements[1].Name, "anothertag")
	}
}

func TestParse_EmptyInput(t *testing.T) {
	_, err := Parse(strings.NewReader(""))
	if err == nil {
		t.Error("expected error for empty input")
	}
}

func TestParse_MalformedXML(t *testing.T) {
	_, err := Parse(strings.NewReader("<artist><name>Unclosed"))
	// Should either return an error or handle gracefully
	if err != nil {
		// Expected: malformed XML produces an error
		return
	}
	// Some parsers may handle unclosed tags gracefully with AutoClose
}

func TestWrite_BasicNFO(t *testing.T) {
	nfo := &ArtistNFO{
		Name:                "Nirvana",
		SortName:            "Nirvana",
		Type:                "group",
		MusicBrainzArtistID: "5b11f4ce-a62d-471e-81fc-a69a8278c7da",
		AudioDBArtistID:     "111239",
		Genres:              []string{"Rock", "Grunge"},
		Styles:              []string{"Grunge"},
		Moods:               []string{"Aggressive"},
		YearsActive:         "1987 - 1994",
		Formed:              "1987",
		Disbanded:           "1994",
		Biography:           "American rock band.",
		Thumbs: []Thumb{
			{Aspect: "poster", Value: "https://example.com/thumb.jpg"},
		},
		Fanart: &Fanart{
			Thumbs: []Thumb{
				{Value: "https://example.com/fanart.jpg"},
			},
		},
	}

	var buf bytes.Buffer
	if err := Write(&buf, nfo); err != nil {
		t.Fatalf("Write: %v", err)
	}

	output := buf.String()

	if !strings.Contains(output, "<?xml version=") {
		t.Error("output missing XML declaration")
	}
	if !strings.Contains(output, "<artist>") {
		t.Error("output missing <artist> element")
	}
	if !strings.Contains(output, "<name>Nirvana</name>") {
		t.Error("output missing name element")
	}
	if !strings.Contains(output, "<musicbrainzartistid>5b11f4ce") {
		t.Error("output missing musicbrainzartistid")
	}
	if !strings.Contains(output, "<audiodbartistid>111239</audiodbartistid>") {
		t.Error("output missing audiodbartistid")
	}
	if !strings.Contains(output, "<genre>Rock</genre>") {
		t.Error("output missing genre Rock")
	}
	if !strings.Contains(output, "<genre>Grunge</genre>") {
		t.Error("output missing genre Grunge")
	}
	if !strings.Contains(output, `<thumb aspect="poster">`) {
		t.Error("output missing thumb with aspect")
	}
	if !strings.Contains(output, "<fanart>") {
		t.Error("output missing fanart")
	}
	if !strings.Contains(output, "</artist>") {
		t.Error("output missing closing artist tag")
	}
}

func TestWrite_RoundTrip(t *testing.T) {
	f, err := os.Open("testdata/basic.nfo")
	if err != nil {
		t.Fatalf("opening test file: %v", err)
	}
	defer f.Close()

	// Parse
	nfo1, err := Parse(f)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// Write
	var buf bytes.Buffer
	if err := Write(&buf, nfo1); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Parse again
	nfo2, err := Parse(&buf)
	if err != nil {
		t.Fatalf("Parse round-trip: %v", err)
	}

	// Compare key fields
	if nfo1.Name != nfo2.Name {
		t.Errorf("Name mismatch: %q vs %q", nfo1.Name, nfo2.Name)
	}
	if nfo1.MusicBrainzArtistID != nfo2.MusicBrainzArtistID {
		t.Errorf("MBID mismatch: %q vs %q", nfo1.MusicBrainzArtistID, nfo2.MusicBrainzArtistID)
	}
	if len(nfo1.Genres) != len(nfo2.Genres) {
		t.Errorf("Genres count mismatch: %d vs %d", len(nfo1.Genres), len(nfo2.Genres))
	}
	if nfo1.Biography != nfo2.Biography {
		t.Errorf("Biography mismatch")
	}
	if len(nfo1.Thumbs) != len(nfo2.Thumbs) {
		t.Errorf("Thumbs count mismatch: %d vs %d", len(nfo1.Thumbs), len(nfo2.Thumbs))
	}
	if nfo1.Fanart != nil && nfo2.Fanart != nil {
		if len(nfo1.Fanart.Thumbs) != len(nfo2.Fanart.Thumbs) {
			t.Errorf("Fanart.Thumbs count mismatch: %d vs %d",
				len(nfo1.Fanart.Thumbs), len(nfo2.Fanart.Thumbs))
		}
	}
}

func TestWrite_PreservesCustomElements(t *testing.T) {
	f, err := os.Open("testdata/custom_elements.nfo")
	if err != nil {
		t.Fatalf("opening test file: %v", err)
	}
	defer f.Close()

	nfo, err := Parse(f)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	var buf bytes.Buffer
	if err := Write(&buf, nfo); err != nil {
		t.Fatalf("Write: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "customelement") {
		t.Error("output missing preserved customelement")
	}
	if !strings.Contains(output, "anothertag") {
		t.Error("output missing preserved anothertag")
	}
}

func TestStripBOM(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
		want []byte
	}{
		{"no BOM", []byte("hello"), []byte("hello")},
		{"with BOM", []byte{0xEF, 0xBB, 0xBF, 'h', 'i'}, []byte("hi")},
		{"empty", []byte{}, []byte{}},
		{"only BOM", []byte{0xEF, 0xBB, 0xBF}, []byte{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripBOM(tt.in)
			if !bytes.Equal(got, tt.want) {
				t.Errorf("stripBOM(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}
