package artist

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
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
// PROVIDER-ID FIELDS ARE GUARDED, BUT ONLY BECAUSE THE STORED SIDE IS HYDRATED
// FIRST. They live in artist_provider_ids, not on the artists row, so a bare
// Repository.GetByID does not populate them. An un-hydrated comparison reads the
// stored value as EMPTY and "restores" "" over a real ID -- guarding them
// without hydration is a DATA-LOSS PATH for the fields it claims to protect.
// enforceLocksBeforeUpdate hydrates before comparing and REFUSES when it cannot.
// Anyone narrowing that hydration must remove these fields from
// lockGuardedFields in the same change.
//
// An operator's own edit of a pinned provider ID is NOT blocked: the field-edit
// handler carries a field-scoped grant (see ContextWithLockOverride). Locks gate
// automated writes. An ungranted single-field write to a pinned provider ID is
// REFUSED by Service.UpdateProviderField before it reaches this chokepoint,
// rather than restored here (see refuseIfFieldLocked); this file's
// restore-and-continue is for the whole-row writers.
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
// unlocked change instead. A pair that is one LOGICAL value is restored
// together instead -- that is what restoreProviderIDCompanions does for a
// provider ID, its fetched-at timestamp and its provenance marker.

// lockGuardedFields is every field this codebase treats as a meaningful lock
// token AND can read, compare and restore from a bare Repository.GetByID.
//
// Derived from lockableFieldNames (merge.go) rather than written by hand, so
// adding a lockable field anywhere cannot silently leave the chokepoint behind.
// ONE exclusion: FieldMembers, which no Artist field holds. The provider-ID
// fields ARE included, which is safe only because enforceLocksBeforeUpdate
// hydrates the stored side before any comparison -- see its doc comment.
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
		out = append(out, FieldName(name))
	}
	slices.Sort(out)
	return out
}()

// lockOverrideKeyType carries a FIELD-SCOPED operator grant: permission for one
// named field's lock to be bypassed on writes made with this context.
type lockOverrideKeyType struct{}

var lockOverrideKey lockOverrideKeyType

// ContextWithLockOverride authorizes writes on ctx to change the named field
// even though the operator has it locked.
//
// WHY THIS EXISTS. A lock gates AUTOMATED writes; the operator keeps control of
// their own data. That is already true for the fifteen artists-row fields,
// because an operator edit routes through the single-column Service.UpdateField,
// which never reaches the chokepoint. The six provider-ID fields have no such
// verb -- Service.UpdateProviderField ends in a whole-row Service.Update -- so
// once those fields became guarded, the operator's own edit was silently
// reverted while the API answered 200. A lock that means "you may still edit
// this" for fifteen fields and "you are locked out" for six is not a policy,
// it is an inconsistency.
//
// SCOPED TO ONE FIELD, and set only by the operator-facing field-edit handler.
// A blanket bypass on UpdateProviderField would be wrong: the rule engine's
// provider-ID backfill calls that same method, and the case the guard genuinely
// protects there is a field an operator pinned while EMPTY ("do not guess this
// one"), which the fill-empty fixer would otherwise populate. Granting per
// field, per request, keeps that protection intact.
func ContextWithLockOverride(ctx context.Context, field string) context.Context {
	normalized := strings.ToLower(strings.TrimSpace(field))
	// A grant naming no guarded field authorizes nothing, and would otherwise
	// fail the way this whole change exists to prevent: the operator's edit
	// reverted, the API answering success. The exported entry point cannot
	// assume every future caller passes a validated name, so say so loudly
	// rather than let a misspelling read as a grant.
	if normalized != "" && !slices.Contains(lockGuardedFields, FieldName(normalized)) {
		slog.Error("lock override names no guarded field; it authorizes nothing and the write will be refused or reverted if that field is locked",
			"field", field)
	}
	return context.WithValue(ctx, lockOverrideKey, normalized)
}

// lockOverrideField returns the field this context authorizes bypassing, if any.
func lockOverrideField(ctx context.Context) (string, bool) {
	f, ok := ctx.Value(lockOverrideKey).(string)
	return f, ok && f != ""
}

// ErrNoProviderIDRepository reports that a locked provider ID could not be
// verified because the Service has no provider-ID repository. It is a sentinel
// so a caller can classify the refusal with errors.Is rather than matching on
// message text -- the same contract the hydration-failure path gets for free by
// wrapping the underlying error.
var ErrNoProviderIDRepository = errors.New("no provider-ID repository is wired, so a locked provider ID cannot be verified")

// providerIDLockFields are the guarded fields stored in artist_provider_ids
// rather than on the artists row. Derived from providerFieldMap so it cannot
// drift from the set of fields that actually live in that table.
var providerIDLockFields = func() []FieldName {
	out := make([]FieldName, 0, len(providerFieldMap))
	for field := range providerFieldMap {
		out = append(out, FieldName(field))
	}
	slices.Sort(out)
	return out
}()

// enforceLocksBeforeUpdate is Service.update's entry point into the guard. It
// hydrates whichever side table the STORED artist's lock set requires before
// comparing, and refuses the write when it cannot.
//
// THE HYDRATION IS THE SAFETY PRECONDITION, not an optimization. A provider ID
// lives in artist_provider_ids, so an un-hydrated stored artist reads as EMPTY
// and the guard would "restore" "" over a real ID -- the guard becoming the
// data-loss path for the fields it protects. Anyone narrowing or reordering this
// must remove those fields from lockGuardedFields in the same change.
//
// Conditional on such a field actually being locked, so an ordinary write costs
// no extra query. FAILS LOUD when hydration cannot run: an unverifiable lock
// must not be treated as an absent one, which is the same rule Service.update
// applies to an unreadable stored row.
//
// A nil on either side means there is nothing to compare. Service.update passes
// a nil stored ONLY for an artist that does not exist; every other read failure
// is refused before reaching here.
func (s *Service) enforceLocksBeforeUpdate(ctx context.Context, stored, incoming *Artist) error {
	if stored == nil || incoming == nil {
		return nil
	}
	locked := buildLockedSet(stored.LockedFields)
	needsProviderIDs := false
	for _, f := range providerIDLockFields {
		if isLocked(locked, string(f)) {
			needsProviderIDs = true
			break
		}
	}
	if needsProviderIDs {
		// A nil repository means a hand-assembled Service: NewService wires one
		// unconditionally and NewServiceWithRepos takes one as a parameter.
		// Skipping the hydration here would compare a locked provider ID against
		// an un-hydrated empty value, conclude nothing changed, and let the write
		// through -- the exact data-loss path this hydration exists to close.
		if s.providers == nil {
			slog.Error("refusing artist update: a provider-ID field is locked but this Service has no provider-ID repository, so the stored ID cannot be read",
				"artist_id", stored.ID)
			return fmt.Errorf("enforcing field locks for %s: %w", stored.ID, ErrNoProviderIDRepository)
		}
		if err := s.hydrateProviderIDs(ctx, stored); err != nil {
			slog.Error("refusing artist update: a provider-ID field is locked but the stored IDs could not be read",
				"artist_id", stored.ID, "error", err)
			return fmt.Errorf("hydrating stored provider IDs for %s to enforce field locks: %w", stored.ID, err)
		}
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
// RESTORE-AND-CONTINUE rather than refuse, and that is a property of THIS
// function's shape, not a package-wide policy. A whole-row persist has no
// natural refusal: the caller handed over an entire artist, most of it
// legitimate. Refusing would discard the unlocked changes too and turn every
// fixer into an all-or-nothing write.
//
// A SINGLE-FIELD VERB DOES REFUSE, because the tradeoff inverts there: the one
// field it exists to write is the locked one, so restoring it silently means the
// whole operation was reverted while the method returned nil.
// Service.UpdateProviderField (and ClearProviderField through it) therefore
// returns a *FieldLockedError before it ever reaches this function -- see
// refuseIfFieldLocked. A write that gets here from that verb is one the operator
// granted, or one on a field that is not locked.
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
	// reportUnenforceableLocks is still deliberately NOT called here, unlike on
	// the merge path -- but the reason has NARROWED now that the provider IDs
	// are guarded. Previously seven tokens were known-but-unguarded here; only
	// "members" remains, and it is honored by applyMemberRefresh, so reporting
	// it as unenforceable would be false.
	//
	// What is left is a genuinely unknown token (a misspelling), which protects
	// nothing anywhere and IS worth reporting -- just not once per whole-row
	// persist. This runs on every rule pass, so a permanently misspelled token
	// would log forever. The merge path already reports it on refresh, bulk
	// fetch and NFO import. Validating at the lock-SETTING API is the fix that
	// reports once, and it is still not this unit's.
	locked := buildLockedSet(stored.LockedFields)
	if len(locked) == 0 {
		return nil
	}
	source := sourceFromContext(ctx)

	granted, hasGrant := lockOverrideField(ctx)

	var restored []string
	for _, f := range lockGuardedFields {
		name := string(f)
		if !isLocked(locked, name) {
			continue
		}
		if hasGrant && granted == name {
			// The operator authorized this one field on this one request. Logged
			// because a bypass of a data-protection control should leave a
			// record: every other outcome of this loop logs, and a silent
			// exception is how an authorization primitive stops being auditable.
			// Info rather than Error -- this is the system working as designed,
			// not a conflict between two intents.
			slog.Info("field lock bypassed by an operator grant; the write was allowed",
				"artist_id", stored.ID,
				"field", name,
				"source", source)
			continue
		}
		// Companions are pinned for a locked provider ID whether or not the ID
		// STRING changed. They are part of the locked field's state, not a
		// side effect of changing it, so a write carrying the same ID with a
		// tampered timestamp or provenance marker must not land either.
		restoreProviderIDCompanions(stored, incoming, name)
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

// restoreProviderIDCompanions copies the state that TRAVELS WITH a provider ID
// from stored onto incoming, for any LOCKED provider-ID field -- whether or not
// the ID string itself changed. That is deliberate: the companions are part of
// the locked field's state, not a side effect of changing it, so a write
// carrying the same ID with a tampered timestamp or provenance marker is still
// an automated write to a pinned field.
//
// Restoring the ID string alone leaves the row self-inconsistent, because
// persistNormalized -> extractProviderIDs -> UpsertAll is delete-and-replace and
// writes the ID with its companions from the same struct. Two exist:
//
//   - The paired fetched-at timestamp. Letting the incoming value through would
//     record that the operator's pinned ID was fetched at the instant of the
//     write that was just rejected -- false provenance on curated data.
//   - MetadataSources[SourceKeyMusicBrainzID], for musicbrainz_id only, which
//     records machine-picked versus operator-confirmed. Letting an incoming
//     machine-picked marker land over a pinned MBID relabels a confirmed
//     identity as a guess, and that key is what the operator-facing re-review
//     query filters on -- so the artist surfaces to the operator as unverified
//     work of their own.
//
// deezer_id, spotify_id and musicbrainz_id carry no fetched-at column, so their
// timestamp arms are absent rather than forgotten.
func restoreProviderIDCompanions(stored, incoming *Artist, field string) {
	switch field {
	case "audiodb_id":
		incoming.AudioDBIDFetchedAt = clonedTime(stored.AudioDBIDFetchedAt)
	case "discogs_id":
		incoming.DiscogsIDFetchedAt = clonedTime(stored.DiscogsIDFetchedAt)
	case "wikidata_id":
		incoming.WikidataIDFetchedAt = clonedTime(stored.WikidataIDFetchedAt)
	case "musicbrainz_id":
		restoreMBIDProvenance(stored, incoming)
	}
}

// clonedTime copies a *time.Time by VALUE. Aliasing the stored pointer would let
// a later mutation of one struct silently rewrite the other -- the same reason
// pinLockState clones LockedAt.
func clonedTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	at := *t
	return &at
}

// restoreMBIDProvenance puts the stored provenance for the MusicBrainz ID back
// onto incoming, INCLUDING the case where the stored row had no entry -- it
// deletes the incoming one rather than leaving it, since an absent marker and a
// machine-picked marker mean different things to the re-review query.
func restoreMBIDProvenance(stored, incoming *Artist) {
	want, hadStored := stored.MetadataSources[SourceKeyMusicBrainzID]
	if !hadStored {
		delete(incoming.MetadataSources, SourceKeyMusicBrainzID)
		return
	}
	if incoming.MetadataSources == nil {
		incoming.MetadataSources = make(map[string]string, 1)
	}
	incoming.MetadataSources[SourceKeyMusicBrainzID] = want
}

// setFieldOnArtist writes one named field back onto an Artist, the inverse of
// FieldValueFromArtist / SliceFieldFromArtist for the ARTISTS-ROW fields. A
// field outside it is a no-op, matching those readers' default branch.
//
// The provider-ID arm writes through applyProviderFieldToArtist, the same setter
// the normalized-table path uses, so the value lands where extractProviderIDs
// will read it back. TestUpdate_RestoresEveryGuardedField drives every entry in
// lockGuardedFields through this switch, so a field entering the set without a
// matching arm fails there rather than silently no-opping.
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
	case "musicbrainz_id", "audiodb_id", "discogs_id", "wikidata_id", "deezer_id", "spotify_id":
		applyProviderFieldToArtist(a, providerFieldMap[field], value)
	}
}
