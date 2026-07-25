package artist

// Blast-radius reporting (issue #2750, read-only half).
//
// This file defines the vocabulary for "what did an automated writer destroy".
// The query itself lives in sqlite_history.go; the service wrapper is in
// history.go. Nothing here writes.
//
// # WHAT THE REPORT CAN AND CANNOT SEE
//
// Every row it reads comes from metadata_changes, and that table is only ever
// written by HistoryService.Record, which Service.update / UpdateField /
// ClearField call. Service.update diffs exactly the fields in trackableFields
// (see service.go). So the report's coverage is exactly TrackableFields() and
// nothing else: a field outside that set that an automated writer destroyed
// leaves no row at all and is invisible here.
//
// That limit is surfaced to the operator rather than left implicit, and the UI
// generates its field list from TrackableFields() so the two cannot drift.
// Reporting a clean zero for a field the mechanism cannot observe would be the
// "unknown rendered as clean" defect this feature exists to avoid.
//
// # THE RETENTION HORIZON IS NOT TIME-BASED
//
// Nothing prunes metadata_changes on a schedule. But artist_id carries
// ON DELETE CASCADE, so deleting an artist -- or MERGING one away, which
// deletes the loser rows -- destroys its change history along with it. The
// honest statement to an operator is "retained until the artist is deleted or
// merged away", never "retained indefinitely".

// Damage classes. A row is one or the other; both describe an automated writer
// replacing a value the operator had.
const (
	// BlastClassBlanked is a non-empty value replaced by an empty one. This is
	// the narrow predicate issue #2750 was originally framed around.
	BlastClassBlanked = "blanked"
	// BlastClassReplaced is a non-empty value replaced by a DIFFERENT non-empty
	// one. Same class of harm and invisible to the blanked-only predicate: an
	// automated writer overwriting an operator's value with a wrong value
	// destroys it just as thoroughly as clearing it does.
	BlastClassReplaced = "replaced"
)

// Attribution states. There are THREE possible answers to "who did this", not
// two, and the third is the point.
const (
	// BlastAttributionAutomated means the source column names an automated
	// writer: a scan, an import, a provider refresh, or a rule.
	BlastAttributionAutomated = "automated"
	// BlastAttributionUnknown means the row is recorded as "manual" and
	// Stillwater CANNOT tell whether an operator made the change or a scan did.
	//
	// Scan-driven changes only began recording source="scan" on 2026-07-19
	// (commit 5942fa7a, PR #2641, implementing issue #2636). Before that they
	// fell through to the "manual" default and are indistinguishable from a
	// genuine operator edit.
	//
	// Rows in this bucket are ALWAYS shown and ALWAYS counted separately. They
	// are never filtered out by date: a date heuristic that is safe for
	// deciding what to SHOW becomes destructive the moment it is used to HIDE,
	// and any cutoff derived from the data itself (for example the earliest
	// scan row) yields nothing on a fresh database, lags the real code-version
	// boundary, and moves when a row is deleted. The cutoff date appears in
	// operator-facing prose only. No query filters on it, so a wrong date
	// cannot hide a row.
	BlastAttributionUnknown = "unknown"
)

// BlastScopeAll is the filter value meaning "do not narrow on this axis". It is
// the default for both Class and Attribution: this is a report about data loss,
// so it errs toward showing everything and letting the operator narrow, rather
// than toward a default that hides damage.
const BlastScopeAll = "all"

// Sort keys accepted by ListBlastRadius. Anything else is rejected by
// BlastRadiusFilter.Validate rather than interpolated into SQL.
const (
	BlastSortCreatedAt  = "created_at"
	BlastSortArtistName = "artist_name"
	BlastSortField      = "field"
)

// BlastRadiusFilter narrows the blast-radius query. The zero value (after
// Validate) is "every destroyed field in the library, newest first".
type BlastRadiusFilter struct {
	// Class narrows to one damage class: BlastClassBlanked, BlastClassReplaced,
	// or BlastScopeAll.
	Class string
	// Attribution narrows to BlastAttributionAutomated,
	// BlastAttributionUnknown, or BlastScopeAll. It never affects the counts
	// returned by CountBlastRadius, which always report both buckets so the
	// unknown one cannot be hidden by a filter.
	Attribution string
	// Field narrows to a single metadata field name (for example "genres").
	Field string
	// ArtistID narrows to a single artist.
	ArtistID string
	// Sort is one of the Blast Sort* constants; Order is "asc" or "desc".
	Sort  string
	Order string
	// Limit and Offset paginate the row list. They do not affect counts.
	Limit  int
	Offset int
}

// Validate normalizes the filter and forces every free-text field that reaches
// SQL into a known-good value. Unrecognized Class/Attribution/Sort/Order values
// are coerced to their defaults rather than rejected: this is a read-only
// report, so a malformed query param should show the operator the unnarrowed
// report, not an error page.
//
// Field and ArtistID are NOT sanitized here because they are bound as query
// parameters, never interpolated.
func (f *BlastRadiusFilter) Validate() {
	switch f.Class {
	case BlastClassBlanked, BlastClassReplaced:
	default:
		f.Class = BlastScopeAll
	}
	switch f.Attribution {
	case BlastAttributionAutomated, BlastAttributionUnknown:
	default:
		f.Attribution = BlastScopeAll
	}
	switch f.Sort {
	case BlastSortArtistName, BlastSortField:
	default:
		f.Sort = BlastSortCreatedAt
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

// BlastRadiusRow is one destroyed field: which artist, which field, what the
// value was, what replaced it, when, and by whom (as far as that is knowable).
type BlastRadiusRow struct {
	MetadataChangeWithArtist
	// Class is BlastClassBlanked or BlastClassReplaced.
	Class string `json:"class"`
	// Attribution is BlastAttributionAutomated or BlastAttributionUnknown.
	Attribution string `json:"attribution"`
}

// BlastRadiusCounts reports how the matching rows split by attribution.
//
// Automated and Unknown are computed WITHOUT applying the filter's Attribution
// narrowing, so both are always populated and the operator can never reach a
// view where the unknown bucket has been silently dropped. Total is the count
// for the CURRENTLY SELECTED attribution scope, which is what pagination needs.
type BlastRadiusCounts struct {
	// Automated counts rows whose source names an automated writer.
	Automated int `json:"automated"`
	// Unknown counts rows recorded as "manual", which may be operator edits or
	// may be pre-2026-07-19 scan damage. Never hidden, never folded into
	// Automated, never collapsed into a single headline number.
	Unknown int `json:"unknown"`
	// Total is Automated+Unknown when the filter's Attribution is
	// BlastScopeAll, or the single selected bucket otherwise. It is the
	// denominator for pagination, not a headline "damage" figure.
	Total int `json:"total"`
}

// AttributionCutoffDate is the date Stillwater began recording scan-driven
// metadata changes with source="scan" instead of letting them fall through to
// the "manual" default (commit 5942fa7a, PR #2641, issue #2636).
//
// This is a property of the SOFTWARE, not of any particular database, which is
// why it is a constant rather than something derived from the data. It is used
// in explanatory text ONLY. Deliberately no query filters on it: if this date
// were ever wrong, the consequence must be misleading prose that a reader can
// question, never a silently shorter list of destroyed values.
const AttributionCutoffDate = "2026-07-19"
