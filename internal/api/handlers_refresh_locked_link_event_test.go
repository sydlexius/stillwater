package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/event"
)

// artistUpdatedRecorder wires a live event bus into the router and records the
// artist IDs carried by every ArtistUpdated event it sees. The bus dispatches
// asynchronously, so reads go through waitForArtistUpdated rather than a bare
// slice read.
type artistUpdatedRecorder struct {
	mu  sync.Mutex
	ids []string
}

// attachArtistUpdatedRecorder starts an event bus, subscribes a recorder to
// ArtistUpdated and installs the bus on the router. The bus is stopped when the
// test finishes.
func attachArtistUpdatedRecorder(t *testing.T, r *Router) *artistUpdatedRecorder {
	t.Helper()
	rec := &artistUpdatedRecorder{}
	bus := event.NewBus(slog.New(slog.NewTextHandler(io.Discard, nil)), 1024)
	bus.Subscribe(event.ArtistUpdated, func(e event.Event) {
		id, ok := e.Data["artist_id"].(string)
		if !ok {
			return
		}
		rec.mu.Lock()
		defer rec.mu.Unlock()
		rec.ids = append(rec.ids, id)
	})
	go bus.Start()
	t.Cleanup(bus.Stop)
	r.eventBus = bus
	return rec
}

// waitForArtistUpdated polls until the recorder has seen artistID or the
// deadline expires, and reports whether it arrived.
func (rec *artistUpdatedRecorder) waitForArtistUpdated(artistID string) bool {
	deadline := time.Now().Add(2 * time.Second)
	for {
		rec.mu.Lock()
		seen := slices.Contains(rec.ids, artistID)
		rec.mu.Unlock()
		if seen {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// postRefreshLink drives handleRefreshLink directly with the {id} path value
// set, mirroring what the router does for POST
// /api/v1/artists/{id}/refresh/link.
func postRefreshLink(t *testing.T, r *Router, artistID, mbid string) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`{"mbid":%q,"source":"musicbrainz"}`, mbid)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+artistID+"/refresh/link", strings.NewReader(body))
	req = req.WithContext(testI18nCtx(t, req.Context()))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", artistID)
	w := httptest.NewRecorder()
	r.handleRefreshLink(w, req)
	return w
}

// TestHandleRefreshLink_LockedArtistPublishesArtistUpdated is the guard for the
// locked-link half of #2754's follow-up: unlike handleArtistRefresh, which
// gates BEFORE it touches the artist, handleRefreshLink gates AFTER persisting
// the user's chosen provider ID. Something genuinely changed on the row, so
// invalidating the health cache without publishing left nothing to recompute
// it -- rules keyed on local state (MBID presence, for one) would keep
// reporting the pre-link violation, and the next cache fill would re-read the
// stale violations. autoLinkAndRefresh already publishes on its own
// locked-skip path for exactly this reason.
//
// The event assertion alone would be satisfied by an implementation that also
// ran the provider refresh, so it is paired with the ABSENT sentinel biography:
// that string can only reach the database via executeRefresh, which the lock
// must still suppress. attachSentinelOrchestrator is what makes that assertion
// real -- a router with no orchestrator could not refresh at all. The positive
// control is TestHandleRefreshLink_UnlockedArtistPublishesAndRefreshes.
func TestHandleRefreshLink_LockedArtistPublishesArtistUpdated(t *testing.T) {
	t.Parallel()
	r, artistSvc := bulkRefreshRouter(t, lockedRefreshFetchResult())
	rec := attachArtistUpdatedRecorder(t, r)
	a := lockRefreshableArtist(t, artistSvc, "Locked Link Event")

	const chosenMBID = "chosen-mbid-locked-link-event"
	// Precondition: the request carries an MBID that differs from the seeded
	// one, so "did the row genuinely change?" is a real question. Without it
	// the publish under test would not be owed in the first place.
	if a.MusicBrainzID == chosenMBID {
		t.Fatal("precondition failed: seeded MBID already equals the chosen MBID")
	}
	if a.Biography != "" {
		t.Fatalf("precondition failed: artist already has a biography (%q), so its absence would prove nothing", a.Biography)
	}

	w := postRefreshLink(t, r, a.ID, chosenMBID)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if got := decodeRefreshStatus(t, w); got != "skipped_locked" {
		t.Errorf("status = %q, want skipped_locked", got)
	}

	saved, err := artistSvc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("reloading artist: %v", err)
	}
	if saved.MusicBrainzID != chosenMBID {
		t.Fatalf("MusicBrainzID = %q, want %q; without the persisted change there is nothing for the event to announce", saved.MusicBrainzID, chosenMBID)
	}

	if !rec.waitForArtistUpdated(a.ID) {
		t.Errorf("ArtistUpdated not published for locked artist %s after a genuine provider-ID change; the health cache was invalidated with nothing left to recompute it", a.ID)
	}

	// The lock's actual promise: no automated provider work. The event above
	// drives a read-only re-evaluation, not a fetch.
	assertSentinelBiographyAbsent(t, artistSvc, a.ID)
}

// TestHandleRefreshLink_UnlockedArtistPublishesAndRefreshes is the positive
// control for the test above. It proves the stubbed orchestrator really does
// land the sentinel biography when no lock intervenes, so the locked test's
// "biography is empty" assertion is a genuine observation rather than a
// broken-for-everyone no-op, and that the recorder observes a publish on the
// ordinary path too.
func TestHandleRefreshLink_UnlockedArtistPublishesAndRefreshes(t *testing.T) {
	t.Parallel()
	r, artistSvc := bulkRefreshRouter(t, lockedRefreshFetchResult())
	rec := attachArtistUpdatedRecorder(t, r)
	a := addRefreshableArtist(t, artistSvc, "Unlocked Link Event")
	if a.Locked {
		t.Fatal("precondition failed: artist is locked; this test covers the unlocked path")
	}

	const chosenMBID = "chosen-mbid-unlocked-link-event"
	w := postRefreshLink(t, r, a.ID, chosenMBID)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response body: %v", err)
	}
	if resp["status"] != "linked_and_refreshed" {
		t.Errorf("status = %v, want linked_and_refreshed; body=%v", resp["status"], resp)
	}

	if !rec.waitForArtistUpdated(a.ID) {
		t.Errorf("ArtistUpdated not published on the ordinary link path for %s", a.ID)
	}
	assertSentinelBiographyPresent(t, artistSvc, a.ID)
}
