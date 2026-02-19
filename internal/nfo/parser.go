package nfo

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
)

// knownElements lists XML element names handled by the structured ArtistNFO fields.
var knownElements = map[string]bool{
	"name": true, "sortname": true, "type": true, "gender": true,
	"disambiguation": true, "musicbrainzartistid": true, "audiodbartistid": true,
	"genre": true, "style": true, "mood": true, "yearsactive": true,
	"born": true, "formed": true, "died": true, "disbanded": true,
	"biography": true, "thumb": true, "fanart": true,
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

// Parse reads a Kodi-compatible artist.nfo from the reader.
// It handles UTF-8 BOM and HTML entities in biography text.
// Unknown XML elements are preserved for round-trip fidelity.
func Parse(r io.Reader) (*ArtistNFO, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("reading nfo data: %w", err)
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
func parseTokens(decoder *xml.Decoder, nfo *ArtistNFO) error {
	var inArtist bool

	for {
		tok, err := decoder.Token()
		if err == io.EOF {
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

// parseKnownElement handles a recognized XML element.
func parseKnownElement(decoder *xml.Decoder, nfo *ArtistNFO, name string, start xml.StartElement) error {
	switch name {
	case "name":
		return decodeCharData(decoder, &nfo.Name)
	case "sortname":
		return decodeCharData(decoder, &nfo.SortName)
	case "type":
		return decodeCharData(decoder, &nfo.Type)
	case "gender":
		return decodeCharData(decoder, &nfo.Gender)
	case "disambiguation":
		return decodeCharData(decoder, &nfo.Disambiguation)
	case "musicbrainzartistid":
		return decodeCharData(decoder, &nfo.MusicBrainzArtistID)
	case "audiodbartistid":
		return decodeCharData(decoder, &nfo.AudioDBArtistID)
	case "genre":
		var s string
		if err := decodeCharData(decoder, &s); err != nil {
			return err
		}
		if s != "" {
			nfo.Genres = append(nfo.Genres, s)
		}
	case "style":
		var s string
		if err := decodeCharData(decoder, &s); err != nil {
			return err
		}
		if s != "" {
			nfo.Styles = append(nfo.Styles, s)
		}
	case "mood":
		var s string
		if err := decodeCharData(decoder, &s); err != nil {
			return err
		}
		if s != "" {
			nfo.Moods = append(nfo.Moods, s)
		}
	case "yearsactive":
		return decodeCharData(decoder, &nfo.YearsActive)
	case "born":
		return decodeCharData(decoder, &nfo.Born)
	case "formed":
		return decodeCharData(decoder, &nfo.Formed)
	case "died":
		return decodeCharData(decoder, &nfo.Died)
	case "disbanded":
		return decodeCharData(decoder, &nfo.Disbanded)
	case "biography":
		return decodeCharData(decoder, &nfo.Biography)
	case "thumb":
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
	case "fanart":
		fanart := &Fanart{}
		if err := parseFanart(decoder, fanart); err != nil {
			return err
		}
		nfo.Fanart = fanart
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
	writeElement(w, "gender", nfo.Gender)
	writeElement(w, "disambiguation", nfo.Disambiguation)
	writeElement(w, "musicbrainzartistid", nfo.MusicBrainzArtistID)
	writeElement(w, "audiodbartistid", nfo.AudioDBArtistID)

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

	for _, thumb := range nfo.Thumbs {
		writeThumb(w, thumb)
	}

	if nfo.Fanart != nil && len(nfo.Fanart.Thumbs) > 0 {
		fmt.Fprintf(w, "  <fanart>\n") //nolint:errcheck
		for _, thumb := range nfo.Fanart.Thumbs {
			fmt.Fprintf(w, "    ") //nolint:errcheck
			writeThumbInline(w, thumb)
		}
		fmt.Fprintf(w, "  </fanart>\n") //nolint:errcheck
	}

	// Write preserved unknown elements
	for _, extra := range nfo.ExtraElements {
		fmt.Fprintf(w, "  ") //nolint:errcheck
		w.Write(extra.Raw)   //nolint:errcheck
		fmt.Fprintf(w, "\n") //nolint:errcheck
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
	fmt.Fprintf(w, "  <%s>%s</%s>\n", name, buf.String(), name) //nolint:errcheck
}

// writeThumb writes a <thumb> element with optional attributes.
func writeThumb(w io.Writer, t Thumb) {
	fmt.Fprintf(w, "  ") //nolint:errcheck
	writeThumbInline(w, t)
}

// writeThumbInline writes a <thumb> element without leading indent.
func writeThumbInline(w io.Writer, t Thumb) {
	fmt.Fprintf(w, "<thumb") //nolint:errcheck
	if t.Aspect != "" {
		fmt.Fprintf(w, " aspect=%q", t.Aspect) //nolint:errcheck,gosec // G705: thumb data is from parsed NFO, not user input
	}
	if t.Preview != "" {
		fmt.Fprintf(w, " preview=%q", t.Preview) //nolint:errcheck,gosec // G705: thumb data is from parsed NFO, not user input
	}
	var buf bytes.Buffer
	xml.EscapeText(&buf, []byte(t.Value))         //nolint:errcheck
	fmt.Fprintf(w, ">%s</thumb>\n", buf.String()) //nolint:errcheck
}

// stripBOM removes a UTF-8 BOM (EF BB BF) from the beginning of the data.
func stripBOM(data []byte) []byte {
	if len(data) >= 3 && data[0] == 0xEF && data[1] == 0xBB && data[2] == 0xBF {
		return data[3:]
	}
	return data
}
