package image

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ExifMeta holds provenance metadata embedded in image files by Stillwater.
// It records where an image came from, when it was saved, and what triggered
// the save. This metadata is stored in the JPEG ImageDescription EXIF tag
// (0x010E) or the PNG tEXt chunk with key "ImageDescription".
type ExifMeta struct {
	Source  string    // Provider name: "fanarttv", "musicbrainz", "user", etc.
	Fetched time.Time // When Stillwater saved the image.
	URL     string    // Original source URL.
	DHash   string    // Perceptual hash at save time (16-char hex).
	Rule    string    // Rule that triggered the save.
	Mode    string    // Automation mode: "auto", "manual", "user".
}

// exifMetaPrefix is the version prefix for the serialized provenance string.
const exifMetaPrefix = "stillwater:v1"

// Marshal serializes ExifMeta into a pipe-delimited string suitable for
// embedding in image metadata. Only non-zero fields are included. The
// format is: stillwater:v1|source=...|fetched=...|url=...|dhash=...|rule=...|mode=...
func (m *ExifMeta) Marshal() string {
	parts := []string{exifMetaPrefix}

	if m.Source != "" {
		parts = append(parts, "source="+m.Source)
	}
	if !m.Fetched.IsZero() {
		parts = append(parts, "fetched="+m.Fetched.UTC().Format(time.RFC3339))
	}
	if m.URL != "" {
		// Escape pipe characters in URLs so they do not break the
		// pipe-delimited serialization format.
		parts = append(parts, "url="+strings.ReplaceAll(m.URL, "|", "%7C"))
	}
	if m.DHash != "" {
		parts = append(parts, "dhash="+m.DHash)
	}
	if m.Rule != "" {
		parts = append(parts, "rule="+m.Rule)
	}
	if m.Mode != "" {
		parts = append(parts, "mode="+m.Mode)
	}

	return strings.Join(parts, "|")
}

// isZero returns true if all fields of ExifMeta are zero-valued.
func (m *ExifMeta) isZero() bool {
	return m.Source == "" && m.Fetched.IsZero() && m.URL == "" &&
		m.DHash == "" && m.Rule == "" && m.Mode == ""
}

// ParseExifMeta deserializes a provenance string back into an ExifMeta struct.
//
// Behavior:
//   - Empty or non-Stillwater strings return a zero struct and nil error.
//   - A prefix-only string ("stillwater:v1") returns a zero struct and nil error.
//   - A recognized prefix with malformed content returns an error.
func ParseExifMeta(s string) (ExifMeta, error) {
	var m ExifMeta

	if s == "" {
		return m, nil
	}

	// Split on pipe delimiter.
	parts := strings.Split(s, "|")

	// First part must be the version prefix.
	if parts[0] != exifMetaPrefix {
		// Not a Stillwater string; return zero struct, no error.
		return m, nil
	}

	// Prefix-only: no fields to parse.
	if len(parts) == 1 {
		return m, nil
	}

	// Parse each key=value pair after the prefix.
	for _, part := range parts[1:] {
		// Use Cut to split on the first "=" only, so values containing
		// "=" (e.g. URLs with query params) are preserved correctly.
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			return ExifMeta{}, fmt.Errorf("exifmeta: malformed field %q (no '=' separator)", part)
		}

		switch key {
		case "source":
			m.Source = value
		case "fetched":
			t, err := time.Parse(time.RFC3339, value)
			if err != nil {
				return ExifMeta{}, fmt.Errorf("exifmeta: invalid fetched time %q: %w", value, err)
			}
			m.Fetched = t
		case "url":
			// Unescape pipe characters that were encoded during Marshal.
			m.URL = strings.ReplaceAll(value, "%7C", "|")
		case "dhash":
			m.DHash = value
		case "rule":
			m.Rule = value
		case "mode":
			m.Mode = value
		default:
			// Unknown fields are silently ignored for forward compatibility.
		}
	}

	return m, nil
}

// ---------------------------------------------------------------------------
// JPEG EXIF injection and reading
// ---------------------------------------------------------------------------

// JPEG marker bytes.
const (
	jpegMarkerPrefix byte = 0xFF
	jpegSOI          byte = 0xD8 // Start of Image
	jpegAPP1         byte = 0xE1 // EXIF APP1 segment
	jpegSOS          byte = 0xDA // Start of Scan (image data follows)
)

// exifHeader is the 6-byte header at the start of an APP1 payload: "Exif\0\0".
var exifHeader = []byte{'E', 'x', 'i', 'f', 0x00, 0x00}

// tiffHeaderLE is a little-endian TIFF header: byte order "II", magic 42,
// and IFD0 offset of 8 (immediately following the header).
var tiffHeaderLE = []byte{
	'I', 'I', // Little-endian byte order
	0x2A, 0x00, // TIFF magic number 42
	0x08, 0x00, 0x00, 0x00, // Offset to IFD0 (8 bytes from start of TIFF data)
}

// injectJPEGDescription builds a minimal EXIF APP1 segment containing the
// given description in the ImageDescription tag (0x010E), strips any existing
// APP1 segments, and inserts the new APP1 right after the SOI marker.
func injectJPEGDescription(data []byte, description string) ([]byte, error) {
	if len(data) < 2 || data[0] != 0xFF || data[1] != jpegSOI {
		return nil, fmt.Errorf("exif: not a valid JPEG (missing SOI marker)")
	}

	// Build the IFD0 entry for ImageDescription (tag 0x010E, type ASCII=2).
	// IFD structure:
	//   2 bytes: entry count (1)
	//   12 bytes per entry: tag(2) + type(2) + count(4) + value/offset(4)
	//   4 bytes: next-IFD offset (0 = no next IFD)
	descBytes := append([]byte(description), 0x00) // null-terminated ASCII
	if len(descBytes) > math.MaxUint32 {
		return nil, fmt.Errorf("exif: description too long (%d bytes)", len(descBytes))
	}
	descLen := uint32(len(descBytes)) // #nosec G115 -- bounds checked above

	// The IFD starts at TIFF offset 8.
	// IFD size: 2 (count) + 12 (one entry) + 4 (next IFD pointer) = 18 bytes.
	// If the description is longer than 4 bytes, it is stored after the IFD
	// and the entry's value field holds an offset pointing to it.
	ifdSize := 2 + 12 + 4 // 18 bytes
	dataOffset := uint32(8 + ifdSize)

	var ifd bytes.Buffer

	// Number of directory entries: 1
	_ = binary.Write(&ifd, binary.LittleEndian, uint16(1))

	// IFD entry: ImageDescription
	_ = binary.Write(&ifd, binary.LittleEndian, uint16(0x010E)) // Tag
	_ = binary.Write(&ifd, binary.LittleEndian, uint16(2))      // Type: ASCII
	_ = binary.Write(&ifd, binary.LittleEndian, descLen)        // Count (includes null)

	if descLen <= 4 {
		// Value fits inline. Pad to 4 bytes.
		var val [4]byte
		copy(val[:], descBytes)
		_, _ = ifd.Write(val[:])
	} else {
		// Value is stored after the IFD; write the offset.
		_ = binary.Write(&ifd, binary.LittleEndian, dataOffset)
	}

	// Next IFD offset: 0 (no more IFDs).
	_ = binary.Write(&ifd, binary.LittleEndian, uint32(0))

	// Assemble the full TIFF payload: header + IFD + description data.
	var tiff bytes.Buffer
	_, _ = tiff.Write(tiffHeaderLE)
	_, _ = tiff.Write(ifd.Bytes())
	if descLen > 4 {
		_, _ = tiff.Write(descBytes)
	}

	// Assemble APP1 segment: marker + length + "Exif\0\0" + TIFF data.
	tiffData := tiff.Bytes()
	// APP1 length covers everything after the marker (2 bytes for the length
	// field itself + exif header + tiff data).
	app1TotalLen := 2 + len(exifHeader) + len(tiffData)
	if app1TotalLen > math.MaxUint16 {
		return nil, fmt.Errorf("exif: APP1 segment too large (%d bytes)", app1TotalLen)
	}
	app1Len := uint16(app1TotalLen) // #nosec G115 -- bounds checked above

	var app1 bytes.Buffer
	app1.Write([]byte{jpegMarkerPrefix, jpegAPP1})
	_ = binary.Write(&app1, binary.BigEndian, app1Len)
	app1.Write(exifHeader)
	app1.Write(tiffData)

	// Strip existing APP1 segments and rebuild the JPEG.
	stripped := stripAPP1(data)

	// Insert our APP1 right after the SOI marker (first 2 bytes).
	var result bytes.Buffer
	result.Write(stripped[:2]) // SOI
	result.Write(app1.Bytes()) // Our APP1
	result.Write(stripped[2:]) // Rest of the JPEG
	return result.Bytes(), nil
}

// readJPEGDescription walks JPEG markers looking for an APP1 segment with
// an EXIF header, then parses the TIFF IFD to find the ImageDescription tag
// (0x010E). Returns empty string if not found.
func readJPEGDescription(data []byte) (string, error) {
	if len(data) < 2 || data[0] != 0xFF || data[1] != jpegSOI {
		return "", fmt.Errorf("exif: not a valid JPEG (missing SOI marker)")
	}

	pos := 2 // Skip SOI

	for pos+1 < len(data) {
		// Every marker starts with 0xFF.
		if data[pos] != jpegMarkerPrefix {
			return "", fmt.Errorf("exif: expected marker at offset %d, got 0x%02X", pos, data[pos])
		}

		markerType := data[pos+1]
		pos += 2

		// SOS marks the start of image data; stop scanning.
		if markerType == jpegSOS {
			break
		}

		// Markers 0x00 and 0xD0-0xD7 (RST) and 0xD9 (EOI) have no payload.
		if markerType == 0x00 || (markerType >= 0xD0 && markerType <= 0xD9) {
			continue
		}

		// Read the 2-byte segment length (includes the length field itself).
		if pos+2 > len(data) {
			break
		}
		segLen := int(binary.BigEndian.Uint16(data[pos : pos+2]))
		if segLen < 2 || pos+segLen > len(data) {
			break
		}

		segData := data[pos+2 : pos+segLen] // payload after the length field
		pos += segLen

		// Only process APP1 segments with EXIF header.
		if markerType != jpegAPP1 {
			continue
		}
		if len(segData) < len(exifHeader) || !bytes.Equal(segData[:len(exifHeader)], exifHeader) {
			continue
		}

		// Parse TIFF data to find ImageDescription.
		tiffData := segData[len(exifHeader):]
		desc, err := findTIFFDescription(tiffData)
		if err != nil {
			continue // Malformed TIFF in this APP1; try the next one.
		}
		if desc != "" {
			return desc, nil
		}
	}

	return "", nil
}

// findTIFFDescription parses minimal TIFF IFD data to extract the
// ImageDescription tag (0x010E). It supports both little-endian ("II") and
// big-endian ("MM") byte order.
func findTIFFDescription(tiff []byte) (string, error) {
	if len(tiff) < 8 {
		return "", fmt.Errorf("tiff data too short")
	}

	// Determine byte order.
	var bo binary.ByteOrder
	switch {
	case tiff[0] == 'I' && tiff[1] == 'I':
		bo = binary.LittleEndian
	case tiff[0] == 'M' && tiff[1] == 'M':
		bo = binary.BigEndian
	default:
		return "", fmt.Errorf("unknown TIFF byte order: %c%c", tiff[0], tiff[1])
	}

	// Verify magic number (42).
	magic := bo.Uint16(tiff[2:4])
	if magic != 42 {
		return "", fmt.Errorf("bad TIFF magic: %d", magic)
	}

	// Read IFD0 offset.
	ifdOffset := bo.Uint32(tiff[4:8])
	if int(ifdOffset)+2 > len(tiff) {
		return "", fmt.Errorf("IFD offset out of range")
	}

	// Read number of entries.
	entryCount := int(bo.Uint16(tiff[ifdOffset : ifdOffset+2]))
	entryStart := int(ifdOffset) + 2

	for i := 0; i < entryCount; i++ {
		off := entryStart + i*12
		if off+12 > len(tiff) {
			break
		}

		tag := bo.Uint16(tiff[off : off+2])
		if tag != 0x010E {
			continue // Not ImageDescription
		}

		// dataType := bo.Uint16(tiff[off+2 : off+4]) // type (2 = ASCII)
		count := bo.Uint32(tiff[off+4 : off+8])
		valueOrOffset := tiff[off+8 : off+12]

		var descBytes []byte
		if count <= 4 {
			// Value stored inline.
			descBytes = valueOrOffset[:count]
		} else {
			// Value stored at offset.
			valOff := bo.Uint32(valueOrOffset)
			if uint64(valOff)+uint64(count) > uint64(len(tiff)) {
				return "", fmt.Errorf("description offset out of range")
			}
			descBytes = tiff[int(valOff) : int(valOff)+int(count)]
		}

		// Strip trailing null(s).
		return string(bytes.TrimRight(descBytes, "\x00")), nil
	}

	return "", nil
}

// stripAPP1 returns a copy of the JPEG data with all APP1 segments removed.
// It walks markers from SOI until SOS (start of scan), copying everything
// except APP1 segments.
func stripAPP1(data []byte) []byte {
	if len(data) < 2 || data[0] != 0xFF || data[1] != jpegSOI {
		return data
	}

	var out bytes.Buffer
	out.Write(data[:2]) // SOI
	pos := 2

	for pos+1 < len(data) {
		if data[pos] != jpegMarkerPrefix {
			// Not a marker; copy the rest verbatim (we are past SOS).
			out.Write(data[pos:])
			break
		}

		markerType := data[pos+1]

		// SOS: copy everything from here to end (compressed image data).
		if markerType == jpegSOS {
			out.Write(data[pos:])
			break
		}

		// Standalone markers (no payload).
		if markerType == 0x00 || (markerType >= 0xD0 && markerType <= 0xD9) {
			out.Write(data[pos : pos+2])
			pos += 2
			continue
		}

		// Read segment length.
		if pos+4 > len(data) {
			out.Write(data[pos:])
			break
		}
		segLen := int(binary.BigEndian.Uint16(data[pos+2 : pos+4]))
		if segLen < 2 || pos+2+segLen > len(data) {
			out.Write(data[pos:])
			break
		}

		// Skip APP1 segments; copy everything else.
		if markerType == jpegAPP1 {
			pos += 2 + segLen
			continue
		}

		out.Write(data[pos : pos+2+segLen])
		pos += 2 + segLen
	}

	return out.Bytes()
}

// ---------------------------------------------------------------------------
// PNG tEXt chunk injection and reading
// ---------------------------------------------------------------------------

// pngSignature is the 8-byte PNG file signature.
var pngSignature = []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}

// pngDescriptionKey is the key used in tEXt chunks for image provenance.
const pngDescriptionKey = "ImageDescription"

// injectPNGDescription builds a tEXt chunk with key "ImageDescription" and
// the given value, then inserts it into the PNG data after the IHDR chunk.
// Any existing ImageDescription tEXt chunk is removed first.
func injectPNGDescription(data []byte, description string) ([]byte, error) {
	if len(data) < len(pngSignature) || !bytes.Equal(data[:len(pngSignature)], pngSignature) {
		return nil, fmt.Errorf("exif: not a valid PNG (bad signature)")
	}

	// Build the tEXt chunk payload: key + null separator + value.
	var chunkData bytes.Buffer
	chunkData.WriteString(pngDescriptionKey)
	chunkData.WriteByte(0x00)
	chunkData.WriteString(description)
	payload := chunkData.Bytes()

	// Build the full chunk: length(4) + type(4) + data + CRC(4).
	// CRC covers the type field and the data (not the length).
	if len(payload) > math.MaxUint32 {
		return nil, fmt.Errorf("exif: tEXt chunk payload too large (%d bytes)", len(payload))
	}
	var chunk bytes.Buffer
	_ = binary.Write(&chunk, binary.BigEndian, uint32(len(payload))) // #nosec G115 -- bounds checked above
	chunkType := []byte("tEXt")
	chunk.Write(chunkType)
	chunk.Write(payload)
	crcData := append(chunkType, payload...)
	_ = binary.Write(&chunk, binary.BigEndian, crc32.ChecksumIEEE(crcData))

	// Walk existing chunks. Insert our tEXt after IHDR, skip existing
	// ImageDescription tEXt chunks.
	var result bytes.Buffer
	result.Write(pngSignature)

	pos := len(pngSignature)
	insertedAfterIHDR := false

	for pos+8 <= len(data) {
		// Each chunk: 4-byte length + 4-byte type + [length bytes data] + 4-byte CRC.
		chunkLen := binary.BigEndian.Uint32(data[pos : pos+4])
		cType := string(data[pos+4 : pos+8])

		// Guard against malformed chunk lengths that would overflow int arithmetic.
		if int64(chunkLen) > int64(len(data)) {
			result.Write(data[pos:])
			break
		}

		// Total chunk size: 4 (length) + 4 (type) + chunkLen (data) + 4 (CRC).
		totalSize := 12 + int(chunkLen)
		if pos+totalSize > len(data) {
			// Malformed chunk; copy the rest as-is.
			result.Write(data[pos:])
			break
		}

		// Skip existing ImageDescription tEXt chunks.
		if cType == "tEXt" && isImageDescriptionChunk(data[pos+8:pos+8+int(chunkLen)]) {
			pos += totalSize
			continue
		}

		// Copy this chunk.
		result.Write(data[pos : pos+totalSize])
		pos += totalSize

		// Insert our new tEXt chunk right after IHDR.
		if cType == "IHDR" && !insertedAfterIHDR {
			result.Write(chunk.Bytes())
			insertedAfterIHDR = true
		}
	}

	return result.Bytes(), nil
}

// readPNGDescription walks PNG chunks looking for a tEXt chunk with key
// "ImageDescription". Returns empty string if not found.
func readPNGDescription(data []byte) (string, error) {
	if len(data) < len(pngSignature) || !bytes.Equal(data[:len(pngSignature)], pngSignature) {
		return "", fmt.Errorf("exif: not a valid PNG (bad signature)")
	}

	pos := len(pngSignature)

	for pos+8 <= len(data) {
		chunkLen := binary.BigEndian.Uint32(data[pos : pos+4])
		cType := string(data[pos+4 : pos+8])

		// Guard against malformed chunk lengths that would overflow int arithmetic.
		if int64(chunkLen) > int64(len(data)) {
			break
		}

		totalSize := 12 + int(chunkLen)
		if pos+totalSize > len(data) {
			break
		}

		// Stop scanning at IDAT or IEND -- text chunks come before image data.
		if cType == "IDAT" || cType == "IEND" {
			break
		}

		if cType == "tEXt" {
			chunkPayload := data[pos+8 : pos+8+int(chunkLen)]
			if isImageDescriptionChunk(chunkPayload) {
				// Key is "ImageDescription\0", value follows.
				valueStart := len(pngDescriptionKey) + 1
				if valueStart <= len(chunkPayload) {
					return string(chunkPayload[valueStart:]), nil
				}
			}
		}

		pos += totalSize
	}

	return "", nil
}

// isImageDescriptionChunk checks whether a tEXt chunk payload starts with
// "ImageDescription\0".
func isImageDescriptionChunk(payload []byte) bool {
	prefix := pngDescriptionKey + "\x00"
	return len(payload) >= len(prefix) && string(payload[:len(prefix)]) == prefix
}

// ---------------------------------------------------------------------------
// Public API
// ---------------------------------------------------------------------------

// InjectMeta embeds provenance metadata into image data (JPEG or PNG).
// If meta is nil, the data is returned unchanged. For unrecognized image
// formats, the data is returned as-is with no error.
func InjectMeta(data []byte, meta *ExifMeta) ([]byte, error) {
	if meta == nil {
		return data, nil
	}

	description := meta.Marshal()

	// Detect format by magic bytes.
	if len(data) >= 2 && data[0] == 0xFF && data[1] == jpegSOI {
		return injectJPEGDescription(data, description)
	}
	if len(data) >= len(pngSignature) && bytes.Equal(data[:len(pngSignature)], pngSignature) {
		return injectPNGDescription(data, description)
	}

	// Unknown format: return as-is.
	return data, nil
}

// ReadProvenance reads an image file and extracts any embedded Stillwater
// provenance metadata. Returns nil, nil for images without a Stillwater tag
// (this is not an error condition).
func ReadProvenance(path string) (*ExifMeta, error) {
	data, err := os.ReadFile(filepath.Clean(path)) //nolint:gosec // G304: path is from trusted internal callers
	if err != nil {
		return nil, fmt.Errorf("reading image for provenance: %w", err)
	}

	var description string

	// Detect format and extract description.
	if len(data) >= 2 && data[0] == 0xFF && data[1] == jpegSOI {
		description, err = readJPEGDescription(data)
		if err != nil {
			return nil, fmt.Errorf("reading JPEG description: %w", err)
		}
	} else if len(data) >= len(pngSignature) && bytes.Equal(data[:len(pngSignature)], pngSignature) {
		description, err = readPNGDescription(data)
		if err != nil {
			return nil, fmt.Errorf("reading PNG description: %w", err)
		}
	} else {
		// Unknown format: no provenance.
		return nil, nil
	}

	if description == "" {
		return nil, nil
	}

	meta, err := ParseExifMeta(description)
	if err != nil {
		return nil, fmt.Errorf("parsing provenance metadata: %w", err)
	}

	// Non-Stillwater ImageDescription strings parse to a zero struct.
	if meta.isZero() {
		return nil, nil
	}

	return &meta, nil
}
