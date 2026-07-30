package artist

import (
	"context"
	"errors"
	"fmt"
)

// AlbumEvidence is a tri-state answer to the question "what albums does this
// artist have?". The three states exist because two of them are routinely
// confused, and confusing them is what makes a wrong MusicBrainz ID get
// written: "this artist genuinely has no albums" and "I could not find out"
// look identical if the answer is only a list that might be empty.
//
// The zero value is EvidenceUnknown ON PURPOSE, and that ordering is
// load-bearing rather than cosmetic. In Go a struct field you forget to set is
// its zero value, so whatever sits at iota 0 is what a buggy or partially
// initialized caller silently asserts. If EvidenceNone were 0, a caller that
// built an AlbumSet without filling in Evidence would be claiming "this artist
// has no albums" -- exactly the false claim that lets a candidate MBID pass a
// catalogue check it never actually faced. With Unknown at 0, the same bug
// produces "I do not know", which every consumer must treat as a reason to
// DECLINE. The failure mode of forgetting a field is therefore refusing to act,
// not acting on a fabricated fact.
type AlbumEvidence int

const (
	// EvidenceUnknown means the album set could not be determined: no path
	// recorded, a read error, a timeout, a source that was unavailable.
	// Callers MUST treat this as "no evidence" and DECLINE to act on it. It is
	// never a license to proceed as though the artist had no albums.
	EvidenceUnknown AlbumEvidence = iota

	// EvidenceNone means the lookup SUCCEEDED and the artist genuinely has no
	// albums. This is a positive determination and may only be returned by a
	// source that actually looked.
	EvidenceNone

	// EvidenceFound means the lookup succeeded and Titles holds the albums.
	EvidenceFound
)

// String renders the evidence state for logs. Naming the state in a log line
// beats printing an integer, since the whole point of the type is that the
// three cases are told apart by whoever reads the record later.
func (e AlbumEvidence) String() string {
	switch e {
	case EvidenceUnknown:
		return "unknown"
	case EvidenceNone:
		return "none"
	case EvidenceFound:
		return "found"
	default:
		return fmt.Sprintf("AlbumEvidence(%d)", int(e))
	}
}

// AlbumSet is an album listing carrying its own evidence state.
type AlbumSet struct {
	// Titles holds the album names. Meaningful only when Evidence is
	// EvidenceFound; the other two states carry no titles.
	Titles []string

	// Evidence says whether Titles is a determination at all. See
	// AlbumEvidence: the zero value is EvidenceUnknown deliberately.
	Evidence AlbumEvidence

	// Origin names which source produced this set ("filesystem", "peer:emby",
	// ...) for DIAGNOSTICS ONLY -- log lines and UI provenance.
	//
	// NO BRANCH ON Origin MAY AFFECT A CONFIDENCE DECISION. Not a special case
	// for a source thought more trustworthy, not a relaxed threshold for a
	// source thought weaker, not "skip the check if Origin is X". Evidence is
	// the only field policy may read. Source-specific policy is precisely how
	// per-source exceptions accumulate until some path once again auto-links an
	// MBID with no catalogue check; keeping Origin inert by rule is what stops
	// that from being reintroduced quietly. If a source is not trustworthy
	// enough to be believed, do not put it in the chain.
	Origin string
}

// AlbumSource resolves an artist's album titles from one place to look.
type AlbumSource interface {
	// LocalAlbums returns what this source knows about the artist's albums.
	//
	// It MUST return EvidenceUnknown on any error, timeout, or unavailability,
	// and MUST NOT return EvidenceNone to mean "I could not look". EvidenceNone
	// is a positive claim that the artist has no albums and is only valid after
	// a lookup that actually succeeded.
	//
	// Returning a non-nil error alongside EvidenceUnknown is expected and lets
	// the caller log the cause; the error is diagnostic, and a caller that only
	// inspects Evidence is still correct.
	LocalAlbums(ctx context.Context, a *Artist) (AlbumSet, error)

	// Name identifies the source for diagnostics. It is what populates
	// AlbumSet.Origin, and carries the same no-branching rule.
	Name() string
}

// FilesystemAlbumSource reads album subdirectories from the artist's on-disk
// path.
//
// It exists to make a three-way distinction that ListLocalAlbums cannot: that
// function returns nil for a missing path, an empty directory, AND an
// unreadable one, so its callers cannot tell an artist with no albums from a
// mount that failed to come up.
type FilesystemAlbumSource struct{}

// NewFilesystemAlbumSource returns a filesystem-backed album source. It holds
// no state; the constructor exists so call sites read the same as the ones for
// sources that will need dependencies.
func NewFilesystemAlbumSource() *FilesystemAlbumSource { return &FilesystemAlbumSource{} }

// Name implements AlbumSource.
func (s *FilesystemAlbumSource) Name() string { return "filesystem" }

// LocalAlbums implements AlbumSource against the artist's Path.
//
// The three outcomes, and why each maps the way it does:
//
//   - No Path recorded -> EvidenceUnknown. An artist with no path has not been
//     shown to have no albums; nobody looked. 43% of a production library can
//     be in this state, so mapping it to EvidenceNone would route most of the
//     library into "catalogue check vacuously satisfied".
//   - Path read succeeded, zero album subdirectories -> EvidenceNone. A real
//     determination: the directory exists, it was listed, it holds nothing.
//   - Path read failed (permission denied, missing mount, path is a file) ->
//     EvidenceUnknown, NEVER EvidenceNone. An unmounted share reads as an empty
//     library, and an empty library must never be usable as proof.
func (s *FilesystemAlbumSource) LocalAlbums(ctx context.Context, a *Artist) (AlbumSet, error) {
	set := AlbumSet{Origin: s.Name()}

	if a == nil || a.Path == "" {
		return set, fmt.Errorf("no artist path recorded")
	}

	// Checked before the read rather than after: a canceled context means the
	// caller has stopped waiting, and a read completed after that point is not
	// an answer anyone asked for. Reporting Unknown here is the safe direction.
	if err := ctx.Err(); err != nil {
		return set, fmt.Errorf("context done before reading %s: %w", a.Path, err)
	}

	albums, err := listLocalAlbums(a.Path)
	if err != nil {
		// The set keeps its zero Evidence (Unknown). This is the branch the
		// whole type exists for: returning EvidenceNone here would convert an
		// unreadable mount into "this artist has no albums".
		return set, fmt.Errorf("reading album directories from %s: %w", a.Path, err)
	}

	if len(albums) == 0 {
		set.Evidence = EvidenceNone
		return set, nil
	}

	set.Titles = albums
	set.Evidence = EvidenceFound
	return set, nil
}

// ChainAlbumSource asks several sources in order and combines their answers.
type ChainAlbumSource struct {
	sources []AlbumSource
}

// NewChainAlbumSource returns a chain over the given sources, tried in the
// order supplied.
func NewChainAlbumSource(sources ...AlbumSource) *ChainAlbumSource {
	return &ChainAlbumSource{sources: sources}
}

// Name implements AlbumSource.
func (c *ChainAlbumSource) Name() string { return "chain" }

// LocalAlbums tries each source in order and combines the results by this rule:
//
//   - The first EvidenceFound wins, returned as-is (Origin names the source
//     that found it, not the chain). Later sources are not consulted; one
//     source finding albums is enough.
//   - EvidenceNone is returned ONLY when EVERY source returned EvidenceNone.
//   - Otherwise -- any source Unknown, and none found albums -> EvidenceUnknown.
//
// That last clause is the entire point of this type. "None" is unanimous or it
// does not happen: a single unreachable source must never be able to turn into
// "this artist has no albums", because that answer is exactly what a caller
// needs in order to stop objecting. Unknown is therefore ABSORBING in the
// no-albums direction (Unknown + None = Unknown) while Found still short-
// circuits, so an unavailable source can only ever cost the chain certainty,
// never manufacture it.
//
// A chain with no sources returns EvidenceUnknown, for the same reason: nothing
// looked, so nothing was determined. The "every source said None" condition is
// checked against a counter rather than as vacuous truth over an empty list.
func (c *ChainAlbumSource) LocalAlbums(ctx context.Context, a *Artist) (AlbumSet, error) {
	var (
		noneCount int
		errs      []error
	)

	for _, src := range c.sources {
		set, err := src.LocalAlbums(ctx, a)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", src.Name(), err))
		}
		// Trust Evidence, not the error: a source may report a partial failure
		// alongside a usable determination, and Evidence is the contract.
		switch set.Evidence {
		case EvidenceFound:
			if set.Origin == "" {
				set.Origin = src.Name()
			}
			return set, nil
		case EvidenceNone:
			noneCount++
		case EvidenceUnknown:
			// Counted only by omission: any Unknown means noneCount can no
			// longer reach len(c.sources), so the unanimity test below fails
			// and the chain reports Unknown. No separate flag needed.
		}
	}

	// errors.Join returns nil for an empty slice, so a fully successful chain
	// reports no error without a special case.
	joined := errors.Join(errs...)

	if len(c.sources) > 0 && noneCount == len(c.sources) {
		return AlbumSet{Evidence: EvidenceNone, Origin: c.Name()}, joined
	}

	return AlbumSet{Evidence: EvidenceUnknown, Origin: c.Name()}, joined
}

// EvidencedComparison is an AlbumComparison that also says whether the local
// side was a determination at all.
//
// AlbumComparison is EMBEDDED rather than held in a named field so the JSON
// shape is unchanged: the existing keys stay at the top level of the object,
// and Evidence is additive. Any handler currently returning an AlbumComparison
// can return this instead without breaking a client.
//
// Evidence is deliberately not JSON-tagged for the wire yet: this PR is a
// foundation and adds no API surface. See CompareAlbumSet.
type EvidencedComparison struct {
	AlbumComparison
	Evidence AlbumEvidence `json:"-"`
}

// CompareAlbumSet compares an evidenced local album set against remote titles.
//
// All matching is delegated to CompareAlbums -- there is deliberately no second
// copy of the normalization or the percentage arithmetic here, so the two paths
// cannot drift. The only thing this adds is carrying the local side's evidence
// state through to the result, so a caller can tell "0% match because the
// catalogues disagree" (Evidence == EvidenceNone or EvidenceFound: a real
// finding) from "0% match because we never read the local albums"
// (EvidenceUnknown: no finding at all).
//
// When Evidence is not EvidenceFound the local side contributes no titles, so
// the comparison is computed against an empty local list. That yields
// LocalCount 0 and MatchPercent 0, which is arithmetically the same shape a
// genuinely empty artist produces -- which is exactly why the Evidence field
// must be read, not the percentage.
func CompareAlbumSet(local AlbumSet, remoteTitles []string) EvidencedComparison {
	var localTitles []string
	if local.Evidence == EvidenceFound {
		localTitles = local.Titles
	}
	return EvidencedComparison{
		AlbumComparison: CompareAlbums(localTitles, remoteTitles),
		Evidence:        local.Evidence,
	}
}
