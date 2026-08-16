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
// package: ApplyMetadata -> applyFields (merge.go, via buildLockedSet). The
// package's other reader, Service.IsFieldLocked, is a query the CALLER has to
// remember to ask -- its production callers are internal/api's Discogs and
// AudioDB identify guards and the refresh path's applyProviderName, and each is
// a hand-placed check on one surface rather than anything on the persist path.
// Established by `grep -rn 'buildLockedSet\|IsFieldLocked(' internal --include='*.go' | grep -v _test`.
// So a writer that reached the database WITHOUT going through the merge engine,
// and without its author remembering to call IsFieldLocked, bypassed the
// operator's per-field locks entirely. The rule fixers are the reachable case:
// Pipeline.FixViolation checks only the ARTIST-level a.Locked flag, the fixers
// then assign straight onto the *Artist struct (a.Biography = ..., a.Name =
// ...), and the pipeline persists the whole struct through Service.Update. A
// pinned biography was overwritten, and fixJunkBio cleared one outright before
// re-querying providers.
//
// The durable answer is not "add an IsFieldLocked call to each fixer". That is
// site-patching a class that has already regenerated once (#2748/#2754 gated
// six surfaces one at a time). It is a CHOKEPOINT on the whole-row persist:
// Service.update is the single funnel every whole-row writer ends in. Its only
// callers are Service.Update and Service.UpdateAfterRuleEvaluation, which are
// in turn the rule package's only whole-row write verbs. Established by
// `grep -rn 's\.update(' internal/artist/*.go` -- two calls, plus one comment
// at the path-only rename explaining why THAT path deliberately writes a single
// column instead -- and
// `grep -rnoE 'artistService\.[A-Za-z]+' internal/rule --include='*.go' | grep -v _test`,
// whose only mutating entries are Update, UpdateAfterRuleEvaluation, MarkDirty,
// MarkRulesEvaluated, UpdateHealthScore and ReconcileImages (the last four
// write their own targeted columns, none of them lockable). So a fixer added
// tomorrow is covered without its author knowing this file exists.
//
// # THE LOCK SET COMES FROM THE STORED ROW, NEVER FROM THE INCOMING STRUCT
//
// A caller handing over an Artist whose LockedFields it forgot to populate (or
// deliberately zeroed) must not thereby switch off the operator's own
// protection. The guard re-reads the locks from the row already in the database
// and ignores whatever the incoming struct claims. Safe behavior is what you
// get by doing nothing.
//
// # THE LOCK STATE ITSELF IS PINNED, NOT JUST THE LOCKED VALUES
//
// Reading the lock set from the stored row is only half the property, and
// without the other half it is not a property at all. sqliteArtistRepo.Update
// writes the WHOLE row -- locked_fields and the artist-level locked /
// lock_source / locked_at columns included, in the same statement as every
// metadata column. So a caller handing over an Artist with those zeroed would
// have its value write reverted AND the lock state erased at once, and the NEXT
// automated write would land completely unguarded. The guarantee would hold for
// exactly one write. pinLockState therefore copies all four columns off the
// stored row unconditionally.
//
// Taking that ability away costs no legitimate caller anything, because the
// legitimate lock mutators own their own targeted SQL and never come through
// here:
//
//   - locked_fields -> Service.SetLockedFields / AddLockedField /
//     RemoveLockedField, via sqliteArtistRepo.SetLockedFields, which writes
//     locked_fields and updated_at only.
//   - locked / lock_source / locked_at -> Service.Lock / Unlock, via
//     sqliteArtistRepo.SetLock, which writes those three and updated_at only,
//     under a WHERE precondition on the prior state.
//
// The one production writer that does set these fields on a struct is the
// scanner's NFO lockdata import (internal/scanner/scanner.go), and it reaches
// the database through Service.Create, not Update, so it is unaffected.
// Established by `grep -rn '\.LockedFields = \|\.Locked = \|\.LockSource = ' internal cmd --include='*.go' | grep -v _test`.
//
// # WHAT THIS CHOKEPOINT DOES NOT COVER
//
// THE LIST BELOW IS THE POINT OF THIS SECTION. A narrowed guard that reads as
// complete is worse than no guard, because an operator who believes a field is
// protected stops watching it. Everything here is deliberately out of scope for
// THIS unit and named so a reader cannot mistake the narrowing for coverage.
//
// PROVIDER-ID FIELDS ARE NOT GUARDED. musicbrainz_id, audiodb_id, discogs_id,
// wikidata_id, deezer_id and spotify_id are lockable tokens, but they live in
// the normalized artist_provider_ids table rather than on the artists row --
// the UPDATE statement in sqliteArtistRepo.Update carries no provider-ID
// column, and a bare Repository.GetByID does not populate them. Comparing an
// un-hydrated stored value against an incoming one would read the stored value
// as EMPTY and "restore" "" over a real ID, so a guard extended to them without
// hydration is not a weaker guard, it is a DATA-LOSS PATH for exactly the
// fields it claims to protect. Excluding them is the safe state; guarding them
// unhydrated is the worst of the three. The follow-up unit adds the hydration
// and widens the set. Until then a lock on a provider ID is honored by
// ApplyMetadata's merge tables and by the Discogs/AudioDB identify guards, and
// is NOT honored on the persist path. lockGuardedFields excludes them by
// deriving against providerFieldMap, so the exclusion cannot drift.
//
// THE SINGLE-COLUMN WRITE VERBS ARE NOT GUARDED. This covers the WHOLE-ROW
// persist (Service.update) and nothing else. UpdateField and ClearField call
// sqliteArtistRepo.UpdateField / ClearField, and UpdateNameGuarded runs its own
// `UPDATE artists SET name = ?` inside the collision transaction
// (name_collision.go). That is a scope decision recorded in the issue, not an
// oversight: their production callers include the operator's own history revert
// and blast-radius restore, which are deliberately lock-blind (see the
// trackableFields comment in service.go), so guarding them needs the scoped
// operator-grant mechanism that is a separate unit of work.
//
// MEMBERS ARE NOT GUARDED. FieldMembers is a lockable token but NOT a column on
// the Artist struct -- band members live in their own table, written by
// UpsertMembers / DeleteMembersByArtistID, which never pass through here. It is
// NOT a merge-table column either -- lockableFieldNames carries it only via
// AllLockableFields, and `grep -n '"members"' internal/artist/merge.go` finds
// it solely in the comment saying so. The one production honoring of a "members"
// lock is applyMemberRefresh (internal/api/handlers_refresh.go), which returns
// early when the token is in the locked set.

// lockGuardedFields is every field this codebase already treats as a meaningful
// lock token, can represent as a value ON THE ARTISTS ROW, and can therefore
// read, compare and restore from a bare Repository.GetByID.
//
// It is derived from lockableFieldNames (merge.go), not written out by hand.
// lockableFieldNames is the union of AllLockableFields with every field the
// merge engine can gate, and that union is what reportUnenforceableLocks uses
// to decide whether a stored lock token is meaningful rather than a silent
// no-op. Deriving from it means adding a lockable field anywhere cannot
// silently leave the chokepoint behind.
//
// TWO EXCLUSIONS, both derived rather than listed, so neither can drift:
//
//   - The provider-ID fields, via providerFieldMap. They are NOT on the artists
//     row, so the stored side of the comparison would read as empty and the
//     guard would restore "" over a real ID. See the file comment. This is the
//     narrowing the follow-up unit reverses, by adding hydration first.
//   - FieldMembers, which no Artist field holds at all.
//
// So a lock on a provider ID is NOT enforced here today. That is a deliberate
// scope boundary, not a claim of coverage -- see the file comment.
//
// Sorted so the restoration log and the returned slice have a stable order
// (lockableFieldNames is a map, and Go randomizes map iteration).
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
// It is a thin wrapper today: every field in lockGuardedFields lives on the
// artists row, so the stored artist Service.update already fetched is a
// complete comparison basis and no side-table read is needed. The wrapper
// exists anyway because the follow-up unit widens the set to the provider-ID
// fields, which DO need a hydration step and a refusal when it cannot run --
// and that belongs on this side of the call, not inside enforceFieldLocks,
// which must stay a pure in-memory comparison.
//
// A nil on either side means there is nothing to compare. Service.update passes
// a nil stored ONLY for an artist that does not exist yet (a first insert via
// Update); every OTHER read failure is refused before reaching here, because an
// unverifiable lock must not be treated as an absent one. See the fetch in
// Service.update.
func (s *Service) enforceLocksBeforeUpdate(ctx context.Context, stored, incoming *Artist) error {
	if stored == nil || incoming == nil {
		return nil
	}
	enforceFieldLocks(ctx, stored, incoming)
	return nil
}

// enforceFieldLocks restores, onto incoming, every field the STORED artist has
// locked and the incoming struct would have changed. It returns the names of
// the fields it restored, in lockGuardedFields order.
//
// RESTORE-AND-CONTINUE rather than refuse, and the shape is deliberate. A
// whole-row persist has no natural refusal: the caller did not ask to write one
// field, it handed over an entire artist, most of which is legitimate. Refusing
// the call would discard the unlocked changes alongside the locked one and turn
// every fixer into an all-or-nothing write. Restoring the locked value and
// letting the rest land is what makes the lock a per-FIELD guarantee. The
// single-field verbs are the ones with a natural refusal, and they refuse (see
// UpdateProviderField).
//
// Both arguments are required; a nil on either side means there is nothing to
// compare and the guard reports no restorations rather than guessing.
//
// The incoming struct is mutated in place. That is the point: the caller goes
// on to persist it, so restoring the value here is what makes the lock hold no
// matter which writer produced the struct. It also means the caller's own
// in-memory copy carries the protected value onward, so a downstream NFO write
// or platform publish in the same request does not ship the rejected value.
//
// Every restoration is logged at ERROR. A guard that quietly repairs a write
// leaves the operator with no way to learn that an automated writer is
// repeatedly attacking a pinned field. The rejected value is logged too, so the
// attempted write stays recoverable from the log even though it never reached
// the database.
func enforceFieldLocks(ctx context.Context, stored, incoming *Artist) []string {
	if stored == nil || incoming == nil {
		return nil
	}
	pinLockState(stored, incoming)

	// The stored row is the ONLY source of the lock set. See the file comment.
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
		rejected, changed := restoreLockedField(stored, incoming, name)
		if !changed {
			continue
		}
		restored = append(restored, name)
		slog.Error("refused an automated write to a locked artist field; the stored value was restored",
			"artist_id", stored.ID,
			"field", name,
			"rejected_value", rejected,
			"source", source)
	}
	return restored
}

// pinLockState copies the stored row's LOCK STATE onto incoming, so a
// whole-row persist can never be the thing that changes what is locked.
//
// Unconditional, with no lock required to earn the treatment: an incoming
// struct that ADDS locks is overridden for the same reason as one that removes
// them, because the persist path is not where lock intent is expressed. See the
// file comment for why restoring a locked VALUE without this buys exactly one
// write of protection.
//
// It is silent, unlike a restored field value. A restoration means an automated
// writer attacked a value the operator pinned, which is a real event worth an
// ERROR line. This, by contrast, fires on nearly every update in the system --
// almost no caller populates lock state deliberately -- so logging here would
// emit a line per write and bury the restorations that matter. The property is
// pinned by the two-write test in lockguard_test.go instead.
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
// differ. It returns the rejected incoming value (for the log) and whether a
// restoration actually happened.
//
// Slice fields are compared and restored as SLICES, not in their joined string
// form. Round-tripping through a comma-join would reorder or split entries
// whose own text contains a comma, making the guard a data-loss path for
// exactly the fields it is protecting.
func restoreLockedField(stored, incoming *Artist, field string) (rejected string, changed bool) {
	if IsSliceField(field) {
		want := SliceFieldFromArtist(stored, field)
		got := SliceFieldFromArtist(incoming, field)
		if slices.Equal(want, got) {
			return "", false
		}
		setFieldOnArtist(incoming, field, "", slices.Clone(want))
		return strings.Join(got, ", "), true
	}
	want := FieldValueFromArtist(stored, field)
	got := FieldValueFromArtist(incoming, field)
	if want == got {
		return "", false
	}
	setFieldOnArtist(incoming, field, want, nil)
	return got, true
}

// setFieldOnArtist writes one named field back onto an Artist. It is the
// inverse of FieldValueFromArtist / SliceFieldFromArtist for the ARTISTS-ROW
// fields; a field outside it is a no-op, matching those readers' default branch.
//
// It deliberately has NO provider-ID arm. Those fields are not in
// lockGuardedFields (see the file comment on why guarding them un-hydrated is a
// data-loss path), so an arm here would be unreachable code implying coverage
// this unit does not provide. The follow-up unit adds the arm and the hydration
// together. TestUpdate_RestoresEveryGuardedField drives every entry in
// lockGuardedFields through this switch, so a field entering the set without a
// matching arm fails there rather than silently no-opping.
//
// Scalar callers pass value and a nil slice; slice callers pass "" and the
// slice. Keeping one function rather than two avoids a second field switch that
// could drift from this one.
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
