package provider

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
)

// imageStatusFor returns the status recorded for prov, or a zero status and
// false when the provider produced none. FetchImages must emit exactly one
// status per provider it iterates, so a missing entry is itself a failure.
func imageStatusFor(statuses []ProviderImageStatus, prov ProviderName) (ProviderImageStatus, bool) {
	for _, st := range statuses {
		if st.Provider == prov {
			return st, true
		}
	}
	return ProviderImageStatus{}, false
}

// newImageStatusOrchestrator registers a mock for each of the four providers
// that carry a provider-specific ID, recording the id each one is called with.
// The returned map is written from the provider callbacks, which FetchImages
// invokes serially, so no synchronization is needed.
func newImageStatusOrchestrator(t *testing.T) (*Orchestrator, map[ProviderName]string) {
	t.Helper()
	registry, settings := setupOrchestratorTest(t)
	calls := make(map[ProviderName]string)

	for _, name := range []ProviderName{NameAudioDB, NameDiscogs, NameDeezer, NameSpotify} {
		// Capture the loop variable's value for the closure.
		provName := name
		registry.Register(&mockProvider{
			name: provName,
			getImgFn: func(_ context.Context, id string) ([]ImageResult, error) {
				calls[provName] = id
				return []ImageResult{{Type: ImageThumb, URL: "http://example.com/" + string(provName) + ".jpg"}}, nil
			},
		})
		// AvailableProviderNames gates on a stored key for key-requiring
		// providers; without one the provider is never iterated and the test
		// would pass vacuously.
		if err := settings.SetAPIKey(context.Background(), provName, "test-key"); err != nil {
			t.Fatalf("SetAPIKey %s: %v", provName, err)
		}
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return NewOrchestrator(registry, settings, logger, nil), calls
}

// TestFetchImagesAudioDBFallsBackToMBID is the acceptance test for the AudioDB
// half of #2457. AudioDB's adapter routes a non-numeric id to artist-mb.php, so
// a missing AudioDB numeric ID must NOT skip it: it must be queried with the
// MBID. Discogs, Deezer and Spotify have no MusicBrainz lookup and must be
// skipped with a reported reason.
func TestFetchImagesAudioDBFallsBackToMBID(t *testing.T) {
	orch, calls := newImageStatusOrchestrator(t)

	const mbid = "mbid-radiohead"
	// Every provider-specific ID unknown -- the 84-93% case from the issue.
	ids := BuildProviderIDMap("", "", "", "")

	result, err := orch.FetchImages(context.Background(), mbid, ids)
	if err != nil {
		t.Fatalf("FetchImages: %v", err)
	}

	// Precondition: the mocks really were reachable. If AvailableProviderNames
	// had excluded them, everything below would assert on an empty world.
	if len(result.ImageProviderStatuses) != 4 {
		t.Fatalf("expected a status for each of the 4 registered providers, got %d: %+v",
			len(result.ImageProviderStatuses), result.ImageProviderStatuses)
	}

	// The fix: AudioDB is queried, and queried with the MBID.
	if got := calls[NameAudioDB]; got != mbid {
		t.Errorf("AudioDB was called with id %q, want the MBID %q (a missing AudioDB ID must fall back, not skip)", got, mbid)
	}
	audioDB, ok := imageStatusFor(result.ImageProviderStatuses, NameAudioDB)
	if !ok || audioDB.Outcome != ImageOutcomeQueried {
		t.Errorf("AudioDB status = %+v (found=%v), want outcome %q", audioDB, ok, ImageOutcomeQueried)
	}
	if len(result.Images) != 1 || !strings.Contains(result.Images[0].URL, string(NameAudioDB)) {
		t.Errorf("expected exactly the AudioDB image to be collected, got %+v", result.Images)
	}

	// The report: the three providers that genuinely cannot be queried are
	// named, with a reason, rather than dropped in silence.
	for _, name := range []ProviderName{NameDiscogs, NameDeezer, NameSpotify} {
		st, ok := imageStatusFor(result.ImageProviderStatuses, name)
		if !ok {
			t.Errorf("no status reported for %s; the operator cannot see it was never searched", name)
			continue
		}
		if st.Outcome != ImageOutcomeSkipped {
			t.Errorf("%s outcome = %q, want %q", name, st.Outcome, ImageOutcomeSkipped)
		}
		if st.Reason != SkipReasonNoProviderID {
			t.Errorf("%s reason = %q, want %q", name, st.Reason, SkipReasonNoProviderID)
		}
		if _, called := calls[name]; called {
			t.Errorf("%s was called with %q despite having no provider-specific ID", name, calls[name])
		}
	}
}

// TestFetchImagesAllIDsPresentNoSkips is the positive control: with every
// provider-specific ID known, nothing is skipped and each provider is queried
// with its OWN id, not the MBID. This guards the AudioDB fix against
// regressing into "always use the MBID", which would throw away the stored
// numeric ID and its direct-lookup endpoint.
func TestFetchImagesAllIDsPresentNoSkips(t *testing.T) {
	orch, calls := newImageStatusOrchestrator(t)

	ids := BuildProviderIDMap("111493", "24941", "3106", "4Z8W4fKeB5YxbusRsdQVPb")

	result, err := orch.FetchImages(context.Background(), "mbid-radiohead", ids)
	if err != nil {
		t.Fatalf("FetchImages: %v", err)
	}

	if len(result.ImageProviderStatuses) != 4 {
		t.Fatalf("expected 4 statuses, got %d: %+v", len(result.ImageProviderStatuses), result.ImageProviderStatuses)
	}
	for _, st := range result.ImageProviderStatuses {
		if st.Outcome != ImageOutcomeQueried {
			t.Errorf("%s outcome = %q, want %q (nothing should be skipped when all IDs are present)",
				st.Provider, st.Outcome, ImageOutcomeQueried)
		}
	}

	want := map[ProviderName]string{
		NameAudioDB: "111493",
		NameDiscogs: "24941",
		NameDeezer:  "3106",
		NameSpotify: "4Z8W4fKeB5YxbusRsdQVPb",
	}
	for name, wantID := range want {
		if calls[name] != wantID {
			t.Errorf("%s called with id %q, want its own id %q", name, calls[name], wantID)
		}
	}
}

// TestFetchImagesErroredProviderScrubbed proves a provider failure is reported
// (not swallowed into an indistinguishable thin result) and that the reported
// text carries no credential. Provider request URLs embed API keys: Fanart.tv in
// an api_key query parameter, TheAudioDB in a path segment.
func TestFetchImagesErroredProviderScrubbed(t *testing.T) {
	registry, settings := setupOrchestratorTest(t)

	const secret = "s3cr3t-key-value"
	registry.Register(&mockProvider{
		name: NameFanartTV,
		getImgFn: func(_ context.Context, _ string) ([]ImageResult, error) {
			return nil, errors.New(`Get "https://webservice.fanart.tv/v3/music/mbid?api_key=` + secret + `": connection refused`)
		},
	})
	registry.Register(&mockProvider{
		name: NameAudioDB,
		getImgFn: func(_ context.Context, _ string) ([]ImageResult, error) {
			return nil, errors.New(`Get "https://www.theaudiodb.com/api/v1/json/` + secret + `/artist-mb.php?i=mbid": connection refused`)
		},
	})
	for _, name := range []ProviderName{NameFanartTV, NameAudioDB} {
		if err := settings.SetAPIKey(context.Background(), name, secret); err != nil {
			t.Fatalf("SetAPIKey %s: %v", name, err)
		}
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	orch := NewOrchestrator(registry, settings, logger, nil)

	result, err := orch.FetchImages(context.Background(), "mbid", BuildProviderIDMap("", "", "", ""))
	if err != nil {
		t.Fatalf("FetchImages: %v", err)
	}

	for _, name := range []ProviderName{NameFanartTV, NameAudioDB} {
		st, ok := imageStatusFor(result.ImageProviderStatuses, name)
		if !ok {
			t.Fatalf("no status reported for failing provider %s", name)
		}
		if st.Outcome != ImageOutcomeErrored {
			t.Errorf("%s outcome = %q, want %q", name, st.Outcome, ImageOutcomeErrored)
		}
		if st.Reason == "" {
			t.Errorf("%s errored with no message; the operator gets no reason", name)
		}
		if strings.Contains(st.Reason, secret) {
			t.Errorf("%s reason leaks the API key: %q", name, st.Reason)
		}
		if !strings.Contains(st.Reason, "REDACTED") {
			t.Errorf("%s reason = %q, want the credential replaced with REDACTED", name, st.Reason)
		}
	}
}

// TestScrubErrorRedactsAudioDBPathKey covers the path-segment credential that
// the query-parameter rule cannot see. TheAudioDB puts its API key in the URL
// path, and a transport failure surfaces a *url.Error carrying the whole URL.
func TestScrubErrorRedactsAudioDBPathKey(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		wantMissing string
		wantPresent string
	}{
		{
			name:        "audiodb path key",
			err:         errors.New(`Get "https://www.theaudiodb.com/api/v1/json/abc123KEY/artist-mb.php?i=mbid": dial tcp: timeout`),
			wantMissing: "abc123KEY",
			wantPresent: "theaudiodb.com/api/v1/json/REDACTED/artist-mb.php",
		},
		{
			name:        "fanart query key",
			err:         errors.New(`Get "https://webservice.fanart.tv/v3/music/mbid?api_key=abc123KEY": timeout`),
			wantMissing: "abc123KEY",
			wantPresent: "api_key=REDACTED",
		},
		{
			// v2 carries the key in an X-API-KEY header, so the segment after
			// /json/ is the ENDPOINT NAME, not a credential. A v\d+ pattern
			// would redact it and destroy the diagnostic the #2457 error banner
			// exists to show. Pin that v2 survives the scrub intact.
			name:        "audiodb v2 endpoint name is not a credential",
			err:         errors.New(`Get "https://www.theaudiodb.com/api/v2/json/lookup/artist_mb/eec63d3c": dial tcp: timeout`),
			wantMissing: "REDACTED",
			wantPresent: "theaudiodb.com/api/v2/json/lookup/artist_mb/eec63d3c",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ScrubError(tc.err)
			if strings.Contains(got, tc.wantMissing) {
				t.Errorf("ScrubError leaked %q: %s", tc.wantMissing, got)
			}
			if !strings.Contains(got, tc.wantPresent) {
				t.Errorf("ScrubError = %q, want it to contain %q", got, tc.wantPresent)
			}
		})
	}
}

// TestProviderAcceptsMBID pins the predicate that drives the skip decision.
// Getting this wrong in either direction is a shipped bug: a false negative
// silently discards a provider's artwork (the #2457 AudioDB case), a false
// positive sends a MusicBrainz UUID to an endpoint that cannot parse it.
func TestProviderAcceptsMBID(t *testing.T) {
	if !ProviderAcceptsMBID(NameAudioDB) {
		t.Error("AudioDB must accept an MBID: its adapter routes non-numeric ids to artist-mb.php")
	}
	for _, name := range []ProviderName{NameDiscogs, NameDeezer, NameSpotify} {
		if ProviderAcceptsMBID(name) {
			t.Errorf("%s has no MusicBrainz lookup endpoint and must not be sent an MBID", name)
		}
	}
}

// TestFetchImagesNotFoundCountsAsQueried pins the ErrNotFound branch, which is
// the load-bearing distinction of #2457: a provider that was ASKED and had
// nothing is not the same as a provider that was never asked. A hostile reviewer
// deleted the status append from that branch and the entire package still
// passed, so the invariant "one status per provider iterated" was unpinned
// exactly where it matters most.
//
// Failure scenario if it regresses: AudioDB is queried with an MBID it has never
// heard of and returns ErrNotFound; Discogs, Deezer and Spotify are skipped for
// want of an ID. With the append gone, every recorded status reads "skipped", so
// allProvidersSkipped() turns true and the UI announces "No provider could be
// searched for this artist" -- a lie about a search that did query AudioDB.
func TestFetchImagesNotFoundCountsAsQueried(t *testing.T) {
	registry, settings := setupOrchestratorTest(t)

	const mbid = "mbid-obscure-artist"
	var audioDBCalledWith string

	registry.Register(&mockProvider{
		name: NameAudioDB,
		getImgFn: func(_ context.Context, id string) ([]ImageResult, error) {
			audioDBCalledWith = id
			return nil, &ErrNotFound{Provider: NameAudioDB, ID: id}
		},
	})
	for _, name := range []ProviderName{NameDiscogs, NameDeezer, NameSpotify} {
		provName := name
		registry.Register(&mockProvider{
			name: provName,
			getImgFn: func(_ context.Context, _ string) ([]ImageResult, error) {
				t.Errorf("%s was queried, but it has no ID and no MusicBrainz lookup: it must be skipped", provName)
				return nil, nil
			},
		})
	}
	for _, name := range []ProviderName{NameAudioDB, NameDiscogs, NameDeezer, NameSpotify} {
		if err := settings.SetAPIKey(context.Background(), name, "test-key"); err != nil {
			t.Fatalf("SetAPIKey %s: %v", name, err)
		}
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	orch := NewOrchestrator(registry, settings, logger, nil)

	result, err := orch.FetchImages(context.Background(), mbid, BuildProviderIDMap("", "", "", ""))
	if err != nil {
		t.Fatalf("FetchImages: %v", err)
	}

	// Precondition: AudioDB really was reached and really did return not-found.
	// Without this the assertions below could pass on a world where nothing ran.
	if audioDBCalledWith != mbid {
		t.Fatalf("AudioDB was called with %q, want the MBID %q -- the not-found path never executed", audioDBCalledWith, mbid)
	}
	if len(result.ImageProviderStatuses) != 4 {
		t.Fatalf("expected a status for each of the 4 providers iterated, got %d: %+v",
			len(result.ImageProviderStatuses), result.ImageProviderStatuses)
	}

	audioDB, ok := imageStatusFor(result.ImageProviderStatuses, NameAudioDB)
	if !ok {
		t.Fatal("AudioDB returned ErrNotFound and contributed NO status; every provider iterated must contribute exactly one")
	}
	if audioDB.Outcome != ImageOutcomeQueried {
		t.Errorf("AudioDB outcome = %q, want %q: not-found means asked-and-empty, not skipped and not errored",
			audioDB.Outcome, ImageOutcomeQueried)
	}

	// The consequence the operator actually sees: a not-found provider keeps the
	// search from reading as "nothing could be searched at all".
	queried := 0
	for _, st := range result.ImageProviderStatuses {
		if st.Outcome == ImageOutcomeQueried {
			queried++
		}
	}
	if queried != 1 {
		t.Errorf("queried-provider count = %d, want 1; statuses: %+v", queried, result.ImageProviderStatuses)
	}

	// A not-found is not a failure: it must not surface as an operator error.
	if len(result.Errors) != 0 {
		t.Errorf("ErrNotFound surfaced as an operator-visible error: %+v", result.Errors)
	}
}
