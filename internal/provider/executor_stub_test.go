package provider

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
)

// scrapeAllCall is one recorded ScrapeAll invocation.
type scrapeAllCall struct {
	MBID, Name, Scope string
	ProviderIDs       map[ProviderName]string
}

// stubExecutor fakes the production scraper executor
// (internal/scraper.Executor) for tests in this package. It implements the same
// ScraperExecutor interface the production path uses, so
// Orchestrator.SetExecutor accepts it exactly as it accepts the real thing.
//
// It answers with a fixed (result, err) pair and records every call. That is
// only a faithful stand-in when the fixture hands back what the REAL executor
// returns for the scenario: a result whose Metadata is non-nil with a non-nil
// URLs map, AttemptedFields/Sources populated as ScrapeAll populates them, and
// so on. A fixture that returns a bare &FetchResult{} makes the code under
// test pass against a shape production never produces, and a test that only
// records calls proves delegation, not behavior.
//
// It lives in a _test.go file because internal/provider's own tests cannot
// import a helper package that itself imports internal/provider (an import
// cycle). Packages that import provider (api, rule) already carry their own
// stubs; a shared providertest package is the slice-2 decision, not this one.
type stubExecutor struct {
	result *FetchResult
	err    error

	mu    sync.Mutex
	calls []scrapeAllCall
}

// ScrapeAll records the call and returns the configured result and error.
func (s *stubExecutor) ScrapeAll(_ context.Context, mbid, name, scope string, providerIDs map[ProviderName]string) (*FetchResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, scrapeAllCall{MBID: mbid, Name: name, Scope: scope, ProviderIDs: providerIDs})
	return s.result, s.err
}

// Calls returns a copy of the recorded invocations.
func (s *stubExecutor) Calls() []scrapeAllCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]scrapeAllCall(nil), s.calls...)
}

// TestFetchMetadata_DelegatesToExecutor pins the production contract of
// FetchMetadata: with an executor set, the call is handed off whole (global
// scope, same IDs map), the executor's result and error come back unchanged,
// and no provider is queried by the orchestrator itself.
func TestFetchMetadata_DelegatesToExecutor(t *testing.T) {
	registry, settings := setupOrchestratorTest(t)
	queried := false
	registry.Register(&mockProvider{
		name: NameAudioDB,
		getArtFn: func(context.Context, string) (*ArtistMetadata, error) {
			queried = true
			return &ArtistMetadata{Name: "from-loop"}, nil
		},
	})
	orch := NewOrchestrator(registry, settings, slog.New(slog.DiscardHandler), nil)

	want := &FetchResult{
		Metadata:        &ArtistMetadata{Name: "from-executor", URLs: map[string]string{}},
		AttemptedFields: []string{"biography"},
	}
	stub := &stubExecutor{result: want}
	orch.SetExecutor(stub)

	ids := map[ProviderName]string{NameAudioDB: "111493"}
	got, err := orch.FetchMetadata(context.Background(), "mbid-1", "Artist", ids)
	if err != nil {
		t.Fatalf("FetchMetadata: %v", err)
	}
	if got != want {
		t.Errorf("result = %p, want the executor's result %p", got, want)
	}
	calls := stub.Calls()
	if len(calls) != 1 {
		t.Fatalf("executor calls = %d, want 1", len(calls))
	}
	c := calls[0]
	if c.MBID != "mbid-1" || c.Name != "Artist" || c.Scope != "global" || c.ProviderIDs[NameAudioDB] != "111493" {
		t.Errorf("executor got %+v, want mbid-1/Artist/global with the AudioDB ID", c)
	}
	if queried {
		t.Error("orchestrator queried a provider itself instead of delegating")
	}

	boom := errors.New("executor failed")
	orch.SetExecutor(&stubExecutor{err: boom})
	if _, err := orch.FetchMetadata(context.Background(), "mbid-1", "Artist", nil); !errors.Is(err, boom) {
		t.Errorf("err = %v, want the executor's error", err)
	}
}
