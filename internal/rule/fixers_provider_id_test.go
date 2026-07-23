package rule

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
)

// recordingUpdater records every UpdateProviderField call so a test can assert
// exactly which fields were written and with what values.
type recordingUpdater struct {
	updates map[string]string // field -> value
}

func (u *recordingUpdater) UpdateProviderField(_ context.Context, _, field, value string) error {
	if u.updates == nil {
		u.updates = make(map[string]string)
	}
	u.updates[field] = value
	return nil
}

// mbURLMetadata builds ArtistMetadata carrying MusicBrainz URL relations for
// Discogs, Deezer, and Spotify. The shared stubMetadataProvider wraps it in a
// FetchResult.
func mbURLMetadata() *provider.ArtistMetadata {
	return &provider.ArtistMetadata{
		URLs: map[string]string{
			"discogs": "https://www.discogs.com/artist/24941",
			"deezer":  "https://www.deezer.com/artist/3106",
			"spotify": "https://open.spotify.com/artist/7dGJo4pcD2V6oG8kP0tJRR",
		},
	}
}

// TestProviderIDBackfill_FillsEmptyFromRelations fills every empty provider ID
// that MusicBrainz relations can supply.
func TestProviderIDBackfill_FillsEmptyFromRelations(t *testing.T) {
	fetcher := &stubMetadataProvider{metadata: mbURLMetadata()}
	updater := &recordingUpdater{}
	f := NewProviderIDBackfillFixer(fetcher, updater, testLogger())

	a := &artist.Artist{ID: "a1", Name: "Test Artist", MusicBrainzID: "mbid-abc"}
	res, err := f.Fix(context.Background(), a, &Violation{RuleID: RuleProviderIDMissing})
	if err != nil {
		t.Fatalf("Fix returned error: %v", err)
	}
	if !res.Fixed {
		t.Fatalf("expected Fixed=true, got %+v", res)
	}
	want := map[string]string{
		"discogs_id": "24941",
		"deezer_id":  "3106",
		"spotify_id": "7dGJo4pcD2V6oG8kP0tJRR",
	}
	if len(updater.updates) != len(want) {
		t.Fatalf("wrote %d fields, want %d: %+v", len(updater.updates), len(want), updater.updates)
	}
	for field, val := range want {
		if got := updater.updates[field]; got != val {
			t.Errorf("field %q = %q, want %q", field, got, val)
		}
	}
}

// TestProviderIDBackfill_NeverOverwritesExisting leaves an already-populated
// provider ID untouched and fills only the empty one.
func TestProviderIDBackfill_NeverOverwritesExisting(t *testing.T) {
	fetcher := &stubMetadataProvider{metadata: mbURLMetadata()}
	updater := &recordingUpdater{}
	f := NewProviderIDBackfillFixer(fetcher, updater, testLogger())

	// Discogs already set to a hand-entered value; only deezer + spotify empty.
	a := &artist.Artist{ID: "a1", Name: "Test Artist", MusicBrainzID: "mbid-abc", DiscogsID: "existing-99"}
	res, err := f.Fix(context.Background(), a, &Violation{RuleID: RuleProviderIDMissing})
	if err != nil {
		t.Fatalf("Fix returned error: %v", err)
	}
	if !res.Fixed {
		t.Fatalf("expected Fixed=true, got %+v", res)
	}
	if _, wrote := updater.updates["discogs_id"]; wrote {
		t.Errorf("backfill overwrote an existing Discogs ID: %+v", updater.updates)
	}
	if updater.updates["deezer_id"] != "3106" || updater.updates["spotify_id"] != "7dGJo4pcD2V6oG8kP0tJRR" {
		t.Errorf("empty providers not backfilled correctly: %+v", updater.updates)
	}
}

// TestProviderIDBackfill_NoMBIDIsNoOp reports a non-fatal no-op when the artist
// has no MusicBrainz ID (nothing to derive relations from) and never fetches.
func TestProviderIDBackfill_NoMBIDIsNoOp(t *testing.T) {
	fetcher := &stubMetadataProvider{metadata: mbURLMetadata()}
	updater := &recordingUpdater{}
	f := NewProviderIDBackfillFixer(fetcher, updater, testLogger())

	a := &artist.Artist{ID: "a1", Name: "Test Artist"} // no MBID
	res, err := f.Fix(context.Background(), a, &Violation{RuleID: RuleProviderIDMissing})
	if err != nil {
		t.Fatalf("Fix returned error: %v", err)
	}
	if res.Fixed {
		t.Errorf("expected a no-op result for a no-MBID artist, got Fixed=true")
	}
	if fetcher.calls != 0 {
		t.Errorf("fetcher was called %d times despite the artist having no MBID", fetcher.calls)
	}
	if len(updater.updates) != 0 {
		t.Errorf("no-MBID artist should write nothing, wrote %+v", updater.updates)
	}
}

// TestProviderIDBackfill_NoDerivableRelationsIsNoOp reports a non-fatal no-op
// when MusicBrainz returns no relations for the in-scope providers.
func TestProviderIDBackfill_NoDerivableRelationsIsNoOp(t *testing.T) {
	fetcher := &stubMetadataProvider{metadata: &provider.ArtistMetadata{URLs: map[string]string{}}}
	updater := &recordingUpdater{}
	f := NewProviderIDBackfillFixer(fetcher, updater, testLogger())

	a := &artist.Artist{ID: "a1", Name: "Test Artist", MusicBrainzID: "mbid-abc"}
	res, err := f.Fix(context.Background(), a, &Violation{RuleID: RuleProviderIDMissing})
	if err != nil {
		t.Fatalf("Fix returned error: %v", err)
	}
	if res.Fixed {
		t.Errorf("expected a no-op when nothing is derivable, got Fixed=true")
	}
	if len(updater.updates) != 0 {
		t.Errorf("nothing derivable should write nothing, wrote %+v", updater.updates)
	}
}

// TestProviderIDBackfill_CoalescesFetchMetadata proves a repeat Fix call on the
// same artist within one evaluation pass issues at most one upstream
// FetchMetadata call.
//
// Two mechanisms cooperate here, in order: (1) issue #2699 -- Fix mutates the
// artist's flat provider-ID fields in place immediately after a successful
// backfill (see Fix's UpdateProviderField loop), which is what lets the
// pipeline's same-pass post-fix re-evaluation see the corrected artist instead
// of re-deriving a stale violation. Once mbURLMetadata's three IDs are all
// filled by the first call, the second call's own needs-fill guard short-circuits
// before any fetch is attempted -- so this test's "1 call" no longer depends on
// cache coalescing to hold. (2) For the general case where a fetch IS still
// needed twice (e.g. two DIFFERENT metadata fixers sharing one artist in a
// pass), the fixer still routes through the per-artist EvaluationContext
// coalescer (#1133/#1134/#1135) rather than issuing its own duplicate
// FetchMetadata call; that plumbing is unchanged and still load-bearing, it is
// just not what this particular scenario exercises anymore. This mirrors
// TestImageFixer_FetchImages_Cached, which pins the coalescing invariant for
// the image fixer's four-rule fanout.
//
// The fixer's own fetcher and the EvaluationContext's orchestrator are the same
// countingEvalProvider, so the coalesced (EC) path and the reverted (direct
// f.fetcher) path both land on one counter.
func TestProviderIDBackfill_SecondPassSkipsFetchOnceFilled(t *testing.T) {
	count := &countingEvalProvider{
		metaResult: &provider.FetchResult{Metadata: mbURLMetadata()},
	}
	updater := &recordingUpdater{}
	f := NewProviderIDBackfillFixer(count, updater, testLogger())

	a := &artist.Artist{ID: "a1", Name: "Test Artist", MusicBrainzID: "mbid-abc"}
	ctx := WithEvaluationContext(context.Background(), NewEvaluationContext(a, count, testLogger()))

	// Two backfill passes on the same artist within one evaluation. The first
	// derives every in-scope ID and writes it onto the in-memory artist (#2699);
	// the second then hits the "every in-scope ID already set" early-exit and
	// must NOT dispatch a fetch at all -- so exactly one fetch total.
	//
	// This does NOT test pass-level fetch coalescing: because the first pass
	// mutates the artist, providerIDFingerprint's cache key deliberately
	// changes, so a genuine second fetch would not coalesce anyway. Coalescing
	// is covered directly by TestEvaluationContext_CoalescesMetadataAndField.
	for i := 0; i < 2; i++ {
		if _, err := f.Fix(ctx, a, &Violation{RuleID: RuleProviderIDMissing}); err != nil {
			t.Fatalf("Fix %d: %v", i, err)
		}
	}

	if got := count.fetchMetaCalls.Load(); got != 1 {
		t.Errorf("FetchMetadata dispatched %d times; want 1 (second pass short-circuits at the all-filled early-exit)", got)
	}
}

// TestProviderIDBackfill_NilFetcherIsNoOp reports a non-fatal no-op and never
// touches the updater when the fixer's metadata fetcher is unwired.
func TestProviderIDBackfill_NilFetcherIsNoOp(t *testing.T) {
	updater := &recordingUpdater{}
	f := NewProviderIDBackfillFixer(nil, updater, testLogger())

	a := &artist.Artist{ID: "a1", Name: "Test Artist", MusicBrainzID: "mbid-abc"}
	res, err := f.Fix(context.Background(), a, &Violation{RuleID: RuleProviderIDMissing})
	if err != nil {
		t.Fatalf("Fix returned error: %v", err)
	}
	if res.Fixed {
		t.Errorf("expected a no-op with an unwired fetcher, got Fixed=true")
	}
	if len(updater.updates) != 0 {
		t.Errorf("unwired fetcher should write nothing, wrote %+v", updater.updates)
	}
}

// TestProviderIDBackfill_NilUpdaterIsNoOp reports a non-fatal no-op and never
// calls the fetcher when the fixer's updater is unwired.
func TestProviderIDBackfill_NilUpdaterIsNoOp(t *testing.T) {
	fetcher := &stubMetadataProvider{metadata: mbURLMetadata()}
	f := NewProviderIDBackfillFixer(fetcher, nil, testLogger())

	a := &artist.Artist{ID: "a1", Name: "Test Artist", MusicBrainzID: "mbid-abc"}
	res, err := f.Fix(context.Background(), a, &Violation{RuleID: RuleProviderIDMissing})
	if err != nil {
		t.Fatalf("Fix returned error: %v", err)
	}
	if res.Fixed {
		t.Errorf("expected a no-op with an unwired updater, got Fixed=true")
	}
	if fetcher.calls != 0 {
		t.Errorf("unwired updater should short-circuit before fetching, fetcher called %d times", fetcher.calls)
	}
}

// TestProviderIDBackfill_FetchErrorPropagates surfaces a MusicBrainz fetch
// failure as an error rather than swallowing it into a no-op FixResult.
func TestProviderIDBackfill_FetchErrorPropagates(t *testing.T) {
	fetchErr := errors.New("musicbrainz: connection reset")
	fetcher := &stubMetadataProvider{err: fetchErr}
	updater := &recordingUpdater{}
	f := NewProviderIDBackfillFixer(fetcher, updater, testLogger())

	a := &artist.Artist{ID: "a1", Name: "Test Artist", MusicBrainzID: "mbid-abc"}
	res, err := f.Fix(context.Background(), a, &Violation{RuleID: RuleProviderIDMissing})
	if err == nil {
		t.Fatal("expected the fetch error to propagate, got nil error")
	}
	if !strings.Contains(err.Error(), fetchErr.Error()) {
		t.Errorf("error %q does not wrap the underlying fetch error %q", err, fetchErr)
	}
	if res != nil {
		t.Errorf("expected a nil FixResult alongside a propagated error, got %+v", res)
	}
	if len(updater.updates) != 0 {
		t.Errorf("a failed fetch should write nothing, wrote %+v", updater.updates)
	}
}

// TestProviderIDBackfill_NilMetadataIsNoOp reports a non-fatal no-op when the
// fetch succeeds but returns no metadata (e.g. artist not found upstream).
func TestProviderIDBackfill_NilMetadataIsNoOp(t *testing.T) {
	fetcher := &stubMetadataProvider{metadata: nil}
	updater := &recordingUpdater{}
	f := NewProviderIDBackfillFixer(fetcher, updater, testLogger())

	a := &artist.Artist{ID: "a1", Name: "Test Artist", MusicBrainzID: "mbid-abc"}
	res, err := f.Fix(context.Background(), a, &Violation{RuleID: RuleProviderIDMissing})
	if err != nil {
		t.Fatalf("Fix returned error: %v", err)
	}
	if res.Fixed {
		t.Errorf("expected a no-op when metadata is nil, got Fixed=true")
	}
	if len(updater.updates) != 0 {
		t.Errorf("nil metadata should write nothing, wrote %+v", updater.updates)
	}
}

// erroringUpdater fails every UpdateProviderField call so tests can assert the
// fixer propagates a persistence failure instead of reporting a false Fixed.
type erroringUpdater struct {
	err error
}

func (u *erroringUpdater) UpdateProviderField(_ context.Context, _, _, _ string) error {
	return u.err
}

// TestProviderIDBackfill_UpdateErrorPropagates surfaces a persistence failure
// from UpdateProviderField as an error rather than reporting a false Fixed.
func TestProviderIDBackfill_UpdateErrorPropagates(t *testing.T) {
	updateErr := errors.New("db: write failed")
	fetcher := &stubMetadataProvider{metadata: mbURLMetadata()}
	updater := &erroringUpdater{err: updateErr}
	f := NewProviderIDBackfillFixer(fetcher, updater, testLogger())

	a := &artist.Artist{ID: "a1", Name: "Test Artist", MusicBrainzID: "mbid-abc"}
	res, err := f.Fix(context.Background(), a, &Violation{RuleID: RuleProviderIDMissing})
	if err == nil {
		t.Fatal("expected the update error to propagate, got nil error")
	}
	if !strings.Contains(err.Error(), updateErr.Error()) {
		t.Errorf("error %q does not wrap the underlying update error %q", err, updateErr)
	}
	if res != nil {
		t.Errorf("expected a nil FixResult alongside a propagated error, got %+v", res)
	}
}

// TestProviderIDBackfill_CanFix matches only the provider_id_missing rule.
func TestProviderIDBackfill_CanFix(t *testing.T) {
	f := NewProviderIDBackfillFixer(&stubMetadataProvider{metadata: mbURLMetadata()}, &recordingUpdater{}, testLogger())
	if !f.CanFix(&Violation{RuleID: RuleProviderIDMissing}) {
		t.Error("CanFix should return true for provider_id_missing")
	}
	if f.CanFix(&Violation{RuleID: RuleDiscographyPopulated}) {
		t.Error("CanFix should return false for a different rule")
	}
}
