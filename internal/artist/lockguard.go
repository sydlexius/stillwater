package artist

import (
	"context"
	"log/slog"
	"slices"
	"strings"
)

// Per-field lock enforcement on the PERSIST path (issue #3037).
//
// # WHY THIS FILE EXISTS
//
// Artist.LockedFields used to be consulted on exactly one WRITE path in this
// package: ApplyMetadata -> applyFields. The other reader, Service.IsFieldLocked,
// is a query the CALLER must remember to ask. So a writer reaching the database
// without the merge engine, and without that call, bypassed per-field locks. The
// rule fixers are the reachable case: Pipeline.FixViolation checks only the
// artist-level a.Locked flag, the fixers assign onto the *Artist struct, and the
// pipeline persists the whole struct through Service.Update.
//
// The durable answer is not an IsFieldLocked call in each fixer -- that is
// site-patching a class that already regenerated once (#2748/#2754). It is a
// CHOKEPOINT on the whole-row persist: Service.update is the single funnel every
// whole-row writer ends in, so a fixer added tomorrow is covered without its
// author knowing this file exists.
//
// # THE LOCK SET COMES FROM THE STORED ROW, AND SO DOES THE LOCK STATE
//
// A caller handing over an Artist whose LockedFields it forgot to populate must
// not thereby switch off the operator's protection, so the guard re-reads the
// locks from the stored row and ignores what the incoming struct claims.
//
// Pinning the lock STATE as well as restoring locked VALUES is what makes that
// last: sqliteArtistRepo.Update writes the whole row, lock columns included, so
// value-only protection would survive exactly one write before the next
// automated write landed unguarded. It costs no legitimate caller anything --
// the lock mutators own targeted SQL and never come through here, and the one
// production writer setting these on an *Artist bound for this path (the
// scanner's NFO lockdata import) goes through Create, not Update.
//
// # WHAT THIS CHOKEPOINT DOES NOT COVER
//
// A narrowed guard that reads as complete is worse than no guard, because an
// operator who believes a field is protected stops watching it. Each item below
// is out of scope for THIS unit.
//
// PROVIDER-ID FIELDS ARE NOT GUARDED. They live in artist_provider_ids, not on
// the artists row, and a bare Repository.GetByID does not populate them. An
// un-hydrated comparison would read the stored value as EMPTY and "restore" ""
// over a real ID -- extending the guard to them without hydration is a DATA-LOSS
// PATH for the fields it claims to protect. The follow-up unit adds the
// hydration and widens the set.
//
// THE SINGLE-COLUMN WRITE VERBS ARE NOT GUARDED. UpdateField, ClearField and
// UpdateNameGuarded each write their own targeted SQL and never pass through
// here. Their callers include the operator's history revert and blast-radius
// restore, which are deliberately lock-blind (see trackableFields in
// service.go), so guarding them needs the scoped operator-grant mechanism that
// is a separate unit of work.
//
// MEMBERS ARE NOT GUARDED. No Artist field holds them -- band members live in
// their own table. A "members" lock is honored by applyMemberRefresh on the
// refresh path, not here.
//
// RESTORE-AND-CONTINUE CAN SPLIT A FIELD PAIR A SINGLE WRITER SET ATOMICALLY.
// Unlike the entries above, this is a state the guard CREATES. internal/rule's
// name_language_pref fixer promotes Name and SortName together; lock only "name"
// and the row afterwards holds the old name beside the new sort_name. It follows
// from per-FIELD locking, and refusing the whole write would discard the
// unlocked change instead. A pair that is one LOGICAL value needs restoring
// together, which is what the follow-up unit does for a provider ID and its
// fetched-at timestamp.

// lockGuardedFields is every field this codebase treats as a meaningful lock
// token AND can read, compare and restore from a bare Repository.GetByID.
//
// Derived from lockableFieldNames (merge.go) rather than written by hand, so
// adding a lockable field anywhere cannot silently leave the chokepoint behind.
// Two exclusions, both derived so neither can drift: the provider-ID fields (not
// on the artists row -- see the file comment on why guarding them un-hydrated
// loses data), and FieldMembers (no Artist field holds it).
//
// Sorted, so the restoration log and the returned slice have a stable order --
// lockableFieldNames is a map and Go randomizes map iteration.
var lockGuardedFields = func() []FieldName {
	out := make([]FieldName, 0, len(lockableFieldNames))
	for name := range lockableFieldNames {
		// Band members are a separate relation (band_members table), written
		// by UpsertMembers / DeleteMembersByArtistID. No Artist field holds
		// them, so this guard cannot see or restore them.
		if name == string(FieldMembers) {
			continue
		}
		// Provider IDs live in artist_provider_ids and are absent from an
		// un-hydrated stored artist. Guarding them here would restore an empty
		// ID over a real one.
		if _, isProviderID := providerFieldMap[name]; isProviderID {
			continue
		}
		out = append(out, FieldName(name))
	}
	slices.Sort(out)
	return out
}()

// enforceLocksBeforeUpdate is Service.update's entry point into the guard.
//
// A thin wrapper today: every guarded field is on the artists row, so the stored
// artist Service.update already fetched is a complete comparison basis. It
// exists because the follow-up unit widens the set to the provider-ID fields,
// which need a hydration step and a refusal when it cannot run -- and that
// belongs here, not inside enforceFieldLocks, which stays a pure in-memory
// comparison.
//
// A nil on either side means there is nothing to compare. Service.update passes
// a nil stored ONLY for an artist that does not exist; every other read failure
// is refused before reaching here, because an unverifiable lock must not be
// treated as an absent one.
func (s *Service) enforceLocksBeforeUpdate(ctx context.Context, stored, incoming *Artist) error {
	if stored == nil || incoming == nil {
		return nil
	}
	enforceFieldLocks(ctx, stored, incoming)
	return nil
}

// enforceFieldLocks restores, onto incoming, every field the STORED artist has
// locked and the incoming struct would have changed. It returns the restored
// names in lockGuardedFields order, which is sorted and therefore stable.
//
// No production caller reads that return value yet. It is kept because the
// follow-up unit consumes it: the rule pipeline collects the restored names to
// tell "my fix landed" from "the guard reverted it", and reports the latter as
// dismissed rather than resolved. The ordering is pinned by
// TestEnforceFieldLocks_ReturnsRestoredNamesInStableOrder so the contract is
// tested here rather than assumed there.
//
// RESTORE-AND-CONTINUE rather than refuse. A whole-row persist has no natural
// refusal: the caller handed over an entire artist, most of it legitimate.
// Refusing would discard the unlocked changes too and turn every fixer into an
// all-or-nothing write. The single-field verbs are the ones with a natural
// refusal, and they refuse (see UpdateProviderField).
//
// The incoming struct is mutated in place. That is the point: the caller
// persists it, and its in-memory copy carries the protected value onward, so a
// downstream NFO write or platform publish in the same request does not ship the
// rejected value.
//
// Every restoration is logged at ERROR with artist_id, field, source and the
// rejected value's LENGTH -- never the value. ERROR rather than WARN is a
// decision, not an inheritance: unlike a caller going away, a restoration means
// two explicit operator intents are in conflict (a field is pinned AND a rule
// that writes it runs in auto mode), and that conflict recurs every pass until
// someone re-scopes the rule or unlocks the field. It is exactly the condition
// an operator must see. source is "rule:<ruleID>", so two rules colliding on one
// field in the same pass are still told apart.
func enforceFieldLocks(ctx context.Context, stored, incoming *Artist) []string {
	if stored == nil || incoming == nil {
		return nil
	}
	pinLockState(stored, incoming)

	// The stored row is the ONLY source of the lock set. See the file comment.
	//
	// reportUnenforceableLocks is deliberately NOT called here, unlike on the
	// merge path. It flags tokens absent from lockableFieldNames, but every
	// token this chokepoint declines to guard -- the six provider IDs and
	// members -- IS in that set and IS enforced elsewhere, so calling it would
	// report nothing about them while making the site look as though unenforced
	// locks were handled. A genuinely unknown token is worth reporting, but not
	// once per whole-row persist: this runs on every rule pass, so a permanently
	// misspelled token would log forever. The merge path already reports it on
	// refresh, bulk fetch and NFO import. Validating at the lock-SETTING API is
	// the fix that reports once; it is not this unit's.
	locked := buildLockedSet(stored.LockedFields)
	if len(locked) == 0 {
		return nil
	}
	source := sourceFromContext(ctx)

	var restored []string
	for _, f := range lockGuardedFields {
		name := string(f)
		if !isLocked(locked, name) {
			continue
		}
		rejectedLen, changed := restoreLockedField(stored, incoming, name)
		if !changed {
			continue
		}
		restored = append(restored, name)
		slog.Error("refused an automated write to a locked artist field; the stored value was restored",
			"artist_id", stored.ID,
			"field", name,
			"rejected_len", rejectedLen,
			"source", source)
	}
	return restored
}

// pinLockState copies the stored row's LOCK STATE onto incoming, so no
// CALLER-SUPPLIED lock state can change what is locked.
//
// THAT SCOPE IS EXACT: it does NOT say a whole-row persist can never change what
// is locked. The state pinned here is the one read into stored by
// Service.update's snapshot, so a lock an operator takes after that read and
// before the write commits is reverted. That TOCTOU window pre-dates this change
// and closing it needs the write to re-read or lock the row -- out of scope for
// this unit. What this DOES buy is that no caller can switch protection off by
// handing over a struct with its locks cleared, which is the attack the rule
// fixers actually present: they build a struct, never populate lock state, and
// persist it.
//
// Unconditional: an incoming struct that ADDS locks is overridden for the same
// reason as one that removes them, because the persist path is not where lock
// intent is expressed.
//
// Silent, unlike a restored value. This fires on nearly every update in the
// system, so logging here would bury the restorations that matter. The property
// is pinned by the two-write test instead.
func pinLockState(stored, incoming *Artist) {
	// Cloned rather than aliased: the caller persists incoming and may hold
	// stored afterwards, and a shared backing array (or a shared *time.Time)
	// would let a later mutation of one silently rewrite the other.
	incoming.LockedFields = slices.Clone(stored.LockedFields)
	incoming.Locked = stored.Locked
	incoming.LockSource = stored.LockSource
	if stored.LockedAt == nil {
		incoming.LockedAt = nil
		return
	}
	at := *stored.LockedAt
	incoming.LockedAt = &at
}

// restoreLockedField copies one field's stored value onto incoming when the two
// differ. It reports the LENGTH of the rejected incoming value and whether a
// restoration happened.
//
// THE REJECTED VALUE ITSELF NEVER LEAVES THIS FUNCTION. It is arbitrary artist
// metadata -- a full biography in the worst case -- and internal/logging's
// redactor matches an allowlist of credential-ish key names, so anything handed
// to a log attribute here would be written verbatim. Returning a length rather
// than the text means a future caller cannot log the payload by accident.
//
// The length is kept because ZERO is the case that matters: a rule CLEARING a
// pinned field (fixJunkBio blanks a biography before re-querying providers) is
// destroying curated data, while a non-zero length is an attempted overwrite.
// An operator triages those differently, and a length is not user text.
//
// Slice fields are compared and restored as SLICES, not in their joined string
// form. Round-tripping through a comma-join would reorder or split entries
// whose own text contains a comma, making the guard a data-loss path for
// exactly the fields it is protecting. The reported length is the joined
// rendering, so an emptied slice reads as zero exactly like an emptied scalar.
func restoreLockedField(stored, incoming *Artist, field string) (rejectedLen int, changed bool) {
	if IsSliceField(field) {
		want := SliceFieldFromArtist(stored, field)
		got := SliceFieldFromArtist(incoming, field)
		if slices.Equal(want, got) {
			return 0, false
		}
		setFieldOnArtist(incoming, field, "", slices.Clone(want))
		return len(strings.Join(got, ", ")), true
	}
	want := FieldValueFromArtist(stored, field)
	got := FieldValueFromArtist(incoming, field)
	if want == got {
		return 0, false
	}
	setFieldOnArtist(incoming, field, want, nil)
	return len(got), true
}

// setFieldOnArtist writes one named field back onto an Artist, the inverse of
// FieldValueFromArtist / SliceFieldFromArtist for the ARTISTS-ROW fields. A
// field outside it is a no-op, matching those readers' default branch.
//
// It deliberately has NO provider-ID arm: those fields are not in
// lockGuardedFields, so an arm here would be unreachable code implying coverage
// this unit does not provide. TestUpdate_RestoresEveryGuardedField drives every
// entry in lockGuardedFields through this switch, so a field entering the set
// without a matching arm fails there rather than silently no-opping.
//
// Scalar callers pass value and a nil slice; slice callers pass "" and the
// slice. One function rather than two avoids a second field switch that could
// drift from this one.
func setFieldOnArtist(a *Artist, field, value string, slice []string) {
	switch field {
	case "genres":
		a.Genres = slice
	case "styles":
		a.Styles = slice
	case "moods":
		a.Moods = slice
	case "biography":
		a.Biography = value
	case "formed":
		a.Formed = value
	case "born":
		a.Born = value
	case "disbanded":
		a.Disbanded = value
	case "died":
		a.Died = value
	case "years_active":
		a.YearsActive = value
	case "type":
		a.Type = value
	case "gender":
		a.Gender = value
	case "origin":
		a.Origin = value
	case "name":
		a.Name = value
	case "sort_name":
		a.SortName = value
	case "disambiguation":
		a.Disambiguation = value
	}
}
