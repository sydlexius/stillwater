package artist

// Source-attribution values for the identify pipeline's MusicBrainz-ID writes
// (issue #2845). Every automated identity write records a metadata_changes row
// so an operator can enumerate what an automated pass changed, and -- when it
// replaced a stored ID -- what it replaced.
//
// The prefix is load-bearing, not decoration. HistoryService.Record validates
// its source argument against an exact allow-list ("manual", "scan", "import",
// "revert") plus the "provider:" and "rule:" PREFIXES, and rejects anything
// else with an invalid-source error. A bare "identify_connection" would fail
// that check at runtime, so these carry "provider:". Widening Record's
// allow-list instead would be a contract change rippling into the
// blast-radius attribution SQL (blastAutomatedSourcePrefixes in
// sqlite_history.go), and is deliberately not done here.
//
// The "provider:" prefix also puts all three in the AUTOMATED bucket of the
// blast-radius report for free, which is correct: these are machine writers.
//
// They live in internal/artist rather than internal/api so a reader of the
// history store (the rule-written-MBID and blast-radius reports) imports a
// vocabulary instead of pasting string literals -- the same reason
// NFOMBIDReportSource lives here.
//
// Each value is distinguishable from the other two by suffix, and from the
// rule-fixer sources ("rule:nfo_has_mbid", "rule:bulk_fetch_images_mbid") by
// prefix, so a report can attribute a write to the exact tier that made it.
const (
	// IdentifySourceConnection attributes an MBID adopted from a connected
	// platform's own library index (identify Tier 1).
	IdentifySourceConnection = "provider:identify_connection"

	// IdentifySourceAlbum attributes an MBID adopted after comparing the
	// artist's local album directories against a candidate's release groups
	// (identify Tier 2).
	IdentifySourceAlbum = "provider:identify_album"

	// IdentifySourceName attributes an MBID adopted from a provider name
	// search that cleared the shared confidence gate (identify Tier 3).
	IdentifySourceName = "provider:identify_name"

	// IdentifySourceOperator attributes an identity a HUMAN chose, which is
	// exactly what Record's existing "manual" source documents. It is spelled
	// out here so the operator path reads from the same vocabulary as the
	// automated ones, and so nobody mints a fourth "provider:identify_link"
	// constant: that would misfile a human decision as automated damage in
	// the blast-radius report.
	IdentifySourceOperator = "manual"
)
