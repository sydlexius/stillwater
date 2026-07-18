// Package collision emits the cross-artist backdrop-collision notifications for
// #2540. When a fanart/backdrop about to be written or pushed for one artist
// perceptually matches ANOTHER artist's fanart, it fires two notifications:
//
//   - an ephemeral SSE warning toast (event.BackdropCollision), and
//   - a durable, operator-fixable rule_violations row (via an injected raiser)
//     that surfaces on the Dashboard Action Queue and carries the back-out
//     auto-fix.
//
// It is NOTIFY-ONLY: the caller ALWAYS proceeds with its write/push. Aliases
// and collaborations legitimately share promo art, so a hard block would
// false-positive; the operator-triggered back-out is the safety valve.
//
// This lives in its own package so the write paths (internal/api, internal/rule)
// and the outbound path (internal/publish) can all share one seam without an
// import cycle. It depends only on internal/event and internal/image, and
// receives its rule-side (violation upsert) and artist-side (name lookup)
// collaborators as plain function values, so it never imports internal/rule or
// internal/artist.
package collision

import (
	"context"
	"fmt"
	"log/slog"
	"math"

	"github.com/sydlexius/stillwater/internal/event"
	"github.com/sydlexius/stillwater/internal/image"
)

// DefaultTolerance is the minimum perceptual similarity at which a candidate
// fanart phash is treated as colliding with another artist's fanart. It is the
// same 0.90 / Hamming<=6 near-duplicate band the read-only #2564 detector
// derives (internal/rule/phash_mismatch.go: defaultPHashMismatchTolerance),
// restated here rather than imported so this package stays free of
// internal/rule. Callers pass this to image.CompareIdentity.
const DefaultTolerance = 0.90

// EventPublisher is the subset of *event.Bus the notifier needs. *event.Bus
// satisfies it; tests pass a fake that records published events.
type EventPublisher interface {
	Publish(event.Event)
}

// ViolationRaiseFunc persists the durable, fixable rule_violations row for a
// detected collision. The wiring supplies a closure over
// rule.Service.RaiseBackdropCollision so this package never imports
// internal/rule. It returns an error only so the notifier can log a failure;
// the caller's write/push is never affected.
type ViolationRaiseFunc func(ctx context.Context, destArtistID, destArtistName, message, collidingArtistID string) error

// ArtistNameFunc resolves an artist id to its display name for the message and
// the toast payload. It returns "" when the artist cannot be resolved; the
// notifier then falls back to the raw id.
type ArtistNameFunc func(ctx context.Context, artistID string) string

// Notifier emits both #2540 notifications on a detected collision.
type Notifier struct {
	pub    EventPublisher
	raise  ViolationRaiseFunc
	nameOf ArtistNameFunc
	logger *slog.Logger
}

// NewNotifier builds a Notifier. Any collaborator may be nil: a nil publisher
// skips the toast, a nil raise func skips the durable entry, a nil name func
// falls back to the colliding artist id. A nil *Notifier is itself a safe
// no-op (see Notify) so wiring never has to special-case the headless/test
// paths.
func NewNotifier(pub EventPublisher, raise ViolationRaiseFunc, nameOf ArtistNameFunc, logger *slog.Logger) *Notifier {
	if logger == nil {
		logger = slog.Default()
	}
	return &Notifier{pub: pub, raise: raise, nameOf: nameOf, logger: logger}
}

// Notify emits the ephemeral toast and upserts the durable fixable violation
// for a cross-artist backdrop collision. It acts ONLY on IdentityMismatch;
// IdentityMatch and IdentityIndeterminate (the fail-open verdicts) produce
// nothing. It never returns an error and never blocks the caller's write/push:
// a failure to raise the durable entry is logged, not propagated.
func (n *Notifier) Notify(ctx context.Context, destArtistID, destArtistName string, res image.IdentityResult) {
	if n == nil || res.Verdict != image.IdentityMismatch {
		return
	}

	collidingName := ""
	if n.nameOf != nil {
		collidingName = n.nameOf(ctx, res.CollidingArtistID)
	}
	label := collidingName
	if label == "" {
		label = res.CollidingArtistID
	}

	pct := int(math.Round(res.Similarity * 100))
	msg := fmt.Sprintf("Backdrop matches %s (%d%% similar, %d artists) - possible cross-artist pollution",
		label, pct, res.MatchCount)

	if n.pub != nil {
		n.pub.Publish(event.Event{
			Type: event.BackdropCollision,
			Data: map[string]any{
				"dest_artist_id":        destArtistID,
				"dest_artist_name":      destArtistName,
				"colliding_artist_id":   res.CollidingArtistID,
				"colliding_artist_name": collidingName,
				"similarity":            pct,
				"match_count":           res.MatchCount,
				"message":               msg,
			},
		})
	}

	if n.raise != nil {
		if err := n.raise(ctx, destArtistID, destArtistName, msg, res.CollidingArtistID); err != nil {
			n.logger.Error("raising backdrop-collision violation",
				slog.String("artist_id", destArtistID),
				slog.String("colliding_artist_id", res.CollidingArtistID),
				slog.String("error", err.Error()))
		}
	}
}
