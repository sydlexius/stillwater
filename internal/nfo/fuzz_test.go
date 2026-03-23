package nfo

import (
	"bytes"
	"os"
	"testing"
)

// FuzzParse feeds arbitrary byte slices to the NFO parser to find panics,
// infinite loops, or other unexpected behavior. The parser must never panic
// regardless of input -- returning an error is acceptable.
func FuzzParse(f *testing.F) {
	// Seed with real-world NFO files from testdata
	for _, name := range []string{"basic.nfo", "minimal.nfo", "html_entities.nfo", "custom_elements.nfo"} {
		data, err := os.ReadFile("testdata/" + name)
		if err != nil {
			f.Fatalf("reading seed file %s: %v", name, err)
		}
		f.Add(data)
	}

	// Seed with crafted edge cases
	f.Add([]byte(""))
	f.Add([]byte("<artist></artist>"))
	f.Add([]byte("<artist><name>A</name></artist>"))
	f.Add([]byte("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<artist>\n  <name>Test</name>\n</artist>"))
	f.Add([]byte("<artist><biography>&nbsp;&mdash;&ouml;</biography></artist>"))
	f.Add([]byte("<artist><lockdata>true</lockdata></artist>"))
	f.Add([]byte("<artist><stillwater version=\"1\" written=\"2024-01-01T00:00:00Z\" /></artist>"))
	f.Add([]byte("<artist><fanart><thumb>url</thumb></fanart></artist>"))
	f.Add([]byte("<artist><unknowntag>value</unknowntag></artist>"))

	// UTF-8 BOM prefix
	f.Add(append([]byte{0xEF, 0xBB, 0xBF}, []byte("<artist><name>BOM</name></artist>")...))

	// Deeply nested XML
	f.Add([]byte("<artist><fanart><thumb aspect=\"poster\" preview=\"p\">url</thumb><thumb>url2</thumb></fanart></artist>"))

	f.Fuzz(func(t *testing.T, data []byte) {
		// The parser must not panic. Errors are expected for invalid input.
		_, _ = Parse(bytes.NewReader(data))
	})
}

// FuzzParseWriteRoundTrip verifies that parsing then writing then
// re-parsing produces consistent results without panics.
func FuzzParseWriteRoundTrip(f *testing.F) {
	for _, name := range []string{"basic.nfo", "minimal.nfo", "custom_elements.nfo"} {
		data, err := os.ReadFile("testdata/" + name)
		if err != nil {
			f.Fatalf("reading seed file %s: %v", name, err)
		}
		f.Add(data)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		nfo1, err := Parse(bytes.NewReader(data))
		if err != nil {
			// Invalid input -- skip round-trip but no panic is the goal.
			return
		}

		// Write must not panic
		var buf bytes.Buffer
		if err := Write(&buf, nfo1); err != nil {
			// Write failure on valid parse output is unexpected but not a panic.
			return
		}

		// Re-parse the written output must not panic
		nfo2, err := Parse(bytes.NewReader(buf.Bytes()))
		if err != nil {
			t.Errorf("re-parse of written NFO failed: %v", err)
			return
		}

		// Key fields should survive the round-trip
		if nfo1.Name != nfo2.Name {
			t.Errorf("name mismatch after round-trip: %q vs %q", nfo1.Name, nfo2.Name)
		}
		if nfo1.MusicBrainzArtistID != nfo2.MusicBrainzArtistID {
			t.Errorf("MBID mismatch after round-trip: %q vs %q",
				nfo1.MusicBrainzArtistID, nfo2.MusicBrainzArtistID)
		}
	})
}
