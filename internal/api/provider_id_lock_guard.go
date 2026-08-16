package api

import (
	"net/http"

	"github.com/sydlexius/stillwater/internal/artist"
)

// refuseLockedProviderIDs answers 409 when any provider-ID field this request
// would write is pinned, and reports whether it did.
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
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":  "field_locked",
			"field":  string(f),
			"reason": "the " + string(f) + " field is locked; unlock it before changing this artist's identity",
		})
		return true
	}
	return false
}

// providerIDFieldIf returns the lock key for field when value is non-empty, and
// "" otherwise, so a caller can pass only the fields its request will write.
func providerIDFieldIf(value string, field artist.FieldName) artist.FieldName {
	if value == "" {
		return ""
	}
	return field
}
