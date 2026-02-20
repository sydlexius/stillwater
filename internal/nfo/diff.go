package nfo

import "strings"

// FieldDiff represents a change in a single field between two NFO versions.
type FieldDiff struct {
	Field    string `json:"field"`
	OldValue string `json:"old_value"`
	NewValue string `json:"new_value"`
	Status   string `json:"status"` // "added", "removed", "changed", "unchanged"
}

// DiffResult holds the complete diff between two NFO versions.
type DiffResult struct {
	Fields  []FieldDiff `json:"fields"`
	HasDiff bool        `json:"has_diff"`
}

// Diff compares two ArtistNFO structs field by field and returns the differences.
// old is the previous version (snapshot), new is the current version.
// Either may be nil, in which case all fields of the other are marked accordingly.
func Diff(old, newNFO *ArtistNFO) *DiffResult {
	result := &DiffResult{}

	type comparison struct {
		name   string
		oldVal string
		newVal string
	}

	comparisons := []comparison{
		{"Name", getStr(old, func(n *ArtistNFO) string { return n.Name }), getStr(newNFO, func(n *ArtistNFO) string { return n.Name })},
		{"Sort Name", getStr(old, func(n *ArtistNFO) string { return n.SortName }), getStr(newNFO, func(n *ArtistNFO) string { return n.SortName })},
		{"Type", getStr(old, func(n *ArtistNFO) string { return n.Type }), getStr(newNFO, func(n *ArtistNFO) string { return n.Type })},
		{"Gender", getStr(old, func(n *ArtistNFO) string { return n.Gender }), getStr(newNFO, func(n *ArtistNFO) string { return n.Gender })},
		{"Disambiguation", getStr(old, func(n *ArtistNFO) string { return n.Disambiguation }), getStr(newNFO, func(n *ArtistNFO) string { return n.Disambiguation })},
		{"MusicBrainz ID", getStr(old, func(n *ArtistNFO) string { return n.MusicBrainzArtistID }), getStr(newNFO, func(n *ArtistNFO) string { return n.MusicBrainzArtistID })},
		{"AudioDB ID", getStr(old, func(n *ArtistNFO) string { return n.AudioDBArtistID }), getStr(newNFO, func(n *ArtistNFO) string { return n.AudioDBArtistID })},
		{"Years Active", getStr(old, func(n *ArtistNFO) string { return n.YearsActive }), getStr(newNFO, func(n *ArtistNFO) string { return n.YearsActive })},
		{"Born", getStr(old, func(n *ArtistNFO) string { return n.Born }), getStr(newNFO, func(n *ArtistNFO) string { return n.Born })},
		{"Formed", getStr(old, func(n *ArtistNFO) string { return n.Formed }), getStr(newNFO, func(n *ArtistNFO) string { return n.Formed })},
		{"Died", getStr(old, func(n *ArtistNFO) string { return n.Died }), getStr(newNFO, func(n *ArtistNFO) string { return n.Died })},
		{"Disbanded", getStr(old, func(n *ArtistNFO) string { return n.Disbanded }), getStr(newNFO, func(n *ArtistNFO) string { return n.Disbanded })},
		{"Biography", getStr(old, func(n *ArtistNFO) string { return n.Biography }), getStr(newNFO, func(n *ArtistNFO) string { return n.Biography })},
	}

	// Slice fields joined for display
	comparisons = append(comparisons,
		comparison{"Genres", getSlice(old, func(n *ArtistNFO) []string { return n.Genres }), getSlice(newNFO, func(n *ArtistNFO) []string { return n.Genres })},
		comparison{"Styles", getSlice(old, func(n *ArtistNFO) []string { return n.Styles }), getSlice(newNFO, func(n *ArtistNFO) []string { return n.Styles })},
		comparison{"Moods", getSlice(old, func(n *ArtistNFO) []string { return n.Moods }), getSlice(newNFO, func(n *ArtistNFO) []string { return n.Moods })},
	)

	for _, c := range comparisons {
		fd := FieldDiff{
			Field:    c.name,
			OldValue: c.oldVal,
			NewValue: c.newVal,
		}
		switch {
		case c.oldVal == "" && c.newVal != "":
			fd.Status = "added"
		case c.oldVal != "" && c.newVal == "":
			fd.Status = "removed"
		case c.oldVal != c.newVal:
			fd.Status = "changed"
		default:
			fd.Status = "unchanged"
		}
		if fd.Status != "unchanged" {
			result.HasDiff = true
		}
		result.Fields = append(result.Fields, fd)
	}

	return result
}

// getStr safely extracts a string field from a potentially nil ArtistNFO.
func getStr(n *ArtistNFO, fn func(*ArtistNFO) string) string {
	if n == nil {
		return ""
	}
	return fn(n)
}

// getSlice safely extracts a slice field and joins it for display.
func getSlice(n *ArtistNFO, fn func(*ArtistNFO) []string) string {
	if n == nil {
		return ""
	}
	return strings.Join(fn(n), ", ")
}
