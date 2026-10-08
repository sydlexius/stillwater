package publish

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"
)

// FanartHealthReporter is told, after each full-set fanart snapshot, whether
// every local backdrop could be read (#3200). *rule.Service implements it, and
// the method set matches that type exactly so no adapter is needed; publish must
// not import rule, so the interface lives here.
//
// slots are 0-based local backdrop indexes (the snapshot's own numbering). They
// are never shown to the operator: the rule service only validates them (a
// non-empty, in-range list). reason is the complete operator-facing message and
// names files by base name.
type FanartHealthReporter interface {
	RaiseFanartUnreadable(ctx context.Context, artistID string, slots []int, reason string) error
	ResolveFanartUnreadable(ctx context.Context, artistID string) error
}

// SetFanartHealthReporter wires the reporter. Call it once at startup, before
// the publisher is used. A nil reporter is a supported state (the snapshot is
// simply not reported); the caller that leaves it unwired is expected to say so
// once at wiring time rather than have every push log about it.
func (p *Publisher) SetFanartHealthReporter(r FanartHealthReporter) {
	if p != nil {
		p.fanartHealth = r
	}
}

// maxReportedFanartNames bounds how many file names go into the operator-facing
// reason before "and N more".
const maxReportedFanartNames = 10

// fanartReportTimeout bounds the report so a stuck database cannot hold the
// per-artist gate (and with it the next push for that artist) for long. A var so
// a test can shorten it.
var fanartReportTimeout = 3 * time.Second

// fanartGateWait bounds how long a pass waits for the artist's gate behind an
// earlier pass. A var so a test can shorten it. The wait also ends when the
// caller's context does.
var fanartGateWait = 3 * time.Second

// fanartSnapshotTakenHook, when set, runs inside snapshotFanartAndReport right
// after the snapshot is taken and before it is reported. nil in production; a
// test parks a pass here to prove the snapshot is taken under the lock.
var fanartSnapshotTakenHook func(artistID string)

// fanartNameList renders at most maxReportedFanartNames base names (never a
// path), then "and N more".
func fanartNameList(names []string) string {
	if len(names) <= maxReportedFanartNames {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(names[:maxReportedFanartNames], ", "), len(names)-maxReportedFanartNames)
}

// fanartHealthReason builds the whole operator-facing sentence for the finding
// (#3469). Files are identified by base name only: a position number does not
// match the name (fanart2.jpg can sit at position 3), so none is shown. The two
// kinds get their own clause because they mean different things: an unreadable
// file is a fault to fix, while a file left out by the snapshot budget is
// readable and only waiting for a smaller set. Each clause has its own
// singular and plural form.
func fanartHealthReason(unreadable, skipped []string) string {
	var clauses []string
	if n := len(unreadable); n > 0 {
		if n == 1 {
			clauses = append(clauses, "1 backdrop file could not be read, so Stillwater is not sending it to your media servers: "+
				fanartNameList(unreadable)+". Check that the file exists, can be read and is not too large.")
		} else {
			clauses = append(clauses, fmt.Sprintf("%d backdrop files could not be read, so Stillwater is not sending them to your media servers: %s. Check that each file exists, can be read and is not too large.",
				n, fanartNameList(unreadable)))
		}
	}
	if n := len(skipped); n > 0 {
		if n == 1 {
			clauses = append(clauses, "1 backdrop file was left out of this push because the set is over the size or count limit for one push: "+
				fanartNameList(skipped)+". The file itself may be fine.")
		} else {
			clauses = append(clauses, fmt.Sprintf("%d backdrop files were left out of this push because the set is over the size or count limit for one push: %s. The files themselves may be fine.",
				n, fanartNameList(skipped)))
		}
	}
	return strings.Join(clauses, " ")
}

// snapshotFanartAndReport is snapshotFanart for a snapshot of the artist's FULL
// backdrop set, followed by telling the health reporter what it found. Every
// caller that snapshots the whole set to push it (or to decide whether to) goes
// through here, so the reporting logic exists once.
//
// A snapshot that ERRORED (cancel, stalled mount) reports nothing, even if it
// holds partial data: it is not evidence about the files, so it neither raises
// nor resolves and an open finding stays open. Otherwise any nil-data slot
// raises with the FULL current list of such slots, and none resolves. An empty
// path list (the artist has no backdrop left) resolves. The reason names the
// files by base name: a slot number does not identify a file under the Kodi
// naming (fanart.jpg, fanart1.jpg) or after a gap. A slot skipped only by the
// snapshot budget is readable, so it gets its own wording.
//
// Resolve runs on every clean pass on purpose: whether a finding is open is
// persisted state that an in-memory "was open" flag would lose on restart. A
// finding is only cleared by a pass that gets here: the reconciler reaches the
// snapshot only when the server holds fewer backdrops than there are files, so
// it can leave a fixed file's finding open until the next push.
//
// THE PER-ARTIST GATE is what makes snapshot-then-report atomic. The rule
// service stamps a resolve with the clock at the time of the call, so without
// it a clean snapshot taken BEFORE a newer failing one, but reported AFTER it
// (a manual sync overlapping the reconciler), would resolve a finding the newer
// snapshot had just raised. Held only for the snapshot and the report, never
// across a peer write. No other lock is taken while holding it (neither
// snapshotFanart nor the reporter touches lockPhashTarget), and a caller that
// holds a phash target lock takes this one second, so the order cannot cycle.
//
// THE WAIT FOR THE GATE IS CANCELABLE AND BOUNDED (caller's context, and
// fanartGateWait). A pass that gives up waiting (a stalled reporter ahead of it)
// still takes its snapshot and the push goes on exactly as before, but it does
// NOT report: it is no longer ordered against the pass ahead, and an unordered
// report is the stale-resolve seam the gate exists to close. One warn line names
// the artist. Passes that do hold the gate keep the ordering guarantee.
//
// A reporter failure is logged and never reaches the caller: it must not fail
// or shorten the sync.
func (p *Publisher) snapshotFanartAndReport(ctx context.Context, artistID string, fanartPaths []string) ([]fanartSnapshot, []string, error) {
	if p.fanartHealth == nil {
		return p.snapshotFanart(ctx, fanartPaths)
	}
	gate := p.fanartReportGate(artistID)
	if !acquireFanartGate(ctx, gate) {
		p.logger.Warn("not reporting unreadable fanart for this pass: gave up waiting behind an earlier pass for the same artist",
			slog.String("artist_id", artistID))
		return p.snapshotFanart(ctx, fanartPaths)
	}
	defer func() { <-gate }()

	snapshot, warnings, err := p.takeFanartSnapshot(ctx, artistID, fanartPaths)
	if err != nil {
		return snapshot, warnings, err
	}
	var slots []int
	var unreadable, skipped []string
	for _, sf := range snapshot {
		if sf.data != nil {
			continue
		}
		slots = append(slots, sf.index)
		if sf.skipped {
			skipped = append(skipped, filepath.Base(sf.path))
		} else {
			unreadable = append(unreadable, filepath.Base(sf.path))
		}
	}
	// The snapshot is already complete, so the report must not die with a request
	// that ends right after it: a lost raise would leave the finding stale until
	// the next push. WithoutCancel keeps the values, drops the cancellation.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fanartReportTimeout)
	defer cancel()
	if len(slots) > 0 {
		reason := fanartHealthReason(unreadable, skipped)
		if rerr := p.fanartHealth.RaiseFanartUnreadable(rctx, artistID, slots, reason); rerr != nil {
			p.logger.Warn("could not record the unreadable fanart finding",
				slog.String("artist_id", artistID), slog.Any("slots", slots), slog.Any("error", rerr))
		}
	} else if rerr := p.fanartHealth.ResolveFanartUnreadable(rctx, artistID); rerr != nil {
		p.logger.Warn("could not clear the unreadable fanart finding",
			slog.String("artist_id", artistID), slog.Any("error", rerr))
	}
	return snapshot, warnings, nil
}

// takeFanartSnapshot is snapshotFanart plus the test seam, kept as ONE step so a
// pass parked in the seam has taken its snapshot and a reordering of the lock
// around this call is visible to a test.
func (p *Publisher) takeFanartSnapshot(ctx context.Context, artistID string, fanartPaths []string) ([]fanartSnapshot, []string, error) {
	snapshot, warnings, err := p.snapshotFanart(ctx, fanartPaths)
	if fanartSnapshotTakenHook != nil {
		fanartSnapshotTakenHook(artistID)
	}
	return snapshot, warnings, err
}

// fanartReportGate returns the artist's snapshot-and-report gate: a channel with
// room for one holder. Entries are one small channel per artist ever pushed and
// are never removed, like phashTargetLocks.
func (p *Publisher) fanartReportGate(artistID string) chan struct{} {
	g, _ := p.fanartReportLocks.LoadOrStore(artistID, make(chan struct{}, 1))
	return g.(chan struct{})
}

// acquireFanartGate takes the gate, or reports false when ctx ends or
// fanartGateWait passes first.
func acquireFanartGate(ctx context.Context, gate chan struct{}) bool {
	select {
	case gate <- struct{}{}:
		return true
	default:
	}
	t := time.NewTimer(fanartGateWait)
	defer t.Stop()
	select {
	case gate <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	case <-t.C:
		return false
	}
}
