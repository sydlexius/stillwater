package musicbrainz

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
)

// FieldDiff represents a difference between a Stillwater field value and the
// last-known MusicBrainz value for the same field.
type FieldDiff struct {
	Field            string `json:"field"`
	StillwaterValue  string `json:"stillwater_value"`
	MusicBrainzValue string `json:"musicbrainz_value"`
	Source           string `json:"source"` // provider that sourced the current Stillwater value
}

// SnapshotFields lists the metadata fields that MusicBrainz can supply and
// that we snapshot during provider refreshes. Styles, moods, and years_active
// are excluded because they are not MusicBrainz-native fields.
var SnapshotFields = []string{
	"name", "sort_name", "type", "gender", "disambiguation",
	"genres", "born", "formed", "died", "disbanded",
}

// ComputeDiffs compares the current artist state against MusicBrainz snapshots
// and returns fields where the values differ. Only fields where the Stillwater
// value is non-empty are included (we never submit empty/deletion diffs).
func ComputeDiffs(a *artist.Artist, mbSnapshots map[string]artist.MBSnapshot, metadataSources map[string]string) []FieldDiff {
	var diffs []FieldDiff

	for _, field := range SnapshotFields {
		snap, ok := mbSnapshots[field]
		if !ok {
			continue
		}

		swValue := artistFieldValue(a, field)
		if swValue == "" {
			// Never submit empty values to MusicBrainz.
			continue
		}

		if !valuesEqual(field, swValue, snap.MBValue) {
			diffs = append(diffs, FieldDiff{
				Field:            field,
				StillwaterValue:  swValue,
				MusicBrainzValue: snap.MBValue,
				Source:           metadataSources[field],
			})
		}
	}

	return diffs
}

// ExtractMBFieldValues extracts field values from provider metadata for the
// fields that MusicBrainz supplied (identified by checking sources). Returns
// a list of MBSnapshot entries ready for upserting.
func ExtractMBFieldValues(meta *provider.ArtistMetadata, sources []provider.FieldSource) []artist.MBSnapshot {
	// Build a set of fields sourced from MusicBrainz.
	mbFields := make(map[string]bool)
	for _, s := range sources {
		if s.Provider == provider.NameMusicBrainz {
			mbFields[s.Field] = true
		}
	}

	var snapshots []artist.MBSnapshot
	for _, field := range SnapshotFields {
		if !mbFields[field] {
			continue
		}
		value := metadataFieldValue(meta, field)
		snapshots = append(snapshots, artist.MBSnapshot{
			Field:   field,
			MBValue: value,
		})
	}
	return snapshots
}

// artistFieldValue extracts a field value from an Artist struct as a string.
// Slice fields (genres) are serialized as sorted JSON arrays.
func artistFieldValue(a *artist.Artist, field string) string {
	switch field {
	case "name":
		return a.Name
	case "sort_name":
		return a.SortName
	case "type":
		return a.Type
	case "gender":
		return a.Gender
	case "disambiguation":
		return a.Disambiguation
	case "genres":
		return sortedJSON(a.Genres)
	case "born":
		return a.Born
	case "formed":
		return a.Formed
	case "died":
		return a.Died
	case "disbanded":
		return a.Disbanded
	default:
		return ""
	}
}

// metadataFieldValue extracts a field value from provider ArtistMetadata as a string.
func metadataFieldValue(meta *provider.ArtistMetadata, field string) string {
	switch field {
	case "name":
		return meta.Name
	case "sort_name":
		return meta.SortName
	case "type":
		return meta.Type
	case "gender":
		return meta.Gender
	case "disambiguation":
		return meta.Disambiguation
	case "genres":
		return sortedJSON(meta.Genres)
	case "born":
		return meta.Born
	case "formed":
		return meta.Formed
	case "died":
		return meta.Died
	case "disbanded":
		return meta.Disbanded
	default:
		return ""
	}
}

// valuesEqual compares two field values with field-appropriate normalization.
// For genres (JSON arrays), values are compared as sorted, lowercased sets.
// For text fields, values are compared with trimmed whitespace and case-insensitive matching.
func valuesEqual(field, a, b string) bool {
	if field == "genres" {
		return sliceJSONEqual(a, b)
	}
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

// sortedJSON serializes a string slice as a sorted JSON array.
// Returns "[]" for nil/empty slices.
func sortedJSON(ss []string) string {
	if len(ss) == 0 {
		return "[]"
	}
	sorted := make([]string, len(ss))
	copy(sorted, ss)
	sort.Strings(sorted)
	b, _ := json.Marshal(sorted)
	return string(b)
}

// sliceJSONEqual compares two JSON-encoded string arrays as sorted, lowercased sets.
func sliceJSONEqual(a, b string) bool {
	return normalizeSliceJSON(a) == normalizeSliceJSON(b)
}

// normalizeSliceJSON parses a JSON array of strings, lowercases, sorts, and
// re-serializes for canonical comparison.
func normalizeSliceJSON(s string) string {
	var items []string
	if err := json.Unmarshal([]byte(s), &items); err != nil {
		// Fall back to direct string comparison for non-JSON values.
		return strings.ToLower(strings.TrimSpace(s))
	}
	for i := range items {
		items[i] = strings.ToLower(strings.TrimSpace(items[i]))
	}
	sort.Strings(items)
	b, _ := json.Marshal(items)
	return string(b)
}
