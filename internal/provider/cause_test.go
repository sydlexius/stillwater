package provider

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/logging/logtest"
)

// causeRecord is the slice of an emitted log record these tests read.
type causeRecord struct {
	Msg   string  `json:"msg"`
	Error string  `json:"error"`
	Cause *string `json:"cause"` // pointer: a MISSING attribute must not read as ""
}

// causeRecords returns every captured record whose message is msg.
func causeRecords(t *testing.T, output, msg string) []causeRecord {
	t.Helper()
	var out []causeRecord
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if line == "" {
			continue
		}
		var rec causeRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("unparsable log line %q: %v", line, err)
		}
		if rec.Msg == msg {
			out = append(out, rec)
		}
	}
	return out
}

// newCauseOrchestrator builds a real Orchestrator over one provider. The
// providers these tests register are AudioDB and Deezer: neither needs an API
// key, so both are always available, and both sit in default priority chains.
func newCauseOrchestrator(t *testing.T, p Provider) (*Orchestrator, *logtest.Buffer) {
	t.Helper()
	registry, settings := setupOrchestratorTest(t)
	registry.Register(p)
	logger, buf := logtest.NewJSONLogger()
	return NewOrchestrator(registry, settings, logger, nil), buf
}

func TestCause_String(t *testing.T) {
	for _, tc := range []struct {
		cause Cause
		want  string
	}{
		{Cause{}, ""},
		{Cause{Class: "scheduled"}, "scheduled"},
		{Cause{Detail: "nfo_exists"}, "nfo_exists"},
		{Cause{Class: "rule", Detail: "nfo_exists"}, "rule:nfo_exists"},
	} {
		if got := tc.cause.String(); got != tc.want {
			t.Errorf("Cause{Class: %q, Detail: %q}.String() = %q, want %q", tc.cause.Class, tc.cause.Detail, got, tc.want)
		}
	}
}

func TestCause_AbsentIsZeroAndEnrichPreservesOuterClass(t *testing.T) {
	bg := context.Background()
	if got := CauseFromContext(bg); got != (Cause{}) {
		t.Fatalf("CauseFromContext on a bare context = %+v, want the zero Cause", got)
	}

	scheduled := WithCause(bg, Cause{Class: CauseClassScheduled})
	cases := []struct {
		name string
		ctx  context.Context
		want Cause
	}{
		{"no outer cause: the instance is the cause", EnrichCause(bg, CauseClassRule, "nfo_exists"),
			Cause{Class: CauseClassRule, Detail: "nfo_exists"}},
		{"outer class kept, instance becomes the detail", EnrichCause(scheduled, CauseClassRule, "nfo_exists"),
			Cause{Class: CauseClassScheduled, Detail: "rule:nfo_exists"}},
		{"a second rule replaces the first, it does not stack",
			EnrichCause(EnrichCause(scheduled, CauseClassRule, "nfo_exists"), CauseClassRule, "bio_exists"),
			Cause{Class: CauseClassScheduled, Detail: "rule:bio_exists"}},
		{"same class as the existing cause replaces the detail, it does not nest",
			EnrichCause(EnrichCause(bg, CauseClassRule, "A"), CauseClassRule, "B"),
			Cause{Class: CauseClassRule, Detail: "B"}},
		{"same class, empty detail: the existing detail is kept",
			EnrichCause(WithCause(bg, Cause{Class: CauseClassScan, Detail: "full"}), CauseClassScan, ""),
			Cause{Class: CauseClassScan, Detail: "full"}},
		{"same class, outer detail is itself a nested cause: replaced",
			EnrichCause(WithCause(bg, Cause{Class: CauseClassRule, Detail: "rule:X"}), CauseClassRule, "B"),
			Cause{Class: CauseClassRule, Detail: "B"}},
		{"same class, empty detail on a bare outer: stays class-only",
			EnrichCause(WithCause(bg, Cause{Class: CauseClassScan}), CauseClassScan, ""),
			Cause{Class: CauseClassScan}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CauseFromContext(tc.ctx); got != tc.want {
				t.Errorf("cause = %+v, want %+v", got, tc.want)
			}
		})
	}
	if got := CauseFromContext(EnrichCause(WithCause(bg, Cause{Class: CauseClassScan}), CauseClassScan, "")).String(); got != "scan" {
		t.Errorf("bare same-class enrich renders %q, want %q", got, "scan")
	}
	if got := CauseFromContext(scheduled); got != (Cause{Class: CauseClassScheduled}) {
		t.Errorf("enriching a child changed the parent context's cause to %+v", got)
	}
}

// Every line a shared fetch point emits reports the cause, and reports the
// explicit marker -- not a blank or absent attribute -- when the caller set
// none. One row per cause-bearing line, each driven by an adapter that makes
// the real code path emit exactly that line.
func TestFetchPoints_LogCauseOrUnattributedMarker(t *testing.T) {
	boom := errors.New("upstream exploded")
	notFound := &ErrNotFound{Provider: NameAudioDB, ID: "mbid-1"}
	artErr := func(err error) func(context.Context, string) (*ArtistMetadata, error) {
		return func(context.Context, string) (*ArtistMetadata, error) { return nil, err }
	}
	imgErr := func(err error) func(context.Context, string) ([]ImageResult, error) {
		return func(context.Context, string) ([]ImageResult, error) { return nil, err }
	}
	searchBoom := func(context.Context, string) ([]ArtistSearchResult, error) { return nil, boom }

	fetchMetadata := func(ctx context.Context, o *Orchestrator) error {
		_, err := o.FetchMetadata(ctx, "mbid-1", "Some Artist", nil)
		return err
	}
	fetchImages := func(ctx context.Context, o *Orchestrator) error {
		// The empty Deezer entry is what "provider-specific ID not known" looks like.
		_, err := o.FetchImages(ctx, "mbid-1", map[ProviderName]string{NameDeezer: ""})
		return err
	}
	linking := func(ctx context.Context, o *Orchestrator) error {
		_, _, err := o.SearchForLinking(ctx, "Some Artist", []ProviderName{NameAudioDB})
		return err
	}

	points := []struct {
		name string
		msg  string
		prov Provider
		call func(ctx context.Context, o *Orchestrator) error
	}{
		{"FetchProviderResult no data", "provider has no data for artist",
			&mockProvider{name: NameAudioDB, getArtFn: artErr(notFound)}, fetchMetadata},
		{"FetchProviderResult GetArtist failed", "provider GetArtist failed",
			&mockProvider{name: NameAudioDB, getArtFn: artErr(boom)}, fetchMetadata},
		{"FetchProviderResult name retry", "retrying with artist name after MBID not-found",
			&mockNameLookupProvider{mockProvider{name: NameAudioDB, getArtFn: artErr(notFound)}}, fetchMetadata},
		{"FetchProviderResult no images", "provider has no images for artist",
			&mockProvider{name: NameAudioDB, getImgFn: imgErr(notFound)}, fetchMetadata},
		{"FetchProviderResult GetImages failed", "provider GetImages failed, preserving existing image data",
			&mockProvider{name: NameAudioDB, getImgFn: imgErr(boom)}, fetchMetadata},
		{"FetchImages skipped", "provider skipped: no provider-specific ID",
			&mockProvider{name: NameDeezer}, fetchImages},
		{"FetchImages no images", "provider has no images for artist",
			&mockProvider{name: NameAudioDB, getImgFn: imgErr(notFound)}, fetchImages},
		{"FetchImages failed", "provider image fetch failed",
			&mockProvider{name: NameAudioDB, getImgFn: imgErr(boom)}, fetchImages},
		{"Search failed", "provider search failed",
			&mockProvider{name: NameAudioDB, searchFn: searchBoom}, func(ctx context.Context, o *Orchestrator) error {
				_, err := o.Search(ctx, "Some Artist")
				return err
			}},
		{"SearchForLinking failed", "provider search failed",
			&mockProvider{name: NameAudioDB, searchFn: searchBoom}, linking},
		{"SearchForLinking panicked", "provider search panicked",
			&mockProvider{name: NameAudioDB, searchFn: func(context.Context, string) ([]ArtistSearchResult, error) {
				panic("adapter blew up")
			}}, linking},
		{"FetchFieldFromProviders", "provider image fetch failed for comparison",
			&mockProvider{name: NameAudioDB, getImgFn: imgErr(boom)}, func(ctx context.Context, o *Orchestrator) error {
				_, err := o.FetchFieldFromProviders(ctx, "mbid-1", "Some Artist", "thumb", nil)
				return err
			}},
	}
	causes := []struct {
		name string
		ctx  context.Context
		want string
	}{
		{"no cause set", context.Background(), "unattributed"},
		{"cause set", WithCause(context.Background(), Cause{Class: CauseClassRule, Detail: "nfo_exists"}), "rule:nfo_exists"},
	}
	for _, pt := range points {
		for _, c := range causes {
			t.Run(pt.name+"/"+c.name, func(t *testing.T) {
				orch, buf := newCauseOrchestrator(t, pt.prov)
				if err := pt.call(c.ctx, orch); err != nil {
					t.Fatalf("fetch entry point failed outright: %v", err)
				}
				recs := causeRecords(t, buf.String(), pt.msg)
				// Precondition: the line under test was emitted at all.
				if len(recs) == 0 {
					t.Fatalf("no %q record emitted; log was:\n%s", pt.msg, buf.String())
				}
				for _, rec := range recs {
					if rec.Cause == nil {
						t.Fatalf("%q record carries no cause attribute at all", pt.msg)
					}
					if *rec.Cause != c.want {
						t.Errorf("%q cause = %q, want %q", pt.msg, *rec.Cause, c.want)
					}
				}
			})
		}
	}
}

// Two operations share one orchestrator and are provably in flight at the same
// moment (each parks inside the provider until both have entered). Each one's
// record must carry its own cause and never the other's.
//
// SearchForLinking rather than Search: it reads no settings, and the fixture's
// in-memory SQLite gives every pooled connection its own empty database, so two
// concurrent settings reads would fail before reaching the provider.
func TestCause_DoesNotLeakBetweenConcurrentOperations(t *testing.T) {
	ops := map[string]Cause{
		"artist-a": {Class: CauseClassRule, Detail: "nfo_exists"},
		"artist-b": {Class: CauseClassScheduled, Detail: "rule:bio_exists"},
	}
	entered := make(chan struct{}, len(ops))
	release := make(chan struct{})
	orch, buf := newCauseOrchestrator(t, &mockProvider{
		name: NameAudioDB,
		searchFn: func(_ context.Context, name string) ([]ArtistSearchResult, error) {
			entered <- struct{}{}
			<-release
			return nil, errors.New("failed for " + name)
		},
	})

	done := make(chan error, len(ops))
	for name, cause := range ops {
		ctx := WithCause(context.Background(), cause)
		go func() {
			_, _, err := orch.SearchForLinking(ctx, name, []ProviderName{NameAudioDB})
			done <- err
		}()
	}
	for range ops {
		select {
		case <-entered: // both operations are inside the provider at once
		case err := <-done:
			t.Fatalf("an operation returned before reaching the provider (err=%v)", err)
		}
	}
	close(release)
	for range ops {
		if err := <-done; err != nil {
			t.Fatalf("SearchForLinking: %v", err)
		}
	}

	recs := causeRecords(t, buf.String(), "provider search failed")
	if len(recs) != len(ops) {
		t.Fatalf("got %d search-failed records, want %d; log was:\n%s", len(recs), len(ops), buf.String())
	}
	for _, rec := range recs {
		name := strings.TrimPrefix(rec.Error, "failed for ")
		want, ok := ops[name]
		if !ok {
			t.Fatalf("record names an operation this test did not start: %q", rec.Error)
		}
		if rec.Cause == nil {
			t.Fatalf("operation %s logged no cause attribute", name)
		}
		if *rec.Cause != want.String() {
			t.Errorf("operation %s logged cause %q, want %q", name, *rec.Cause, want.String())
		}
	}
}

type carryTestKey struct{}

func TestCarryCause(t *testing.T) {
	want := Cause{Class: CauseClassScan, Detail: "full"}

	t.Run("copies the source cause", func(t *testing.T) {
		src := WithCause(context.Background(), want)
		if got := CauseFromContext(CarryCause(context.Background(), src)); got != want {
			t.Errorf("cause = %+v, want %+v", got, want)
		}
	})

	t.Run("no source cause returns dst itself", func(t *testing.T) {
		dst := context.WithValue(context.Background(), carryTestKey{}, "dst")
		got := CarryCause(dst, context.Background())
		if got != dst {
			t.Error("CarryCause with a cause-less source did not return the very same dst")
		}
		if c := CauseFromContext(got); c != (Cause{}) {
			t.Errorf("cause = %+v, want the zero Cause", c)
		}
	})

	t.Run("takes only the cause: not cancellation, deadline or other values", func(t *testing.T) {
		base := WithCause(context.WithValue(context.Background(), carryTestKey{}, "src"), want)
		src, cancel := context.WithTimeout(base, time.Hour)
		cancel() // src is now canceled and has a deadline
		got := CarryCause(context.Background(), src)
		if got.Err() != nil {
			t.Errorf("result is canceled (%v); src's cancellation must not carry over", got.Err())
		}
		if _, ok := got.Deadline(); ok {
			t.Error("result has a deadline; src's deadline must not carry over")
		}
		if v := got.Value(carryTestKey{}); v != nil {
			t.Errorf("result sees src's value %v; only the cause may carry over", v)
		}
		if c := CauseFromContext(got); c != want {
			t.Errorf("cause = %+v, want %+v", c, want)
		}
	})
}

func TestCauseAttr(t *testing.T) {
	bare := CauseAttr(context.Background())
	if bare.Key != "cause" || bare.Value.String() != "unattributed" {
		t.Errorf("bare context attr = %v, want cause=unattributed", bare)
	}
	ctx := WithCause(context.Background(), Cause{Class: CauseClassWatcher, Detail: "create"})
	got := CauseAttr(ctx)
	if got.Key != "cause" || got.Value.String() != "watcher:create" {
		t.Errorf("attr = %v, want cause=watcher:create", got)
	}
}
