package conflict

import (
	"context"
	"errors"
	"fmt"
)

// BlockedError is returned by Gate methods when a write must be refused
// because the ledger shows a conflict. It carries enough context for
// callers to render a 409 JSON body and for the UI to echo the cause.
type BlockedError struct {
	// Axis is the gate axis that blocked the write (image, nfo, or
	// round_trip). Surfaces to the API caller in the error code.
	Axis Axis
	// Reason is a short human-readable explanation, safe for logs and for
	// inclusion in the 409 body.
	Reason string
	// Ledger is the snapshot that produced the block. Callers should
	// marshal this into the 409 body so the UI can render the same banner
	// state the server used for the decision.
	Ledger Ledger
}

func (e *BlockedError) Error() string {
	return fmt.Sprintf("write blocked on axis %s: %s", e.Axis, e.Reason)
}

// IsBlocked reports whether err is a BlockedError (or wraps one). Useful in
// handlers that bubble errors up multiple layers before deciding the HTTP
// status.
func IsBlocked(err error) bool {
	var be *BlockedError
	return errors.As(err, &be)
}

// AsBlocked returns the BlockedError wrapped by err if present. Useful in
// handlers that want to read the axis or ledger for the 409 payload.
func AsBlocked(err error) (*BlockedError, bool) {
	var be *BlockedError
	if errors.As(err, &be) {
		return be, true
	}
	return nil, false
}

// Gate enforces the conflict ledger at write time. AllowImageWrite and
// AllowNFOWrite return nil to permit the write or a *BlockedError to refuse
// it. The gate reads the cached ledger from the detector; callers wanting
// an up-to-the-second check should Invalidate first.
type Gate struct {
	detector *Detector
}

// NewGate constructs a gate wired to the given detector.
func NewGate(d *Detector) *Gate {
	return &Gate{detector: d}
}

// AllowImageWrite returns nil if Stillwater may write image files to disk
// right now, or a *BlockedError if any enabled unmanaged connection has
// image saving on or any round-trip overlap exists.
func (g *Gate) AllowImageWrite(ctx context.Context) error {
	l := g.detector.Current(ctx)
	if len(l.RoundTrips) > 0 {
		return &BlockedError{
			Axis:   AxisRoundTrip,
			Reason: fmt.Sprintf("library paths overlap between %d connection pair(s); any image write reaches multiple servers on shared disk", len(l.RoundTrips)),
			Ledger: l,
		}
	}
	if l.AnyImageConflict() {
		return &BlockedError{
			Axis:   AxisImage,
			Reason: "one or more enabled connections have server-side image saving on; the server would duplicate Stillwater's artwork files. Flip 'Let Stillwater manage' or disable the saver on the peer.",
			Ledger: l,
		}
	}
	return nil
}

// AllowNFOWrite is the NFO-axis equivalent of AllowImageWrite.
func (g *Gate) AllowNFOWrite(ctx context.Context) error {
	l := g.detector.Current(ctx)
	if len(l.RoundTrips) > 0 {
		return &BlockedError{
			Axis:   AxisRoundTrip,
			Reason: fmt.Sprintf("library paths overlap between %d connection pair(s); any NFO write reaches multiple servers on shared disk", len(l.RoundTrips)),
			Ledger: l,
		}
	}
	if l.AnyNFOConflict() {
		return &BlockedError{
			Axis:   AxisNFO,
			Reason: "one or more enabled connections have server-side NFO saving on; the server would rewrite Stillwater's artist.nfo. Flip 'Let Stillwater manage' or disable the saver on the peer.",
			Ledger: l,
		}
	}
	return nil
}
