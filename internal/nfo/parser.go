package nfo

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
)

// knownElements lists XML element names handled by the structured ArtistNFO fields.
var knownElements = map[string]bool{
	"name": true, "sortname": true, "type": true, "gender": true,
	"disambiguation": true, "musicbrainzartistid": true, "audiodbartistid": true,
	"discogsartistid": true, "wikidataid": true,
	"deezerartistid": true, "spotifyartistid": true,
	"genre": true, "style": true, "mood": true, "yearsactive": true,
	"born": true, "formed": true, "died": true, "disbanded": true,
	"biography": true, "thumb": true, "fanart": true, "lockdata": true,
	"stillwater": true, "album": true,
}

// htmlEntityReplacer handles common HTML entities that are not valid XML.
var htmlEntityReplacer = strings.NewReplacer(
	"&nbsp;", "&#160;",
	"&mdash;", "&#8212;",
	"&ndash;", "&#8211;",
	"&laquo;", "&#171;",
	"&raquo;", "&#187;",
	"&ldquo;", "&#8220;",
	"&rdquo;", "&#8221;",
	"&lsquo;", "&#8216;",
	"&rsquo;", "&#8217;",
	"&bull;", "&#8226;",
	"&hellip;", "&#8230;",
	"&copy;", "&#169;",
	"&reg;", "&#174;",
	"&trade;", "&#8482;",
	"&eacute;", "&#233;",
	"&egrave;", "&#232;",
	"&ouml;", "&#246;",
	"&uuml;", "&#252;",
	"&auml;", "&#228;",
)

// maxNFOBytes caps the size of an artist.nfo file read into memory. Kodi NFOs
// are a few KB; this is generous headroom against a malicious or corrupted
// file exhausting memory via io.ReadAll.
const maxNFOBytes = 10 << 20 // 10 MB

// Parse reads a Kodi-compatible artist.nfo from the reader.
// It handles UTF-8 BOM and HTML entities in biography text.
// Unknown XML elements are preserved for round-trip fidelity.
func Parse(r io.Reader) (*ArtistNFO, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxNFOBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading nfo data: %w", err)
	}
	if int64(len(data)) > maxNFOBytes {
		return nil, fmt.Errorf("nfo file too large (max %d bytes)", maxNFOBytes)
	}

	data = stripBOM(data)

	if len(data) == 0 {
		return nil, fmt.Errorf("empty nfo file")
	}

	// Replace HTML entities with XML-safe numeric references
	content := htmlEntityReplacer.Replace(string(data))

	nfo := &ArtistNFO{}

	decoder := xml.NewDecoder(strings.NewReader(content))
	decoder.Strict = false
	decoder.AutoClose = xml.HTMLAutoClose

	if err := parseTokens(decoder, nfo); err != nil {
		return nil, fmt.Errorf("parsing nfo xml: %w", err)
	}

	return nfo, nil
}

// parseTokens walks the XML token stream, populating known fields and
// capturing unknown elements as raw bytes.
//
//nolint:gocognit // xml.Decoder token-stream consumer: <artist> entry guard, per-token type switch over StartElement/EndElement, known-vs-unknown element dispatch, and invalid-XML-name skip path for elements the lenient decoder accepted but a strict re-serializer would reject. The token-loop shape is xml.Decoder's contract.
func parseTokens(decoder *xml.Decoder, nfo *ArtistNFO) error {
	var inArtist bool

	for {
		tok, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}

		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "artist" {
				inArtist = true
				continue
			}
			if !inArtist {
				continue
			}

			name := t.Name.Local
			if knownElements[name] {
				if err := parseKnownElement(decoder, nfo, name, t); err != nil {
					return err
				}
			} else if !isValidXMLName(t.Name) {
				// The lenient decoder accepted an element with an invalid XML
				// name (e.g., a namespace-qualified name whose local part starts
				// with a digit like <A:0>). Re-serializing it would produce
				// output that fails strict XML parsing. Skip the entire subtree.
				if err := decoder.Skip(); err != nil {
					return err
				}
			} else {
				raw, err := captureRawElement(decoder, t)
				if err != nil {
					return err
				}
				nfo.ExtraElements = append(nfo.ExtraElements, RawElement{
					Name: name,
					Raw:  raw,
				})
			}

		case xml.EndElement:
			if t.Name.Local == "artist" {
				inArtist = false
			}
		}
	}

	return nil
}

// stringFieldTarget maps simple string-valued element names to the ArtistNFO
// field they populate. All targets are populated by reading character data only;
// no attributes are involved.
//
// The map is keyed on the XML element local name. Values are accessor functions
// that return a pointer to the target string field given an *ArtistNFO, allowing
// a single decodeCharData call to handle every entry in the table.
var stringFieldTarget = map[string]func(*ArtistNFO) *string{
	"name":                func(n *ArtistNFO) *string { return &n.Name },
	"sortname":            func(n *ArtistNFO) *string { return &n.SortName },
	"type":                func(n *ArtistNFO) *string { return &n.Type },
	"gender":              func(n *ArtistNFO) *string { return &n.Gender },
	"disambiguation":      func(n *ArtistNFO) *string { return &n.Disambiguation },
	"musicbrainzartistid": func(n *ArtistNFO) *string { return &n.MusicBrainzArtistID },
	"audiodbartistid":     func(n *ArtistNFO) *string { return &n.AudioDBArtistID },
	"discogsartistid":     func(n *ArtistNFO) *string { return &n.DiscogsArtistID },
	"wikidataid":          func(n *ArtistNFO) *string { return &n.WikidataID },
	"deezerartistid":      func(n *ArtistNFO) *string { return &n.DeezerArtistID },
	"spotifyartistid":     func(n *ArtistNFO) *string { return &n.SpotifyArtistID },
	"yearsactive":         func(n *ArtistNFO) *string { return &n.YearsActive },
	"born":                func(n *ArtistNFO) *string { return &n.Born },
	"formed":              func(n *ArtistNFO) *string { return &n.Formed },
	"died":                func(n *ArtistNFO) *string { return &n.Died },
	"disbanded":           func(n *ArtistNFO) *string { return &n.Disbanded },
	"biography":           func(n *ArtistNFO) *string { return &n.Biography },
}

// parseKnownElement handles a recognized XML element.
// Simple string fields are dispatched through stringFieldTarget; structured
// elements (thumb, fanart, lockdata, album, stillwater) are handled by
// dedicated parse helpers.
func parseKnownElement(decoder *xml.Decoder, nfo *ArtistNFO, name string, start xml.StartElement) error {
	// Fast path: plain string fields.
	if target := stringFieldTarget[name]; target != nil {
		return decodeCharData(decoder, target(nfo))
	}

	// Slice-appending fields and structured elements.
	switch name {
	case "genre":
		return parseSliceField(decoder, &nfo.Genres)
	case "style":
		return parseSliceField(decoder, &nfo.Styles)
	case "mood":
		return parseSliceField(decoder, &nfo.Moods)
	case "thumb":
		return parseThumb(decoder, nfo, start)
	case "fanart":
		return parseFanartElement(decoder, nfo)
	case "lockdata":
		return parseLockData(decoder, nfo)
	case "album":
		return parseAlbumElement(decoder, nfo)
	case "stillwater":
		return parseStillwater(decoder, nfo, start)
	default:
		return fmt.Errorf("unhandled known element %q: add a case or remove from knownElements", name)
	}
}

// parseSliceField reads a single string element and appends a non-empty value
// to the target slice. It is used for repeating elements such as genre, style,
// and mood.
func parseSliceField(decoder *xml.Decoder, target *[]string) error {
	var s string
	if err := decodeCharData(decoder, &s); err != nil {
		return err
	}
	if s != "" {
		*target = append(*target, s)
	}
	return nil
}

// parseThumb reads a <thumb> element with optional aspect and preview
// attributes and appends the result to nfo.Thumbs.
func parseThumb(decoder *xml.Decoder, nfo *ArtistNFO, start xml.StartElement) error {
	thumb := Thumb{}
	for _, attr := range start.Attr {
		switch attr.Name.Local {
		case "aspect":
			thumb.Aspect = attr.Value
		case "preview":
			thumb.Preview = attr.Value
		}
	}
	if err := decodeCharData(decoder, &thumb.Value); err != nil {
		return err
	}
	nfo.Thumbs = append(nfo.Thumbs, thumb)
	return nil
}

// parseFanartElement reads the <fanart> element and assigns it to nfo.Fanart.
func parseFanartElement(decoder *xml.Decoder, nfo *ArtistNFO) error {
	fanart := &Fanart{}
	if err := parseFanart(decoder, fanart); err != nil {
		return err
	}
	nfo.Fanart = fanart
	return nil
}

// parseLockData reads the <lockdata> element and sets nfo.LockData.
func parseLockData(decoder *xml.Decoder, nfo *ArtistNFO) error {
	var s string
	if err := decodeCharData(decoder, &s); err != nil {
		return err
	}
	nfo.LockData = parseBoolString(s)
	return nil
}

// parseAlbumElement reads a single <album> entry and appends it to nfo.Albums.
// Wholly-empty entries are skipped to avoid polluting round-tripped output.
func parseAlbumElement(decoder *xml.Decoder, nfo *ArtistNFO) error {
	album := DiscographyAlbum{}
	if err := parseAlbum(decoder, &album); err != nil {
		return err
	}
	if album.Title != "" || album.Year != "" || album.MusicBrainzReleaseGroupID != "" {
		nfo.Albums = append(nfo.Albums, album)
	}
	return nil
}

// parseStillwater reads the <stillwater> element attributes and sets
// nfo.Stillwater. The element body (if any) is consumed via decoder.Skip.
func parseStillwater(decoder *xml.Decoder, nfo *ArtistNFO, start xml.StartElement) error {
	nfo.Stillwater = &StillwaterMeta{}
	for _, attr := range start.Attr {
		switch attr.Name.Local {
		case "version":
			nfo.Stillwater.Version = attr.Value
		case "written":
			nfo.Stillwater.Written = attr.Value
		}
	}
	if err := decoder.Skip(); err != nil {
		return fmt.Errorf("skipping stillwater element content: %w", err)
	}
	return nil
}

// parseFanart handles the nested <fanart><thumb>...</thumb></fanart> structure.
func parseFanart(decoder *xml.Decoder, fanart *Fanart) error {
	for {
		tok, err := decoder.Token()
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "thumb" {
				thumb := Thumb{}
				for _, attr := range t.Attr {
					switch attr.Name.Local {
					case "aspect":
						thumb.Aspect = attr.Value
					case "preview":
						thumb.Preview = attr.Value
					}
				}
				if err := decodeCharData(decoder, &thumb.Value); err != nil {
					return err
				}
				fanart.Thumbs = append(fanart.Thumbs, thumb)
			} else {
				// Skip unknown elements inside fanart
				if err := decoder.Skip(); err != nil {
					return err
				}
			}
		case xml.EndElement:
			if t.Name.Local == "fanart" {
				return nil
			}
		}
	}
}

// parseAlbum reads the child elements of a single <album> entry.
// Unknown children are skipped to keep the discography list well-formed.
func parseAlbum(decoder *xml.Decoder, album *DiscographyAlbum) error {
	for {
		tok, err := decoder.Token()
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "title":
				if err := decodeCharData(decoder, &album.Title); err != nil {
					return err
				}
			case "year":
				if err := decodeCharData(decoder, &album.Year); err != nil {
					return err
				}
			case "musicbrainzreleasegroupid":
				if err := decodeCharData(decoder, &album.MusicBrainzReleaseGroupID); err != nil {
					return err
				}
			default:
				if err := decoder.Skip(); err != nil {
					return err
				}
			}
		case xml.EndElement:
			if t.Name.Local == "album" {
				return nil
			}
		}
	}
}

// decodeCharData reads character data until the closing tag.
func decodeCharData(decoder *xml.Decoder, target *string) error {
	var buf strings.Builder
	for {
		tok, err := decoder.Token()
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case xml.CharData:
			buf.Write(t)
		case xml.EndElement:
			*target = strings.TrimSpace(buf.String())
			return nil
		}
	}
}

// captureRawElement reads an unknown element and its children as raw XML bytes.
func captureRawElement(decoder *xml.Decoder, start xml.StartElement) ([]byte, error) {
	var buf bytes.Buffer
	enc := xml.NewEncoder(&buf)

	if err := enc.EncodeToken(start.Copy()); err != nil {
		return nil, err
	}

	depth := 1
	for depth > 0 {
		tok, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		switch tok.(type) {
		case xml.StartElement:
			depth++
		case xml.EndElement:
			depth--
		}
		if err := enc.EncodeToken(xml.CopyToken(tok)); err != nil {
			return nil, err
		}
	}

	if err := enc.Flush(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Write writes an ArtistNFO as XML to the writer.
// It includes an XML declaration and preserves unknown elements.
func Write(w io.Writer, nfo *ArtistNFO) error {
	if _, err := io.WriteString(w, xml.Header); err != nil {
		return err
	}

	if _, err := io.WriteString(w, "<artist>\n"); err != nil {
		return err
	}

	// Write known fields in standard Kodi order
	writeElement(w, "name", nfo.Name)
	writeElement(w, "sortname", nfo.SortName)
	writeElement(w, "type", nfo.Type)
	// Only write gender for individual artist types; group/orchestra/choir
	// have no meaningful gender.
	if isIndividualType(nfo.Type) {
		writeElement(w, "gender", nfo.Gender)
	}
	writeElement(w, "disambiguation", nfo.Disambiguation)
	writeElement(w, "musicbrainzartistid", nfo.MusicBrainzArtistID)
	writeElement(w, "audiodbartistid", nfo.AudioDBArtistID)
	writeElement(w, "discogsartistid", nfo.DiscogsArtistID)
	writeElement(w, "wikidataid", nfo.WikidataID)
	writeElement(w, "deezerartistid", nfo.DeezerArtistID)
	writeElement(w, "spotifyartistid", nfo.SpotifyArtistID)

	for _, g := range nfo.Genres {
		writeElement(w, "genre", g)
	}
	for _, s := range nfo.Styles {
		writeElement(w, "style", s)
	}
	for _, m := range nfo.Moods {
		writeElement(w, "mood", m)
	}

	writeElement(w, "yearsactive", nfo.YearsActive)
	writeElement(w, "born", nfo.Born)
	writeElement(w, "formed", nfo.Formed)
	writeElement(w, "died", nfo.Died)
	writeElement(w, "disbanded", nfo.Disbanded)
	writeElement(w, "biography", nfo.Biography)

	// Write lockdata element to protect NFO from platform overwrites.
	// Only written when true; omitted entirely when false.
	if nfo.LockData {
		fmt.Fprintf(w, "  <lockdata>true</lockdata>\n") //nolint:errcheck // best-effort write to io.Writer; intermediate serializer fragment errors are intentionally ignored
	}

	// Write Stillwater provenance element to identify the NFO writer.
	if nfo.Stillwater != nil {
		fmt.Fprintf(w, "  <stillwater version=%q written=%q />\n", //nolint:errcheck // best-effort write to io.Writer; intermediate serializer fragment errors are intentionally ignored
			nfo.Stillwater.Version, nfo.Stillwater.Written)
	}

	// Write discography <album> entries after metadata but before image
	// references, matching the Kodi NFO ordering convention.
	for _, album := range nfo.Albums {
		writeAlbum(w, album)
	}

	for _, thumb := range nfo.Thumbs {
		writeThumb(w, thumb)
	}

	if nfo.Fanart != nil && len(nfo.Fanart.Thumbs) > 0 {
		fmt.Fprintf(w, "  <fanart>\n") //nolint:errcheck // best-effort write to io.Writer; intermediate serializer fragment errors are intentionally ignored
		for _, thumb := range nfo.Fanart.Thumbs {
			fmt.Fprintf(w, "    ") //nolint:errcheck // best-effort write to io.Writer; intermediate serializer fragment errors are intentionally ignored
			writeThumbInline(w, thumb)
		}
		fmt.Fprintf(w, "  </fanart>\n") //nolint:errcheck // best-effort write to io.Writer; intermediate serializer fragment errors are intentionally ignored
	}

	// Write preserved unknown elements
	for _, extra := range nfo.ExtraElements {
		fmt.Fprintf(w, "  ") //nolint:errcheck // best-effort write to io.Writer; intermediate serializer fragment errors are intentionally ignored
		w.Write(extra.Raw)   //nolint:errcheck // best-effort write to io.Writer; intermediate serializer fragment errors are intentionally ignored
		fmt.Fprintf(w, "\n") //nolint:errcheck // best-effort write to io.Writer; intermediate serializer fragment errors are intentionally ignored
	}

	if _, err := io.WriteString(w, "</artist>\n"); err != nil {
		return err
	}

	return nil
}

// writeElement writes a simple XML element if the value is non-empty.
func writeElement(w io.Writer, name, value string) {
	if value == "" {
		return
	}
	var buf bytes.Buffer
	if err := xml.EscapeText(&buf, []byte(value)); err != nil {
		return
	}
	fmt.Fprintf(w, "  <%s>%s</%s>\n", name, buf.String(), name) //nolint:errcheck // best-effort write to io.Writer; intermediate serializer fragment errors are intentionally ignored
}

// writeAlbum writes a single <album> entry with title, year, and optional
// MusicBrainz release group ID. Empty entries are skipped to keep output tidy.
func writeAlbum(w io.Writer, a DiscographyAlbum) {
	if a.Title == "" && a.Year == "" && a.MusicBrainzReleaseGroupID == "" {
		return
	}
	fmt.Fprintf(w, "  <album>\n") //nolint:errcheck // best-effort write to io.Writer; intermediate serializer fragment errors are intentionally ignored
	writeInnerElement(w, "title", a.Title)
	writeInnerElement(w, "year", a.Year)
	writeInnerElement(w, "musicbrainzreleasegroupid", a.MusicBrainzReleaseGroupID)
	fmt.Fprintf(w, "  </album>\n") //nolint:errcheck // best-effort write to io.Writer; intermediate serializer fragment errors are intentionally ignored
}

// writeInnerElement writes a nested XML element with four-space indent.
// Used for children of structured elements like <album>.
func writeInnerElement(w io.Writer, name, value string) {
	if value == "" {
		return
	}
	var buf bytes.Buffer
	if err := xml.EscapeText(&buf, []byte(value)); err != nil {
		return
	}
	fmt.Fprintf(w, "    <%s>%s</%s>\n", name, buf.String(), name) //nolint:errcheck // best-effort write to io.Writer; intermediate serializer fragment errors are intentionally ignored
}

// writeThumb writes a <thumb> element with optional attributes.
func writeThumb(w io.Writer, t Thumb) {
	fmt.Fprintf(w, "  ") //nolint:errcheck // best-effort write to io.Writer; intermediate serializer fragment errors are intentionally ignored
	writeThumbInline(w, t)
}

// writeThumbInline writes a <thumb> element without leading indent.
func writeThumbInline(w io.Writer, t Thumb) {
	fmt.Fprintf(w, "<thumb") //nolint:errcheck // best-effort write to io.Writer; intermediate serializer fragment errors are intentionally ignored
	if t.Aspect != "" {
		fmt.Fprintf(w, " aspect=%q", t.Aspect) //nolint:errcheck // best-effort write to io.Writer; intermediate serializer fragment errors are intentionally ignored
	}
	if t.Preview != "" {
		fmt.Fprintf(w, " preview=%q", t.Preview) //nolint:errcheck // best-effort write to io.Writer; intermediate serializer fragment errors are intentionally ignored
	}
	var buf bytes.Buffer
	xml.EscapeText(&buf, []byte(t.Value))         //nolint:errcheck // EscapeText to bytes.Buffer; Write to in-memory buffer cannot fail
	fmt.Fprintf(w, ">%s</thumb>\n", buf.String()) //nolint:errcheck // best-effort write to io.Writer; intermediate serializer fragment errors are intentionally ignored
}

// parseBoolString interprets a string as a boolean value.
// Handles the formats used by Kodi, Emby, and Jellyfin:
// "true", "1", "yes" (case-insensitive) are true; everything else is false.
func parseBoolString(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "1", "yes":
		return true
	default:
		return false
	}
}

// isValidXMLName reports whether an xml.Name can be safely re-serialized by
// the standard library's xml.Encoder. The XML spec requires that element names
// begin with a letter or underscore; subsequent characters may also include
// digits, hyphens, and periods. Only the local name is validated here; the
// Space field in encoding/xml holds the namespace URI, not an XML Name.
func isValidXMLName(name xml.Name) bool {
	return isValidXMLNameString(name.Local)
}

// isValidXMLNameString checks whether s is a valid XML Name production.
// It implements a simplified check based on the XML 1.0 NameStartChar and
// NameChar rules, covering the ASCII range plus common Unicode letters and
// digits. This is intentionally conservative: names that might technically be
// valid per the full Unicode-aware XML spec but would trip up Go's encoder are
// still rejected.
func isValidXMLNameString(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if i == 0 {
			// NameStartChar: letter, underscore, or colon (colon is technically
			// allowed in XML names but reserved for namespace use; we accept it
			// here since the encoder handles it).
			if !unicode.IsLetter(r) && r != '_' && r != ':' {
				return false
			}
		} else {
			// NameChar: NameStartChar plus digit, hyphen, period, middle dot,
			// and combining characters.
			if !unicode.IsLetter(r) && !unicode.IsDigit(r) &&
				r != '_' && r != ':' && r != '-' && r != '.' &&
				r != '\u00B7' && !unicode.Is(unicode.Mn, r) {
				return false
			}
		}
	}
	return true
}

// stripBOM removes a UTF-8 BOM (EF BB BF) from the beginning of the data.
func stripBOM(data []byte) []byte {
	if len(data) >= 3 && data[0] == 0xEF && data[1] == 0xBB && data[2] == 0xBF {
		return data[3:]
	}
	return data
}
