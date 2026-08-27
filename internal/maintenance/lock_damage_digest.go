package maintenance

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/sydlexius/stillwater/internal/artist"
)

// lock_damage_digest.go -- the approval token that makes #3079's preview
// BINDING on the write.
//
// # THE DEFECT THIS CLOSES
//
// The dry run and the repair are separate process invocations. Each re-runs
// selectPreGuardCandidates from scratch, so before this nothing carried the
// approved set forward and the write restored whatever the predicate matched
// AT WRITE TIME.
//
// That is not a theoretical gap. Selection condition 1 is "the field is
// locked NOW", so any lock added between preview and repair pulls
// previously-invisible damage into the population. On a production clone
// PreGuardUnlocked was 3014: three thousand pre-cutoff unattributable damage
// rows sat one ordinary UI lock-toggle away from becoming write targets, none
// of them ever previewed. Demonstrated end to end -- preview 215, toggle one
// lock, repair wrote 216.
//
// It also failed in the other direction. The preview short-circuited before
// the guarded compare-and-set, so a candidate whose stored value had already
// diverged was previewed as "would restore" and then silently declined at
// write time: preview 215, repair 214.
//
// THIS MATTERS MORE HERE THAN IT WOULD ANYWHERE ELSE. The maintainer's own
// ruling on this population is that the preview-and-approve is "doing the
// real safety work" -- properties 1 and 2 (the time bound, reversibility)
// explicitly do NOT discriminate an operator's write from a machine's.
// RestoreLockedFieldGuarded protects the VALUE of each row; nothing protected
// the MEMBERSHIP OF THE SET. Without a binding token the safety argument for
// an unattributed mass restore rests on operator discipline rather than on
// the mechanism, which is exactly what #3074 rejected.
//
// # WHY A DIGEST AND NOT A COUNT
//
// A count cannot catch a SWAP. One row leaving the set and another joining it
// between preview and repair leaves the count identical while changing what
// gets written, and the swap is the case an attacker-shaped accident produces
// (an operator locks one field and unlocks another in the same sitting). The
// digest is computed over the sorted change-ids, so any membership change at
// all -- add, drop, or swap -- moves it.
//
// # WHY CHANGE-IDS AND NOT (artist, field)
//
// The change-id identifies the SPECIFIC damage row the restore was approved
// against, not merely the pair. If a new damaging write lands on an
// already-approved pair between preview and repair, the pair is unchanged but
// the row to restore from is not -- and the operator approved a restore of
// the value the OLD row carried. Keying on change-ids refuses that; keying on
// pairs would wave it through.
//
// # WHAT IT DELIBERATELY DOES NOT COVER
//
// No field VALUES enter the digest. They are library content, and the digest
// is printed to stdout for the operator to copy onto a command line. Change
// ids are opaque UUIDs. A value that changes underneath an unchanged change
// id is caught by RestoreLockedFieldGuarded's compare-and-set instead, which
// is the layer that owns per-row value safety -- the digest owns set
// membership, and the two are complementary rather than redundant.

// lockDamageDigestVersion prefixes the digest input so a future change to
// what the digest covers cannot silently validate against a token computed
// under the old rules. A stale token then fails LOUDLY as a mismatch rather
// than passing because both sides happened to hash the same bytes.
const lockDamageDigestVersion = "swlockdmg/v1"

// lockDamageDigestLen is how many hex characters of the SHA-256 the operator
// actually types. 16 hex chars is 64 bits: far beyond accidental collision
// for a set that is at most a few thousand rows, and short enough to copy
// without error. The gate is an integrity check against drift, not a
// defense against an adversary who can already run the binary.
const lockDamageDigestLen = 16

// LockDamageDigest computes the approval token for a candidate set.
//
// ORDER-INDEPENDENT BY CONSTRUCTION: the ids are sorted before hashing, so
// the token depends on WHICH rows are in the set and on nothing else -- not
// on the query's ORDER BY, not on the preview's display order, not on map
// iteration. Two runs that select the same rows agree even if a future change
// reorders the report.
func LockDamageDigest(restored []LockDamageRestore) string {
	ids := make([]string, 0, len(restored))
	for i := range restored {
		ids = append(ids, restored[i].ChangeID)
	}
	slices.Sort(ids)

	// Built as one string and hashed once. hash.Hash.Write never returns an
	// error (the contract is explicit), but writing through fmt.Fprintf would
	// still oblige every call site to say so; composing the input first keeps
	// the framing visible in one place.
	//
	// The version and the COUNT are a framed prefix, and every id is
	// newline-terminated. Framing matters: without it, two different sets
	// whose concatenated ids happen to coincide would collide. With the count
	// and a delimiter that cannot appear in a UUID, they cannot.
	var b strings.Builder
	b.WriteString(lockDamageDigestVersion)
	b.WriteByte('\n')
	b.WriteString(strconv.Itoa(len(ids)))
	b.WriteByte('\n')
	for _, id := range ids {
		b.WriteString(id)
		b.WriteByte('\n')
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])[:lockDamageDigestLen]
}

// LockDamageDigestMatches reports whether an operator-supplied token matches
// the digest of the set about to be written.
//
// Case-insensitive and whitespace-trimmed: the operator is copying a hex
// string off a terminal, and rejecting a correct set over a stray space or a
// capital letter trains people to work around the gate rather than heed it.
// Nothing else is normalized -- a wrong token is a wrong token.
func LockDamageDigestMatches(expected, actual string) bool {
	return strings.EqualFold(strings.TrimSpace(expected), strings.TrimSpace(actual))
}

// LockDamageDriftError reports that the candidate set at write time is not
// the set the operator approved. It carries the counts and the two digests so
// the failure message can say what moved without another query.
//
// A TYPED ERROR, not a bare fmt.Errorf: the caller must be able to tell this
// refusal (write nothing, stamp nothing, exit non-zero, the operator re-runs
// the preview) apart from a transient database failure, and matching on
// message text is the drift this repo avoids elsewhere.
type LockDamageDriftError struct {
	Expected string
	Actual   string
	// ActualCount is how many rows the predicate selected NOW. There is
	// deliberately no ApprovedCount: the digest is the only thing carried
	// forward from the preview, and inventing a count field the gate cannot
	// actually populate would put a number in the message that is either
	// guessed or always zero. The operator has the approved count in front of
	// them, in the preview output the digest came from.
	ActualCount int
}

func (e *LockDamageDriftError) Error() string {
	return fmt.Sprintf("the candidate set changed since the preview was approved: "+
		"expected digest %s, but the %d row(s) selected now digest to %s. "+
		"Nothing was written and completion was not recorded. "+
		"Re-run the dry run, review the new candidate list, and pass the digest it prints",
		e.Expected, e.ActualCount, e.Actual)
}

// chainKey projects a candidate onto the key the chain-depth map is indexed
// by. A helper rather than an inline literal so the two call sites cannot
// disagree about which fields make up the key.
func chainKey(c artist.LockDamageCandidate) artist.LockDamagePairKey {
	return artist.LockDamagePairKey{ArtistID: c.ArtistID, Field: c.Field}
}

// verifyApprovedDigest refuses a pass whose candidate set is not the set the
// operator approved. It returns nil when no digest was supplied (a dry run,
// or the attributed pass, neither of which needs one) and when the digest
// matches.
//
// CALLED AFTER SELECTION AND BEFORE THE FIRST WRITE. That placement is the
// whole point: the set is fully decided, so the comparison is exact, and
// nothing has been written yet, so refusing costs nothing to undo.
func verifyApprovedDigest(opts LockDamageOpts, candidates []artist.LockDamageCandidate) error {
	if opts.ApprovedDigest == "" {
		return nil
	}
	// The digest is computed over the same projection the preview printed, so
	// the two sides hash identical inputs by construction rather than by two
	// functions agreeing.
	selected := make([]LockDamageRestore, 0, len(candidates))
	for i := range candidates {
		selected = append(selected, LockDamageRestore{ChangeID: candidates[i].ChangeID})
	}
	actual := LockDamageDigest(selected)
	if LockDamageDigestMatches(opts.ApprovedDigest, actual) {
		return nil
	}
	return &LockDamageDriftError{
		Expected: strings.TrimSpace(opts.ApprovedDigest), Actual: actual,
		ActualCount: len(candidates),
	}
}
