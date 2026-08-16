package rule

import (
	"context"

	"github.com/sydlexius/stillwater/internal/artist"
)

// Source attribution for rule-engine writes (issue #3037).
//
// The artist writes this package makes through artist.Service.Update (and the
// run paths' UpdateAfterRuleEvaluation) land in artist.Service.update,
// which records a metadata_changes row for each changed field in
// artist.trackableFields, sourced from the CONTEXT
// (artist.sourceFromContext). Untagged, that source defaults to "manual", and
// the blast-radius report's classifier (classifyBlastAttribution in
// internal/artist/sqlite_history.go) then buckets a rule-caused overwrite as
// UNKNOWN -- indistinguishable from an operator's own edit. That is what makes
// rule damage unattributable after the fact, and it is why these writes are
// stamped rather than left to the default.
//
// Narrower single-column writers in this package do NOT reach that diff at all
// and so are outside this policy: RenameDirectory (fixers.go, and see the
// comment on artist.Service.RenameDirectory for why it avoids s.update),
// UpdateImageProvenance, UpdateImageHashes and UpdateHealthScore.
//
// WHICH WRITES ACTUALLY PRODUCE A ROW is narrower than which ones are tagged,
// and the difference is deliberate. trackableFields covers the text metadata
// fields only, so an image-slot or MusicBrainz-ID write diffs nothing and its
// tag is inert today. Those writes are tagged regardless (the lone deliberate
// exception is the deprecated EvaluateAndPersistHealth in helpers.go, which
// carries its own note), because whether a row appears
// is a property of that field list -- which grows -- and not of the call site.
// A tag placed only where a row is produced today is a tag that silently stops
// covering the site the day the list widens.
//
// The inert sites are untested BY CONSTRUCTION: producing no history row, they
// give a test nothing to assert a source against, so the green suite is not
// evidence that their tags are correct. Only the sites that do produce a row
// (FixViolation, the run paths, and the bulk fetch-metadata write) are guarded
// by tests in history_source_test.go.
//
// The "rule:" prefix is load-bearing twice over. artist.HistoryService.Record
// validates its source against an exact allow-list plus the "provider:" and
// "rule:" prefixes and REJECTS anything else, and
// blastAutomatedSourcePrefixes/classifyBlastAttribution recognize the same
// prefix, so a prefixed source lands in the AUTOMATED bucket instead of the
// unknown one. A future recovery query can likewise select on
// `source LIKE 'rule:%'`.
//
// The value after the prefix is an id-shaped token -- a catalogue RULE ID where
// one applies, otherwise an operation name in the same snake_case shape (see
// ruleHistorySourceMultiple and the phash constants below) -- never a
// human-readable rule name. The activity feed renders it verbatim
// (historySourceLabel in web/templates/artist_history.templ trims the prefix
// and interpolates the remainder), matching the existing
// artist.NFOMBIDReportSource ("rule:nfo_has_mbid"). A display name would
// neither resolve nor match those.

const (
	// ruleHistorySourceMultiple attributes a batched writeback that more than
	// one rule's fixer contributed to.
	//
	// The run paths persist the artist ONCE after every fixer for that artist
	// has run, so when two rules each mutated a different field the single
	// write cannot say which rule changed which field. Naming one of them would
	// be a confident wrong answer on a recovery surface; this says "a rule pass
	// did it, and which one is not recoverable from this row" instead. It still
	// carries the prefix, so the row is attributed to the rule engine and is
	// selectable by a recovery query.
	ruleHistorySourceMultiple = "rule:multiple_rules"

	// ruleHistorySourceBulkFetchMetadata attributes the bulk fetch-metadata
	// job's artist write. That job applies provider metadata to the text fields
	// unattended and library-wide, which makes it the write in this package
	// most likely to produce history rows at scale. Named after
	// BulkTypeFetchMetadata.
	ruleHistorySourceBulkFetchMetadata = "rule:bulk_fetch_metadata"

	// ruleHistorySourceBulkFetchImages attributes the bulk fetch-images job's
	// artist write. Named after BulkTypeFetchImages, and a sibling of the
	// existing "rule:bulk_fetch_images_mbid" that job already records for the
	// MusicBrainz ID it self-heals.
	ruleHistorySourceBulkFetchImages = "rule:bulk_fetch_images"

	// ruleHistorySourcePHashRemediate and ruleHistorySourcePHashRestore
	// attribute the perceptual-hash maintenance passes. Neither is a catalogue
	// rule id: they are operator-initiated repair operations that live in this
	// package and write through the same artist service, so they are named for
	// the operation rather than borrowed from a rule they do not run.
	ruleHistorySourcePHashRemediate = "rule:phash_mismatch_remediate"
	ruleHistorySourcePHashRestore   = "rule:phash_quarantine_restore"
)

// ruleHistorySource builds the source value for a write caused by one rule.
func ruleHistorySource(ruleID string) string {
	return "rule:" + ruleID
}

// withRuleHistorySource returns ctx tagged with source, or ctx unchanged when
// source is empty.
//
// The empty case is not a defensive no-op: a run that fixed nothing still
// persists the artist to store a recomputed health score, and tagging that
// write would attribute a rule fix to a pass that made none. It also leaves an
// already-tagged ctx (FixViolation stamps its own before re-entering the shared
// persist helper) alone rather than re-stamping it.
func withRuleHistorySource(ctx context.Context, source string) context.Context {
	if source == "" {
		return ctx
	}
	return artist.ContextWithSource(ctx, source)
}
