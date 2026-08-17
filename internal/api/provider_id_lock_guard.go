package api

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/sydlexius/stillwater/internal/artist"
)

// refuseLockedProviderIDs answers 423 Locked when any provider-ID field this
// request would write is pinned, and reports whether it did.
//
// 423 rather than 409, and the choice is the house shape rather than a novel
// one: handlers_artist_duplicates.go:414 already answers 423 for a merge
// refused on a lock. It is also the only ADDITIVE option here. The Deezer link
// route already documents a 409 for the conflict gate, so expressing the lock
// refusal as a second 409 would restructure that response into a oneOf --
// converting "this field is guaranteed" into "this field might be present",
// which oasdiff correctly reports as breaking. A new status code is additive;
// widening an existing one is not.
//
// The two older match-by-name flows (handlers_discogs.go, handlers_audiodb.go)
// still answer 409 for their own lock refusal. That inconsistency is known and
// left for the unit that revisits them; the body shape is identical, so a
// client discriminating on the "error" key handles both.
//
// WHY A REFUSAL RATHER THAN A GRANT. These are link / identify / re-identify
// flows: the operator is choosing a NEW identity for the artist, which is
// exactly what a lock on the identity field says not to change. That differs
// from the field-edit API, where the operator is correcting the pinned value
// itself and carries a scoped grant. It also matches what the Discogs and
// TheAudioDB match-by-name flows already do (handlers_discogs.go,
// handlers_audiodb.go), so all six provider IDs now behave alike here.
//
// The alternative -- letting the write reach the persist chokepoint -- answers
// 200 while the guard silently reverts the value, which tells the operator their
// link succeeded when nothing was stored.
//
// THE CALLER'S CONTRACT: pass every field this request WRITES, by any route --
// not every field its body carries. Those differ. handleRefreshLink's
// re-identify discard clears musicbrainz_id with no body value at all, so a
// check keyed on the body would skip the field on exactly the path that erases
// it. providerIDFieldIf is a convenience for the common "writes it iff the body
// supplies it" case, not the rule.
//
// Call this BEFORE mutating the artist. A refusal after the fact still returns
// 409, but leaves the in-memory artist carrying values the operator was told
// were rejected -- and any later code reading that struct sees the write.
//
// Only the fields the request writes are checked, so a pinned Discogs ID does
// not block an MBID-only link.
func (r *Router) refuseLockedProviderIDs(w http.ResponseWriter, a *artist.Artist, fields ...artist.FieldName) bool {
	for _, f := range fields {
		if f == "" || !r.artistService.IsFieldLocked(a, f) {
			continue
		}
		writeJSON(w, http.StatusLocked, map[string]any{
			"error":  "field_locked",
			"field":  string(f),
			"reason": "the " + string(f) + " field is locked; unlock it before changing this artist's identity",
		})
		return true
	}
	return false
}

// writeFieldLockRefusal answers 423 Locked when err is a service-layer lock
// refusal, and reports whether it did. A caller passes every error from a
// provider-field write verb and falls through to its own handling on false.
//
// SAME STATUS AND SAME BODY SHAPE as refuseLockedProviderIDs above, deliberately:
// an operator meeting a lock refusal on the field-edit API and on a link flow is
// meeting the same condition, and a client discriminating on the "error" key
// should not need to know which layer noticed.
//
// THE REASON COMES FROM THE TYPED ERROR, never from err.Error() on an untyped
// one. artist.FieldLockedError.Reason is a hand-authored sentence chosen for an
// operator; a rendered error chain can carry wrapped driver text, a column name
// or an id. This is the same rule fieldRefusalReason states for the validation
// error, and the reason the errors.As below is not a string match.
//
// A NON-REFUSAL ERROR RETURNS FALSE and is left entirely alone -- the caller
// still logs it and still answers 500. This function never swallows an error it
// did not recognize.
//
// NOT REACHABLE FROM TODAY'S TWO CALL SITES, AND SAID SO PLAINLY RATHER THAN
// IMPLIED. handleFieldUpdate and handleFieldClear each wrap the context with
// artist.ContextWithLockOverride for the SAME field they then write, and the
// field name is an exact lowercase key from providerFieldMap (IsEditableField is
// a map lookup, so no case variant reaches here), so the grant always matches and
// the service verb never refuses them. This is therefore NOT a new operator-facing
// status on those routes and openapi.yaml deliberately does not claim one for
// them. What it buys is that the day a caller of those verbs arrives WITHOUT a
// grant -- or a future edit drops one -- the operator sees a lock refusal instead
// of a 500 that reads as a server fault. The alternative, leaving the 500, is a
// silent misclassification waiting on a one-line change.
func (r *Router) writeFieldLockRefusal(w http.ResponseWriter, artistID string, err error) bool {
	var le *artist.FieldLockedError
	if !errors.As(err, &le) {
		return false
	}
	r.logger.Info("refused a field write: the operator has that field locked",
		slog.String("artist_id", artistID),
		slog.String("field", le.Field))
	writeJSON(w, http.StatusLocked, map[string]any{
		"error":  "field_locked",
		"field":  le.Field,
		"reason": le.Reason,
	})
	return true
}

// providerIDFieldIf returns the lock key for field when value is non-empty, and
// "" otherwise, so a caller can pass only the fields its request will write.
func providerIDFieldIf(value string, field artist.FieldName) artist.FieldName {
	if value == "" {
		return ""
	}
	return field
}
