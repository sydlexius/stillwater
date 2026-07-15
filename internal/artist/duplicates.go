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
	if db == nil {
		return nil, fmt.Errorf("detecting duplicates: nil db")
	}
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

	// repMBID tracks, per union-find component (keyed by ROOT), the single
	// non-empty MusicBrainz ID that component is bound to.  It is seeded from
	// each row's own MBID (every node is initially its own root) and propagated
	// on every merge so a later empty-MBID "bridge" row cannot smuggle a second,
	// conflicting MBID into an already-bound component.
	repMBID := make(map[string]string, len(rows))
	for _, r := range rows {
		repMBID[r.id] = r.mbid
	}
	union := makeGuardedUnion(parent, rank, repMBID, find)

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

// Guarded-union outcomes.  A union is REFUSED when the two components are each
// already bound to a different non-empty MBID: merging them would offer two
// distinct artists (that merely collide on name) as an irreversible merge
// candidate -- the #2527 data-loss vector.
const (
	unionMerged  = "merged"         // the two components were joined
	unionJoined  = "already-joined" // a and b were already in the same component
	unionRefused = "refused"        // conflicting non-empty MBIDs; NOT joined
)

// makeGuardedUnion returns a union-by-rank function that refuses to merge two
// components bound to different non-empty MBIDs, and otherwise propagates the
// surviving non-empty MBID onto the merged component's new root.  It returns
// the outcome so callers can distinguish a refusal from a real or no-op merge.
func makeGuardedUnion(parent map[string]string, rank map[string]int, repMBID map[string]string, find func(string) string) func(string, string) string {
	return func(a, b string) string {
		ra, rb := find(a), find(b)
		if ra == rb {
			return unionJoined
		}
		ma, mb := repMBID[ra], repMBID[rb]
		if ma != "" && mb != "" && ma != mb {
			return unionRefused
		}
		if rank[ra] < rank[rb] {
			ra, rb = rb, ra
		}
		parent[rb] = ra
		if rank[ra] == rank[rb] {
			rank[ra]++
		}
		// ra is the new root.  Propagate the non-empty MBID (at most one of the
		// two is non-empty here, or they are equal) so the component stays bound.
		if repMBID[ra] == "" {
			repMBID[ra] = repMBID[rb]
		}
		return unionMerged
	}
}

// unionByNameKey merges all artists that share a normalized name key.
//
// With the guarded union, a naive pivot-on-first loop is wrong: if a
// conflicting-MBID row sorts first and becomes the bucket pivot, both genuine
// duplicates' edges to the pivot are refused and they are left un-unioned (a
// silent false negative).  Rather than pay the O(k^2) all-pairs sweep over the
// whole bucket, partition the bucket by MBID and run the guarded cross-edges
// only between the distinct-MBID representatives (O(p^2) in the small number of
// distinct MBIDs).
func unionByNameKey(rows []artistRow, union func(string, string) string) {
	nameKeyToRows := make(map[string][]artistRow)
	for _, r := range rows {
		k := NormalizeIdentityKey(r.name)
		if k == "" {
			continue
		}
		nameKeyToRows[k] = append(nameKeyToRows[k], r)
	}
	for _, bucket := range nameKeyToRows {
		// Partition by MBID. Rows sharing an MBID (or both empty) can never
		// conflict, so union each partition through its first member. Then run
		// guarded unions only between the DISTINCT-MBID representatives (O(p^2)
		// in the number of distinct MBIDs, which is tiny): this joins the
		// empty-MBID partition into the first compatible non-empty one and lets
		// the guard keep differing non-empty MBIDs apart. Preserves the
		// all-pairs correctness (a conflicting row cannot strand genuine
		// duplicates) without the O(k^2) sweep over the whole bucket.
		firstByMBID := make(map[string]string)
		var reps []string // representative id per distinct MBID, first-seen order
		for _, r := range bucket {
			if rep, ok := firstByMBID[r.mbid]; ok {
				union(rep, r.id)
			} else {
				firstByMBID[r.mbid] = r.id
				reps = append(reps, r.id)
			}
		}
		for i := 0; i < len(reps); i++ {
			for j := i + 1; j < len(reps); j++ {
				union(reps[i], reps[j])
			}
		}
	}
}

// unionByMBID merges all artists that share a non-empty MusicBrainz ID and
// marks the resulting root in hasMBIDEdge so the Reason can be set to "mbid".
//
// Every row in a bucket shares the SAME non-empty MBID, so a component holding
// any bucket member already has repMBID == that MBID: the guarded union can
// never refuse within a single-MBID bucket.  Unioning each member with the
// bucket's first through a representative is therefore correct and O(k); the
// former all-pairs sweep was pure waste.  An MBID edge always exists, so mark
// hasMBIDEdge on the resulting root.
func unionByMBID(rows []artistRow, find func(string) string, union func(string, string) string, hasMBIDEdge map[string]bool) {
	mbidToIDs := make(map[string][]string)
	for _, r := range rows {
		if r.mbid != "" {
			mbidToIDs[r.mbid] = append(mbidToIDs[r.mbid], r.id)
		}
	}
	for _, ids := range mbidToIDs {
		for i := 1; i < len(ids); i++ {
			union(ids[0], ids[i]) // same MBID -> never refused
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
