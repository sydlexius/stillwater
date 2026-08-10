// Package mbidcheck re-validates a stored MusicBrainz ID against MusicBrainz.
//
// # WHY THIS PACKAGE EXISTS AT ALL
//
// Stillwater has never checked a stored MusicBrainz ID ("MBID") beyond its
// UUID shape. A shape check passes for any well-formed id, including one
// belonging to a completely different act that happens to share the artist's
// name, so a wrong id has been indistinguishable from a right one and has
// propagated into every downstream fetch, NFO write and platform push as fact.
//
// The one rule that reads like it would catch this does not:
// checkArtistIDMismatch compares the artist's FOLDER NAME against its STORED
// NAME. In the motivating production case both strings were identical, so it
// scored 100% and passed while the stored id pointed at a different act
// entirely. Name comparison alone cannot separate two artists who share a
// name; only the release catalogue can.
//
// This package is the single-artist half of the answer: given one artist and
// its stored id, fetch from MusicBrainz and produce a classified verdict. It
// writes nothing, schedules nothing, and never mutates an artist. The
// rate-limited sweep that drives it and the operator surfacing of its verdicts
// are separate work.
//
// # WHY A SEPARATE PACKAGE
//
// It cannot live in internal/artist: that package is imported by
// internal/provider/musicbrainz, so an artist -> musicbrainz dependency is an
// import cycle. It does not belong in internal/rule either, which evaluates
// local state synchronously and does no network I/O. So: its own package,
// depending on internal/artist for the domain model and on narrow local
// interfaces for the MusicBrainz calls.
//
// # THE POPULATION, AND THE MISTAKE NOT TO MAKE
//
// Nothing here reads a provenance marker, and nothing added later should. The
// #2715 marker stamps ids adopted from that point forward, and nothing writes
// an operator-confirmed value today, so in practice it partitions artists into
// "machine-picked" and "unknown" -- not into "machine-picked" and
// "operator-confirmed". Every artist damaged before #2715 landed is unmarked.
// A resolver that skipped unmarked artists would skip exactly the population
// this feature exists to find and would report "nothing to fix".
//
// The same shape governs the local album catalogue, which is why this package
// takes an artist.AlbumSource rather than calling artist.ListLocalAlbums:
// EvidenceUnknown ("I could not look") must never be read as EvidenceNone
// ("this artist has no albums"). Collapsing those would vindicate a wrong id
// against a vacuously empty catalogue.
package mbidcheck

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
)

// ArtistFetcher fetches one artist's metadata by MusicBrainz id.
//
// Declared here rather than taking a *musicbrainz.Adapter so the resolver
// depends on the two calls it actually makes and the tests stay hermetic.
// *musicbrainz.Adapter satisfies it.
type ArtistFetcher interface {
	GetArtist(ctx context.Context, mbid string) (*provider.ArtistMetadata, error)
}

// ReleaseGroupFetcher fetches one artist's release groups by MusicBrainz id.
// *musicbrainz.Adapter satisfies it, as does provider.ReleaseGroupFetcher.
type ReleaseGroupFetcher interface {
	GetReleaseGroups(ctx context.Context, mbid string) ([]provider.ReleaseGroupInfo, error)
}

// MusicBrainzClient is the whole provider surface the resolver needs.
type MusicBrainzClient interface {
	ArtistFetcher
	ReleaseGroupFetcher
}

// ErrNoStoredMBID is returned by Resolve when there is nothing to check.
//
// It is an ERROR rather than a verdict on purpose. An artist with no stored id
// is not "not checkable" -- it is not a member of this feature's population at
// all, and writing a ledger row for it would put an artist that has no id into
// a report about ids. The caller filters, and this error catches a caller that
// did not.
var ErrNoStoredMBID = errors.New("mbidcheck: artist has no stored musicbrainz id")

// Default classification thresholds. Both are percentages on 0-100 and both
// are overridable, so the sweep can expose them as settings without this
// package growing a config dependency.
const (
	// DefaultNameSimilarityPercent is the score at or above which the remote
	// name is considered to match the local one.
	//
	// 80 mirrors the existing artist_id_mismatch tolerance (0.8) in
	// internal/rule, so the two name comparisons in the product agree on what
	// "same name" means. A LOWER bar than provider search's 60 would be wrong
	// here (this judges an id already adopted, not a candidate), and a higher
	// one starts rejecting legitimate alias and sort-name spellings.
	DefaultNameSimilarityPercent = 80

	// DefaultCatalogueMatchPercent is the share of LOCAL albums that must have
	// a remote counterpart for the catalogue to count as matching.
	//
	// 25, and the number is deliberately low. CompareAlbums scores the
	// percentage of local album DIRECTORIES with a title-equal remote release
	// group, so it is depressed by everything ordinary about a real library:
	// live sets, bootlegs, regional retitlings, folders that are not albums,
	// and MusicBrainz release groups titled differently from the disc. A high
	// bar would fail correct ids in bulk and bury the real findings.
	//
	// What this check must catch is not "imperfect overlap" but "NO overlap":
	// the motivating case scores 0%, and the production snapshot's 18 artists
	// score 0% against a remote catalogue that is entirely empty. 25 sits far
	// enough above 0 to still flag a near-total disagreement while leaving a
	// messy but correct library alone.
	DefaultCatalogueMatchPercent = 25
)

// Result is one artist's verdict plus the evidence behind it.
//
// Validation is the row for the ledger. Everything else is machine-readable
// context the sweep needs and the ledger's frozen columns cannot carry, so a
// caller never has to parse Detail prose to make a decision.
type Result struct {
	// Validation is the ledger row. Always populated, always passing
	// artist.MBIDValidation.Validate.
	Validation artist.MBIDValidation

	// LocalEvidence is the tri-state answer the album source gave.
	//
	// This is how a caller tells the two not-checkable local cases apart. The
	// ledger's reason vocabulary is a closed set frozen in a SQL CHECK and has
	// one value for both ("no_local_albums"), so EvidenceNone ("the artist
	// genuinely has no albums", a permanent state) and EvidenceUnknown ("the
	// path is missing or unreadable", a transient one worth retrying) reach
	// the same reason. They must NOT reach the same retry policy, and this
	// field is what keeps them separable.
	LocalEvidence artist.AlbumEvidence

	// RemoteReleaseGroupCount is how many release groups MusicBrainz returned,
	// -1 when they were never fetched.
	//
	// Zero here with local albums present is the strongest single signal in
	// this feature -- the id resolves to an entry that released nothing, while
	// the operator holds albums -- and the sweep should be able to surface it
	// as its own finding rather than as one more low percentage.
	RemoteReleaseGroupCount int

	// NameScorePercent is the best 0-100 similarity between the local name and
	// any name MusicBrainz carries for the id, -1 when the id never resolved.
	NameScorePercent int

	// Transient is set when the verdict reflects a condition on OUR side
	// (provider outage, timeout, canceled sweep, unreadable library path)
	// rather than anything about the artist.
	//
	// A provider outage that files twelve hundred artists as suspect is a
	// catastrophic false-positive event, so this field exists to let the sweep
	// apply a skip-don't-clear policy: keep the prior verdict rather than
	// overwrite a real finding with an artifact of a network blip.
	Transient bool
}

// ZeroRemoteCatalogue reports the headline case: the id resolves, the operator
// has albums on disk, and MusicBrainz lists no releases at all for that id.
//
// Exposed as a method so the sweep can raise a distinct operator-facing
// finding without string-matching Detail.
func (r Result) ZeroRemoteCatalogue() bool {
	return r.RemoteReleaseGroupCount == 0 &&
		r.LocalEvidence == artist.EvidenceFound &&
		r.Validation.Outcome == artist.MBIDOutcomeFailed
}

// Resolver checks one artist's stored MusicBrainz id at a time.
//
// It is stateless apart from its dependencies and safe for concurrent use, but
// note that its dependencies are not necessarily: the MusicBrainz adapter
// serializes behind a shared rate limiter, so a caller gains nothing by
// running many Resolve calls in parallel.
type Resolver struct {
	mb     MusicBrainzClient
	albums artist.AlbumSource
	logger *slog.Logger
	now    func() time.Time

	nameThreshold      int
	catalogueThreshold int
}

// Option configures a Resolver.
type Option func(*Resolver)

// WithNameThreshold overrides DefaultNameSimilarityPercent. Values outside
// 0-100 are ignored rather than clamped, so a misconfigured setting falls back
// to the documented default instead of silently disabling the check.
func WithNameThreshold(percent int) Option {
	return func(r *Resolver) {
		if percent >= 0 && percent <= 100 {
			r.nameThreshold = percent
		}
	}
}

// WithCatalogueThreshold overrides DefaultCatalogueMatchPercent, with the same
// ignore-out-of-range behavior as WithNameThreshold.
func WithCatalogueThreshold(percent int) Option {
	return func(r *Resolver) {
		if percent >= 0 && percent <= 100 {
			r.catalogueThreshold = percent
		}
	}
}

// WithLogger sets the logger. A nil logger is ignored.
func WithLogger(l *slog.Logger) Option {
	return func(r *Resolver) {
		if l != nil {
			r.logger = l
		}
	}
}

// WithClock overrides the clock used for MBIDValidation.CheckedAt, for tests.
func WithClock(now func() time.Time) Option {
	return func(r *Resolver) {
		if now != nil {
			r.now = now
		}
	}
}

// New returns a Resolver over the given MusicBrainz client and album source.
//
// Both dependencies are required; passing nil for either is a programming
// error and New panics, because a Resolver with a nil album source would
// otherwise resolve every artist to "not checkable" and read as a working
// feature finding nothing -- the exact failure mode this area keeps producing.
func New(mb MusicBrainzClient, albums artist.AlbumSource, opts ...Option) *Resolver {
	if mb == nil {
		panic("mbidcheck.New: nil MusicBrainz client")
	}
	if albums == nil {
		panic("mbidcheck.New: nil album source")
	}
	r := &Resolver{
		mb:                 mb,
		albums:             albums,
		logger:             slog.Default(),
		now:                time.Now,
		nameThreshold:      DefaultNameSimilarityPercent,
		catalogueThreshold: DefaultCatalogueMatchPercent,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Resolve checks one artist's stored MusicBrainz id and classifies the answer.
//
// It performs no writes of any kind: it never touches the artist record, never
// mutates MusicBrainzID, and never persists the returned row. Handing the
// Result to the ledger is the caller's job.
//
// # THE CLASSIFICATION TABLE
//
// The ledger's schema deliberately leaves the reason-to-outcome pairing
// unconstrained, so this table is the authority. Read top to bottom; the first
// matching row wins.
//
//	CONDITION                                    OUTCOME        REASON
//	-------------------------------------------  -------------  ----------------------------
//	no stored id                                 (error)        ErrNoStoredMBID
//	context already done                         not_checkable  provider_unavailable
//	GetArtist -> ErrNotFound                     not_checkable  mbid_not_found
//	GetArtist -> any other error, or nil result  not_checkable  provider_unavailable
//	local albums EvidenceUnknown                 not_checkable  no_local_albums
//	local albums EvidenceNone                    not_checkable  no_local_albums
//	GetReleaseGroups -> any error                not_checkable  provider_unavailable
//	remote catalogue empty, local albums exist    failed         catalogue_mismatch
//	name below threshold AND catalogue below      failed         resolves_to_different_artist
//	catalogue below threshold (name matches)      failed         catalogue_mismatch
//	name below threshold (catalogue matches)      failed         name_mismatch
//	otherwise                                     validated      (none)
//
// The reasoning behind each pairing:
//
//   - mbid_not_found is NOT a failure. MusicBrainz merges duplicate artist
//     entries and retires the id that lost, so an id that resolves to nobody
//     is quite possibly RIGHT and merely stale. Calling that "failed" would
//     send operators to break correct data. It is kept strictly distinct from
//     "resolves to a different artist", which is wrong rather than stale.
//
//   - provider_unavailable is never attributable to the artist, so it is
//     not_checkable and carries Transient. Every non-ErrNotFound provider
//     error lands here, including timeouts and cancellation: an outage that
//     files the whole library as suspect is worse than checking nothing.
//
//   - no_local_albums is not_checkable rather than validated-on-name-alone.
//     Name matching alone is exactly the test that let the motivating case
//     through, so an artist whose catalogue cannot be compared is reported as
//     unverified rather than waved past. LocalEvidence separates "genuinely no
//     albums" from "could not look".
//
//   - an EMPTY remote catalogue with local albums present is catalogue_mismatch
//     rather than resolves_to_different_artist, matching the ledger's own
//     documentation: this is the motivating shape, where the id belongs to
//     someone with no music releases at all. It is checked BEFORE the combined
//     name-and-catalogue test so it is never absorbed into the generic
//     different-artist bucket, and Result.ZeroRemoteCatalogue reports it
//     without parsing prose.
//
//   - BOTH signals disagreeing is resolves_to_different_artist: a live entry
//     with a different name and a disjoint catalogue is somebody else, and
//     that is the headline finding.
//
//   - catalogue disagreeing ALONE is catalogue_mismatch. This is the case the
//     feature exists for: the motivating production case matched the name
//     exactly, at 100% by any string measure, on a completely wrong artist.
//
//   - name disagreeing ALONE is failed/name_mismatch rather than validated.
//     It should be rare, since the name is scored against the primary name,
//     the sort-name and every alias, with sort-name word order normalized. A
//     failed row changes nothing automatically -- it is a flag for review --
//     so flagging an anomaly costs an operator a look, while vindicating one
//     hides an id nobody will ever re-examine.
func (r *Resolver) Resolve(ctx context.Context, a *artist.Artist) (Result, error) {
	if a == nil {
		return Result{}, fmt.Errorf("mbidcheck: nil artist")
	}
	mbid := strings.TrimSpace(a.MusicBrainzID)
	if mbid == "" {
		return Result{}, ErrNoStoredMBID
	}

	// Checked before any work: a canceled sweep must not attribute its own
	// shutdown to an artist.
	if err := ctx.Err(); err != nil {
		return r.transient(a, mbid, "", fmt.Sprintf("check abandoned before starting: %v", err)), nil
	}

	meta, err := r.mb.GetArtist(ctx, mbid)
	switch {
	case err != nil:
		var notFound *provider.ErrNotFound
		if errors.As(err, &notFound) {
			return r.notFound(a, mbid), nil
		}
		return r.transient(a, mbid, "", fmt.Sprintf("musicbrainz artist lookup failed: %v", err)), nil
	case meta == nil:
		// A nil result with a nil error is a broken client, not a statement
		// about the artist. Classify it as our problem, never theirs.
		return r.transient(a, mbid, "", "musicbrainz returned no artist and no error"), nil
	}

	resolvedName := meta.Name
	nameScore := bestNameScore(a, meta)

	set, albumErr := r.albums.LocalAlbums(ctx, a)
	switch set.Evidence {
	case artist.EvidenceFound:
		// The only state with a catalogue to compare. Falls through to the
		// release-group fetch below.
	case artist.EvidenceUnknown:
		// "I could not look" is NOT "the artist has no albums". Transient,
		// because a missing mount or a permission problem is worth retrying.
		res := r.notCheckable(a, mbid, artist.MBIDReasonNoLocalAlbums,
			fmt.Sprintf("local album catalogue could not be read (evidence unknown): %v", albumErr))
		res.Validation.ResolvedName = resolvedName
		res.LocalEvidence = artist.EvidenceUnknown
		res.NameScorePercent = nameScore
		res.Transient = true
		return res, nil
	case artist.EvidenceNone:
		// A real determination: the source looked and found nothing. Not
		// transient, and not a validation either -- there is no catalogue to
		// compare, and the name alone has already been shown insufficient.
		res := r.notCheckable(a, mbid, artist.MBIDReasonNoLocalAlbums,
			"artist has no albums on disk, so the catalogue comparison had nothing to compare")
		res.Validation.ResolvedName = resolvedName
		res.LocalEvidence = artist.EvidenceNone
		res.NameScorePercent = nameScore
		return res, nil
	default:
		// An AlbumEvidence value this code does not recognize is, by
		// definition, not a determination. It resolves toward "unknown"
		// rather than toward the comparison, so a future fourth state cannot
		// silently acquire the power to vindicate an id.
		res := r.notCheckable(a, mbid, artist.MBIDReasonNoLocalAlbums,
			fmt.Sprintf("album source reported an unrecognized evidence state %q", set.Evidence))
		res.Validation.ResolvedName = resolvedName
		res.NameScorePercent = nameScore
		res.Transient = true
		return res, nil
	}

	remote, err := r.mb.GetReleaseGroups(ctx, mbid)
	if err != nil {
		// Including ErrNotFound. The artist lookup already succeeded, so a
		// not-found on the release-group browse is a provider anomaly rather
		// than proof of an empty catalogue, and mistaking it for one would
		// condemn a correct id on the strongest evidence this feature has.
		res := r.transient(a, mbid, resolvedName,
			fmt.Sprintf("musicbrainz release-group lookup failed: %v", err))
		res.LocalEvidence = set.Evidence
		res.NameScorePercent = nameScore
		return res, nil
	}

	remoteTitles := make([]string, 0, len(remote))
	for _, rg := range remote {
		remoteTitles = append(remoteTitles, rg.Title)
	}

	// CompareAlbumSet delegates to CompareAlbums, whose MatchPercent is ALREADY
	// a 0-100 percentage of local albums with a remote counterpart. It is not a
	// fraction and must not be divided: emitting 0.965 for a 96.5% match would
	// file a near-perfect catalogue as the strongest possible evidence the id
	// is wrong, and artist.MBIDValidation.Validate's 0-100 range check would
	// not catch it because a fraction is inside that range.
	comp := artist.CompareAlbumSet(set, remoteTitles)
	cataloguePercent := float64(comp.MatchPercent)

	nameMatches := nameScore >= r.nameThreshold
	catalogueMatches := comp.MatchPercent >= r.catalogueThreshold

	res := Result{
		LocalEvidence:           set.Evidence,
		RemoteReleaseGroupCount: len(remote),
		NameScorePercent:        nameScore,
		Validation: artist.MBIDValidation{
			ArtistID:              a.ID,
			MBID:                  mbid,
			ResolvedName:          resolvedName,
			CatalogueMatchPercent: &cataloguePercent,
			CheckedAt:             r.now().UTC(),
		},
	}

	switch {
	case len(remote) == 0:
		res.Validation.Outcome = artist.MBIDOutcomeFailed
		res.Validation.Reason = artist.MBIDReasonCatalogueMismatch
		res.Validation.Detail = fmt.Sprintf(
			"remote catalogue empty: musicbrainz lists no releases for this id, while %d album(s) are on disk (resolved name %q, name similarity %d%%)",
			comp.LocalCount, resolvedName, nameScore)
	case !nameMatches && !catalogueMatches:
		res.Validation.Outcome = artist.MBIDOutcomeFailed
		res.Validation.Reason = artist.MBIDReasonResolvesToDifferentArtist
		res.Validation.Detail = fmt.Sprintf(
			"id resolves to %q: name similarity %d%% (threshold %d%%) and catalogue match %d%% (threshold %d%%, %d of %d local albums)",
			resolvedName, nameScore, r.nameThreshold,
			comp.MatchPercent, r.catalogueThreshold, comp.MatchCount, comp.LocalCount)
	case !catalogueMatches:
		res.Validation.Outcome = artist.MBIDOutcomeFailed
		res.Validation.Reason = artist.MBIDReasonCatalogueMismatch
		res.Validation.Detail = fmt.Sprintf(
			"catalogue match %d%% is below the %d%% threshold: %d of %d local albums appear among %d remote release group(s) for %q",
			comp.MatchPercent, r.catalogueThreshold, comp.MatchCount, comp.LocalCount, len(remote), resolvedName)
	case !nameMatches:
		res.Validation.Outcome = artist.MBIDOutcomeFailed
		res.Validation.Reason = artist.MBIDReasonNameMismatch
		res.Validation.Detail = fmt.Sprintf(
			"catalogue matches at %d%% but name similarity is %d%% (threshold %d%%): local %q vs remote %q",
			comp.MatchPercent, nameScore, r.nameThreshold, a.Name, resolvedName)
	default:
		res.Validation.Outcome = artist.MBIDOutcomeValidated
		res.Validation.Reason = artist.MBIDReasonNone
		res.Validation.Detail = fmt.Sprintf(
			"name similarity %d%%, catalogue match %d%% (%d of %d local albums among %d remote release groups)",
			nameScore, comp.MatchPercent, comp.MatchCount, comp.LocalCount, len(remote))
	}

	if err := res.Validation.Validate(); err != nil {
		return Result{}, fmt.Errorf("mbidcheck: built an invalid verdict for artist %s: %w", a.ID, err)
	}
	r.log(res)
	return res, nil
}

// notFound builds the "MusicBrainz has no artist under this id" verdict.
func (r *Resolver) notFound(a *artist.Artist, mbid string) Result {
	res := r.notCheckable(a, mbid, artist.MBIDReasonMBIDNotFound,
		"musicbrainz has no artist under this id; it may have been merged upstream, which leaves a correct-but-stale id")
	res.NameScorePercent = -1
	return res
}

// transient builds a provider_unavailable verdict. resolvedName is carried
// when the artist lookup had already succeeded.
func (r *Resolver) transient(a *artist.Artist, mbid, resolvedName, detail string) Result {
	res := r.notCheckable(a, mbid, artist.MBIDReasonProviderUnavailable, detail)
	res.Validation.ResolvedName = resolvedName
	res.Transient = true
	return res
}

// notCheckable builds the common shape of every not-checkable verdict:
// CatalogueMatchPercent stays nil, because nil means "not measured" and a
// literal 0 would be indistinguishable from the strongest evidence this
// feature can produce.
func (r *Resolver) notCheckable(a *artist.Artist, mbid string, reason artist.MBIDValidationReason, detail string) Result {
	return Result{
		LocalEvidence:           artist.EvidenceUnknown,
		RemoteReleaseGroupCount: -1,
		NameScorePercent:        -1,
		Validation: artist.MBIDValidation{
			ArtistID:  a.ID,
			MBID:      mbid,
			Outcome:   artist.MBIDOutcomeNotCheckable,
			Reason:    reason,
			Detail:    detail,
			CheckedAt: r.now().UTC(),
		},
	}
}

// log emits one line per verdict at a level matching its severity.
func (r *Resolver) log(res Result) {
	attrs := []any{
		slog.String("artist_id", res.Validation.ArtistID),
		slog.String("mbid", res.Validation.MBID),
		slog.String("outcome", string(res.Validation.Outcome)),
		slog.String("reason", string(res.Validation.Reason)),
		slog.String("local_evidence", res.LocalEvidence.String()),
		slog.Int("remote_release_groups", res.RemoteReleaseGroupCount),
		slog.Int("name_similarity_percent", res.NameScorePercent),
	}
	if res.Validation.CatalogueMatchPercent != nil {
		attrs = append(attrs, slog.Float64("catalogue_match_percent", *res.Validation.CatalogueMatchPercent))
	}
	if res.Validation.Outcome == artist.MBIDOutcomeFailed {
		r.logger.Warn("stored musicbrainz id failed re-validation", attrs...)
		return
	}
	r.logger.Debug("stored musicbrainz id re-validated", attrs...)
}

// bestNameScore scores the local artist name against every name MusicBrainz
// carries for the id: the primary name, the sort-name, and each alias.
//
// Taking the MAXIMUM is deliberate -- matching any one of an artist's real
// names is positive evidence, and an artist with forty aliases must not be
// penalized for the thirty-nine that do not match.
//
// Sort-name word order is normalized on BOTH sides before scoring, in addition
// to scoring the raw strings. provider.NormalizeName strips a LEADING "The "
// but not a TRAILING ", The", so "Beatles, The" against "The Beatles" scores
// 64 by raw comparison and would be reported as a name mismatch on an artist
// whose catalogue matches perfectly. Since a failed row is an operator's time,
// spending it on a known-benign spelling difference is a defect.
func bestNameScore(a *artist.Artist, meta *provider.ArtistMetadata) int {
	candidates := make([]string, 0, 2+len(meta.Aliases))
	candidates = append(candidates, meta.Name, meta.SortName)
	candidates = append(candidates, meta.Aliases...)

	best := provider.BestNameSimilarity(a.Name, candidates...)
	if a.SortName != "" {
		if s := provider.BestNameSimilarity(a.SortName, candidates...); s > best {
			best = s
		}
	}
	if best == 100 {
		return best
	}

	normLocal := make([]string, 0, 2)
	for _, n := range []string{a.Name, a.SortName} {
		if v := unsortName(n); v != "" {
			normLocal = append(normLocal, v)
		}
	}
	normRemote := make([]string, 0, len(candidates))
	for _, n := range candidates {
		if v := unsortName(n); v != "" {
			normRemote = append(normRemote, v)
		}
	}
	for _, l := range normLocal {
		if s := provider.BestNameSimilarity(l, normRemote...); s > best {
			best = s
		}
	}
	return best
}

// unsortName rewrites a sort-name-style string into natural order: "Beatles,
// The" becomes "the beatles", which provider.NormalizeName then reduces to
// "beatles". A name with no comma is returned normalized but otherwise
// untouched. Only a SINGLE trailing comma-separated fragment is moved, so
// "Emerson, Lake & Palmer" is left alone by the trailing-article test below.
func unsortName(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if idx := strings.LastIndex(s, ","); idx > 0 {
		head := strings.TrimSpace(s[:idx])
		tail := strings.TrimSpace(s[idx+1:])
		// Only articles move. Anything else after a comma is part of the name
		// (a band with a comma in it, a "Surname, Forename" person whose
		// reordering would not change the normalized comparison anyway).
		switch strings.ToLower(tail) {
		case "the", "a", "an":
			s = tail + " " + head
		}
	}
	return provider.NormalizeName(s)
}
