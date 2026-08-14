package collision

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/event"
	"github.com/sydlexius/stillwater/internal/image"
)

// fakePublisher records every event published to it.
type fakePublisher struct {
	events []event.Event
}

func (f *fakePublisher) Publish(e event.Event) { f.events = append(f.events, e) }

// raiseCall captures one invocation of the injected violation raiser.
type raiseCall struct {
	destID      string
	destName    string
	message     string
	collidingID string
}

func TestNotify_Mismatch_EmitsBothSurfaces(t *testing.T) {
	pub := &fakePublisher{}
	var raised []raiseCall
	raise := func(_ context.Context, destID, destName, msg, collidingID string) error {
		raised = append(raised, raiseCall{destID, destName, msg, collidingID})
		return nil
	}
	nameOf := func(_ context.Context, id string) string {
		if id == "colliding-1" {
			return "Other Artist"
		}
		return ""
	}
	n := NewNotifier(pub, raise, nameOf, nil, nil)

	res := image.IdentityResult{
		Verdict:           image.IdentityMismatch,
		CollidingArtistID: "colliding-1",
		Similarity:        0.94,
		MatchCount:        2,
	}
	n.Notify(context.Background(), "dest-9", "Dest Artist", res)

	// SSE toast published exactly once, with the right type + structured data.
	if len(pub.events) != 1 {
		t.Fatalf("expected 1 published event, got %d", len(pub.events))
	}
	ev := pub.events[0]
	if ev.Type != event.BackdropCollision {
		t.Errorf("event type = %q, want %q", ev.Type, event.BackdropCollision)
	}
	if got := ev.Data["colliding_artist_id"]; got != "colliding-1" {
		t.Errorf("colliding_artist_id = %v, want colliding-1", got)
	}
	if got := ev.Data["colliding_artist_name"]; got != "Other Artist" {
		t.Errorf("colliding_artist_name = %v, want Other Artist", got)
	}
	if got := ev.Data["dest_artist_id"]; got != "dest-9" {
		t.Errorf("dest_artist_id = %v, want dest-9", got)
	}
	if got := ev.Data["similarity"]; got != 94 {
		t.Errorf("similarity = %v, want 94 (rounded pct)", got)
	}
	if got := ev.Data["match_count"]; got != 2 {
		t.Errorf("match_count = %v, want 2", got)
	}
	msg, _ := ev.Data["message"].(string)
	if !strings.Contains(msg, "Other Artist") || !strings.Contains(msg, "94%") || !strings.Contains(msg, "2 artists") {
		t.Errorf("message %q missing colliding name / pct / count", msg)
	}
	if strings.ContainsRune(msg, '—') {
		t.Errorf("message %q must not contain an em-dash", msg)
	}

	// Durable violation raised once for the DEST artist with the same message
	// and the colliding artist id (the operator-fixable Action Queue entry).
	if len(raised) != 1 {
		t.Fatalf("expected 1 raised violation, got %d", len(raised))
	}
	if raised[0].destID != "dest-9" || raised[0].collidingID != "colliding-1" {
		t.Errorf("raised on wrong artists: %+v", raised[0])
	}
	if raised[0].message != msg {
		t.Errorf("raised message %q != toast message %q", raised[0].message, msg)
	}
}

func TestNotify_FailOpen_Verdicts(t *testing.T) {
	for _, verdict := range []image.IdentityVerdict{image.IdentityMatch, image.IdentityIndeterminate} {
		pub := &fakePublisher{}
		raisedN := 0
		raise := func(_ context.Context, _, _, _, _ string) error { raisedN++; return nil }
		n := NewNotifier(pub, raise, nil, nil, nil)

		n.Notify(context.Background(), "dest", "Dest", image.IdentityResult{Verdict: verdict})

		if len(pub.events) != 0 {
			t.Errorf("verdict %s: published %d events, want 0 (fail-open)", verdict, len(pub.events))
		}
		if raisedN != 0 {
			t.Errorf("verdict %s: raised %d violations, want 0 (fail-open)", verdict, raisedN)
		}
	}
}

func TestNotify_MissingCollidingName_FallsBackToID(t *testing.T) {
	pub := &fakePublisher{}
	n := NewNotifier(pub, nil, nil, nil, nil) // nil nameOf -> label falls back to id
	n.Notify(context.Background(), "dest", "Dest", image.IdentityResult{
		Verdict:           image.IdentityMismatch,
		CollidingArtistID: "raw-id-7",
		Similarity:        0.90,
		MatchCount:        1,
	})
	if len(pub.events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(pub.events))
	}
	msg, _ := pub.events[0].Data["message"].(string)
	if !strings.Contains(msg, "raw-id-7") {
		t.Errorf("message %q should fall back to the colliding artist id", msg)
	}
}

func TestNotify_RaiseError_DoesNotPanicOrBlock(t *testing.T) {
	pub := &fakePublisher{}
	raise := func(_ context.Context, _, _, _, _ string) error { return errors.New("db down") }
	n := NewNotifier(pub, raise, nil, nil, nil)
	// Must still publish the toast and return normally even when the durable
	// raise fails (notify-only: the caller's write/push must never be blocked).
	n.Notify(context.Background(), "dest", "Dest", image.IdentityResult{
		Verdict:           image.IdentityMismatch,
		CollidingArtistID: "c",
		Similarity:        0.91,
		MatchCount:        1,
	})
	if len(pub.events) != 1 {
		t.Fatalf("expected the toast to still publish despite raise error, got %d events", len(pub.events))
	}
}

// TestNotify_NotifyEnabled_Disabled_SuppressesToastButStillRaises is the
// #2970 load-bearing test: disabling the rule must suppress ONLY the
// ephemeral toast. The durable violation write is unconditional, because
// nothing else can ever re-derive an event-driven finding missed while
// disabled (see IsRuleEnabled / RaiseBackdropCollision doc comments). An
// implementation that also skips the raise (the rejected early-return
// option) would still pass a suppression-only test, so this asserts BOTH
// halves.
func TestNotify_NotifyEnabled_Disabled_SuppressesToastButStillRaises(t *testing.T) {
	pub := &fakePublisher{}
	raisedN := 0
	raise := func(_ context.Context, _, _, _, _ string) error { raisedN++; return nil }
	notifyOn := func(_ context.Context) bool { return false }
	n := NewNotifier(pub, raise, nil, notifyOn, nil)

	n.Notify(context.Background(), "dest", "Dest", image.IdentityResult{
		Verdict:           image.IdentityMismatch,
		CollidingArtistID: "c",
		Similarity:        0.95,
		MatchCount:        1,
	})

	if len(pub.events) != 0 {
		t.Errorf("toast published %d events while disabled, want 0", len(pub.events))
	}
	if raisedN != 1 {
		t.Errorf("durable raise called %d times while disabled, want exactly 1 (the recording must never stop)", raisedN)
	}
}

// TestNotify_NotifyEnabled_Enabled_EmitsToast is the positive control for the
// test above: an implementation that unconditionally suppresses the toast
// (ignoring notifyOn entirely) would still pass a disabled-only test, so this
// pins that the predicate is actually consulted both ways.
func TestNotify_NotifyEnabled_Enabled_EmitsToast(t *testing.T) {
	pub := &fakePublisher{}
	raisedN := 0
	raise := func(_ context.Context, _, _, _, _ string) error { raisedN++; return nil }
	notifyOn := func(_ context.Context) bool { return true }
	n := NewNotifier(pub, raise, nil, notifyOn, nil)

	n.Notify(context.Background(), "dest", "Dest", image.IdentityResult{
		Verdict:           image.IdentityMismatch,
		CollidingArtistID: "c",
		Similarity:        0.95,
		MatchCount:        1,
	})

	if len(pub.events) != 1 {
		t.Errorf("toast published %d events while enabled, want 1", len(pub.events))
	}
	if raisedN != 1 {
		t.Errorf("durable raise called %d times while enabled, want exactly 1", raisedN)
	}
}

// TestNotify_NotifyEnabled_NilPredicate_EmitsToast pins back-compat: existing
// callers (before #2970) pass nil for notifyOn and must keep emitting.
func TestNotify_NotifyEnabled_NilPredicate_EmitsToast(t *testing.T) {
	pub := &fakePublisher{}
	n := NewNotifier(pub, nil, nil, nil, nil)

	n.Notify(context.Background(), "dest", "Dest", image.IdentityResult{
		Verdict:           image.IdentityMismatch,
		CollidingArtistID: "c",
		Similarity:        0.95,
		MatchCount:        1,
	})

	if len(pub.events) != 1 {
		t.Errorf("toast published %d events with nil predicate, want 1 (fail open)", len(pub.events))
	}
}

// TestNotify_NotifyEnabled_LookupFails_EmitsToast: a predicate that cannot
// determine the answer (e.g. a transient DB error surfaced as false) must
// still favor emitting the toast over silently hiding a real collision.
// Modeled here as the predicate itself returning true on its own failure
// path, mirroring what the cmd/stillwater wiring closure does around
// rule.Service.IsRuleEnabled's error return.
func TestNotify_NotifyEnabled_LookupFails_EmitsToast(t *testing.T) {
	pub := &fakePublisher{}
	lookupErr := errors.New("db down")
	notifyOn := func(_ context.Context) bool {
		// Simulates the wiring closure's fail-open branch: err != nil -> true.
		_ = lookupErr
		return true
	}
	n := NewNotifier(pub, nil, nil, notifyOn, nil)

	n.Notify(context.Background(), "dest", "Dest", image.IdentityResult{
		Verdict:           image.IdentityMismatch,
		CollidingArtistID: "c",
		Similarity:        0.95,
		MatchCount:        1,
	})

	if len(pub.events) != 1 {
		t.Errorf("toast published %d events on lookup failure, want 1 (fail open)", len(pub.events))
	}
}

func TestNotify_NilReceiver_IsNoOp(t *testing.T) {
	var n *Notifier
	// Must not panic.
	n.Notify(context.Background(), "dest", "Dest", image.IdentityResult{Verdict: image.IdentityMismatch})
}
