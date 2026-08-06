// This file adds the peer's SCAN-STATE surface: the answer to "is a library
// scan running on this server right now?".
//
// It exists because every other signal in this package is a snapshot of an
// index the peer rebuilds ASYNCHRONOUSLY, and a snapshot taken mid-rebuild is
// indistinguishable from a snapshot of a finished one. TriggerLibraryScanRaw
// is fire-and-forget (POST /Library/Refresh, no job id, no completion
// callback), so before this there was no way to know whether a scan the caller
// itself asked for had finished -- only how long the caller had waited, which
// is not the same thing and never becomes the same thing no matter how long
// the wait.
//
// That gap is why the post-move relink cannot safely drop a peer link: "the
// item is gone" and "the item is mid-scan" are the same observation, and a
// timeout cannot separate them (see internal/publish/relink.go's
// relinkPollBudget). This file supplies the missing discriminator so a future
// caller can require "the scan is DONE" as positive evidence before treating
// an absence as real. It deliberately ships with no consumers.
package mediabrowser

import (
	"context"
	"fmt"
	"strings"
)

// scheduledTasksPath is the scheduled-task list on both Emby and Jellyfin.
// The endpoint and the response shape agree across the two servers (both
// descend from the same Media Browser API), unlike the item-refresh query
// string that TriggerArtistRefreshRaw takes per-platform. Should they ever
// diverge, follow that precedent -- pass the differing piece in from the
// platform package rather than branching on a platform string here.
const scheduledTasksPath = "/ScheduledTasks"

// libraryScanTaskKeys are the task Keys that mean "the library is being
// rebuilt". RefreshLibrary is the full-library scan POST /Library/Refresh
// starts, on both servers.
//
// Matched by Key, never by Name: Name is human-facing, localized, and
// re-worded between releases ("Scan media library" / "Scan Media Library"),
// whereas Key is the server's stable programmatic identifier. A check that
// matched on Name would silently stop matching for any non-English server and
// report "idle" for a scan that is very much running -- a fail-OPEN answer to
// the one question this file exists to answer safely.
var libraryScanTaskKeys = map[string]bool{
	"RefreshLibrary": true,
}

// scheduledTask is the subset of the peer's ScheduledTaskInfo this package
// reads. The full payload carries triggers, category, description, and last
// execution result; none of that bears on "is it running now", so it is not
// decoded.
type scheduledTask struct {
	Key   string `json:"Key"`
	State string `json:"State"`
	// Name is decoded but DELIBERATELY never matched on. It exists so a test
	// can build a fixture where Name and Key disagree and prove the matcher
	// ignores Name -- without the field, a Key-vs-Name test cannot express the
	// case it claims to cover (#2426 review).
	Name string `json:"Name"`
}

// taskStateIdle is the peer's State value for a task that is not running, and
// the ONLY value this package treats as idle.
//
// The other values observed in the wild are taskStateRunning and
// taskStateCancelling; both mean the scan has not finished, and both are
// treated as NOT idle -- not by being listed as exclusions, but by simply
// failing to match this one. That direction matters: a value neither constant
// anticipates (a future server release) also fails to match, and so is also
// not idle.
const taskStateIdle = "Idle"

// taskStateRunning and taskStateCancelling are the peer's non-idle states.
// They are declared for documentation and for the tests to reference rather
// than being matched against in production code -- see taskStateIdle for why
// the check is a positive match on idle instead of an exclusion list.
//
// The doubled-L spelling is the SERVER'S, carried verbatim off the wire; the
// misspell linter flags it as the British form, which is correct about English
// and wrong about this identifier. Renaming it to the American spelling would
// make the constant stop describing the value it names.
const (
	taskStateRunning = "Running"
	//nolint:misspell // "Cancelling" is the peer's literal wire value, not prose.
	taskStateCancelling = "Cancelling"
)

// LibraryScanIdleRaw reports whether the peer currently has NO library scan
// running.
//
// (idle, err). A non-nil error means the question was NOT ANSWERED, and idle
// is false in that case -- callers must not read the bool without checking the
// error first, and the false is chosen so that a caller which ignores the error
// anyway still gets the safe answer.
//
// FAIL-CLOSED BY CONSTRUCTION. Every path that does not positively observe an
// idle scan task returns not-idle:
//
//   - transport error, auth failure, non-2xx  -> error, not idle
//   - a decode failure                        -> error, not idle
//   - the scan task present and Running       -> not idle, no error
//   - the scan task present and Idle          -> IDLE
//   - the scan task ABSENT from the list      -> not idle, ERROR
//
// The last case is the subtle one and it is why this returns an error rather
// than optimistically saying "no scan task listed, so nothing is scanning". A
// missing task means the peer did not tell us what we asked -- an older or
// customized server, a permission-filtered list, a renamed key -- and treating
// silence as "idle" would hand a caller a confident green light derived from
// no evidence at all. That is exactly the "a zero renders as clean" trap
// documented in internal/dupimages/cache.go: the absence of a reported problem
// is not the presence of a verified answer.
//
// Idle is therefore a POSITIVE ALLOW-LIST of one state: the scan task exists
// and the peer says it is Idle. Everything else is "not proven idle", which a
// destructive caller must treat as "do not act".
//
// NOTE ON WHAT IDLE DOES AND DOES NOT PROVE: idle means no scan is running AT
// THIS MOMENT. It does not prove a scan ever ran, nor that the caller's own
// earlier trigger has already been picked up -- a scan requested a moment ago
// may not have started yet, so an immediate check can read Idle. A caller
// needing "my scan finished" must observe the transition (busy, then idle),
// not a single idle reading. Stating this here because a caller that mistakes
// one idle sample for "my scan completed" would reintroduce precisely the
// unsound inference this primitive exists to eliminate.
func LibraryScanIdleRaw(ctx context.Context, t Transport, classifyAuth AuthErrorClassifier) (bool, error) {
	var tasks []scheduledTask
	if err := t.Get(ctx, scheduledTasksPath, &tasks); err != nil {
		return false, fmt.Errorf("reading scheduled tasks: %w", classifyAuth(err))
	}

	for i := range tasks {
		if !libraryScanTaskKeys[tasks[i].Key] {
			continue
		}
		// Compared case-insensitively: the servers have not been consistent
		// about casing across versions, the same reason TypeMusicArtist is
		// matched that way. Any state that is not idle -- the two known
		// non-idle constants above, OR a value added by a future release --
		// counts as NOT idle, which is the safe direction for an unrecognized
		// value.
		return strings.EqualFold(strings.TrimSpace(tasks[i].State), taskStateIdle), nil
	}

	return false, fmt.Errorf("no library-scan task (%s) in the peer's scheduled-task list, so scan state is unknown",
		strings.Join(sortedTaskKeys(), ", "))
}

// sortedTaskKeys renders libraryScanTaskKeys for the not-found error message.
// A plain map range would order non-deterministically, which would make the
// error text differ run to run; there is one key today, and this keeps the
// message stable if more are added.
func sortedTaskKeys() []string {
	keys := make([]string, 0, len(libraryScanTaskKeys))
	for k := range libraryScanTaskKeys {
		keys = append(keys, k)
	}
	// Insertion into a fixed small set; sort for determinism.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}
