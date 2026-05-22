package nfo

import (
	"strings"

	"github.com/sydlexius/stillwater/internal/provider"
)

// ReleaseTypeFilter is a set of MusicBrainz primary-type values to include
// when merging release groups into an NFO discography. Comparisons are
// case-insensitive. A nil or empty filter includes all release types.
type ReleaseTypeFilter []string

// DefaultReleaseTypeFilter returns the default filter (Album and EP).
func DefaultReleaseTypeFilter() ReleaseTypeFilter {
	return ReleaseTypeFilter{"Album", "EP"}
}

// ParseReleaseTypeFilter parses a comma-separated include parameter such as
// "Album,EP,Single" into a ReleaseTypeFilter. Empty tokens are ignored.
func ParseReleaseTypeFilter(s string) ReleaseTypeFilter {
	if s == "" {
		return DefaultReleaseTypeFilter()
	}
	var out ReleaseTypeFilter
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	if len(out) == 0 {
		return DefaultReleaseTypeFilter()
	}
	return out
}

// contains reports whether the filter includes the given primary type.
// Comparison is case-insensitive. A nil filter accepts all types.
func (f ReleaseTypeFilter) contains(primaryType string) bool {
	if len(f) == 0 {
		return true
	}
	for _, t := range f {
		if strings.EqualFold(t, primaryType) {
			return true
		}
	}
	return false
}

// Includes reports whether the filter accepts the given MusicBrainz primary
// type. It is the exported form of contains, used by the discography_populated
// rule checker to count release groups against the same filter the merge
// applies. A nil or empty filter accepts every type.
func (f ReleaseTypeFilter) Includes(primaryType string) bool {
	return f.contains(primaryType)
}

// CountReleaseGroups returns the number of release groups whose PrimaryType
// the filter accepts. The discography_populated checker uses this to measure
// how much of an artist's discography MusicBrainz exposes for the configured
// release types, so the coverage ratio matches what MergeDiscographyFromMBReleaseGroups
// would actually merge.
func (f ReleaseTypeFilter) CountReleaseGroups(groups []provider.ReleaseGroupInfo) int {
	n := 0
	for _, rg := range groups {
		if f.contains(rg.PrimaryType) {
			n++
		}
	}
	return n
}

// MergeDiscographyResult summarizes what MergeDiscographyFromMBReleaseGroups
// changed. The caller (handler or rule fixer) may surface these counts in the
// response or a log message.
type MergeDiscographyResult struct {
	// Added is the number of release groups that were not in the NFO and were
	// inserted.
	Added int
	// Kept is the number of existing NFO entries that were preserved because
	// they matched an incoming release group by MBID.
	Kept int
	// Skipped is the number of incoming release groups that were filtered
	// out because their PrimaryType is not in the release-type filter. An
	// incoming group that matches an existing entry by MBID increments Kept,
	// not Skipped.
	Skipped int
	// Total is the count of candidate release groups from the provider before
	// filtering (len(groups)).
	Total int
}

// MergeDiscographyFromMBReleaseGroups merges MusicBrainz release groups into
// the Albums slice of existing. It is the canonical package-level helper used
// by both the handler (issue #1065) and the rule fixer (issue #1063).
//
// Merge semantics:
//   - Existing entries that carry a MusicBrainzReleaseGroupID are keyed by
//     that MBID. When an incoming release group matches a keyed entry, the
//     existing entry wins: the user may have manually refined title or year,
//     so their version is never overwritten.
//   - Existing entries without an MBID (user-added albums) are always
//     preserved in their original positions at the start of the output.
//   - New release groups (no matching MBID in existing) are appended after
//     the preserved entries, in the order they arrive from the provider.
//   - Release groups whose PrimaryType is not in filter are skipped (not
//     added, not kept; they increment Skipped).
//
// existing may be nil; in that case a fresh NFO with only the new entries is
// returned. The function never modifies the caller's slice in place.
func MergeDiscographyFromMBReleaseGroups(
	existing []DiscographyAlbum,
	groups []provider.ReleaseGroupInfo,
	filter ReleaseTypeFilter,
) (merged []DiscographyAlbum, result MergeDiscographyResult) {
	result.Total = len(groups)

	// Index existing MBID-tagged entries for O(1) conflict detection.
	// Entries without an MBID are collected separately so they land first.
	type existingEntry struct {
		album DiscographyAlbum
		seen  bool // true when an incoming group matched and kept this entry
	}

	mbidIndex := make(map[string]*existingEntry, len(existing))
	var noMBIDEntries []DiscographyAlbum

	for _, alb := range existing {
		if alb.MusicBrainzReleaseGroupID == "" {
			noMBIDEntries = append(noMBIDEntries, alb)
		} else {
			alb := alb // capture
			mbidIndex[alb.MusicBrainzReleaseGroupID] = &existingEntry{album: alb}
		}
	}

	// Pass over incoming release groups.
	var toAdd []DiscographyAlbum

	for _, rg := range groups {
		if !filter.contains(rg.PrimaryType) {
			result.Skipped++
			continue
		}

		if rg.ID != "" {
			if entry, exists := mbidIndex[rg.ID]; exists {
				// Existing entry wins; mark as seen so it is included once.
				entry.seen = true
				result.Kept++
				continue
			}
		}

		// New entry: map from provider.ReleaseGroupInfo to nfo.DiscographyAlbum.
		alb := DiscographyAlbum{
			Title:                     rg.Title,
			Year:                      releaseYear(rg.FirstReleaseDate),
			MusicBrainzReleaseGroupID: rg.ID,
		}
		toAdd = append(toAdd, alb)
		result.Added++
	}

	// Assemble the final slice: user albums first, then ALL existing MBID
	// albums in their original order (whether or not the incoming set
	// contained them -- a partial upstream response must not drop albums
	// that are already stored), then new entries.
	//
	// A dedup map prevents a hypothetical duplicate MBID in existing from
	// emitting the same entry twice.
	emitted := make(map[string]bool, len(mbidIndex))
	merged = append(merged, noMBIDEntries...)
	for _, alb := range existing {
		if alb.MusicBrainzReleaseGroupID == "" {
			continue
		}
		if emitted[alb.MusicBrainzReleaseGroupID] {
			continue
		}
		if entry, ok := mbidIndex[alb.MusicBrainzReleaseGroupID]; ok {
			merged = append(merged, entry.album)
			emitted[alb.MusicBrainzReleaseGroupID] = true
		}
	}
	merged = append(merged, toAdd...)

	return merged, result
}

// releaseYear extracts the four-digit year from a MusicBrainz first-release-date
// string, which may be "YYYY", "YYYY-MM", or "YYYY-MM-DD". Returns empty string
// when the input is shorter than four characters or does not start with digits.
func releaseYear(date string) string {
	if len(date) >= 4 {
		year := date[:4]
		for _, c := range year {
			if c < '0' || c > '9' {
				return ""
			}
		}
		return year
	}
	return ""
}
