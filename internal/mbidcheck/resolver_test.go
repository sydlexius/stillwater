package mbidcheck

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/internal/provider/musicbrainz"
)

// The real adapter must satisfy the narrow interfaces this package declares,
// or the hermetic fakes below would be testing a shape production never uses.
var _ MusicBrainzClient = (*musicbrainz.Adapter)(nil)

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

// fakeMB is a scripted MusicBrainz client.
type fakeMB struct {
	meta    *provider.ArtistMetadata
	metaErr error

	groups    []provider.ReleaseGroupInfo
	groupsErr error

	artistCalls, groupCalls int
}

func (f *fakeMB) GetArtist(_ context.Context, _ string) (*provider.ArtistMetadata, error) {
	f.artistCalls++
	return f.meta, f.metaErr
}

func (f *fakeMB) GetReleaseGroups(_ context.Context, _ string) ([]provider.ReleaseGroupInfo, error) {
	f.groupCalls++
	return f.groups, f.groupsErr
}

// stubAlbums returns a fixed AlbumSet, so a test can pin an evidence state
// without needing a filesystem. The filesystem-backed source is exercised
// separately by TestResolveUsesRealFilesystemAlbumSource.
type stubAlbums struct {
	set artist.AlbumSet
	err error
}

func (s stubAlbums) Name() string { return "stub" }

func (s stubAlbums) LocalAlbums(_ context.Context, _ *artist.Artist) (artist.AlbumSet, error) {
	set := s.set
	if set.Origin == "" {
		set.Origin = s.Name()
	}
	return set, s.err
}

func found(titles ...string) stubAlbums {
	return stubAlbums{set: artist.AlbumSet{Titles: titles, Evidence: artist.EvidenceFound}}
}

func rgs(titles ...string) []provider.ReleaseGroupInfo {
	out := make([]provider.ReleaseGroupInfo, 0, len(titles))
	for i, t := range titles {
		out = append(out, provider.ReleaseGroupInfo{ID: fmt.Sprintf("rg-%d", i), Title: t})
	}
	return out
}

// newTestResolver is New with the logger silenced. Every verdict logs, and a
// table of forty cases otherwise drowns a real failure in WARN lines.
func newTestResolver(mb MusicBrainzClient, albums artist.AlbumSource, opts ...Option) *Resolver {
	return New(mb, albums, append([]Option{WithLogger(slog.New(slog.DiscardHandler))}, opts...)...)
}

func testArtist() *artist.Artist {
	return &artist.Artist{
		ID:            "artist-1",
		Name:          "Example Band",
		MusicBrainzID: "11111111-2222-3333-4444-555555555555",
		Path:          "/library/Example Band",
	}
}

// ---------------------------------------------------------------------------
// Classification table
// ---------------------------------------------------------------------------

// TestResolveClassification walks every row of the classification table in the
// Resolve doc comment. Each case asserts the outcome, the reason, and the
// evidence fields the sweep will branch on.
func TestResolveClassification(t *testing.T) {
	t.Parallel()

	fixed := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name   string
		artist *artist.Artist
		mb     *fakeMB
		albums artist.AlbumSource

		wantOutcome   artist.MBIDValidationOutcome
		wantReason    artist.MBIDValidationReason
		wantTransient bool
		// wantPercent is checked only when wantPercentSet; otherwise the test
		// asserts CatalogueMatchPercent is nil ("not measured").
		wantPercent    float64
		wantPercentSet bool
		wantEvidence   artist.AlbumEvidence
		wantResolved   string
		wantZeroRemote bool
		// wantDetail, when set, must appear in the operator-facing detail. It
		// pins WHICH branch produced the verdict where two branches share a
		// reason.
		wantDetail string
	}{
		{
			name:   "validated: name and catalogue both match",
			artist: testArtist(),
			mb: &fakeMB{
				meta:   &provider.ArtistMetadata{Name: "Example Band"},
				groups: rgs("First Record", "Second Record", "Third Record"),
			},
			albums:         found("First Record", "Second Record", "Third Record"),
			wantOutcome:    artist.MBIDOutcomeValidated,
			wantReason:     artist.MBIDReasonNone,
			wantPercent:    100,
			wantPercentSet: true,
			wantEvidence:   artist.EvidenceFound,
			wantResolved:   "Example Band",
		},
		{
			name:   "failed: resolves to a different artist (name AND catalogue disagree)",
			artist: testArtist(),
			mb: &fakeMB{
				meta:   &provider.ArtistMetadata{Name: "Completely Other Person"},
				groups: rgs("Unrelated Release", "Another Unrelated"),
			},
			albums:         found("First Record", "Second Record"),
			wantOutcome:    artist.MBIDOutcomeFailed,
			wantReason:     artist.MBIDReasonResolvesToDifferentArtist,
			wantPercent:    0,
			wantPercentSet: true,
			wantEvidence:   artist.EvidenceFound,
			wantResolved:   "Completely Other Person",
		},
		{
			// The 18-artist production case: same name, zero remote releases,
			// albums on disk.
			name:   "failed: zero remote release groups while local albums exist",
			artist: testArtist(),
			mb: &fakeMB{
				meta:   &provider.ArtistMetadata{Name: "Example Band"},
				groups: nil,
			},
			albums:         found("First Record", "Second Record"),
			wantOutcome:    artist.MBIDOutcomeFailed,
			wantReason:     artist.MBIDReasonCatalogueMismatch,
			wantPercent:    0,
			wantPercentSet: true,
			wantEvidence:   artist.EvidenceFound,
			wantResolved:   "Example Band",
			wantZeroRemote: true,
			wantDetail:     "remote catalogue empty",
		},
		{
			// The empty-catalogue signal takes priority over the generic
			// different-artist bucket even when the name ALSO disagrees, so
			// "musicbrainz lists nothing for this id" stays one distinguishable
			// finding rather than being blurred into the pile.
			name:   "failed: zero remote release groups outranks a name mismatch",
			artist: testArtist(),
			mb: &fakeMB{
				meta:   &provider.ArtistMetadata{Name: "Someone Entirely Else"},
				groups: nil,
			},
			albums:         found("First Record", "Second Record"),
			wantOutcome:    artist.MBIDOutcomeFailed,
			wantReason:     artist.MBIDReasonCatalogueMismatch,
			wantPercent:    0,
			wantPercentSet: true,
			wantEvidence:   artist.EvidenceFound,
			wantResolved:   "Someone Entirely Else",
			wantZeroRemote: true,
			wantDetail:     "remote catalogue empty",
		},
		{
			// The motivating shape with a non-empty but disjoint catalogue:
			// exact name match, no album overlap at all.
			name:   "failed: catalogue mismatch with an exact name match",
			artist: testArtist(),
			mb: &fakeMB{
				meta:   &provider.ArtistMetadata{Name: "Example Band"},
				groups: rgs("Spoken Word Lecture", "Interview Sessions"),
			},
			albums:         found("First Record", "Second Record", "Third Record"),
			wantOutcome:    artist.MBIDOutcomeFailed,
			wantReason:     artist.MBIDReasonCatalogueMismatch,
			wantPercent:    0,
			wantPercentSet: true,
			wantEvidence:   artist.EvidenceFound,
			wantResolved:   "Example Band",
		},
		{
			name:   "failed: name mismatch while the catalogue matches",
			artist: testArtist(),
			mb: &fakeMB{
				meta:   &provider.ArtistMetadata{Name: "Totally Different Name"},
				groups: rgs("First Record", "Second Record"),
			},
			albums:         found("First Record", "Second Record"),
			wantOutcome:    artist.MBIDOutcomeFailed,
			wantReason:     artist.MBIDReasonNameMismatch,
			wantPercent:    100,
			wantPercentSet: true,
			wantEvidence:   artist.EvidenceFound,
			wantResolved:   "Totally Different Name",
		},
		{
			name:   "not_checkable: mbid not found at musicbrainz",
			artist: testArtist(),
			mb: &fakeMB{
				metaErr: &provider.ErrNotFound{Provider: provider.NameMusicBrainz, ID: "x"},
			},
			albums:        found("First Record"),
			wantOutcome:   artist.MBIDOutcomeNotCheckable,
			wantReason:    artist.MBIDReasonMBIDNotFound,
			wantTransient: false,
			wantEvidence:  artist.EvidenceUnknown,
		},
		{
			name:   "not_checkable: provider unavailable on the artist lookup",
			artist: testArtist(),
			mb: &fakeMB{
				metaErr: &provider.ErrProviderUnavailable{Provider: provider.NameMusicBrainz, Cause: errors.New("connection refused")},
			},
			albums:        found("First Record"),
			wantOutcome:   artist.MBIDOutcomeNotCheckable,
			wantReason:    artist.MBIDReasonProviderUnavailable,
			wantTransient: true,
			wantEvidence:  artist.EvidenceUnknown,
		},
		{
			name:          "not_checkable: bare network error on the artist lookup",
			artist:        testArtist(),
			mb:            &fakeMB{metaErr: errors.New("dial tcp: i/o timeout")},
			albums:        found("First Record"),
			wantOutcome:   artist.MBIDOutcomeNotCheckable,
			wantReason:    artist.MBIDReasonProviderUnavailable,
			wantTransient: true,
			wantEvidence:  artist.EvidenceUnknown,
		},
		{
			name:          "not_checkable: client returned no artist and no error",
			artist:        testArtist(),
			mb:            &fakeMB{},
			albums:        found("First Record"),
			wantOutcome:   artist.MBIDOutcomeNotCheckable,
			wantReason:    artist.MBIDReasonProviderUnavailable,
			wantTransient: true,
			wantEvidence:  artist.EvidenceUnknown,
		},
		{
			name:   "not_checkable: provider unavailable on the release-group lookup",
			artist: testArtist(),
			mb: &fakeMB{
				meta:      &provider.ArtistMetadata{Name: "Example Band"},
				groupsErr: &provider.ErrProviderUnavailable{Provider: provider.NameMusicBrainz, Cause: errors.New("503")},
			},
			albums:        found("First Record"),
			wantOutcome:   artist.MBIDOutcomeNotCheckable,
			wantReason:    artist.MBIDReasonProviderUnavailable,
			wantTransient: true,
			wantEvidence:  artist.EvidenceFound,
			wantResolved:  "Example Band",
		},
		{
			// A not-found on the release-group BROWSE is a provider anomaly,
			// not proof of an empty catalogue. Mistaking it for one would
			// condemn a correct id on this feature's strongest evidence.
			name:   "not_checkable: not-found on the release-group lookup is not an empty catalogue",
			artist: testArtist(),
			mb: &fakeMB{
				meta:      &provider.ArtistMetadata{Name: "Example Band"},
				groupsErr: &provider.ErrNotFound{Provider: provider.NameMusicBrainz, ID: "x"},
			},
			albums:        found("First Record"),
			wantOutcome:   artist.MBIDOutcomeNotCheckable,
			wantReason:    artist.MBIDReasonProviderUnavailable,
			wantTransient: true,
			wantEvidence:  artist.EvidenceFound,
			wantResolved:  "Example Band",
		},
		{
			name:   "not_checkable: local albums unknown (could not look)",
			artist: testArtist(),
			mb: &fakeMB{
				meta:   &provider.ArtistMetadata{Name: "Example Band"},
				groups: rgs("First Record"),
			},
			albums: stubAlbums{
				set: artist.AlbumSet{Evidence: artist.EvidenceUnknown},
				err: errors.New("permission denied"),
			},
			wantOutcome:   artist.MBIDOutcomeNotCheckable,
			wantReason:    artist.MBIDReasonNoLocalAlbums,
			wantTransient: true,
			wantEvidence:  artist.EvidenceUnknown,
			wantResolved:  "Example Band",
		},
		{
			name:   "not_checkable: local albums none (looked, found nothing)",
			artist: testArtist(),
			mb: &fakeMB{
				meta:   &provider.ArtistMetadata{Name: "Example Band"},
				groups: rgs("First Record"),
			},
			albums:        stubAlbums{set: artist.AlbumSet{Evidence: artist.EvidenceNone}},
			wantOutcome:   artist.MBIDOutcomeNotCheckable,
			wantReason:    artist.MBIDReasonNoLocalAlbums,
			wantTransient: false,
			wantEvidence:  artist.EvidenceNone,
			wantResolved:  "Example Band",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// PRECONDITION: the fixture must actually have the property the
			// case is named for, or the assertion below is vacuous.
			set, _ := tc.albums.LocalAlbums(t.Context(), tc.artist)
			if set.Evidence != tc.wantEvidence && tc.wantEvidence != artist.EvidenceUnknown {
				t.Fatalf("precondition: album fixture evidence = %v, case expects %v", set.Evidence, tc.wantEvidence)
			}
			if tc.artist.MusicBrainzID == "" {
				t.Fatal("precondition: fixture artist must carry a stored MBID")
			}

			r := newTestResolver(tc.mb, tc.albums, WithClock(func() time.Time { return fixed }))
			got, err := r.Resolve(t.Context(), tc.artist)
			if err != nil {
				t.Fatalf("Resolve: unexpected error: %v", err)
			}

			if got.Validation.Outcome != tc.wantOutcome {
				t.Errorf("outcome = %q, want %q (detail: %s)", got.Validation.Outcome, tc.wantOutcome, got.Validation.Detail)
			}
			if got.Validation.Reason != tc.wantReason {
				t.Errorf("reason = %q, want %q (detail: %s)", got.Validation.Reason, tc.wantReason, got.Validation.Detail)
			}
			if got.Transient != tc.wantTransient {
				t.Errorf("Transient = %v, want %v", got.Transient, tc.wantTransient)
			}
			if got.LocalEvidence != tc.wantEvidence {
				t.Errorf("LocalEvidence = %v, want %v", got.LocalEvidence, tc.wantEvidence)
			}
			if got.Validation.ResolvedName != tc.wantResolved {
				t.Errorf("ResolvedName = %q, want %q", got.Validation.ResolvedName, tc.wantResolved)
			}
			if tc.wantDetail != "" && !strings.Contains(got.Validation.Detail, tc.wantDetail) {
				t.Errorf("Detail = %q, want it to contain %q", got.Validation.Detail, tc.wantDetail)
			}
			if got.ZeroRemoteCatalogue() != tc.wantZeroRemote {
				t.Errorf("ZeroRemoteCatalogue() = %v, want %v", got.ZeroRemoteCatalogue(), tc.wantZeroRemote)
			}

			switch {
			case tc.wantPercentSet:
				if got.Validation.CatalogueMatchPercent == nil {
					t.Fatalf("CatalogueMatchPercent = nil, want %v measured", tc.wantPercent)
				}
				if math.Abs(*got.Validation.CatalogueMatchPercent-tc.wantPercent) > 1e-9 {
					t.Errorf("CatalogueMatchPercent = %v, want %v", *got.Validation.CatalogueMatchPercent, tc.wantPercent)
				}
			default:
				// nil means NOT MEASURED. Writing 0 here would make the
				// strongest evidence this feature produces the default state
				// of every unmeasured row.
				if got.Validation.CatalogueMatchPercent != nil {
					t.Errorf("CatalogueMatchPercent = %v, want nil (never measured)", *got.Validation.CatalogueMatchPercent)
				}
			}

			// Every verdict this package emits must be persistable.
			if err := got.Validation.Validate(); err != nil {
				t.Errorf("emitted verdict is not persistable: %v", err)
			}
			if got.Validation.ArtistID != tc.artist.ID {
				t.Errorf("ArtistID = %q, want %q", got.Validation.ArtistID, tc.artist.ID)
			}
			if got.Validation.MBID != tc.artist.MusicBrainzID {
				t.Errorf("MBID = %q, want %q", got.Validation.MBID, tc.artist.MusicBrainzID)
			}
			if !got.Validation.CheckedAt.Equal(fixed) {
				t.Errorf("CheckedAt = %v, want %v", got.Validation.CheckedAt, fixed)
			}
		})
	}
}

// TestResolveEmitsPercentNotFraction is the guard against the specific defect
// artist.MBIDValidation.Validate cannot catch: a fraction (0.5 for 50%) is
// inside the legal 0-100 range, so a resolver that divided by 100 would file a
// near-perfect catalogue match as near-total disagreement and every schema
// check in the stack would pass it.
//
// The fixture is chosen so the fraction and the percentage are far apart and
// neither is 0 or 1.
func TestResolveEmitsPercentNotFraction(t *testing.T) {
	t.Parallel()

	local := found("A Record", "B Record", "C Record", "D Record")
	mb := &fakeMB{
		meta:   &provider.ArtistMetadata{Name: "Example Band"},
		groups: rgs("A Record", "B Record", "C Record"),
	}

	// PRECONDITION: CompareAlbums must itself report a percentage, and one
	// that is neither 0 nor 100, or the fraction and the percent could not be
	// told apart.
	set, _ := local.LocalAlbums(t.Context(), testArtist())
	comp := artist.CompareAlbumSet(set, []string{"A Record", "B Record", "C Record"})
	if comp.MatchPercent != 75 {
		t.Fatalf("precondition: CompareAlbums MatchPercent = %d, want 75", comp.MatchPercent)
	}

	r := newTestResolver(mb, local)
	got, err := r.Resolve(t.Context(), testArtist())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Validation.CatalogueMatchPercent == nil {
		t.Fatal("CatalogueMatchPercent = nil, want a measurement")
	}
	pct := *got.Validation.CatalogueMatchPercent
	if pct != 75 {
		t.Fatalf("CatalogueMatchPercent = %v, want 75 (a PERCENT on 0-100, not a fraction)", pct)
	}
	// Stated separately so a failure names the actual defect.
	if pct > 0 && pct <= 1 {
		t.Fatalf("CatalogueMatchPercent = %v looks like a FRACTION, not a percent", pct)
	}
	if got.Validation.Outcome != artist.MBIDOutcomeValidated {
		t.Errorf("outcome = %q, want validated at 75%% catalogue match", got.Validation.Outcome)
	}
}

// TestProviderOutageNeverBlamesTheArtist pins the guard against a catastrophic
// false-positive event: a MusicBrainz outage that files the whole library as
// suspect. Every transport-shaped failure must land as not_checkable with
// provider_unavailable, must be marked Transient, and must never be failed.
func TestProviderOutageNeverBlamesTheArtist(t *testing.T) {
	t.Parallel()

	outages := map[string]error{
		"connection refused":  &provider.ErrProviderUnavailable{Provider: provider.NameMusicBrainz, Cause: errors.New("connect: connection refused")},
		"rate limited":        &provider.ErrProviderUnavailable{Provider: provider.NameMusicBrainz, Cause: errors.New("429"), RetryAfter: time.Second},
		"deadline exceeded":   context.DeadlineExceeded,
		"context canceled":    context.Canceled,
		"bare transport":      errors.New("dial tcp 1.2.3.4:443: i/o timeout"),
		"wrapped unavailable": fmt.Errorf("fetching: %w", &provider.ErrProviderUnavailable{Provider: provider.NameMusicBrainz, Cause: errors.New("502")}),
	}

	for name, outage := range outages {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// PRECONDITION: the injected error must not be an ErrNotFound, or
			// this would be testing the mbid_not_found path by accident.
			var nf *provider.ErrNotFound
			if errors.As(outage, &nf) {
				t.Fatalf("precondition: outage fixture %q is an ErrNotFound", name)
			}

			for _, stage := range []string{"artist", "release-groups"} {
				mb := &fakeMB{meta: &provider.ArtistMetadata{Name: "Example Band"}}
				if stage == "artist" {
					mb.metaErr = outage
				} else {
					mb.groupsErr = outage
				}

				r := newTestResolver(mb, found("First Record"))
				got, err := r.Resolve(t.Context(), testArtist())
				if err != nil {
					t.Fatalf("%s stage: Resolve returned an error: %v", stage, err)
				}
				if got.Validation.Outcome == artist.MBIDOutcomeFailed {
					t.Fatalf("%s stage: provider outage produced a FAILED verdict; an outage must never be attributed to the artist", stage)
				}
				if got.Validation.Outcome != artist.MBIDOutcomeNotCheckable {
					t.Errorf("%s stage: outcome = %q, want not_checkable", stage, got.Validation.Outcome)
				}
				if got.Validation.Reason != artist.MBIDReasonProviderUnavailable {
					t.Errorf("%s stage: reason = %q, want provider_unavailable", stage, got.Validation.Reason)
				}
				if !got.Transient {
					t.Errorf("%s stage: Transient = false, want true so the sweep keeps the prior verdict", stage)
				}
				if got.Validation.CatalogueMatchPercent != nil {
					t.Errorf("%s stage: CatalogueMatchPercent = %v, want nil (nothing was measured)", stage, *got.Validation.CatalogueMatchPercent)
				}
			}
		})
	}
}

// TestResolveCancelledContextIsNotCheckable pins the sweep-shutdown case: a
// canceled context must not reach the provider at all, and must not produce a
// verdict attributable to the artist.
func TestResolveCancelledContextIsNotCheckable(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	// PRECONDITION: the context really is done before the call.
	if ctx.Err() == nil {
		t.Fatal("precondition: context should already be canceled")
	}

	mb := &fakeMB{meta: &provider.ArtistMetadata{Name: "Example Band"}, groups: rgs("First Record")}
	r := newTestResolver(mb, found("First Record"))

	got, err := r.Resolve(ctx, testArtist())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Validation.Outcome != artist.MBIDOutcomeNotCheckable || got.Validation.Reason != artist.MBIDReasonProviderUnavailable {
		t.Errorf("outcome/reason = %q/%q, want not_checkable/provider_unavailable", got.Validation.Outcome, got.Validation.Reason)
	}
	if !got.Transient {
		t.Error("Transient = false, want true")
	}
	if mb.artistCalls != 0 || mb.groupCalls != 0 {
		t.Errorf("provider was called on a canceled context: artist=%d groups=%d", mb.artistCalls, mb.groupCalls)
	}
}

// TestZeroRemoteCatalogueDistinguishedFromUnknownLocal is the anti-vindication
// test. The same empty remote catalogue must be a FAILURE when local albums
// were genuinely found and NOT a failure when the local side is unknown --
// otherwise an unreadable mount vindicates or condemns an id on no evidence.
func TestZeroRemoteCatalogueDistinguishedFromUnknownLocal(t *testing.T) {
	t.Parallel()

	newMB := func() *fakeMB {
		return &fakeMB{meta: &provider.ArtistMetadata{Name: "Example Band"}, groups: nil}
	}

	// PRECONDITION: both runs face an EMPTY remote catalogue.
	if len(newMB().groups) != 0 {
		t.Fatal("precondition: remote fixture must be empty")
	}

	withAlbums, err := newTestResolver(newMB(), found("First Record", "Second Record")).Resolve(t.Context(), testArtist())
	if err != nil {
		t.Fatalf("Resolve (albums found): %v", err)
	}
	if !withAlbums.ZeroRemoteCatalogue() {
		t.Errorf("albums on disk + empty remote catalogue: ZeroRemoteCatalogue() = false, want true (outcome %q/%q)",
			withAlbums.Validation.Outcome, withAlbums.Validation.Reason)
	}
	if withAlbums.Validation.Outcome != artist.MBIDOutcomeFailed {
		t.Errorf("albums on disk + empty remote catalogue: outcome = %q, want failed", withAlbums.Validation.Outcome)
	}

	unknownSrc := stubAlbums{set: artist.AlbumSet{Evidence: artist.EvidenceUnknown}, err: errors.New("mount missing")}
	unknown, err := newTestResolver(newMB(), unknownSrc).Resolve(t.Context(), testArtist())
	if err != nil {
		t.Fatalf("Resolve (evidence unknown): %v", err)
	}
	if unknown.Validation.Outcome == artist.MBIDOutcomeFailed {
		t.Error("unreadable local library produced a FAILED verdict; 'could not look' is not evidence")
	}
	if unknown.Validation.Outcome == artist.MBIDOutcomeValidated {
		t.Error("unreadable local library produced a VALIDATED verdict; an unread catalogue must never vindicate an id")
	}
	if unknown.ZeroRemoteCatalogue() {
		t.Error("ZeroRemoteCatalogue() = true on unknown local evidence")
	}
}

// TestNoLocalAlbumsReasonsAreSeparableByEvidence pins the field that lets the
// sweep tell a permanent state from a retryable one. The ledger's reason
// vocabulary is a closed SQL CHECK with one value for both cases, so if
// LocalEvidence collapsed them the distinction would be unrecoverable.
func TestNoLocalAlbumsReasonsAreSeparableByEvidence(t *testing.T) {
	t.Parallel()

	mb := func() *fakeMB {
		return &fakeMB{meta: &provider.ArtistMetadata{Name: "Example Band"}, groups: rgs("First Record")}
	}

	none, err := newTestResolver(mb(), stubAlbums{set: artist.AlbumSet{Evidence: artist.EvidenceNone}}).Resolve(t.Context(), testArtist())
	if err != nil {
		t.Fatalf("Resolve (none): %v", err)
	}
	unknown, err := newTestResolver(mb(), stubAlbums{set: artist.AlbumSet{Evidence: artist.EvidenceUnknown}, err: errors.New("no path")}).
		Resolve(t.Context(), testArtist())
	if err != nil {
		t.Fatalf("Resolve (unknown): %v", err)
	}

	// PRECONDITION: both really do carry the same ledger reason, which is what
	// makes the extra field load-bearing.
	if none.Validation.Reason != artist.MBIDReasonNoLocalAlbums || unknown.Validation.Reason != artist.MBIDReasonNoLocalAlbums {
		t.Fatalf("precondition: both cases should carry no_local_albums, got %q and %q",
			none.Validation.Reason, unknown.Validation.Reason)
	}

	if none.LocalEvidence != artist.EvidenceNone {
		t.Errorf("EvidenceNone case: LocalEvidence = %v, want none", none.LocalEvidence)
	}
	if unknown.LocalEvidence != artist.EvidenceUnknown {
		t.Errorf("EvidenceUnknown case: LocalEvidence = %v, want unknown", unknown.LocalEvidence)
	}
	if none.Transient {
		t.Error("EvidenceNone case: Transient = true; a genuinely empty library is not a retryable condition")
	}
	if !unknown.Transient {
		t.Error("EvidenceUnknown case: Transient = false; an unreadable library is worth retrying")
	}
}

// TestResolveNoStoredMBID: an artist with no stored id is not a member of this
// feature's population, so it must produce an error rather than a ledger row.
func TestResolveNoStoredMBID(t *testing.T) {
	t.Parallel()

	a := testArtist()
	a.MusicBrainzID = "   "
	mb := &fakeMB{meta: &provider.ArtistMetadata{Name: "Example Band"}}

	got, err := newTestResolver(mb, found("First Record")).Resolve(t.Context(), a)
	if !errors.Is(err, ErrNoStoredMBID) {
		t.Fatalf("error = %v, want ErrNoStoredMBID", err)
	}
	if got.Validation.Outcome != "" {
		t.Errorf("a verdict was produced for an artist with no id: %q", got.Validation.Outcome)
	}
	if mb.artistCalls != 0 {
		t.Errorf("provider called %d times for an artist with no id", mb.artistCalls)
	}
}

// TestResolveNeverMutatesTheArtist pins the acceptance criterion that no
// identity is changed as a result of this pass.
func TestResolveNeverMutatesTheArtist(t *testing.T) {
	t.Parallel()

	a := testArtist()
	before, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("snapshotting artist: %v", err)
	}

	mb := &fakeMB{
		meta:   &provider.ArtistMetadata{Name: "Completely Other Person", MusicBrainzID: "99999999-9999-9999-9999-999999999999"},
		groups: rgs("Unrelated"),
	}
	if _, err := newTestResolver(mb, found("First Record")).Resolve(t.Context(), a); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	after, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("re-snapshotting artist: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("Resolve mutated the artist record:\n before: %s\n  after: %s", before, after)
	}
}

// TestNameScoringToleratesSortNameAndAliases pins the name comparison against
// the benign spellings that would otherwise generate failed rows an operator
// has to triage: sort-name word order, a leading article, and a Latin-script
// alias. Each fixture has a perfectly matching catalogue, so name scoring is
// the only thing that can decide the outcome.
func TestNameScoringToleratesSortNameAndAliases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		localName   string
		localSort   string
		remote      provider.ArtistMetadata
		wantOutcome artist.MBIDValidationOutcome
	}{
		{
			name:        "sort-name word order",
			localName:   "Beatles, The",
			remote:      provider.ArtistMetadata{Name: "The Beatles"},
			wantOutcome: artist.MBIDOutcomeValidated,
		},
		{
			name:        "remote sort-name matches the local name",
			localName:   "Beatles, The",
			remote:      provider.ArtistMetadata{Name: "The Beatles", SortName: "Beatles, The"},
			wantOutcome: artist.MBIDOutcomeValidated,
		},
		{
			name:        "alias matches where the primary name does not",
			localName:   "Nightwish",
			remote:      provider.ArtistMetadata{Name: "ナイトウィッシュ", Aliases: []string{"Nightwish"}},
			wantOutcome: artist.MBIDOutcomeValidated,
		},
		{
			name:        "local sort name matches the remote primary name",
			localName:   "Some Local Spelling",
			localSort:   "Real Remote Name",
			remote:      provider.ArtistMetadata{Name: "Real Remote Name"},
			wantOutcome: artist.MBIDOutcomeValidated,
		},
		{
			// The control: nothing matches, so the name check must still bite.
			name:        "genuinely different name still fails",
			localName:   "Example Band",
			remote:      provider.ArtistMetadata{Name: "Zzyzx Quartet", Aliases: []string{"Nothing Like It"}},
			wantOutcome: artist.MBIDOutcomeFailed,
		},
	}

	albums := []string{"First Record", "Second Record"}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := testArtist()
			a.Name = tc.localName
			a.SortName = tc.localSort

			meta := tc.remote
			mb := &fakeMB{meta: &meta, groups: rgs(albums...)}
			r := newTestResolver(mb, found(albums...))

			got, err := r.Resolve(t.Context(), a)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}

			// PRECONDITION: the catalogue matches perfectly, so the name is
			// the only signal in play.
			if got.Validation.CatalogueMatchPercent == nil || *got.Validation.CatalogueMatchPercent != 100 {
				t.Fatalf("precondition: catalogue should match 100%%, got %v", got.Validation.CatalogueMatchPercent)
			}
			if got.Validation.Outcome != tc.wantOutcome {
				t.Errorf("outcome = %q (reason %q, name score %d%%), want %q",
					got.Validation.Outcome, got.Validation.Reason, got.NameScorePercent, tc.wantOutcome)
			}
			if tc.wantOutcome == artist.MBIDOutcomeFailed && got.Validation.Reason != artist.MBIDReasonNameMismatch {
				t.Errorf("reason = %q, want name_mismatch", got.Validation.Reason)
			}
		})
	}
}

// TestCatalogueThresholdBoundary pins where the threshold sits and that it is
// inclusive, so a change to the number is a deliberate edit rather than a
// silent drift.
func TestCatalogueThresholdBoundary(t *testing.T) {
	t.Parallel()

	// 1 of 4 local albums = 25%, exactly at DefaultCatalogueMatchPercent.
	local := []string{"A Record", "B Record", "C Record", "D Record"}

	atThreshold, err := newTestResolver(&fakeMB{meta: &provider.ArtistMetadata{Name: "Example Band"}, groups: rgs("A Record")}, found(local...)).
		Resolve(t.Context(), testArtist())
	if err != nil {
		t.Fatalf("Resolve (at threshold): %v", err)
	}
	// PRECONDITION: the fixture really does land exactly on the threshold.
	if atThreshold.Validation.CatalogueMatchPercent == nil || *atThreshold.Validation.CatalogueMatchPercent != float64(DefaultCatalogueMatchPercent) {
		t.Fatalf("precondition: fixture must score exactly %d%%, got %v",
			DefaultCatalogueMatchPercent, atThreshold.Validation.CatalogueMatchPercent)
	}
	if atThreshold.Validation.Outcome != artist.MBIDOutcomeValidated {
		t.Errorf("at the threshold: outcome = %q, want validated (threshold is inclusive)", atThreshold.Validation.Outcome)
	}

	// 1 of 5 local albums = 20%, one step below.
	belowLocal := append(append([]string{}, local...), "E Record")
	below, err := newTestResolver(&fakeMB{meta: &provider.ArtistMetadata{Name: "Example Band"}, groups: rgs("A Record")}, found(belowLocal...)).
		Resolve(t.Context(), testArtist())
	if err != nil {
		t.Fatalf("Resolve (below threshold): %v", err)
	}
	if below.Validation.CatalogueMatchPercent == nil || *below.Validation.CatalogueMatchPercent != 20 {
		t.Fatalf("precondition: fixture must score 20%%, got %v", below.Validation.CatalogueMatchPercent)
	}
	if below.Validation.Outcome != artist.MBIDOutcomeFailed || below.Validation.Reason != artist.MBIDReasonCatalogueMismatch {
		t.Errorf("below the threshold: outcome/reason = %q/%q, want failed/catalogue_mismatch",
			below.Validation.Outcome, below.Validation.Reason)
	}
}

// TestThresholdOptions pins that the options actually reach the classification
// and that an out-of-range value falls back to the documented default rather
// than silently disabling a check.
func TestThresholdOptions(t *testing.T) {
	t.Parallel()

	mb := func() *fakeMB {
		return &fakeMB{meta: &provider.ArtistMetadata{Name: "Example Band"}, groups: rgs("A Record")}
	}
	local := found("A Record", "B Record", "C Record", "D Record") // 25%

	// Raising the bar above 25 must turn the same fixture into a failure.
	raised, err := newTestResolver(mb(), local, WithCatalogueThreshold(50)).Resolve(t.Context(), testArtist())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if raised.Validation.Outcome != artist.MBIDOutcomeFailed {
		t.Errorf("with a 50%% threshold: outcome = %q, want failed", raised.Validation.Outcome)
	}

	// An out-of-range value is ignored, so the default (25) still applies and
	// the same fixture validates.
	bogus, err := newTestResolver(mb(), local, WithCatalogueThreshold(-5), WithNameThreshold(9999)).Resolve(t.Context(), testArtist())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if bogus.Validation.Outcome != artist.MBIDOutcomeValidated {
		t.Errorf("with out-of-range thresholds: outcome = %q, want validated (defaults retained)", bogus.Validation.Outcome)
	}
}

// TestNewPanicsOnNilDependencies: a Resolver with a nil album source would
// resolve every artist to "not checkable" and read as a working feature
// finding nothing.
func TestNewPanicsOnNilDependencies(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		mb     MusicBrainzClient
		albums artist.AlbumSource
	}{
		{"nil client", nil, found()},
		{"nil album source", &fakeMB{}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if recover() == nil {
					t.Error("New did not panic on a nil dependency")
				}
			}()
			_ = New(tc.mb, tc.albums)
		})
	}
}

// TestResolveUsesRealFilesystemAlbumSource wires the production album source
// against a real directory, so the tri-state contract is exercised by the code
// that will actually run rather than only by a stub.
func TestResolveUsesRealFilesystemAlbumSource(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	libPath := filepath.Join(root, "Example Band")
	for _, album := range []string{"First Record", "Second Record"} {
		if err := os.MkdirAll(filepath.Join(libPath, album), 0o755); err != nil {
			t.Fatalf("seeding album dir: %v", err)
		}
	}

	// PRECONDITION: the fixture on disk really holds the two album dirs.
	entries, err := os.ReadDir(libPath)
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("precondition: fixture should hold 2 album dirs, holds %d", len(entries))
	}

	src := artist.NewFilesystemAlbumSource()

	t.Run("readable path with albums yields a real verdict", func(t *testing.T) {
		a := testArtist()
		a.Path = libPath
		mb := &fakeMB{meta: &provider.ArtistMetadata{Name: "Example Band"}, groups: rgs("First Record", "Second Record")}

		got, err := newTestResolver(mb, src).Resolve(t.Context(), a)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if got.LocalEvidence != artist.EvidenceFound {
			t.Fatalf("LocalEvidence = %v, want found", got.LocalEvidence)
		}
		if got.Validation.Outcome != artist.MBIDOutcomeValidated {
			t.Errorf("outcome = %q, want validated (%s)", got.Validation.Outcome, got.Validation.Detail)
		}
	})

	t.Run("missing path is unknown, never an empty catalogue", func(t *testing.T) {
		a := testArtist()
		a.Path = filepath.Join(root, "Does Not Exist")
		// The remote catalogue is EMPTY, which with albums on disk would be
		// the headline failure. An unreadable path must not reach that verdict.
		mb := &fakeMB{meta: &provider.ArtistMetadata{Name: "Example Band"}, groups: nil}

		got, err := newTestResolver(mb, src).Resolve(t.Context(), a)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if got.LocalEvidence != artist.EvidenceUnknown {
			t.Fatalf("LocalEvidence = %v, want unknown", got.LocalEvidence)
		}
		if got.Validation.Outcome != artist.MBIDOutcomeNotCheckable {
			t.Errorf("outcome = %q, want not_checkable", got.Validation.Outcome)
		}
		if got.Validation.Reason != artist.MBIDReasonNoLocalAlbums {
			t.Errorf("reason = %q, want no_local_albums", got.Validation.Reason)
		}
	})

	t.Run("empty but readable path is none, not unknown", func(t *testing.T) {
		empty := filepath.Join(root, "Empty Artist")
		if err := os.MkdirAll(empty, 0o755); err != nil {
			t.Fatalf("seeding empty dir: %v", err)
		}
		a := testArtist()
		a.Path = empty
		mb := &fakeMB{meta: &provider.ArtistMetadata{Name: "Example Band"}, groups: rgs("First Record")}

		got, err := newTestResolver(mb, src).Resolve(t.Context(), a)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if got.LocalEvidence != artist.EvidenceNone {
			t.Fatalf("LocalEvidence = %v, want none", got.LocalEvidence)
		}
		if got.Transient {
			t.Error("Transient = true for a readable empty directory; that is a determination, not a retryable failure")
		}
	})
}

// TestDetailIsInformativeWithoutBeingParsed keeps the operator-facing prose
// honest: every non-validated verdict must say something, and the numbers a
// caller branches on must be available as FIELDS so nobody parses the prose.
func TestDetailIsInformativeWithoutBeingParsed(t *testing.T) {
	t.Parallel()

	mb := &fakeMB{meta: &provider.ArtistMetadata{Name: "Someone Else"}, groups: rgs("Unrelated")}
	got, err := newTestResolver(mb, found("First Record")).Resolve(t.Context(), testArtist())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if strings.TrimSpace(got.Validation.Detail) == "" {
		t.Error("a failed verdict carries no detail")
	}
	if got.NameScorePercent < 0 || got.NameScorePercent > 100 {
		t.Errorf("NameScorePercent = %d, want 0-100 on a resolved id", got.NameScorePercent)
	}
	if got.RemoteReleaseGroupCount != 1 {
		t.Errorf("RemoteReleaseGroupCount = %d, want 1", got.RemoteReleaseGroupCount)
	}
}

// TestUnrecognizedEvidenceStateDeclines pins the default branch: an evidence
// value this package does not know about must resolve toward "unknown", never
// toward the catalogue comparison. A future fourth state must not be able to
// acquire the power to vindicate an id by default.
func TestUnrecognizedEvidenceStateDeclines(t *testing.T) {
	t.Parallel()

	bogus := artist.AlbumEvidence(99)
	// PRECONDITION: the value really is outside the known set.
	for _, known := range []artist.AlbumEvidence{artist.EvidenceUnknown, artist.EvidenceNone, artist.EvidenceFound} {
		if bogus == known {
			t.Fatalf("precondition: %v collides with a known evidence state", bogus)
		}
	}

	src := stubAlbums{set: artist.AlbumSet{Titles: []string{"First Record"}, Evidence: bogus}}
	mb := &fakeMB{meta: &provider.ArtistMetadata{Name: "Example Band"}, groups: rgs("First Record")}

	got, err := newTestResolver(mb, src).Resolve(t.Context(), testArtist())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Validation.Outcome != artist.MBIDOutcomeNotCheckable {
		t.Errorf("outcome = %q, want not_checkable", got.Validation.Outcome)
	}
	if got.Validation.CatalogueMatchPercent != nil {
		t.Errorf("CatalogueMatchPercent = %v, want nil; an unrecognized state measured nothing", *got.Validation.CatalogueMatchPercent)
	}
	if mb.groupCalls != 0 {
		t.Errorf("release groups were fetched (%d calls) for an unrecognized evidence state", mb.groupCalls)
	}
}
