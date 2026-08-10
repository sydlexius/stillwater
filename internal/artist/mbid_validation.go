package artist

import (
	"errors"
	"fmt"
	"time"
)

// MusicBrainz ID re-validation ledger (issue #2810, persistence only).
//
// # WHAT THIS FILE IS FOR
//
// Stillwater stores a MusicBrainz ID (an "MBID") for an artist and has never
// checked it against MusicBrainz. The only test it has ever applied is that
// the string looks like a UUID, which any well-formed id passes -- including
// one belonging to a completely different act that happens to share the
// artist's name. So today a wrong id is indistinguishable from a right one,
// and it propagates into every downstream fetch, NFO write and platform push
// as fact.
//
// This file defines the VOCABULARY and the RECORD for the answer: for one
// artist, was its stored id checked, and what did the check find. It performs
// no checking itself. The resolver that talks to MusicBrainz and the sweep
// that drives it are separate work; they write MBIDValidation rows through
// MBIDValidationRepository and read them back for the operator.
//
// # THE POPULATION THIS COVERS, AND THE MISTAKE NOT TO MAKE
//
// Every artist carrying a non-empty MBID is in scope. That deliberately
// INCLUDES artists with no provenance marker on their id.
//
// The marker introduced by #2715 stamps ids adopted from that point forward.
// Nothing writes an operator-confirmed value today, so in practice the marker
// partitions artists into "machine-picked" and "unknown" -- NOT into
// "machine-picked" and "operator-confirmed". Unmarked means "nobody has
// established where this came from", which is a reason to check it, never a
// reason to skip it. Every artist damaged before #2715 landed is unmarked, so
// a query that filtered on the marker would miss precisely the population this
// feature exists to find and would report "nothing to fix".
//
// Nothing in this file or its repository filters on provenance, and nothing
// added later should.

// MBIDValidationOutcome is the verdict for one artist's stored MusicBrainz ID.
//
// There are three, and the third is the one that matters most. Read the
// per-constant comments before adding a fourth.
type MBIDValidationOutcome = string

// The three outcomes. Stored verbatim in mbid_validation.outcome, which
// carries a CHECK constraint listing exactly these values.
const (
	// MBIDOutcomeValidated means the id resolved to a MusicBrainz artist whose
	// name AND release catalogue match the local one.
	//
	// Operationally: this id has been affirmatively confirmed. It is positive
	// evidence, not the absence of a complaint.
	MBIDOutcomeValidated MBIDValidationOutcome = "validated"

	// MBIDOutcomeFailed means the id resolved to something, and that something
	// is not this artist.
	//
	// Operationally: the artist needs an operator's attention. Nothing is
	// repaired automatically as a result -- auto-reverting an identity would
	// repeat the original defect in the opposite direction -- so a failed row
	// is a flag for review and nothing more.
	MBIDOutcomeFailed MBIDValidationOutcome = "failed"

	// MBIDOutcomeNotCheckable means Stillwater could not reach a verdict.
	//
	// Operationally this is NOT a pass, and treating it as one is the specific
	// failure this ledger is designed to prevent. An id that does not resolve
	// at all may be perfectly correct but stale (the artist was merged into
	// another entry upstream in MusicBrainz); a provider outage says nothing
	// about the id whatsoever; an artist with no albums on disk gives the
	// catalogue comparison nothing to compare. In all three cases the honest
	// report is "unknown", carrying the reason so the operator can tell which
	// kind of unknown it is.
	MBIDOutcomeNotCheckable MBIDValidationOutcome = "not_checkable"
)

// MBIDValidationReason is the machine-readable "why" behind a non-validated
// outcome. A validated row carries the empty reason; every other row must
// carry one, which the database enforces.
type MBIDValidationReason = string

// The reason vocabulary. Stored verbatim in mbid_validation.reason, which
// carries a CHECK constraint listing exactly these values plus the empty
// string.
//
// The schema deliberately does NOT constrain which reason may pair with which
// outcome. Classification belongs to the resolver, and freezing its taxonomy
// into a schema check would make a correct future refinement need a migration.
const (
	// MBIDReasonNone is the reason on a validated row: there is nothing to
	// explain. It is the empty string so the column reads naturally in SQL.
	MBIDReasonNone MBIDValidationReason = ""

	// MBIDReasonResolvesToDifferentArtist: the id resolves to a live
	// MusicBrainz artist that is not this one. The headline failure -- the id
	// is wrong, not merely stale.
	MBIDReasonResolvesToDifferentArtist MBIDValidationReason = "resolves_to_different_artist"

	// MBIDReasonNameMismatch: the remote artist's name does not match the
	// local one closely enough. A supporting reason, never sufficient on its
	// own to condemn an id -- the motivating production case had an EXACT name
	// match on a completely wrong artist.
	MBIDReasonNameMismatch MBIDValidationReason = "name_mismatch"

	// MBIDReasonCatalogueMismatch: the remote artist's releases do not overlap
	// the albums on disk. This is the load-bearing signal. In the motivating
	// case the assigned id belonged to a person with no music releases at all
	// while the operator's library held several albums by the actual musician
	// of that name; catalogue overlap is what separates them, and name
	// similarity is what failed to.
	MBIDReasonCatalogueMismatch MBIDValidationReason = "catalogue_mismatch"

	// MBIDReasonMBIDNotFound: MusicBrainz has no artist under this id.
	//
	// Kept strictly distinct from "resolves to a different artist", because
	// the two call for opposite responses. An id that resolves to someone else
	// is wrong. An id that resolves to nobody is quite possibly RIGHT and
	// merely stale -- MusicBrainz merges duplicate artist entries, retiring
	// the id that lost. Reporting the second as a failure would send operators
	// to break correct data.
	MBIDReasonMBIDNotFound MBIDValidationReason = "mbid_not_found"

	// MBIDReasonProviderUnavailable: MusicBrainz could not be reached, or
	// answered with an error. Says nothing at all about the id. The next pass
	// tries again.
	MBIDReasonProviderUnavailable MBIDValidationReason = "provider_unavailable"

	// MBIDReasonNoLocalAlbums: the artist has no albums on disk, so the
	// catalogue comparison had nothing to work with.
	//
	// This is not-checkable rather than validated-on-name-alone, deliberately.
	// Name matching alone is exactly the test that let the motivating case
	// through, so an artist that can only be name-matched is reported as
	// unverified rather than waved past.
	MBIDReasonNoLocalAlbums MBIDValidationReason = "no_local_albums"
)

// ErrMBIDValidationNotFound is returned by GetByArtistID when the artist has
// no ledger row -- that is, the artist has never been checked. Callers must
// keep "never checked" distinct from "checked and found fine"; they are the
// same distinction MBIDOutcomeNotCheckable draws, one level up.
var ErrMBIDValidationNotFound = errors.New("mbid validation record not found")

// MBIDValidation is one artist's latest MusicBrainz ID verdict.
//
// One row per artist: re-checking an artist REPLACES its verdict rather than
// appending to it. The ledger answers "where does this artist stand now", and
// the per-write audit trail lives in metadata_changes already.
type MBIDValidation struct {
	// ArtistID is the artist whose stored id was checked. Also the row's
	// primary key.
	ArtistID string `json:"artist_id"`

	// MBID is the id that was actually checked, recorded rather than re-read
	// later. If the artist's stored id changes after the check, this column is
	// what reveals that the verdict describes the previous one.
	MBID string `json:"mbid"`

	// Outcome is one of the MBIDOutcome* constants.
	Outcome MBIDValidationOutcome `json:"outcome"`

	// Reason is one of the MBIDReason* constants. Empty exactly when Outcome
	// is MBIDOutcomeValidated; the database rejects any other combination.
	Reason MBIDValidationReason `json:"reason"`

	// Detail is optional prose for the operator (which albums failed to match,
	// which provider error was seen). Never parsed, never matched on.
	Detail string `json:"detail,omitempty"`

	// ResolvedName is the artist name MusicBrainz returned for this id, empty
	// when the id did not resolve. It is evidence: it is what lets an operator
	// see a same-name-different-act case for themselves.
	ResolvedName string `json:"resolved_name,omitempty"`

	// CatalogueMatchPercent is how much of the local album catalogue matched
	// the remote release groups, 0-100.
	//
	// A POINTER, and that is load-bearing. Zero percent is a real measurement
	// and the single strongest piece of evidence an id is wrong -- it is
	// exactly what the motivating case produces. "Never measured" is a
	// different fact, and a plain float64 would render the two identical,
	// making the most damning evidence the default state of every unmeasured
	// row. nil means not measured.
	CatalogueMatchPercent *float64 `json:"catalogue_match_percent,omitempty"`

	// CheckedAt is when the verdict was reached, UTC.
	CheckedAt time.Time `json:"checked_at"`
}

// Validate reports whether the record is well-formed enough to persist.
//
// It duplicates the schema's CHECK constraints on purpose, so a caller gets a
// named Go error at the call site instead of an opaque SQLite constraint
// failure from three layers down. The database remains the authority: this
// method being wrong or skipped cannot let a malformed row land.
func (v *MBIDValidation) Validate() error {
	if v.ArtistID == "" {
		return errors.New("mbid validation: artist id is required")
	}
	if v.MBID == "" {
		return errors.New("mbid validation: mbid is required")
	}
	if v.CatalogueMatchPercent != nil && (*v.CatalogueMatchPercent < 0 || *v.CatalogueMatchPercent > 100) {
		return fmt.Errorf("mbid validation: catalogue match percent out of range [0,100], got %v", *v.CatalogueMatchPercent)
	}
	switch v.Outcome {
	case MBIDOutcomeValidated, MBIDOutcomeFailed, MBIDOutcomeNotCheckable:
	default:
		return fmt.Errorf("mbid validation: unknown outcome %q", v.Outcome)
	}
	switch v.Reason {
	case MBIDReasonNone,
		MBIDReasonResolvesToDifferentArtist,
		MBIDReasonNameMismatch,
		MBIDReasonCatalogueMismatch,
		MBIDReasonMBIDNotFound,
		MBIDReasonProviderUnavailable,
		MBIDReasonNoLocalAlbums:
	default:
		return fmt.Errorf("mbid validation: unknown reason %q", v.Reason)
	}
	// Every non-validated verdict must say why; a validated one must not
	// pretend to. See MBIDOutcomeNotCheckable for why a blank "unknown" is the
	// dangerous case.
	if v.Outcome == MBIDOutcomeValidated && v.Reason != MBIDReasonNone {
		return fmt.Errorf("mbid validation: a validated outcome carries no reason, got %q", v.Reason)
	}
	if v.Outcome != MBIDOutcomeValidated && v.Reason == MBIDReasonNone {
		return fmt.Errorf("mbid validation: outcome %q requires a reason", v.Outcome)
	}
	return nil
}

// MBIDValidationFilter narrows a ledger listing.
//
// THE ZERO VALUE MEANS "EVERY ROW", and that direction is deliberate. This is
// a report about possibly-misidentified artists, so an unset or unrecognized
// filter must widen to the full ledger, never silently narrow it. A filtering
// helper whose default hides rows is how a population gets reported as clean.
type MBIDValidationFilter struct {
	// Outcome narrows to a single outcome. Empty means every outcome.
	// An unrecognized value is coerced to empty by Validate rather than
	// rejected, so a malformed query shows the operator MORE than they asked
	// for, never less.
	Outcome MBIDValidationOutcome

	// Limit and Offset paginate the row list. They do not affect the count.
	Limit  int
	Offset int
}

// Default and maximum page sizes for a ledger listing, mirroring the other
// artist-domain reports (see NFOMBIDFilter.Validate).
const (
	mbidValidationDefaultLimit = 50
	mbidValidationMaxLimit     = 500
)

// Validate normalizes the filter so every value reaching SQL is a known-good
// one. Outcome is checked against the constant set and blanked if unknown --
// blanking widens the result, per the type's contract.
func (f *MBIDValidationFilter) Validate() {
	switch f.Outcome {
	case MBIDOutcomeValidated, MBIDOutcomeFailed, MBIDOutcomeNotCheckable:
	default:
		f.Outcome = ""
	}
	if f.Limit <= 0 {
		f.Limit = mbidValidationDefaultLimit
	}
	if f.Limit > mbidValidationMaxLimit {
		f.Limit = mbidValidationMaxLimit
	}
	if f.Offset < 0 {
		f.Offset = 0
	}
}

// The MBIDValidationRepository interface itself lives in repository.go
// alongside the package's other repository contracts.
