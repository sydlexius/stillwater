package artist

import "time"

// Rule-written MusicBrainz ID reporting (issue #2809, read-only).
//
// This file defines the vocabulary for "which artists did the nfo_has_mbid rule
// fixer assign a MusicBrainz ID to". The query lives in sqlite_history.go; the
// service wrapper is in history.go. Nothing here writes, validates an ID, or
// reverts anything: the operator gets a list to work through, and the deciding
// stays theirs.
//
// # WHY EVERY WRITE, NOT THE LATEST ONE PER ARTIST
//
// The blast-radius report windows down to the newest row per (artist, field)
// because it answers "what is broken RIGHT NOW". This report answers a
// different question -- "what did this fixer do" -- and every one of its writes
// is a separate act the operator may want to see. Collapsing them would hide a
// second guess made after the first, which is exactly the kind of history an
// operator chasing a misidentification needs.
//
// # WHAT THIS REPORT CANNOT SEE
//
// Only the rule-fixer path records a change under the exact source this
// report matches (NFOMBIDReportSource). Two other code paths assign a
// MusicBrainz ID and record nothing at all, permanently: the two
// automatic-match tiers of the identify flow. The generic artist-update path
// records nothing either, because the tracked-field list does not include any
// provider ID. So an artist misidentified by one of those three paths leaves
// no trace anywhere and can NEVER appear here, however long this report runs.
//
// The bulk rule executor is different (issue #2825/#2845): it now records its
// own MBID assignments too, under the distinct source
// "rule:bulk_fetch_images_mbid". That row exists and is visible on the
// artist's own change history -- it is simply not enumerable by THIS report's
// exact-match filter. So the bulk path's gap here is a scoping choice, not a
// missing record: widening the query to match a source prefix instead of one
// exact value is a real option, not a permanent limit like the other three.
//
// That distinction is stated to the operator in the response, in the CSV, in
// the API description, and in the docs. Presenting this list as the complete
// set of machine-assigned IDs would be the "unknown rendered as clean" defect
// that the caveats exist to prevent.

// NFOMBIDReportSource is the exact metadata_changes.source value the
// nfo_has_mbid rule fixer writes. The query matches on this and nothing else.
//
// Deliberately NOT combined with the machine-picked provenance marker: that
// marker was introduced later, so filtering on it would drop the entire
// pre-marker population -- which is the already-damaged set this report exists
// to find. Matching on the source alone captures both eras.
const NFOMBIDReportSource = "rule:nfo_has_mbid"

// Sort keys accepted by ListNFOMBIDWrites. Anything else is coerced to
// NFOMBIDSortCreatedAt by Validate rather than interpolated into SQL.
const (
	NFOMBIDSortCreatedAt  = "created_at"
	NFOMBIDSortArtistName = "artist_name"
)

// NFOMBIDFilter narrows the report. The zero value (after Validate) is "every
// ID this rule ever wrote, newest first".
type NFOMBIDFilter struct {
	// ArtistID narrows to a single artist. Bound as a query parameter, never
	// interpolated, so it needs no sanitizing here.
	ArtistID string
	// Sort is one of the NFOMBIDSort* constants; Order is "asc" or "desc".
	Sort  string
	Order string
	// Limit and Offset paginate the row list. They do not affect counts.
	Limit  int
	Offset int
}

// Validate normalizes the filter and forces every value that reaches SQL into a
// known-good one. Unrecognized sort/order values are coerced to their defaults
// rather than rejected: this is a read-only report about possible
// misidentification, so a malformed query parameter must show the operator the
// unnarrowed list, never an error page that hides how many artists are affected.
func (f *NFOMBIDFilter) Validate() {
	if f.Sort != NFOMBIDSortArtistName {
		f.Sort = NFOMBIDSortCreatedAt
	}
	if f.Order != "asc" {
		f.Order = "desc"
	}
	if f.Limit <= 0 {
		f.Limit = 50
	}
	if f.Limit > 500 {
		f.Limit = 500
	}
	if f.Offset < 0 {
		f.Offset = 0
	}
}

// NFOMBIDWriteRow is one MusicBrainz ID the nfo_has_mbid rule fixer wrote:
// which artist, the audit text recorded at the time, when, and what ID the
// artist carries now.
//
// There is deliberately NO "replaced value" field. Two independent properties
// of the writer make one permanently empty: the rule-fix history path always
// records an empty previous value, and this rule only ever runs on an artist
// whose MusicBrainz ID is blank. So this rule never destroyed an ID an operator
// had; it filled a blank with one that may be wrong. A column that could only
// ever be empty would invite the reader to conclude something was overwritten.
type NFOMBIDWriteRow struct {
	// ID is the change-record id, stable enough to key a UI row on.
	ID string `json:"id"`
	// ArtistID and ArtistName identify the affected artist.
	ArtistID   string `json:"artist_id"`
	ArtistName string `json:"artist_name"`
	// Message is the fixer's own audit text, recorded verbatim at the time of
	// the write and shown verbatim here.
	//
	// It is NOT parsed. Two wordings exist in the wild: an early short form and
	// a later one that also states the matched name, the search source, the
	// confidence score, and the runner-up. Any parser would have to handle both
	// and would still be guessing, so the actionable identifier comes from
	// CurrentMusicBrainzID below and this stays what it is: recorded audit prose.
	Message string `json:"message"`
	// Source is the recorded source, always NFOMBIDReportSource. Carried so an
	// exported row is self-describing rather than relying on the file name.
	Source string `json:"source"`
	// CreatedAt is when the fixer wrote the ID.
	CreatedAt time.Time `json:"created_at"`
	// CurrentMusicBrainzID is the ID the artist carries NOW, which may differ
	// from the one this write assigned if something changed it since.
	CurrentMusicBrainzID string `json:"current_musicbrainz_id"`
	// HasCurrentMusicBrainzID distinguishes "the artist has no MusicBrainz ID
	// today" from "the artist has one and it is the empty string", which the
	// storage layer cannot rule out on its own. A caller must render the false
	// case as "none recorded", never as a blank cell that reads like a clean
	// value.
	HasCurrentMusicBrainzID bool `json:"has_current_musicbrainz_id"`
}

// NFOMBIDCounts summarizes the matching rows.
//
// Both numbers are a FLOOR, never a census. See NFOMBIDCaveatRetention: the
// change history of a deleted or merged-away artist is gone, and a known
// consequence of this rule's misidentifications was duplicate artist rows, so
// some affected artists have very likely already been merged and are
// unmeasurable. Callers must present these as "at least this many".
type NFOMBIDCounts struct {
	// Writes counts individual ID assignments. An artist the fixer wrote twice
	// contributes two.
	Writes int `json:"writes"`
	// Artists counts distinct artists with at least one such write. This is the
	// number an operator sizing the review work wants.
	Artists int `json:"artists"`
	// Total is Writes, repeated under the name pagination uses.
	Total int `json:"total"`
}

// Caveat texts. Held as constants so the JSON response, the CSV note rows, and
// the templates cannot drift into saying different things about the same limit.
// Written as operator-facing prose, not as internal references.
const (
	// NFOMBIDCaveatScope is the coverage limit and the most important of these.
	NFOMBIDCaveatScope = "This report covers MusicBrainz IDs written by the automatic NFO rule fix only. " +
		"The automatic match tiers of the Identify flow can also assign a MusicBrainz ID and record " +
		"no change at all, so an artist they affected can never appear here. The bulk rule run now " +
		"records its own assignments too, but under a different, more specific label, so this report " +
		"still will not list them even though a record of them does exist and can be found on the " +
		"artist's own history. Either way, this list is not the complete set of machine-assigned IDs " +
		"and an artist's absence from it is not evidence its ID was chosen by a person."

	// NFOMBIDCaveatFloor states that the counts under-report by construction.
	NFOMBIDCaveatFloor = "Treat every count here as a minimum. Rows can be missing for reasons this " +
		"report cannot detect, so the real number of affected artists is at least this large and " +
		"may be larger."

	// NFOMBIDCaveatRetention states the one way rows disappear.
	NFOMBIDCaveatRetention = "Change history is kept until an artist is deleted or merged into another. " +
		"Either removes that artist's history, including rows this report would have shown. " +
		"A known side effect of these misidentifications was duplicate artists, so some affected " +
		"artists have probably already been merged away and cannot be counted at all."

	// NFOMBIDCaveatNoPriorValue is fact (B) in operator terms.
	NFOMBIDCaveatNoPriorValue = "This fix only ran on artists that had no MusicBrainz ID, so it never " +
		"overwrote an ID you had set. Each row is a blank field filled in with an ID that may be wrong."

	// NFOMBIDCaveatNotConfirmed forbids reading an unlisted artist as vetted.
	NFOMBIDCaveatNotConfirmed = "An artist not listed here has not been confirmed by anyone. " +
		"Stillwater does not yet record operator confirmation of a MusicBrainz ID, so " +
		"\"not on this list\" means only that this particular fix did not write the ID."

	// NFOMBIDCaveatMessageWording explains why the audit text varies row to row.
	NFOMBIDCaveatMessageWording = "The recorded note is shown exactly as it was written at the time. " +
		"Older entries record only the ID and the artist name; newer ones also record the matched " +
		"name, where the match came from, the confidence score, and the runner-up. Use the current " +
		"MusicBrainz ID column, not the note, when acting on a row."
)
