package artist

import (
	"context"
	"strings"
)

// history_producer.go -- the producer vocabulary for metadata_changes
// (issue #3078, PR 1 of 3).
//
// `source` (history.go's validHistorySource) answers WHAT TRIGGERED a write:
// the operator's own write path ("manual"), a scan, a revert, a rule pass.
// `producer` answers a different question: WHAT SUPPLIED THE VALUE. The two
// are independent. A Pull from Emby is operator-TRIGGERED (source="manual")
// and platform-AUTHORED (producer="platform:emby"); neither fact implies the
// other, and collapsing them into one column is exactly the design this file
// avoids -- see migration 029's header for the full argument.
//
// THIS FILE SHIPS THE VOCABULARY ONLY. Nothing in this PR (#3078 PR 1) calls
// ContextWithProducer or ContextWithFieldProducers from any write-path call
// site. Every row this PR writes carries producer="". Stamping the real
// producer at each write path (refresh, platform pull, field-update modal,
// rule engine, scanner, restore) is #3078's PR 2 and PR 3.
//
// THE EMPTY STRING IS THE DEFAULT, AND IT IS NOT "operator". This is the
// single most load-bearing decision in this file. "" means "the writer did
// not record this" -- it makes NO claim about who or what produced the
// value, so it cannot be a WRONG claim. Defaulting to ProducerOperator would
// back-fill a guess into every historical row, and into every future write
// path someone forgets to stamp, and the guess would point in the single
// most damaging direction available: laundering an automated write as a
// human decision. That is the "unknown rendered as clean" failure this whole
// area of the codebase (lockguard.go, sqlite_history.go's blast-radius
// predicates) exists to stop.
//
// A consequence worth stating plainly: a metadata_changes row written AFTER
// migration 029 with producer = "" is a BUG IN THE WRITER, not a fact about
// the operator -- every write path is expected to stamp a producer once PR 2
// and PR 3 land. created_at makes those findable without new machinery: any
// row with a post-029-deploy created_at and producer = "" is a write path
// nobody stamped yet.
const (
	// ProducerUnrecorded is the default and the only value this PR ever
	// writes. "The writer did not say." Never treat it as "the operator", nor
	// as "automated" -- it is neither claim.
	ProducerUnrecorded = ""

	// ProducerOperator attributes the VALUE ITSELF to a human: the operator
	// typed or chose it. Distinct from source="manual", which only says the
	// write went through the operator write path -- a provider refresh also
	// goes through that path and must NOT be stamped ProducerOperator.
	ProducerOperator = "operator"

	// ProducerRestore attributes a write to a previously-stored value being
	// put back (an Undo / blast-radius restore / the locked-field damage
	// repair). It deliberately asserts NO authorship: the operator asked for
	// the value back, but the text being restored may itself have been
	// provider-supplied. "restore" is the only honest token available for
	// that case.
	ProducerRestore = "restore"

	// ProducerNFO attributes a value to text read out of an NFO file on disk
	// during a library scan.
	ProducerNFO = "nfo"

	// ProducerFilesystem attributes a value to something OBSERVED on disk
	// rather than read as text -- image presence flags, counts. Distinct from
	// ProducerNFO because it is not sourced from parsed NFO text.
	ProducerFilesystem = "filesystem"

	// The following prefixes are documented here, not minted as constants,
	// because the suffix varies per call site (a provider name, a platform
	// type, a rule id) and a constant per value would not scale:
	//
	//   "provider:<name>"  a metadata provider supplied the value (e.g.
	//                       "provider:musicbrainz"). The bare prefix
	//                       "provider:" (no name) is valid too, for the case
	//                       where a provider refresh cleared or produced a
	//                       field but no single provider's FieldSource names
	//                       it -- see the #3078 plan's write-path inventory
	//                       for why that case exists and cannot be avoided.
	//   "platform:<type>"  a connected platform (Emby, Jellyfin) supplied the
	//                       value via a pull.
	//   "rule:<rule_id>"   a rule engine pass supplied the value. Mirrors the
	//                       source token for rule writes deliberately: it
	//                       lets a future repair predicate read ONE column
	//                       (producer LIKE 'rule:%' OR ... ) instead of a
	//                       union of source and producer.
)

// producerCtxKey is the type for the context keys this file defines. A
// distinct type from artist.ctxKey (service.go) is not needed -- ctxKey is
// already package-private and shared by sourceKey/historyIDKey, so the
// producer keys reuse it for the same reason those do: no cross-package key
// collisions are possible with an unexported string-backed type.
const (
	producerKey       ctxKey = "history_producer"
	fieldProducersKey ctxKey = "history_field_producers"
)

// ContextWithProducer returns a child context that carries a single scalar
// producer value, applied to every field a write touches. Use this for a
// write path where one producer covers the whole change (e.g. a platform
// pull, which supplies every field it writes from the same platform).
//
// Mirrors ContextWithSource (service.go) in shape and intent: the value
// travels on the context so callers don't need to thread it through every
// method signature, in particular HistoryService.Record's, which this issue
// requires stay unchanged (see history.go).
func ContextWithProducer(ctx context.Context, producer string) context.Context {
	return context.WithValue(ctx, producerKey, producer)
}

// ContextWithFieldProducers returns a child context that carries a per-field
// producer overlay. Use this for a write path where different fields in the
// same operation can have different producers -- the refresh path is the
// motivating case: result.Sources names a provider per field, and fields a
// provider refresh emptied (with no FieldSource at all) need a different
// producer than fields it populated.
//
// The map is used as-is; callers own copying it if they need to reuse it
// after this call.
func ContextWithFieldProducers(ctx context.Context, producers map[string]string) context.Context {
	return context.WithValue(ctx, fieldProducersKey, producers)
}

// producerForField resolves the producer for a single field write: the
// per-field overlay (ContextWithFieldProducers) takes precedence when it
// names this field, then the scalar (ContextWithProducer), then
// ProducerUnrecorded when neither is set.
//
// This PR (#3078 PR 1) adds no caller that puts either value on a context, so
// every call in this PR's tree returns ProducerUnrecorded. The resolution
// order exists now so PR 2/PR 3 can stamp real producers without touching
// this function or any of its callers again.
func producerForField(ctx context.Context, field string) string {
	if overlay, ok := ctx.Value(fieldProducersKey).(map[string]string); ok {
		if p, ok := overlay[field]; ok {
			return p
		}
	}
	if p, ok := ctx.Value(producerKey).(string); ok {
		return p
	}
	return ProducerUnrecorded
}

// validHistoryProducer reports whether producer is one of the recognized
// metadata-change producer values. Allows the empty string (the only value
// this PR writes), the four named constants above, and the "provider:",
// "platform:" and "rule:" prefixes (including the bare "provider:" prefix
// with no suffix -- see the doc block above).
//
// An invalid producer is NOT a reason to fail a write. Callers that validate
// (PR 2's untrusted-input path) must degrade an unrecognized value to "" and
// log the rejected token, never reject the whole write -- a history row lost
// because someone typo'd a producer is a worse outcome than an unattributed
// one. This function only answers the yes/no question; degrading on "no" is
// the caller's job.
func validHistoryProducer(producer string) bool {
	switch producer {
	case ProducerUnrecorded, ProducerOperator, ProducerRestore, ProducerNFO, ProducerFilesystem:
		return true
	}
	// The bare "provider:" is deliberately valid; the bare "platform:" and
	// "rule:" are not. A provider refresh that EMPTIED a field has no
	// FieldSource and so no provider name to record -- "provider:" attributes
	// it honestly without inventing one (see the doc block above). A platform
	// type and a rule ID, by contrast, are always known at their write site,
	// so a bare prefix there is a caller bug and must not validate as a
	// complete token.
	if strings.HasPrefix(producer, "provider:") {
		return true
	}
	if after, ok := strings.CutPrefix(producer, "platform:"); ok {
		return after != ""
	}
	if after, ok := strings.CutPrefix(producer, "rule:"); ok {
		return after != ""
	}
	return false
}
