package api

import (
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/sydlexius/stillwater/internal/dupimages"
)

// newIsolationRouter builds a router the way every test helper does: through
// NewRouter with NO DupImageCache. It deliberately has no drain cleanup and no
// reset call, because the property under test is that none is needed.
func newIsolationRouter() *Router {
	return NewRouter(RouterDeps{
		Logger:   slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})),
		StaticFS: os.DirFS("../../web/static"),
	})
}

// TestDupImageCache_RouterWithoutInjectionOwnsPrivateCache is the structural
// guard for #2936. The duplicate-image cache used to be the process-wide
// dupimages.Shared() singleton, so a test that populated it leaked that state
// into every other test in the package unless the author remembered to Reset it
// (and to stay serial). Here router A populates its cache and the test never
// resets anything; router B must still see a cold cache. Against the old
// singleton B observes A's counts and this test fails.
//
// Safe under t.Parallel and -race: it touches only caches it created, and
// asserts only pointer non-identity with Shared() (it never reads or writes the
// singleton), so it neither disturbs nor depends on any other test's use of it.
func TestDupImageCache_RouterWithoutInjectionOwnsPrivateCache(t *testing.T) {
	t.Parallel()

	a := newIsolationRouter()
	b := newIsolationRouter()

	cacheA, cacheB := a.dupImageCache(), b.dupImageCache()
	if cacheA == cacheB {
		t.Fatal("two routers built without DupImageCache share one cache instance")
	}
	if cacheA == dupimages.Shared() || cacheB == dupimages.Shared() {
		t.Fatal("a router built without DupImageCache adopted the process-wide Shared() cache")
	}

	cacheA.Set(dupimages.Counts{Library: 7, Platforms: []dupimages.PlatformCount{embyCount(3)}})

	if !cacheA.Get().Computed {
		t.Fatal("precondition: router A's cache did not retain what the test set, so the isolation check below would pass vacuously")
	}
	if got := cacheB.Get(); got.Computed || got.Library != 0 || len(got.Platforms) != 0 {
		t.Errorf("router B observed router A's cache state: %+v; each router must own its cache", got)
	}
}

// TestDupImageCache_InjectedCacheIsTheOneRouterUses pins the production seam:
// cmd/stillwater/main.go injects dupimages.Shared() because the maintenance
// scheduler refreshes that same instance, so the router must use exactly what
// it was given rather than substitute its own. This pins the router's side of
// the seam only; that main.go actually passes Shared() is pinned by
// TestDupImageCacheWiringNamesOneInstance in cmd/stillwater.
func TestDupImageCache_InjectedCacheIsTheOneRouterUses(t *testing.T) {
	t.Parallel()

	// A private cache stands in for Shared(): injecting the real singleton here
	// would reintroduce the cross-test sharing this change removes.
	injected := dupimages.New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	r := NewRouter(RouterDeps{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})),
		StaticFS:      os.DirFS("../../web/static"),
		DupImageCache: injected,
	})

	if got := r.dupImageCache(); got != injected {
		t.Fatalf("dupImageCache() = %p, want the injected instance %p", got, injected)
	}
}
