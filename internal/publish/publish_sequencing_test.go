package publish

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
)

// embyRequestRecorder captures, in arrival order, the Emby write requests a
// single publish generates against one fake server: the metadata push
// (POST /Items/{id}) and the two /Items/{id}/Refresh variants -- the push
// path's NON-destructive refresh (emby refreshItem, no MetadataRefreshMode)
// and the destructive FullRefresh re-import (MetadataRefreshMode=FullRefresh,
// ReplaceAllMetadata=true). Recording both refreshes on one server lets the
// test assert their real HTTP ordering instead of stubbing the dispatcher.
type embyRequestRecorder struct {
	mu    sync.Mutex
	order []string // "push" | "refresh-nondestructive" | "refresh-destructive"
}

func (rec *embyRequestRecorder) record(kind string) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.order = append(rec.order, kind)
}

func (rec *embyRequestRecorder) snapshot() []string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([]string(nil), rec.order...)
}

func (rec *embyRequestRecorder) count() int {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return len(rec.order)
}

// newEmbySequenceServer returns an httptest server that classifies each POST it
// receives and records the arrival order into rec.
func newEmbySequenceServer(rec *embyRequestRecorder) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			switch {
			case strings.HasSuffix(r.URL.Path, "/Refresh"):
				// FullRefresh + ReplaceAllMetadata=true marks the destructive
				// NFO->platform re-import (refresh_trigger.go). The push path's
				// own refresh sends neither.
				if r.URL.Query().Get("MetadataRefreshMode") == "FullRefresh" &&
					r.URL.Query().Get("ReplaceAllMetadata") == "true" {
					rec.record("refresh-destructive")
				} else {
					rec.record("refresh-nondestructive")
				}
			case strings.Contains(r.URL.Path, "/Items/"):
				rec.record("push")
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}

// TestPublishMetadata_SequencesDestructiveRefreshAfterPush is the #2336 review-P2
// regression. PublishMetadata previously spawned the destructive NFO re-import
// and the API push (whose own non-destructive refresh persists Emby's in-memory
// item back to the NFO) as two UNORDERED goroutines racing on the same item; if
// the non-destructive refresh landed after the re-import it clobbered the
// on-disk NFO and dropped Disambiguation/YearsActive at the source.
//
// The fix sequences them: push + its non-destructive refresh run FIRST and are
// awaited, THEN the destructive FullRefresh fires. This test drives a real
// end-to-end publish against a single fake Emby server and asserts the
// destructive refresh is the LAST refresh observed -- deterministically.
//
// Measured: with the sequencing reverted (re-import fired before/concurrent
// with the push) the destructive refresh can precede the non-destructive one
// and the ordering assertion fails; with the fix it is always last -> GREEN.
func TestPublishMetadata_SequencesDestructiveRefreshAfterPush(t *testing.T) {
	rec := &embyRequestRecorder{}
	srv := newEmbySequenceServer(rec)
	defer srv.Close()

	conn := &connection.Connection{
		ID: "c-emby", Type: connection.TypeEmby, Enabled: true, Status: "ok", Name: "Emby",
		URL: srv.URL,
		// Opt in to the destructive re-import; PlatformUserID satisfies the
		// emby client constructor (no locked-field GET is issued for this
		// artist because LockSortName is false).
		Emby: &connection.EmbyConfig{PlatformUserID: "u1", FeatureTriggerRefresh: true},
	}

	p := New(Deps{
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c-emby", PlatformArtistID: "emby-pid"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c-emby": conn,
		}},
		Logger: silentLogger(),
	})

	// Real path with no artist.nfo -> WriteBackNFO creates one so the gated
	// re-import is allowed to fire (see review-P3 gate).
	p.PublishMetadata(context.Background(), &artist.Artist{ID: "a1", Name: "X", Path: t.TempDir()})

	// Expect three POSTs: push, its non-destructive refresh, and the destructive
	// re-import. Wait on that observable outcome (goroutine-dispatched).
	waitForEmbyRequests(t, rec, 3)

	order := rec.snapshot()
	idxPush, idxNonDestructive, idxDestructive := -1, -1, -1
	for i, k := range order {
		switch k {
		case "push":
			if idxPush == -1 {
				idxPush = i
			}
		case "refresh-nondestructive":
			if idxNonDestructive == -1 {
				idxNonDestructive = i
			}
		case "refresh-destructive":
			if idxDestructive == -1 {
				idxDestructive = i
			}
		}
	}

	if idxPush == -1 {
		t.Fatalf("no metadata push observed; order=%v", order)
	}
	if idxNonDestructive == -1 {
		t.Fatalf("no non-destructive push refresh observed; order=%v", order)
	}
	if idxDestructive == -1 {
		t.Fatalf("no destructive FullRefresh re-import observed; order=%v", order)
	}
	// The destructive re-import MUST be the last refresh: strictly after the
	// push and after the push path's own non-destructive refresh.
	if idxDestructive < idxPush {
		t.Errorf("destructive re-import (idx %d) ran before the push (idx %d); order=%v", idxDestructive, idxPush, order)
	}
	if idxDestructive < idxNonDestructive {
		t.Errorf("destructive re-import (idx %d) ran before the push's non-destructive refresh (idx %d); order=%v", idxDestructive, idxNonDestructive, order)
	}
}

// waitForEmbyRequests polls up to 2s for the recorder to observe want requests.
func waitForEmbyRequests(t *testing.T, rec *embyRequestRecorder, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if rec.count() >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected %d emby requests, got %d (order=%v)", want, rec.count(), rec.snapshot())
}
