package nfo

import (
	"bytes"
	"strings"
	"testing"
)

const discographyNFO = `<?xml version="1.0" encoding="UTF-8"?>
<artist>
  <name>Nirvana</name>
  <type>group</type>
  <album>
    <title>Bleach</title>
    <year>1989</year>
  </album>
  <album>
    <title>Nevermind</title>
    <year>1991</year>
    <musicbrainzreleasegroupid>1b022e01-4da6-387b-8658-8678046e4cef</musicbrainzreleasegroupid>
  </album>
  <album>
    <title>In Utero</title>
    <year>1993</year>
  </album>
</artist>
`

// TestParseDiscography verifies <album> child elements parse into the Albums slice.
func TestParseDiscography(t *testing.T) {
	n, err := Parse(strings.NewReader(discographyNFO))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(n.Albums) != 3 {
		t.Fatalf("Albums count = %d, want 3", len(n.Albums))
	}
	if n.Albums[0].Title != "Bleach" || n.Albums[0].Year != "1989" {
		t.Errorf("Albums[0] = %+v", n.Albums[0])
	}
	if n.Albums[1].MusicBrainzReleaseGroupID != "1b022e01-4da6-387b-8658-8678046e4cef" {
		t.Errorf("Albums[1].MBID = %q", n.Albums[1].MusicBrainzReleaseGroupID)
	}
	if n.Albums[2].Title != "In Utero" || n.Albums[2].Year != "1993" {
		t.Errorf("Albums[2] = %+v", n.Albums[2])
	}
}

// TestWriteDiscography verifies Albums are emitted in order with nested children.
func TestWriteDiscography(t *testing.T) {
	n := &ArtistNFO{
		Name: "Test",
		Type: "group",
		Albums: []DiscographyAlbum{
			{Title: "First", Year: "2000"},
			{Title: "Second", Year: "2005", MusicBrainzReleaseGroupID: "abc-123"},
		},
	}
	var buf bytes.Buffer
	if err := Write(&buf, n); err != nil {
		t.Fatalf("Write: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "<album>") || !strings.Contains(out, "<title>First</title>") {
		t.Errorf("missing first album:\n%s", out)
	}
	if !strings.Contains(out, "<musicbrainzreleasegroupid>abc-123</musicbrainzreleasegroupid>") {
		t.Errorf("missing mbid:\n%s", out)
	}
	// Ensure ordering is preserved (First comes before Second in output).
	firstIdx := strings.Index(out, "First")
	secondIdx := strings.Index(out, "Second")
	if firstIdx < 0 || secondIdx < 0 || firstIdx > secondIdx {
		t.Errorf("album ordering not preserved:\n%s", out)
	}
}

// TestDiscographyRoundTrip verifies parse -> write -> parse preserves all fields.
func TestDiscographyRoundTrip(t *testing.T) {
	n1, err := Parse(strings.NewReader(discographyNFO))
	if err != nil {
		t.Fatalf("Parse1: %v", err)
	}
	var buf bytes.Buffer
	if err := Write(&buf, n1); err != nil {
		t.Fatalf("Write: %v", err)
	}
	n2, err := Parse(&buf)
	if err != nil {
		t.Fatalf("Parse2: %v", err)
	}
	if len(n1.Albums) != len(n2.Albums) {
		t.Fatalf("album count changed: %d vs %d", len(n1.Albums), len(n2.Albums))
	}
	for i := range n1.Albums {
		if n1.Albums[i] != n2.Albums[i] {
			t.Errorf("album %d differs: %+v vs %+v", i, n1.Albums[i], n2.Albums[i])
		}
	}
}

// TestWriteAlbum_SkipsEmpty ensures fully-empty album entries are not emitted.
func TestWriteAlbum_SkipsEmpty(t *testing.T) {
	n := &ArtistNFO{
		Name: "Test",
		Albums: []DiscographyAlbum{
			{}, // all empty
			{Title: "Kept"},
		},
	}
	var buf bytes.Buffer
	if err := Write(&buf, n); err != nil {
		t.Fatalf("Write: %v", err)
	}
	out := buf.String()
	if strings.Count(out, "<album>") != 1 {
		t.Errorf("expected exactly one <album>, got output:\n%s", out)
	}
	if !strings.Contains(out, "<title>Kept</title>") {
		t.Errorf("kept album missing:\n%s", out)
	}
}
