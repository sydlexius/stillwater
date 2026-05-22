package artist

// duplicates.go -- near-duplicate artist detection.
//
// DetectDuplicates scans the artists table entirely in memory (no stored column,
// no migration) and returns groups of artists that are likely the same artist.
// Two artists belong to the same group when:
//   - Their NormalizeIdentityKey values are equal (name-key match), OR
//   - They share a non-empty MusicBrainz artist ID (MBID match).
//
// MBID equality is the higher-confidence signal: it catches cases where
// filesystem-reserved-character substitution (AC/DC -> AC_DC vs ACDC) prevents
// the name key from colliding, as long as at least one record carries the MBID.
//
// Groups with two or more members are returned sorted by the first member's
// name.  Platform-only artists (path = '') are excluded because they have no
// on-disk representation that a merge could act on.
//
// The detection runs fully in Go from two queries (artists + their MBIDs), so
// it does NOT touch the sqlite_artist.go List / buildWhereClause path and
// remains parallel-safe with the concurrent Wave-3 issues.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
)

// NearDuplicateGroup holds artists that are likely the same physical artist.
// Named with the Near prefix to avoid colliding with the existing DuplicateGroup
// in alias.go which has a different shape.
type NearDuplicateGroup struct {
	// Key is the normalized identity key shared by the name-match members.
	// For MBID-only matches that happen to have different name keys, this
	// is the MBID itself.
	Key string

	// Reason is "name_key" when the group was formed by normalized-name
	// collision, or "mbid" when every member carries the same non-empty MBID.
	// A group merged from a name-key collision that pulled in members with
	// differing or absent MBIDs stays "name_key" regardless of whether any
	// MBID edge existed during union-find.
	Reason string

	// Members is the list of artists in this group, at least 2 entries.
	Members []NearDuplicateArtist
}

// NearDuplicateArtist is the subset of artist data needed by the duplicate view.
// Using a dedicated struct keeps the detection query lean and ensures the
// handler never imports the full Artist model from the .templ side.
type NearDuplicateArtist struct {
	ID   string
	Name string
	Path string
	MBID string // empty when no MusicBrainz provider row exists
}

// artistRow is an internal row read from the DB query.
type artistRow struct {
	id   string
	name string
	path string
	mbid string // empty when no row in artist_provider_ids for musicbrainz
}

// DetectDuplicates loads all path-bearing artists and their MusicBrainz IDs,
// then groups them by normalized name key and MBID, merging overlapping groups
// via union-find.  Artists whose path column is empty (platform-only / API-
// imported) are excluded because they have no filesystem directory to merge.
// The returned slice contains only groups with 2 or more members.
//
// db is the raw *sql.DB handle; use the artist repo's DB() accessor to obtain
// it from the service layer without coupling detection to the Service struct.
func DetectDuplicates(ctx context.Context, db *sql.DB) ([]NearDuplicateGroup, error) {
	rows, err := queryDuplicateCandidates(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("detecting duplicates: %w", err)
	}

	return groupDuplicates(rows), nil
}

// queryDuplicateCandidates issues a single SQL query joining artists with their
// MusicBrainz provider ID (LEFT JOIN so artists without an MBID are included).
// Only rows with a non-empty path are returned -- platform-only artists cannot
// be merged on disk and are not useful in the duplicate view.
func queryDuplicateCandidates(ctx context.Context, db *sql.DB) ([]artistRow, error) {
	const q = `
		SELECT
			a.id,
			a.name,
			a.path,
			COALESCE(p.provider_id, '') AS mbid
		FROM artists a
		LEFT JOIN artist_provider_ids p
			ON p.artist_id = a.id AND p.provider = 'musicbrainz'
		WHERE a.path <> ''
		ORDER BY a.name
	`
	sqlRows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("querying artist candidates: %w", err)
	}
	defer sqlRows.Close() //nolint:errcheck // read-only query

	var artists []artistRow
	for sqlRows.Next() {
		var r artistRow
		if err := sqlRows.Scan(&r.id, &r.name, &r.path, &r.mbid); err != nil {
			return nil, fmt.Errorf("scanning artist row: %w", err)
		}
		artists = append(artists, r)
	}
	if err := sqlRows.Err(); err != nil {
		return nil, fmt.Errorf("iterating artist rows: %w", err)
	}
	return artists, nil
}

// groupDuplicates applies union-find merging over the candidate rows.
//
// Algorithm:
//  1. Compute each artist's normalized key and index by key -> set of artist IDs.
//  2. Separately index non-empty MBIDs -> set of artist IDs.
//  3. Run union-find: for each group in the name-key index and each group in
//     the MBID index, merge artists that share membership in either group.
//  4. Collect groups of 2+ members; set Reason to "mbid" only when every
//     member carries the same non-empty MBID; otherwise set "name_key".
//
// The function is split into helpers to keep each part under the cognitive-
// complexity budget: initUnionFind, unionByNameKey, unionByMBID, collectGroups.
func groupDuplicates(rows []artistRow) []NearDuplicateGroup {
	if len(rows) == 0 {
		return nil
	}

	parent, rank := initUnionFind(rows)

	find := makeFind(parent)
	union := makeUnion(parent, rank, find)

	// Track which union-find roots gained an MBID edge so Reason can be set.
	hasMBIDEdge := make(map[string]bool)

	unionByNameKey(rows, union)
	unionByMBID(rows, find, union, hasMBIDEdge)

	// Re-root hasMBIDEdge entries after all unions have settled (path
	// compression may have moved some roots during phase 2).
	finalEdges := make(map[string]bool, len(hasMBIDEdge))
	for root := range hasMBIDEdge {
		finalEdges[find(root)] = true
	}

	return collectGroups(rows, find, finalEdges)
}

// initUnionFind allocates the parent and rank maps with each artist pointing to
// itself (every node is its own root initially).
func initUnionFind(rows []artistRow) (parent map[string]string, rank map[string]int) {
	parent = make(map[string]string, len(rows))
	rank = make(map[string]int, len(rows))
	for _, r := range rows {
		parent[r.id] = r.id
	}
	return parent, rank
}

// makeFind returns a path-compressing find function closed over parent.
func makeFind(parent map[string]string) func(string) string {
	var find func(string) string
	find = func(id string) string {
		if parent[id] != id {
			parent[id] = find(parent[id])
		}
		return parent[id]
	}
	return find
}

// makeUnion returns a union-by-rank function closed over parent, rank, and find.
func makeUnion(parent map[string]string, rank map[string]int, find func(string) string) func(string, string) {
	return func(a, b string) {
		ra, rb := find(a), find(b)
		if ra == rb {
			return
		}
		if rank[ra] < rank[rb] {
			ra, rb = rb, ra
		}
		parent[rb] = ra
		if rank[ra] == rank[rb] {
			rank[ra]++
		}
	}
}

// unionByNameKey merges all artists that share a normalized name key.
func unionByNameKey(rows []artistRow, union func(string, string)) {
	nameKeyToIDs := make(map[string][]string)
	for _, r := range rows {
		k := NormalizeIdentityKey(r.name)
		if k == "" {
			continue
		}
		nameKeyToIDs[k] = append(nameKeyToIDs[k], r.id)
	}
	for _, ids := range nameKeyToIDs {
		for i := 1; i < len(ids); i++ {
			union(ids[0], ids[i])
		}
	}
}

// unionByMBID merges all artists that share a non-empty MusicBrainz ID and
// marks the resulting root in hasMBIDEdge so the Reason can be set to "mbid".
func unionByMBID(rows []artistRow, find func(string) string, union func(string, string), hasMBIDEdge map[string]bool) {
	mbidToIDs := make(map[string][]string)
	for _, r := range rows {
		if r.mbid != "" {
			mbidToIDs[r.mbid] = append(mbidToIDs[r.mbid], r.id)
		}
	}
	for _, ids := range mbidToIDs {
		for i := 1; i < len(ids); i++ {
			union(ids[0], ids[i])
			hasMBIDEdge[find(ids[0])] = true
		}
	}
}

// collectGroups walks rows, buckets them by union-find root, discards singleton
// buckets, and returns sorted NearDuplicateGroup slices.
func collectGroups(rows []artistRow, find func(string) string, finalEdges map[string]bool) []NearDuplicateGroup {
	rootToMembers := make(map[string][]NearDuplicateArtist)
	for _, r := range rows {
		root := find(r.id)
		rootToMembers[root] = append(rootToMembers[root], NearDuplicateArtist{
			ID:   r.id,
			Name: r.name,
			Path: r.path,
			MBID: r.mbid,
		})
	}

	var groups []NearDuplicateGroup
	for root, members := range rootToMembers {
		if len(members) < 2 {
			continue
		}
		sort.Slice(members, func(i, j int) bool {
			return members[i].Name < members[j].Name
		})

		// A group is "mbid"-confirmed only when every member carries the same
		// non-empty MBID.  A mixed group (name-key collision that swept in
		// members with differing or absent MBIDs) stays "name_key" so that
		// the display header is coherent: reason == "mbid" <=> key is the
		// shared MBID, reason == "name_key" <=> key is the normalized name key.
		reason := "name_key"
		key := NormalizeIdentityKey(members[0].Name)
		if finalEdges[root] {
			if sharedMBID := findSharedMBID(members); sharedMBID != "" {
				reason = "mbid"
				key = sharedMBID
			}
		}

		groups = append(groups, NearDuplicateGroup{
			Key:     key,
			Reason:  reason,
			Members: members,
		})
	}

	sort.Slice(groups, func(i, j int) bool {
		return groups[i].Members[0].Name < groups[j].Members[0].Name
	})
	return groups
}

// findSharedMBID returns the MBID shared by all members in the group when all
// have the same non-empty MBID.  Returns "" when they differ or are empty.
func findSharedMBID(members []NearDuplicateArtist) string {
	if len(members) == 0 {
		return ""
	}
	mbid := members[0].MBID
	if mbid == "" {
		return ""
	}
	for _, m := range members[1:] {
		if m.MBID != mbid {
			return ""
		}
	}
	return mbid
}
