package api

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/provider"
)

// countingRGFetcher fails on any non-normalized id, modeling MusicBrainz
// rejecting a padded or uppercase MBID. It can be configured to return an error
// on all calls via the fetchErr field.
type countingRGFetcher struct {
	calls    []string
	fetchErr error
}

func (f *countingRGFetcher) GetReleaseGroups(_ context.Context, id string) ([]provider.ReleaseGroupInfo, error) {
	f.calls = append(f.calls, id)
	if f.fetchErr != nil {
		return nil, f.fetchErr
	}
	if id != normalizeMBID(id) {
		return nil, errors.New("invalid mbid")
	}
	return []provider.ReleaseGroupInfo{{Title: "Album"}}, nil
}

const (
	rgClean  = "5b11f4ce-a62d-471e-81fc-a69a8278c7da"
	rgPadded = " 5B11F4CE-A62D-471E-81FC-A69A8278C7DA "
)

// One MBID spelled two ways must cost ONE fetch, asked with the normalized id
// (#2868), and holds must recognize a spelling other than the fetched one.
func TestReleaseGroupCache_TitlesKeysOnNormalizedMBID(t *testing.T) {
	f := &countingRGFetcher{}
	c := &releaseGroupCache{fetcher: f, entries: make(map[string]releaseGroupEntry)}

	if _, ok := c.titles(context.Background(), rgClean); !ok {
		t.Fatal("first lookup should be known")
	}
	if !c.holds(rgPadded) {
		t.Error("holds must see a different spelling as already cached")
	}
	if titles, ok := c.titles(context.Background(), rgPadded); !ok || len(titles) != 1 {
		t.Fatalf("second spelling = %v, %v; want the cached titles", titles, ok)
	}
	if len(f.calls) != 1 || f.calls[0] != rgClean {
		t.Fatalf("fetch calls = %q, want exactly [%q]", f.calls, rgClean)
	}
}

// A padded spelling fetched FIRST must not poison the shared entry for the
// clean spelling.
func TestReleaseGroupCache_PaddedFirstDoesNotPoisonClean(t *testing.T) {
	f := &countingRGFetcher{}
	c := &releaseGroupCache{fetcher: f, entries: make(map[string]releaseGroupEntry)}

	if _, ok := c.titles(context.Background(), rgPadded); !ok {
		t.Fatal("padded spelling should resolve via the normalized id")
	}
	if titles, ok := c.titles(context.Background(), rgClean); !ok || len(titles) != 1 {
		t.Fatalf("clean spelling = %v, %v; want titles", titles, ok)
	}
	if len(f.calls) != 1 || f.calls[0] != rgClean {
		t.Fatalf("fetch calls = %q, want exactly [%q]", f.calls, rgClean)
	}
}

// A failed fetch is cached under the normalized MBID key, so subsequent lookups
// of the same MBID spelled two ways both report failure without a second fetch.
// The failure is logged with the RAW spelling the caller supplied.
func TestReleaseGroupCache_FailureCachedAndLogged(t *testing.T) {
	var logs bytes.Buffer
	fetchErr := errors.New("provider down")
	f := &countingRGFetcher{fetchErr: fetchErr}
	c := &releaseGroupCache{
		fetcher: f,
		logger:  slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})),
		entries: make(map[string]releaseGroupEntry),
	}

	// Fetch with padded spelling fails.
	if titles, ok := c.titles(context.Background(), rgPadded); ok || len(titles) != 0 {
		t.Fatalf("padded fetch = %v, %v; want nil, false", titles, ok)
	}

	// Second lookup with clean spelling uses cached failure without a second fetch.
	if titles, ok := c.titles(context.Background(), rgClean); ok || len(titles) != 0 {
		t.Fatalf("clean fetch = %v, %v; want nil, false", titles, ok)
	}

	// Only one fetch was made.
	if len(f.calls) != 1 || f.calls[0] != rgClean {
		t.Fatalf("fetch calls = %q, want exactly [%q]", f.calls, rgClean)
	}

	// The failure is logged with the RAW (padded) MBID.
	logStr := logs.String()
	if !strings.Contains(logStr, rgPadded) {
		t.Errorf("log output does not contain raw padded MBID %q:\n%s", rgPadded, logStr)
	}
}
