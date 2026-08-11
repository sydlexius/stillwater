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

	// The id each call actually received. Recorded rather than discarded
	// because WHICH id reaches MusicBrainz is the one thing a resolver can get
	// wrong while satisfying every other assertion in this file: passing a.ID,
	// a stale field, or an untrimmed string would produce a perfectly shaped
	// verdict about the wrong artist. See TestProviderReceivesTheTrimmedStoredMBID.
	artistMBID, groupMBID string
}

func (f *fakeMB) GetArtist(_ context.Context, mbid string) (*provider.ArtistMetadata, error) {
	f.artistCalls++
	f.artistMBID = mbid
	return f.meta, f.metaErr
}

func (f *fakeMB) GetReleaseGroups(_ context.Context, mbid string) ([]provider.ReleaseGroupInfo, error) {
	f.groupCalls++
	f.groupMBID = mbid
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
		wantAnomaly   bool
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
		{
			// A source claiming "found" while returning nothing found. It must
			// never reach the comparison, where an empty local side scores a
			// DEFAULT 0 indistinguishable from a measured one. See
			// TestEmptyFoundNeverCondemnsOrForgesTheSignal for the full guard.
			name:   "not_checkable: evidence found with no titles is a source defect",
			artist: testArtist(),
			mb: &fakeMB{
				meta:   &provider.ArtistMetadata{Name: "Example Band"},
				groups: rgs("First Record"),
			},
			albums:        stubAlbums{set: artist.AlbumSet{Evidence: artist.EvidenceFound}},
			wantOutcome:   artist.MBIDOutcomeNotCheckable,
			wantReason:    artist.MBIDReasonNoLocalAlbums,
			wantTransient: true,
			wantAnomaly:   true,
			wantEvidence:  artist.EvidenceFound,
			wantResolved:  "Example Band",
			wantDetail:    "returned no titles",
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
			if got.Anomaly != tc.wantAnomaly {
				t.Errorf("Anomaly = %v, want %v", got.Anomaly, tc.wantAnomaly)
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
	//
	// THE FOUR ROWS BELOW ARE NOT EQUALLY STRONG, and it is worth being blunt
	// about which is which rather than letting the table read as four proofs.
	//
	//   - The ABOVE-range rows (150, 9999) are the load-bearing ones. Honoring
	//     either would turn this fixture's validated verdict into a failure, so
	//     "the value was ignored" is observable here through the outcome, and
	//     the assertion can genuinely fail.
	//   - The BELOW-range rows (-5, -1) prove much less. A negative threshold
	//     makes EVERYTHING match, which produces the same validated verdict as
	//     the retained default, so the outcome assertion passes whether the
	//     value was honored or ignored. What they establish is only that a
	//     negative option is accepted without panicking or erroring -- real
	//     smoke value, and no more than that.
	//
	// The below-range direction is covered properly by
	// TestOutOfRangeThresholdsRetainTheDefaults, which asserts the resolver's
	// threshold FIELDS directly instead of inferring them from an outcome that
	// cannot distinguish the two cases.
	for _, tc := range []struct {
		name string
		opt  Option
	}{
		{"catalogue threshold above range", WithCatalogueThreshold(150)},
		{"catalogue threshold below range", WithCatalogueThreshold(-5)},
		{"name threshold above range", WithNameThreshold(9999)},
		{"name threshold below range", WithNameThreshold(-1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bogus, err := newTestResolver(mb(), local, tc.opt).Resolve(t.Context(), testArtist())
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if bogus.Validation.Outcome != artist.MBIDOutcomeValidated {
				t.Errorf("with an out-of-range threshold: outcome = %q/%q, want validated (documented defaults retained)",
					bogus.Validation.Outcome, bogus.Validation.Reason)
			}
		})
	}
}

// TestOutOfRangeThresholdsRetainTheDefaults states the same guard as a direct
// assertion on the resolver's own fields, so a failure names the defect
// (a threshold silently took an impossible value) rather than only its effect
// on one fixture. A below-range value cannot be caught by outcome alone: it
// makes everything match, which looks exactly like the default validating.
func TestOutOfRangeThresholdsRetainTheDefaults(t *testing.T) {
	t.Parallel()

	for _, percent := range []int{-1, -5, 101, 9999} {
		r := New(&fakeMB{}, found("First Record"),
			WithLogger(slog.New(slog.DiscardHandler)),
			WithNameThreshold(percent), WithCatalogueThreshold(percent))
		if r.nameThreshold != DefaultNameSimilarityPercent {
			t.Errorf("WithNameThreshold(%d): nameThreshold = %d, want the default %d",
				percent, r.nameThreshold, DefaultNameSimilarityPercent)
		}
		if r.catalogueThreshold != DefaultCatalogueMatchPercent {
			t.Errorf("WithCatalogueThreshold(%d): catalogueThreshold = %d, want the default %d",
				percent, r.catalogueThreshold, DefaultCatalogueMatchPercent)
		}
	}

	// PRECONDITION-style control: an IN-range value must still reach the
	// resolver, or the assertions above would pass on an option that does
	// nothing at all.
	r := New(&fakeMB{}, found("First Record"), WithLogger(slog.New(slog.DiscardHandler)),
		WithNameThreshold(55), WithCatalogueThreshold(60))
	if r.nameThreshold != 55 || r.catalogueThreshold != 60 {
		t.Errorf("in-range options did not reach the resolver: name=%d catalogue=%d", r.nameThreshold, r.catalogueThreshold)
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

// ---------------------------------------------------------------------------
// Guards added after the hostile review of the first commit
// ---------------------------------------------------------------------------

// TestEmptyFoundNeverCondemnsOrForgesTheSignal covers the worst shape an album
// source can hand this package: EvidenceFound with an EMPTY title list, a
// source contradicting its own contract.
//
// Two distinct defects follow from letting it reach the comparison, and this
// test pins both:
//
//  1. CompareAlbums only computes MatchPercent when LocalCount > 0, so an empty
//     local side yields a DEFAULT 0 that is arithmetically identical to the
//     strongest evidence this feature can produce. The id gets condemned, and a
//     non-nil 0 is written to CatalogueMatchPercent for a comparison that never
//     ran -- through a side door, the exact "nil means never measured"
//     invariant the pointer type exists to hold.
//  2. With an empty REMOTE catalogue too, ZeroRemoteCatalogue() would fire on
//     an artist holding nothing on disk, forging the headline finding (the 18
//     production artists whose defining property is albums on disk against an
//     empty remote catalogue).
//
// FilesystemAlbumSource cannot produce this state today. ChainAlbumSource
// returns a member source's answer as-is, so it is one buggy future source
// away, and this package's whole thesis is never to trust an unverified claim.
func TestEmptyFoundNeverCondemnsOrForgesTheSignal(t *testing.T) {
	t.Parallel()

	emptyFound := stubAlbums{set: artist.AlbumSet{Titles: nil, Evidence: artist.EvidenceFound}}

	// PRECONDITION: the fixture really is the self-contradicting shape, or
	// every assertion below is about some other case.
	set, _ := emptyFound.LocalAlbums(t.Context(), testArtist())
	if set.Evidence != artist.EvidenceFound {
		t.Fatalf("precondition: fixture evidence = %v, want found", set.Evidence)
	}
	if len(set.Titles) != 0 {
		t.Fatalf("precondition: fixture must carry NO titles, carries %d", len(set.Titles))
	}
	// PRECONDITION: and the comparison really would report a bare 0 for it, so
	// the defect being guarded is real rather than hypothetical.
	comp := artist.CompareAlbumSet(set, []string{"Some Remote Record", "Another"})
	if comp.MatchPercent != 0 || comp.LocalCount != 0 {
		t.Fatalf("precondition: CompareAlbumSet on an empty local side = %d%% over %d local, want 0%% over 0",
			comp.MatchPercent, comp.LocalCount)
	}

	for _, tc := range []struct {
		name   string
		groups []provider.ReleaseGroupInfo
	}{
		{"remote catalogue present", rgs("Some Remote Record", "Another")},
		{"remote catalogue empty", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mb := &fakeMB{meta: &provider.ArtistMetadata{Name: "Example Band"}, groups: tc.groups}
			got, err := newTestResolver(mb, emptyFound).Resolve(t.Context(), testArtist())
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}

			if got.Validation.Outcome == artist.MBIDOutcomeFailed {
				t.Errorf("an album source that returned no titles produced a FAILED verdict (reason %q, detail %q); "+
					"a source contradicting its own contract is not evidence against the id",
					got.Validation.Reason, got.Validation.Detail)
			}
			if got.Validation.Outcome == artist.MBIDOutcomeValidated {
				t.Error("an empty catalogue VALIDATED the id; an uncompared catalogue must never vindicate one")
			}
			if got.Validation.Outcome != artist.MBIDOutcomeNotCheckable {
				t.Errorf("outcome = %q, want not_checkable", got.Validation.Outcome)
			}
			if got.Validation.CatalogueMatchPercent != nil {
				t.Errorf("CatalogueMatchPercent = %v, want nil; NOTHING was compared, and a non-nil 0 is "+
					"indistinguishable from the strongest evidence this feature produces",
					*got.Validation.CatalogueMatchPercent)
			}
			if got.ZeroRemoteCatalogue() {
				t.Error("ZeroRemoteCatalogue() = true with no albums on disk; that finding claims the operator HAS albums")
			}
			if !got.Transient {
				t.Error("Transient = false; a Stillwater-side defect must not overwrite a real prior verdict")
			}
			if !got.Anomaly {
				t.Error("Anomaly = false; a source breaking its own contract is a programming error and must log loudly")
			}
			if got.LocalAlbumCount != 0 {
				t.Errorf("LocalAlbumCount = %d, want 0", got.LocalAlbumCount)
			}
			// The operator-facing prose must not present a non-comparison as a
			// below-threshold failure ("0 of 0 local albums").
			if strings.Contains(got.Validation.Detail, "of 0 local albums") {
				t.Errorf("Detail = %q reports a below-threshold comparison over an empty catalogue", got.Validation.Detail)
			}
			if err := got.Validation.Validate(); err != nil {
				t.Errorf("emitted verdict is not persistable: %v", err)
			}
		})
	}
}

// TestZeroRemoteCatalogueRequiresAlbumsOnDisk pins the method's own doc
// comment: the finding claims the operator HAS albums on disk, so a Result
// whose local side is empty must not satisfy it however the evidence field
// reads. Without the count check the sentence was aspirational, and the
// headline 18-artist signal could fire on artists lacking its defining
// property.
func TestZeroRemoteCatalogueRequiresAlbumsOnDisk(t *testing.T) {
	t.Parallel()

	base := func() Result {
		return Result{
			LocalEvidence:           artist.EvidenceFound,
			LocalAlbumCount:         2,
			RemoteReleaseGroupCount: 0,
			Validation:              artist.MBIDValidation{Outcome: artist.MBIDOutcomeFailed},
		}
	}

	// PRECONDITION: the base Result really is the headline finding, or the
	// negative cases below prove nothing.
	if !base().ZeroRemoteCatalogue() {
		t.Fatal("precondition: the base fixture should BE the zero-remote-catalogue finding")
	}

	for _, tc := range []struct {
		name   string
		mutate func(*Result)
	}{
		{"no albums on disk", func(r *Result) { r.LocalAlbumCount = 0 }},
		{"album count never established", func(r *Result) { r.LocalAlbumCount = -1 }},
		{"local evidence not found", func(r *Result) { r.LocalEvidence = artist.EvidenceUnknown }},
		{"remote groups never fetched", func(r *Result) { r.RemoteReleaseGroupCount = -1 }},
		{"outcome not failed", func(r *Result) { r.Validation.Outcome = artist.MBIDOutcomeNotCheckable }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res := base()
			tc.mutate(&res)
			if res.ZeroRemoteCatalogue() {
				t.Errorf("ZeroRemoteCatalogue() = true for %q; the finding asserts albums on disk against an empty remote catalogue", tc.name)
			}
		})
	}
}

// TestEveryVerdictIsPersistable is the guard the old suite only appeared to
// carry. TestResolveClassification calls Validate() on every case, but every
// fixture there has a valid ArtistID, so that assertion passes whether or not
// Resolve validates anything -- it tests the fixture, not the code.
//
// Here the artist is deliberately unpersistable (no ID), so a path that skips
// Validate() returns a nil error and a row the repository rejects three layers
// down as an opaque SQLite constraint failure. Every classification path is
// walked, because the ones that used to skip validation were exactly the outage
// and error paths where a sweep spends most of a bad day.
func TestEveryVerdictIsPersistable(t *testing.T) {
	t.Parallel()

	// PRECONDITION: the fixture must genuinely fail Validate, or every case is
	// vacuous in the same way the old assertion was.
	probe := artist.MBIDValidation{
		ArtistID:  "",
		MBID:      "11111111-2222-3333-4444-555555555555",
		Outcome:   artist.MBIDOutcomeNotCheckable,
		Reason:    artist.MBIDReasonProviderUnavailable,
		CheckedAt: time.Now().UTC(),
	}
	if err := probe.Validate(); err == nil {
		t.Fatal("precondition: a row with no artist id should fail Validate; this test cannot detect anything")
	}

	badArtist := func() *artist.Artist {
		a := testArtist()
		a.ID = "" // unpersistable: Validate requires an artist id
		return a
	}

	cases := []struct {
		name   string
		mb     *fakeMB
		albums artist.AlbumSource
		ctx    func(t *testing.T) context.Context
	}{
		{
			name:   "validated",
			mb:     &fakeMB{meta: &provider.ArtistMetadata{Name: "Example Band"}, groups: rgs("First Record")},
			albums: found("First Record"),
		},
		{
			name:   "failed",
			mb:     &fakeMB{meta: &provider.ArtistMetadata{Name: "Someone Else"}, groups: rgs("Unrelated")},
			albums: found("First Record"),
		},
		{
			name:   "mbid not found",
			mb:     &fakeMB{metaErr: &provider.ErrNotFound{Provider: provider.NameMusicBrainz, ID: "x"}},
			albums: found("First Record"),
		},
		{
			name:   "provider unavailable on the artist lookup",
			mb:     &fakeMB{metaErr: errors.New("dial tcp: i/o timeout")},
			albums: found("First Record"),
		},
		{
			name:   "client returned no artist and no error",
			mb:     &fakeMB{},
			albums: found("First Record"),
		},
		{
			name:   "provider unavailable on the release-group lookup",
			mb:     &fakeMB{meta: &provider.ArtistMetadata{Name: "Example Band"}, groupsErr: errors.New("503")},
			albums: found("First Record"),
		},
		{
			name:   "local albums unknown",
			mb:     &fakeMB{meta: &provider.ArtistMetadata{Name: "Example Band"}, groups: rgs("First Record")},
			albums: stubAlbums{set: artist.AlbumSet{Evidence: artist.EvidenceUnknown}, err: errors.New("no mount")},
		},
		{
			name:   "local albums none",
			mb:     &fakeMB{meta: &provider.ArtistMetadata{Name: "Example Band"}, groups: rgs("First Record")},
			albums: stubAlbums{set: artist.AlbumSet{Evidence: artist.EvidenceNone}},
		},
		{
			name:   "local albums found but empty",
			mb:     &fakeMB{meta: &provider.ArtistMetadata{Name: "Example Band"}, groups: rgs("First Record")},
			albums: stubAlbums{set: artist.AlbumSet{Evidence: artist.EvidenceFound}},
		},
		{
			name:   "unrecognized evidence state",
			mb:     &fakeMB{meta: &provider.ArtistMetadata{Name: "Example Band"}, groups: rgs("First Record")},
			albums: stubAlbums{set: artist.AlbumSet{Titles: []string{"First Record"}, Evidence: artist.AlbumEvidence(99)}},
		},
		{
			name:   "context already canceled",
			mb:     &fakeMB{meta: &provider.ArtistMetadata{Name: "Example Band"}, groups: rgs("First Record")},
			albums: found("First Record"),
			ctx: func(t *testing.T) context.Context {
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				return ctx
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx := t.Context()
			if tc.ctx != nil {
				ctx = tc.ctx(t)
			}

			got, err := newTestResolver(tc.mb, tc.albums).Resolve(ctx, badArtist())
			if err == nil {
				t.Fatalf("Resolve returned a nil error and outcome %q/%q for an artist that cannot be persisted; "+
					"the caller would hand this row to the repository and get an opaque constraint failure",
					got.Validation.Outcome, got.Validation.Reason)
			}
			if !strings.Contains(err.Error(), "invalid verdict") {
				t.Errorf("error = %v, want the resolver's own named invalid-verdict error", err)
			}
			// A rejected verdict is returned as the zero Result, so a caller
			// that ignores the error cannot persist a half-built row either.
			if got.Validation.Outcome != "" {
				t.Errorf("outcome = %q alongside an error, want the zero Result", got.Validation.Outcome)
			}
		})
	}
}

// TestEveryVerdictIsLogged pins the operational half of the same defect: only
// the main comparison path used to log, and anything short of "failed" went to
// Debug, which is off in production. A MusicBrainz outage during a sweep
// therefore produced no line at any level for any affected artist -- a sweep
// that checked nothing was indistinguishable from a sweep that found nothing.
//
// The handler is set to Info so the assertion is about what a PRODUCTION
// operator would actually see, not about what a debug run could dig out.
func TestEveryVerdictIsLogged(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		mb        *fakeMB
		albums    artist.AlbumSource
		wantLevel slog.Level
	}{
		{
			name:      "provider outage on the artist lookup",
			mb:        &fakeMB{metaErr: errors.New("dial tcp: i/o timeout")},
			albums:    found("First Record"),
			wantLevel: slog.LevelWarn,
		},
		{
			name:      "provider outage on the release-group lookup",
			mb:        &fakeMB{meta: &provider.ArtistMetadata{Name: "Example Band"}, groupsErr: errors.New("503")},
			albums:    found("First Record"),
			wantLevel: slog.LevelWarn,
		},
		{
			name:      "failed verdict",
			mb:        &fakeMB{meta: &provider.ArtistMetadata{Name: "Someone Else"}, groups: rgs("Unrelated")},
			albums:    found("First Record"),
			wantLevel: slog.LevelWarn,
		},
		{
			name:      "mbid not found",
			mb:        &fakeMB{metaErr: &provider.ErrNotFound{Provider: provider.NameMusicBrainz, ID: "x"}},
			albums:    found("First Record"),
			wantLevel: slog.LevelInfo,
		},
		{
			name:      "no local albums",
			mb:        &fakeMB{meta: &provider.ArtistMetadata{Name: "Example Band"}, groups: rgs("First Record")},
			albums:    stubAlbums{set: artist.AlbumSet{Evidence: artist.EvidenceNone}},
			wantLevel: slog.LevelInfo,
		},
		{
			name:      "unrecognized evidence state is a programming error",
			mb:        &fakeMB{meta: &provider.ArtistMetadata{Name: "Example Band"}, groups: rgs("First Record")},
			albums:    stubAlbums{set: artist.AlbumSet{Titles: []string{"First Record"}, Evidence: artist.AlbumEvidence(99)}},
			wantLevel: slog.LevelError,
		},
		{
			name:      "album source reported found with no titles",
			mb:        &fakeMB{meta: &provider.ArtistMetadata{Name: "Example Band"}, groups: rgs("First Record")},
			albums:    stubAlbums{set: artist.AlbumSet{Evidence: artist.EvidenceFound}},
			wantLevel: slog.LevelError,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})

			// PRECONDITION: the handler really does drop Debug, so "a line was
			// seen" cannot be satisfied by a Debug line a production operator
			// would never get.
			if handler.Enabled(t.Context(), slog.LevelDebug) {
				t.Fatal("precondition: the test handler must not be enabled at Debug")
			}

			r := New(tc.mb, tc.albums, WithLogger(slog.New(handler)))
			if _, err := r.Resolve(t.Context(), testArtist()); err != nil {
				t.Fatalf("Resolve: %v", err)
			}

			if buf.Len() == 0 {
				t.Fatalf("no log line at all for %q; an operator watching a sweep sees nothing", tc.name)
			}

			var rec map[string]any
			if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
				t.Fatalf("log line is not JSON (%q): %v", buf.String(), err)
			}
			if got := rec["level"]; got != tc.wantLevel.String() {
				t.Errorf("log level = %v, want %v (line: %s)", got, tc.wantLevel, buf.String())
			}
			// The line has to identify the artist and say what happened, or it
			// is noise rather than a report.
			if rec["artist_id"] != "artist-1" {
				t.Errorf("log line does not name the artist: %s", buf.String())
			}
			if rec["outcome"] == nil || rec["outcome"] == "" {
				t.Errorf("log line carries no outcome: %s", buf.String())
			}
		})
	}
}

// TestRejectedVerdictIsLoggedBeforeReturning closes the one hole in "every
// verdict is logged": the path where Resolve BUILDS a verdict, finds it
// unpersistable, and returns an error.
//
// That verdict is the one an operator most needs in the log -- it is a
// Stillwater defect, not a fact about the artist or about MusicBrainz -- and it
// used to leave no line at any level, so the only trace was an error string in
// whatever the caller decided to do with it. The log's own doc comment promised
// every verdict was logged, which made the gap invisible to a reader.
//
// The handler is set to Info, so this asserts what a PRODUCTION operator would
// see rather than what a debug run could dig out.
func TestRejectedVerdictIsLoggedBeforeReturning(t *testing.T) {
	t.Parallel()

	badArtist := func() *artist.Artist {
		a := testArtist()
		a.ID = "" // unpersistable: Validate requires an artist id
		return a
	}

	// PRECONDITION: the fixture really does build a verdict that fails Validate,
	// or this test would be asserting the absence of a line nobody skipped.
	probe := artist.MBIDValidation{
		ArtistID:  "",
		MBID:      "11111111-2222-3333-4444-555555555555",
		Outcome:   artist.MBIDOutcomeNotCheckable,
		Reason:    artist.MBIDReasonProviderUnavailable,
		CheckedAt: time.Now().UTC(),
	}
	if err := probe.Validate(); err == nil {
		t.Fatal("precondition: a row with no artist id should fail Validate; this test cannot detect anything")
	}

	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	// PRECONDITION: the handler drops Debug, so a Debug line cannot satisfy the
	// assertion below on behalf of a production operator who would never see it.
	if handler.Enabled(t.Context(), slog.LevelDebug) {
		t.Fatal("precondition: the test handler must not be enabled at Debug")
	}

	mb := &fakeMB{metaErr: errors.New("dial tcp: i/o timeout")}
	r := New(mb, found("First Record"), WithLogger(slog.New(handler)))

	got, err := r.Resolve(t.Context(), badArtist())
	if err == nil {
		t.Fatalf("precondition: Resolve should have rejected the verdict, got outcome %q", got.Validation.Outcome)
	}

	if buf.Len() == 0 {
		t.Fatal("a rejected verdict produced no log line at any level; the one verdict that signals a Stillwater defect is invisible")
	}

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("log line is not JSON (%q): %v", buf.String(), err)
	}
	if rec["level"] != slog.LevelError.String() {
		t.Errorf("log level = %v, want ERROR (line: %s)", rec["level"], buf.String())
	}
	// The CONTENT, not merely the existence of a line. A line naming neither
	// the artist nor what went wrong is noise an operator cannot act on.
	if _, ok := rec["artist_id"]; !ok {
		t.Errorf("log line carries no artist_id key: %s", buf.String())
	}
	if rec["outcome"] != string(artist.MBIDOutcomeNotCheckable) {
		t.Errorf("outcome = %v, want %q: %s", rec["outcome"], artist.MBIDOutcomeNotCheckable, buf.String())
	}
	if rec["reason"] != string(artist.MBIDReasonProviderUnavailable) {
		t.Errorf("reason = %v, want %q: %s", rec["reason"], artist.MBIDReasonProviderUnavailable, buf.String())
	}
	errAttr, _ := rec["error"].(string)
	if !strings.Contains(errAttr, "artist id is required") {
		t.Errorf("log line does not carry the validation error (error = %q): %s", errAttr, buf.String())
	}
	if rec["mbid"] != testArtist().MusicBrainzID {
		t.Errorf("mbid = %v, want %q: %s", rec["mbid"], testArtist().MusicBrainzID, buf.String())
	}
}

// TestUnknownEvidenceDetailNeverPrintsANilError pins the operator-facing prose
// on the EvidenceUnknown branch.
//
// artist.AlbumSource permits reporting EvidenceUnknown with a NIL error -- "I
// could not look" is a determination, and a source is not obliged to attach a
// cause. Formatting that nil with %v renders the literal string "<nil>", which
// this package then stores in the ledger and shows an operator as the reason
// their library could not be read. Both directions are pinned, because a fix
// that dropped the cause entirely would pass a nil-only assertion while losing
// the diagnostic on every real failure.
func TestUnknownEvidenceDetailNeverPrintsANilError(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		albumErr   error
		wantDetail string
	}{
		{"cause reported", errors.New("permission denied"), "permission denied"},
		{"no cause reported", nil, "no cause"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			src := stubAlbums{set: artist.AlbumSet{Evidence: artist.EvidenceUnknown}, err: tc.albumErr}

			// PRECONDITION: the fixture really is EvidenceUnknown with the
			// error-ness the case is named for, or the assertions below are
			// about some other branch.
			set, err := src.LocalAlbums(t.Context(), testArtist())
			if set.Evidence != artist.EvidenceUnknown {
				t.Fatalf("precondition: fixture evidence = %v, want unknown", set.Evidence)
			}
			if (err == nil) != (tc.albumErr == nil) {
				t.Fatalf("precondition: fixture error = %v, want nil-ness %v", err, tc.albumErr == nil)
			}

			mb := &fakeMB{meta: &provider.ArtistMetadata{Name: "Example Band"}, groups: rgs("First Record")}
			got, resolveErr := newTestResolver(mb, src).Resolve(t.Context(), testArtist())
			if resolveErr != nil {
				t.Fatalf("Resolve: %v", resolveErr)
			}

			// PRECONDITION: this really is the branch under test.
			if got.LocalEvidence != artist.EvidenceUnknown || got.Validation.Reason != artist.MBIDReasonNoLocalAlbums {
				t.Fatalf("precondition: evidence/reason = %v/%q, want unknown/no_local_albums",
					got.LocalEvidence, got.Validation.Reason)
			}

			if strings.Contains(got.Validation.Detail, "<nil>") {
				t.Errorf("Detail = %q shows an operator the literal string \"<nil>\" as the cause", got.Validation.Detail)
			}
			if !strings.Contains(got.Validation.Detail, tc.wantDetail) {
				t.Errorf("Detail = %q, want it to contain %q", got.Validation.Detail, tc.wantDetail)
			}
			// The rest of the branch is unchanged and must stay that way.
			if !got.Transient {
				t.Error("Transient = false; an unreadable library is worth retrying")
			}
			if got.Validation.ResolvedName != "Example Band" {
				t.Errorf("ResolvedName = %q, want the resolved name carried through", got.Validation.ResolvedName)
			}
			if got.Validation.CatalogueMatchPercent != nil {
				t.Errorf("CatalogueMatchPercent = %v, want nil (nothing was compared)", *got.Validation.CatalogueMatchPercent)
			}
		})
	}
}

// TestProviderReceivesTheTrimmedStoredMBID pins WHICH id reaches MusicBrainz.
//
// Nothing else in this file asserts it: both fake methods used to discard their
// mbid argument, so a resolver that queried MusicBrainz with a.ID, with a stale
// field, or with an untrimmed string would pass every other test here while
// production fetched metadata for the wrong artist -- and would then file a
// perfectly well-formed verdict about it.
//
// The whitespace fixture is the load-bearing one. Resolve trims before use, so
// the stored value and the value sent differ, which is what makes "the trimmed
// value was sent" observable at all rather than trivially true.
func TestProviderReceivesTheTrimmedStoredMBID(t *testing.T) {
	t.Parallel()

	const stored = "11111111-2222-3333-4444-555555555555"

	for _, tc := range []struct {
		name  string
		field string
	}{
		{"stored exactly", stored},
		{"stored with surrounding whitespace", "  \t" + stored + "\n "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := testArtist()
			a.MusicBrainzID = tc.field

			// PRECONDITION: the whitespace case must genuinely differ from the
			// trimmed value, or it would assert nothing the first case does not
			// already cover.
			if tc.name == "stored with surrounding whitespace" && a.MusicBrainzID == stored {
				t.Fatalf("precondition: fixture id %q does not differ from the trimmed value", a.MusicBrainzID)
			}
			// PRECONDITION: the artist's own primary key must differ from its
			// MBID, or "the resolver passed a.ID" would be indistinguishable
			// from passing the id.
			if a.ID == stored {
				t.Fatalf("precondition: artist ID %q collides with the stored MBID", a.ID)
			}

			mb := &fakeMB{
				meta:   &provider.ArtistMetadata{Name: "Example Band"},
				groups: rgs("First Record"),
			}
			got, err := newTestResolver(mb, found("First Record")).Resolve(t.Context(), a)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}

			// PRECONDITION: both provider calls actually happened, or an
			// unrecorded id would read as a matching one.
			if mb.artistCalls != 1 {
				t.Fatalf("GetArtist called %d times, want 1", mb.artistCalls)
			}
			if mb.groupCalls != 1 {
				t.Fatalf("GetReleaseGroups called %d times, want 1", mb.groupCalls)
			}

			if mb.artistMBID != stored {
				t.Errorf("GetArtist received %q, want the trimmed stored id %q", mb.artistMBID, stored)
			}
			if mb.groupMBID != stored {
				t.Errorf("GetReleaseGroups received %q, want the trimmed stored id %q", mb.groupMBID, stored)
			}
			// The ledger row must record the same id that was actually checked,
			// or the verdict names one id and was measured against another.
			if got.Validation.MBID != stored {
				t.Errorf("Validation.MBID = %q, want the trimmed stored id %q", got.Validation.MBID, stored)
			}
		})
	}
}

// TestValidatedVerdictLogsAtDebug is the other half of the level policy: the
// common, good case must NOT add a line to a production log for every artist in
// the library. Stated separately so a failure names which direction broke.
func TestValidatedVerdictLogsAtDebug(t *testing.T) {
	t.Parallel()

	mb := &fakeMB{meta: &provider.ArtistMetadata{Name: "Example Band"}, groups: rgs("First Record")}

	var atInfo bytes.Buffer
	r := New(mb, found("First Record"), WithLogger(slog.New(slog.NewJSONHandler(&atInfo, &slog.HandlerOptions{Level: slog.LevelInfo}))))
	got, err := r.Resolve(t.Context(), testArtist())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// PRECONDITION: this really is the validated path.
	if got.Validation.Outcome != artist.MBIDOutcomeValidated {
		t.Fatalf("precondition: outcome = %q, want validated (%s)", got.Validation.Outcome, got.Validation.Detail)
	}
	if atInfo.Len() != 0 {
		t.Errorf("a validated verdict logged at Info or above: %s", atInfo.String())
	}

	// And it is logged, just quietly: a Debug run must still show it.
	var atDebug bytes.Buffer
	mb2 := &fakeMB{meta: &provider.ArtistMetadata{Name: "Example Band"}, groups: rgs("First Record")}
	r2 := New(mb2, found("First Record"), WithLogger(slog.New(slog.NewJSONHandler(&atDebug, &slog.HandlerOptions{Level: slog.LevelDebug}))))
	if _, err := r2.Resolve(t.Context(), testArtist()); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if atDebug.Len() == 0 {
		t.Error("a validated verdict produced no line even at Debug")
	}
}

// TestAlbumSourceErrorIsCarriedAlongsideAUsableAnswer pins the diagnostic
// channel ChainAlbumSource goes to real trouble to keep open: it returns the
// failures of earlier sources ALONGSIDE the first EvidenceFound, precisely so a
// primary source that is quietly broken while a fallback covers for it is not
// invisible. A resolver that inspected only Evidence would close that channel
// one level up, re-hiding exactly what the chain exposed.
func TestAlbumSourceErrorIsCarriedAlongsideAUsableAnswer(t *testing.T) {
	t.Parallel()

	brokenPrimary := errors.New("primary album source: connection refused")
	src := stubAlbums{
		set: artist.AlbumSet{Titles: []string{"First Record"}, Evidence: artist.EvidenceFound},
		err: brokenPrimary,
	}

	// PRECONDITION: the fixture is the awkward shape -- a USABLE determination
	// with a non-nil error. If the source returned Unknown, the error would
	// already surface through the not-checkable detail string.
	set, err := src.LocalAlbums(t.Context(), testArtist())
	if set.Evidence != artist.EvidenceFound || len(set.Titles) == 0 {
		t.Fatalf("precondition: fixture must carry a usable determination, got %v with %d titles", set.Evidence, len(set.Titles))
	}
	if err == nil {
		t.Fatal("precondition: fixture must return a non-nil error alongside it")
	}

	var buf bytes.Buffer
	mb := &fakeMB{meta: &provider.ArtistMetadata{Name: "Example Band"}, groups: rgs("First Record")}
	r := New(mb, src, WithLogger(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))))

	got, resolveErr := r.Resolve(t.Context(), testArtist())
	if resolveErr != nil {
		t.Fatalf("Resolve: %v", resolveErr)
	}

	// The usable answer still decides the verdict: Evidence is the contract.
	if got.Validation.Outcome != artist.MBIDOutcomeValidated {
		t.Errorf("outcome = %q, want validated; the error is diagnostic and must not change the decision", got.Validation.Outcome)
	}
	if !errors.Is(got.AlbumSourceErr, brokenPrimary) {
		t.Errorf("AlbumSourceErr = %v, want the source's error carried through", got.AlbumSourceErr)
	}
	if !strings.Contains(buf.String(), "connection refused") {
		t.Errorf("the album source's error never reached the log: %s", buf.String())
	}
}

// TestNameThresholdBoundary mirrors TestCatalogueThresholdBoundary for the
// other threshold: it pins where the number sits AND that the comparison is
// inclusive. Without it, mutating `nameScore >= r.nameThreshold` to `>` passed
// the whole suite, because every other name fixture scores 100 or far below 80.
//
// Every fixture here has a perfectly matching catalogue, so the name is the
// only signal that can decide the outcome.
func TestNameThresholdBoundary(t *testing.T) {
	t.Parallel()

	albums := []string{"First Record", "Second Record"}

	// Two 20-character strings differing in the last 4 characters score exactly
	// 80; differing in the last 5 score 75, the nearest reachable step below.
	const (
		localName   = "Abcdefghijklmnopqrst"
		atThreshold = "Abcdefghijklmnopwxyz"
		belowThresh = "Abcdefghijklmnovwxyz"
	)

	// PRECONDITION: the fixtures really do land on and below the threshold, and
	// the "at" one really is AT it rather than above.
	if got := provider.NameSimilarity(localName, atThreshold); got != DefaultNameSimilarityPercent {
		t.Fatalf("precondition: %q vs %q scores %d, want exactly %d", localName, atThreshold, got, DefaultNameSimilarityPercent)
	}
	if got := provider.NameSimilarity(localName, belowThresh); got >= DefaultNameSimilarityPercent {
		t.Fatalf("precondition: %q vs %q scores %d, want below %d", localName, belowThresh, got, DefaultNameSimilarityPercent)
	}

	run := func(t *testing.T, remoteName string) Result {
		t.Helper()
		a := testArtist()
		a.Name = localName
		mb := &fakeMB{meta: &provider.ArtistMetadata{Name: remoteName}, groups: rgs(albums...)}
		got, err := newTestResolver(mb, found(albums...)).Resolve(t.Context(), a)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		// PRECONDITION: the catalogue matches perfectly, so nothing but the
		// name can move the outcome.
		if got.Validation.CatalogueMatchPercent == nil || *got.Validation.CatalogueMatchPercent != 100 {
			t.Fatalf("precondition: catalogue should match 100%%, got %v", got.Validation.CatalogueMatchPercent)
		}
		return got
	}

	at := run(t, atThreshold)
	if at.NameScorePercent != DefaultNameSimilarityPercent {
		t.Fatalf("at the threshold: NameScorePercent = %d, want exactly %d", at.NameScorePercent, DefaultNameSimilarityPercent)
	}
	if at.Validation.Outcome != artist.MBIDOutcomeValidated {
		t.Errorf("at the threshold: outcome = %q/%q, want validated (the threshold is INCLUSIVE)",
			at.Validation.Outcome, at.Validation.Reason)
	}

	below := run(t, belowThresh)
	if below.NameScorePercent >= DefaultNameSimilarityPercent {
		t.Fatalf("below the threshold: NameScorePercent = %d, want below %d", below.NameScorePercent, DefaultNameSimilarityPercent)
	}
	if below.Validation.Outcome != artist.MBIDOutcomeFailed || below.Validation.Reason != artist.MBIDReasonNameMismatch {
		t.Errorf("below the threshold: outcome/reason = %q/%q, want failed/name_mismatch",
			below.Validation.Outcome, below.Validation.Reason)
	}
}

// TestSortNameHandlingCoversPeopleAndLeavesBandsAlone pins unsortNames against
// the case the original comment waved away: it claimed reordering a "Surname,
// Forename" person "would not change the normalized comparison anyway", which
// was measurably false -- "Bowie, David" scored 28 against "David Bowie", so a
// person foldered surname-first with a perfect catalogue was filed as
// name_mismatch.
//
// The ambiguous direction matters as much: a band whose name genuinely contains
// a comma must not be mangled. Because bestNameScore takes the MAXIMUM over
// candidates, an extra reading can only raise a score, so the band cases assert
// the name as written still scores 100.
func TestSortNameHandlingCoversPeopleAndLeavesBandsAlone(t *testing.T) {
	t.Parallel()

	t.Run("unsortNames readings", func(t *testing.T) {
		t.Parallel()

		for _, tc := range []struct {
			in   string
			want []string
		}{
			{"Bowie, David", []string{"bowie david", "david bowie"}},
			{"Beatles, The", []string{"beatles the", "beatles"}},
			{"Example Band", []string{"example band"}},
			// Multi-word tails are names as written, not sort order.
			{"Emerson, Lake & Palmer", []string{"emerson lake  palmer"}},
			{"Crosby, Stills & Nash", []string{"crosby stills  nash"}},
			// Two commas: the correct rearrangement is genuinely unclear.
			{"Davis, Miles, Jr.", []string{"davis miles jr"}},
			// A name that normalizes to nothing must not panic or invent one.
			{"!!!", nil},
			{"", nil},
		} {
			got := unsortNames(tc.in)
			if len(got) != len(tc.want) {
				t.Errorf("unsortNames(%q) = %v, want %v", tc.in, got, tc.want)
				continue
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("unsortNames(%q) = %v, want %v", tc.in, got, tc.want)
					break
				}
			}
		}
	})

	t.Run("classification", func(t *testing.T) {
		t.Parallel()

		albums := []string{"First Record", "Second Record"}

		for _, tc := range []struct {
			name        string
			localName   string
			remoteName  string
			wantOutcome artist.MBIDValidationOutcome
		}{
			{
				name:        "person foldered surname-first",
				localName:   "Bowie, David",
				remoteName:  "David Bowie",
				wantOutcome: artist.MBIDOutcomeValidated,
			},
			{
				name:        "person stored natural-order against a remote sort name",
				localName:   "David Bowie",
				remoteName:  "Bowie, David",
				wantOutcome: artist.MBIDOutcomeValidated,
			},
			{
				name:        "band with a comma in its real name",
				localName:   "Emerson, Lake & Palmer",
				remoteName:  "Emerson, Lake & Palmer",
				wantOutcome: artist.MBIDOutcomeValidated,
			},
			{
				// The control: the extra reading must not start matching
				// genuinely different people.
				name:        "different person still fails",
				localName:   "Bowie, David",
				remoteName:  "Zzyzx Quartet",
				wantOutcome: artist.MBIDOutcomeFailed,
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()

				a := testArtist()
				a.Name = tc.localName
				mb := &fakeMB{meta: &provider.ArtistMetadata{Name: tc.remoteName}, groups: rgs(albums...)}

				got, err := newTestResolver(mb, found(albums...)).Resolve(t.Context(), a)
				if err != nil {
					t.Fatalf("Resolve: %v", err)
				}
				// PRECONDITION: the catalogue matches perfectly, so the name is
				// the only signal in play.
				if got.Validation.CatalogueMatchPercent == nil || *got.Validation.CatalogueMatchPercent != 100 {
					t.Fatalf("precondition: catalogue should match 100%%, got %v", got.Validation.CatalogueMatchPercent)
				}
				if got.Validation.Outcome != tc.wantOutcome {
					t.Errorf("local %q vs remote %q: outcome = %q (reason %q, name score %d%%), want %q",
						tc.localName, tc.remoteName, got.Validation.Outcome, got.Validation.Reason,
						got.NameScorePercent, tc.wantOutcome)
				}
			})
		}
	})
}

// TestCanceledCheckIsNotLoggedAsAnOutage is the SEVENTH instance of this
// package's dominant defect class: a condition on OUR side reported as though
// it were the provider's or the artist's.
//
// classify's first act on a canceled context is to build a transient
// provider_unavailable verdict, and log reports provider_unavailable at WARN
// specifically so a real outage is visible per artist. A shutdown part-way
// through a pass hits that path for every remaining artist at once, so without
// the demotion a routine stop produces a burst of "provider unavailable" WARN
// lines and is indistinguishable in the log from the MusicBrainz outage that
// WARN exists to announce.
//
// The handler is set to Info, so the assertion is about what a PRODUCTION
// operator sees rather than what a debug run could dig out.
func TestCanceledCheckIsNotLoggedAsAnOutage(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	// PRECONDITION: the handler drops Debug, so "no WARN was emitted" cannot be
	// satisfied by a line the demotion did not actually move.
	if handler.Enabled(t.Context(), slog.LevelDebug) {
		t.Fatal("precondition: the test handler must not be enabled at Debug")
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	r := New(&fakeMB{meta: &provider.ArtistMetadata{Name: "Example Band"}, groups: rgs("First Record")},
		found("First Record"), WithLogger(slog.New(handler)))
	res, err := r.Resolve(ctx, testArtist())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	// PRECONDITION: the cancellation really did produce the transient
	// provider_unavailable verdict this test is about. Without it the assertion
	// below would hold over some other verdict that simply never logs at WARN.
	if !res.Transient {
		t.Fatalf("precondition: a canceled check must produce a TRANSIENT verdict, got %+v", res.Validation)
	}
	if res.Validation.Reason != artist.MBIDReasonProviderUnavailable {
		t.Fatalf("precondition: reason = %q, want %q -- the WARN path is the one under test",
			res.Validation.Reason, artist.MBIDReasonProviderUnavailable)
	}

	if buf.Len() != 0 {
		t.Errorf("a canceled check logged at Info or above: %s -- a shutdown would read as a provider outage", buf.String())
	}
}

// TestARealOutageStillWarnsWhileTheContextIsLive is the other half. Without it
// the demotion above could be a blanket "never warn on a transient verdict",
// which would silence the per-artist outage line that makes a sweep which
// checked nothing distinguishable from one that found nothing -- the exact
// defect TestEveryVerdictIsLogged was written to close.
func TestARealOutageStillWarnsWhileTheContextIsLive(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})

	r := New(&fakeMB{metaErr: errors.New("dial tcp: i/o timeout")}, found("First Record"),
		WithLogger(slog.New(handler)))
	res, err := r.Resolve(t.Context(), testArtist())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	// PRECONDITION: same verdict shape as the canceled case above, so the only
	// thing that can distinguish the two is the context's state.
	if !res.Transient || res.Validation.Reason != artist.MBIDReasonProviderUnavailable {
		t.Fatalf("precondition: want a transient provider_unavailable verdict, got %+v", res.Validation)
	}

	if buf.Len() == 0 {
		t.Fatal("a real outage produced no line at Info or above; a sweep that checked nothing looks like one that found nothing")
	}
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("log line is not JSON (%q): %v", buf.String(), err)
	}
	if got := rec["level"]; got != slog.LevelWarn.String() {
		t.Errorf("log level = %v, want WARN for a genuine outage (line: %s)", got, buf.String())
	}
}
